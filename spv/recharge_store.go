package spv

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// The recharge store is the replay-side replacement for the flat
// spvTransactiondb key-value layout ("<hash>Fee", "<hash>Address",
// "<hash>Output", "<hash>Input" plus one 8-byte-height bucket per failed list).
//
// Why it has to be rebuilt rather than wrapped
// --------------------------------------------
// pgp-chain wrote those keys from the SPV listener as it processed ELA blocks,
// so a key existed or did not depending on how far the node had synced at the
// moment of the read. Six execution-path read points consume them:
//
//	#3 FindOutputFeeAndaddressByTxHash   the esc address / amount / fee
//	#4 FindOutRechargeInput              the recharge memo
//	#5 GetRechargeDataByTxhash           all outputs of a recharge
//	#6 IsFailedElaTx (in-memory list)    the failedTxList half
//	#7 IsFailedElaTx (KV scan)           the persisted half
//	#8 SavePayloadInfo's "already seen"  the spvTxhash short circuit (listener
//	                                     side, not a consensus read, but it has
//	                                     to key on the same (height, hash))
//
// All of them answer "what did the ELA chain say about this tx", and none of them
// said *when*. Keying by tx hash alone cannot be replayed: the same hash can be
// unknown at one point of the chain's history and known later, and a node
// replaying block N must see exactly what the producing node saw at block N.
//
// So every record is stored under (ELA height, tx hash) and read with an
// "as of this height" rule: the newest record written at a height <= the block's
// ELA anchor. The anchor is header.Nonce (pgp-chain/consensus/pbft/pbft.go:492
// stores types.EncodeNonce(GetSpvHeight()) there), which
// core/evm.go hands to vm.BlockContext.Nonce and state_transition passes down.
//
// Two failure modes this deliberately refuses
// -------------------------------------------
//  1. "No record" must never look like "record with zero values". Every read
//     returns ErrRechargeUnavailable rather than an empty RechargeDatas; the old
//     signature of FindOutputFeeAndaddressByTxHash returned (0, 0x0, 0) on every
//     error path, which is indistinguishable from a legitimate zero-amount
//     recharge.
//  2. "Store not advanced to this height" must never look like "no records at
//     this height". A node whose SPV listener is at ELA height 500 answering a
//     query for height 4000 would otherwise report every tx as unknown and every
//     tx as not-failed, i.e. silently build a different state root. The
//     covered-through marker turns that into a hard error on the exact block.
//
// Why the 0x3F1 precompile reads this store and never the network
// --------------------------------------------------------------
// This is the justification for deviating from pgp-chain, and it is the reason
// block 30115 can be reproduced at all. pgp-chain's getMainChainRechargeData
// (pgp-chain/core/vm/contracts.go:1490-1497) starts with
//
//	if err := spv.TrySetRechargeDataFromSpvService(hash); err != nil { ... }
//
// and TrySetRechargeDataFromSpvService (pgp-chain/spv/recharge.go:101-123) falls
// through to SpvService.GetTransaction(txID) — a network fetch from the ELA
// chain, which may also write the result back into the local database. A
// consensus-path network read cannot be replayed.
//
// The fallback is gated, though, and the gate is what makes this safe:
//
//	fee, addr, output := FindOutputFeeAndaddressByTxHash(elaHash)
//	if fee <= 0 && output <= 0 && addr == blackAddr {
//		// only now does the network query happen
//	}
//
// FindOutputFeeAndaddressByTxHash (pgp-chain/spv/spv_module.go:762-807) is a pure
// KV read of the same four keys GetRechargeDataByTxhash reads. So:
//
//	| local data present | pgp behaviour                          | ours                     | equivalent? |
//	|--------------------|----------------------------------------|--------------------------|-------------|
//	| yes                | gate not taken -> no-op -> read KV     | read store               | yes         |
//	| no                 | network fetch; on miss it errors       | ErrRechargeUnavailable   | no, but this is the only reproducible answer |
//
// The "no" row is the whole deviation: where pgp would have gone to the network
// we fail. It fails loudly, and only for a block whose data the producing node
// also had locally, because a producing node that had to fetch over the network
// would be non-deterministic against every other node and could not have agreed
// on the same stateRoot in the first place.
//
// CONSEQUENCE FOR IMPLEMENTERS: the 0x3F1 precompile must call
// CurrentRechargeView().Recharge(hash) and nothing else. TrySetRechargeDataFromSpvService
// is deliberately NOT ported: it was the network fallback itself, it was only ever
// called from the precompile, and leaving it on the package surface would be an
// open invitation to reintroduce the network read. The listener instead fills the
// store from the ELA blocks it already has, via ApplyElaBlock.

var (
	// ErrRechargeStoreNotInitialised is returned when no store has been installed.
	ErrRechargeStoreNotInitialised = errors.New("spv: recharge store is not initialised")

	// ErrRechargeHeightNotCovered is returned when the store has not been
	// advanced as far as the ELA height being asked about. It is an error and
	// not an empty answer on purpose; see the failure modes above.
	ErrRechargeHeightNotCovered = errors.New("spv: recharge store has not been advanced to this ELA height")

	// ErrRechargeUnavailable is returned when the store is covered at the
	// requested height but holds no record for the tx.
	ErrRechargeUnavailable = errors.New("spv: no recharge record for this ELA tx at this ELA height")

	// ErrRechargeElaTxFailed is returned by Recharge for an ELA tx that was
	// returned or failed. pgp-chain returned errors.New("is failed elaTx: ...")
	// here; keeping it as a distinct value lets the caller tell "this tx is
	// known-bad" from "this tx is unknown".
	ErrRechargeElaTxFailed = errors.New("spv: ELA tx is marked failed at this ELA height")

	// ErrRechargeStoreCorrupt is returned when an index entry has no matching
	// data record, which means the store was not written atomically.
	ErrRechargeStoreCorrupt = errors.New("spv: recharge store index is corrupt")
)

// Store key layout. Every dataset is (prefix, ELA height big endian, tx hash) so
// that a prefix scan yields the dataset in ascending ELA-height order, which is
// what makes the "as of height H" walk a forward scan with an early break.
//
// No prefix may be a prefix of another, otherwise NewIterator(prefix, nil)
// would spill into the neighbouring dataset.
var (
	keyRechargeData   = []byte("spv/recharge/v1/data/")
	keyRechargeIndex  = []byte("spv/recharge/v1/ridx/")
	keyFailedData     = []byte("spv/recharge/v1/fail/")
	keyFailedIndex    = []byte("spv/recharge/v1/fidx/")
	keyCoveredThrough = []byte("spv/recharge/v1/covered")
	keyElaBlockHash   = []byte("spv/recharge/v1/elablk/")
)

// elaBlockRetention is how many recent (ELA height -> ELA block hash) entries are
// kept for reorg forensics. Only the last one is actually needed for detection,
// the window just makes a post-mortem easier.
const elaBlockRetention = 1024

// storedRecharge is the RLP form of RechargeDatas. The values are already scaled
// to wei (pgp-chain multiplied the ELA fixed64 values by `rate` at read time);
// scaling at write time means the multiplication happens once instead of at
// every read.
type storedRecharge struct {
	Addresses []common.Address
	Amounts   []*big.Int
	Fees      []*big.Int
	Memos     [][]byte
}

// RechargeStore is an ELA-height-indexed store of recharge records and of the
// "this ELA tx failed" marks.
//
// It is safe for concurrent use: every mutation goes through a single batch, and
// the covered-through marker moves monotonically.
type RechargeStore struct {
	db ethdb.KeyValueStore

	// writeMu serialises the coverage marker update. Without it two concurrent
	// writers could each read the old marker and the later batch could move it
	// backwards, which would turn valid reads into ErrRechargeHeightNotCovered.
	writeMu sync.Mutex
}

// NewRechargeStore wraps a key-value store. The store is expected to be the same
// leveldb instance the SPV listener writes to, so that a single fsync covers the
// record, its index and the coverage marker.
func NewRechargeStore(db ethdb.KeyValueStore) *RechargeStore {
	return &RechargeStore{db: db}
}

var (
	rechargeStoreMu sync.RWMutex
	rechargeStore   *RechargeStore
)

// SetRechargeStore installs the process-wide store. Passing nil removes it.
func SetRechargeStore(s *RechargeStore) {
	rechargeStoreMu.Lock()
	defer rechargeStoreMu.Unlock()
	rechargeStore = s
}

// GetRechargeStore returns the installed store, or nil.
func GetRechargeStore() *RechargeStore {
	rechargeStoreMu.RLock()
	defer rechargeStoreMu.RUnlock()
	return rechargeStore
}

// InstallHeightView pins the legacy state view to elaHeight, i.e. publishes a
// view that answers every LegacyStateView read as of that height.
//
// CALLER CONTRACT (core/):
//
//   - WHEN: once per block, from the block execution entry point, after the
//     header is known and the EVM of that block has not yet executed a single
//     transaction. The argument is uint32(header.Nonce), i.e. the ELA height the
//     block was produced against (pgp-chain stores types.EncodeNonce(GetSpvHeight())
//     there, consensus/pbft/pbft.go:492).
//   - UNTIL WHEN: pair every call with a deferred UninstallHeightView(), so the
//     window in which a stale view is visible is the block execution itself.
//   - CONCURRENCY: this is a process-wide slot, so two blocks must not be
//     executed concurrently under different heights. The *view* is a value type
//     with no mutable state, so concurrent reads through one installed view are
//     fine; it is the install/uninstall window that is exclusive.
//   - IF FORGOTTEN: the previous block's view stays installed and every read
//     answers with the previous ELA height, silently and with no error. That is
//     the one failure mode this API cannot detect; it is why Uninstall is part of
//     the contract and why CurrentRechargeView exists.
//   - RETURN: ErrRechargeStoreNotInitialised when SpvDbInit has not run yet.
func InstallHeightView(elaHeight uint32) error {
	s := GetRechargeStore()
	if s == nil {
		return ErrRechargeStoreNotInitialised
	}
	SetStateView(s.HeightView(elaHeight))
	return nil
}

// UninstallHeightView clears the installed view, which makes
// CurrentRechargeView return nil and every precompile fail with
// ErrLegacyStateViewMissing instead of reading a stale height.
//
// It is safe to call when nothing is installed. Uninstall alone does not touch
// the store, so a listener writing in the background is unaffected.
func UninstallHeightView() {
	SetStateView(nil)
}

// HeightView returns a LegacyStateView that answers as of elaHeight.
func (s *RechargeStore) HeightView(elaHeight uint32) LegacyStateView {
	return heightScopedView{store: s, height: elaHeight}
}

// heightScopedView binds a store to one ELA height. It holds no mutable state,
// so two blocks can be executed with two different views concurrently.
type heightScopedView struct {
	store  *RechargeStore
	height uint32
}

func (v heightScopedView) ELAHeight() (uint32, error) {
	if v.store == nil {
		return 0, ErrRechargeStoreNotInitialised
	}
	return v.height, nil
}

func (v heightScopedView) Recharge(txHash string) (RechargeDatas, *big.Int, error) {
	if v.store == nil {
		return nil, nil, ErrRechargeStoreNotInitialised
	}
	return v.store.RechargeAt(uint64(v.height), txHash)
}

func (v heightScopedView) IsFailedElaTx(txHash string) (bool, error) {
	if v.store == nil {
		return false, ErrRechargeStoreNotInitialised
	}
	return v.store.IsFailedElaTxAt(uint64(v.height), txHash)
}

// ---------------------------------------------------------------------------
// keys
// ---------------------------------------------------------------------------

func storeKey(prefix []byte, hash common.Hash, height uint64) []byte {
	key := make([]byte, 0, len(prefix)+32+8)
	key = append(key, prefix...)
	key = append(key, hash[:]...)
	var h [8]byte
	binary.BigEndian.PutUint64(h[:], height)
	return append(key, h[:]...)
}

// normaliseElaHash accepts the forms that reach this package from calldata
// (with or without 0x) and rejects anything that is not exactly 32 bytes of hex,
// so a truncated hash can never alias another record.
func normaliseElaHash(s string) (common.Hash, error) {
	s = trimHexPrefix(s)
	if len(s) != 64 {
		return common.Hash{}, fmt.Errorf("spv: ELA tx hash %q is not 32 bytes", s)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return common.Hash{}, fmt.Errorf("spv: ELA tx hash %q is not hex: %w", s, err)
	}
	return common.HexToHash(s), nil
}

// ---------------------------------------------------------------------------
// coverage
// ---------------------------------------------------------------------------

// coverage reads the covered-through marker.
//
// found is false when the store has never been advanced, which is different from
// "advanced to height 0": the former means the listener has applied nothing and
// must refuse every query, the latter means ELA block 0 was applied and height 0
// is a legitimate answer.
func (s *RechargeStore) coverage() (height uint64, found bool, err error) {
	ok, err := s.db.Has(keyCoveredThrough)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	value, err := s.db.Get(keyCoveredThrough)
	if err != nil {
		return 0, false, err
	}
	if len(value) != 8 {
		return 0, false, fmt.Errorf("%w: covered marker is %d bytes", ErrRechargeStoreCorrupt, len(value))
	}
	return binary.BigEndian.Uint64(value), true, nil
}

// CoveredThrough reports the highest ELA height whose records have been fully
// applied, and whether the store has been advanced at all.
func (s *RechargeStore) CoveredThrough() (height uint64, covered bool, err error) {
	return s.coverage()
}

// AdvanceCoverage records that every recharge record in the ELA block at
// elaHeight has been written, and remembers the block hash so a later ELA reorg
// leaves a trace.
//
// ORDER MATTERS: this must be called *after* every PutRecharge for that block,
// not when the block is received. "Block received" and "block's recharge txs
// written" are different states, and advancing on the former would let the
// precompile observe "covered" with missing records, i.e. degrade
// ErrRechargeHeightNotCovered into ErrRechargeUnavailable — the exact confusion
// the coverage marker exists to prevent. ApplyElaBlock does this correctly.
//
// The marker moves forward only. ELA reorgs cannot rewind it, so after a reorg
// the store may serve pre-reorg data; see the warning emitted when the same
// height is seen with a different hash.
func (s *RechargeStore) AdvanceCoverage(elaHeight uint64, elaBlockHash common.Hash) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.rememberElaBlock(elaHeight, elaBlockHash); err != nil {
		return err
	}
	covered, found, err := s.coverage()
	if err != nil {
		return err
	}
	if found && elaHeight <= covered {
		return nil
	}
	if err := s.putCovered(elaHeight); err != nil {
		return err
	}
	// Rolling prune of the block-hash window. Only heights strictly below the
	// retention window are removed, so a restart never loses the tip.
	if elaHeight > elaBlockRetention {
		_ = s.db.Delete(storeKey(keyElaBlockHash, common.Hash{}, elaHeight-elaBlockRetention))
	}
	return nil
}

// rememberElaBlock stores the ELA block hash for elaHeight and warns when the
// same height is written twice with different hashes, which is what an ELA reorg
// looks like from here.
//
// We keep the old data and keep going: the reorg depth on the ELA chain is far
// smaller than on this chain, and a hard rejection here would stop a node on a
// condition it cannot fix. The warning is the deliverable, so that a later
// unexpected BAD BLOCK has an explanation in the log instead of being a mystery.
func (s *RechargeStore) rememberElaBlock(elaHeight uint64, elaBlockHash common.Hash) error {
	if elaBlockHash == (common.Hash{}) {
		return nil
	}
	key := storeKey(keyElaBlockHash, common.Hash{}, elaHeight)
	ok, err := s.db.Has(key)
	if err != nil {
		return err
	}
	if ok {
		previous, err := s.db.Get(key)
		if err != nil {
			return err
		}
		if len(previous) == 32 && !bytes.Equal(previous, elaBlockHash[:]) {
			log.Warn("ELA block hash changed for an already applied ELA height; "+
				"the ELA chain reorged and the recharge store is serving pre-reorg data",
				"elaHeight", elaHeight,
				"storedHash", common.BytesToHash(previous),
				"newHash", elaBlockHash)
		}
		return nil
	}
	return s.db.Put(key, elaBlockHash[:])
}

func (s *RechargeStore) putCovered(height uint64) error {
	var h [8]byte
	binary.BigEndian.PutUint64(h[:], height)
	return s.db.Put(keyCoveredThrough, h[:])
}

// requireCovered refuses to answer for an ELA height the store has not reached.
//
// There is deliberately no special case for ELA height 0. pgp-chain had no height
// dimension on the recharge path at all, so there is no height-0 behaviour to
// copy, and inventing one would be the same class of mistake as returning a zero
// value: a block sealed before the first ELA anchor reads the store exactly like
// any other block and gets ErrRechargeHeightNotCovered while the store is empty,
// or ErrRechargeUnavailable once the store is covered — both errors, neither a
// fabrication.
func (s *RechargeStore) requireCovered(elaHeight uint64) error {
	covered, found, err := s.coverage()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: asked for %d, the store has not been advanced at all", ErrRechargeHeightNotCovered, elaHeight)
	}
	if elaHeight > covered {
		return fmt.Errorf("%w: asked for %d, covered through %d", ErrRechargeHeightNotCovered, elaHeight, covered)
	}
	return nil
}

// ---------------------------------------------------------------------------
// writes
// ---------------------------------------------------------------------------

// PutRecharge stores the recharge outputs of an ELA main chain tx observed at
// ELA height elaHeight.
//
// It does not advance the coverage marker; call AdvanceCoverage once every
// recharge tx of that ELA block has been written (ApplyElaBlock does both in the
// right order).
//
// Re-storing the same (height, hash) replaces the record; storing the same hash
// at a later height adds a new version, so a read at an earlier height still
// sees what that height saw.
func (s *RechargeStore) PutRecharge(elaHeight uint64, elaHash string, datas RechargeDatas) error {
	if len(datas) == 0 {
		return errors.New("spv: refusing to store an empty recharge record")
	}
	hash, err := normaliseElaHash(elaHash)
	if err != nil {
		return err
	}
	record := storedRecharge{
		Addresses: make([]common.Address, len(datas)),
		Amounts:   make([]*big.Int, len(datas)),
		Fees:      make([]*big.Int, len(datas)),
		Memos:     make([][]byte, len(datas)),
	}
	for i, d := range datas {
		if d == nil {
			return fmt.Errorf("spv: nil recharge entry %d for %s", i, elaHash)
		}
		if d.TargetAmount == nil || d.Fee == nil {
			return fmt.Errorf("spv: recharge entry %d for %s has a nil amount or fee", i, elaHash)
		}
		record.Addresses[i] = d.TargetAddress
		record.Amounts[i] = new(big.Int).Set(d.TargetAmount)
		record.Fees[i] = new(big.Int).Set(d.Fee)
		record.Memos[i] = d.TargetData
	}
	encoded, err := rlp.EncodeToBytes(&record)
	if err != nil {
		return fmt.Errorf("spv: encoding recharge record for %s: %w", elaHash, err)
	}
	return s.put(keyRechargeData, keyRechargeIndex, elaHeight, hash, encoded)
}

// MarkFailedElaTx records that the ELA main chain tx was returned or failed.
// It is idempotent and, like PutRecharge, does not advance the coverage marker.
func (s *RechargeStore) MarkFailedElaTx(elaHeight uint64, elaHash string) error {
	hash, err := normaliseElaHash(elaHash)
	if err != nil {
		return err
	}
	return s.put(keyFailedData, keyFailedIndex, elaHeight, hash, []byte{1})
}

// put writes the data entry and its index entry in one batch: an index entry
// without its record would make a later read report ErrRechargeStoreCorrupt, and
// a record whose index entry is missing would be invisible.
//
// It deliberately does NOT advance the coverage marker. Coverage means "every
// record of every ELA block up to H has been written", which is only true after
// the last PutRecharge of block H; advancing it per record would expose a
// half-written block as "covered". AdvanceCoverage is the only writer.
func (s *RechargeStore) put(dataPrefix, indexPrefix []byte, elaHeight uint64, hash common.Hash, value []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	batch := s.db.NewBatch()
	if err := batch.Put(storeKey(dataPrefix, hash, elaHeight), value); err != nil {
		return err
	}
	if err := batch.Put(storeKey(indexPrefix, hash, elaHeight), []byte{1}); err != nil {
		return err
	}
	return batch.Write()
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

// lookupAt returns the newest value written for hash at a height <= elaHeight.
// found is false when nothing was written at or before that height, which is a
// legitimate answer (the tx did not exist yet, from this height's point of view).
func (s *RechargeStore) lookupAt(dataPrefix, indexPrefix []byte, elaHeight uint64, hash common.Hash) (value []byte, at uint64, found bool, err error) {
	// NewIterator takes the prefix without a start offset, so the hash is folded
	// into the prefix and the remaining 8 bytes are scanned in ascending order.
	prefix := make([]byte, 0, len(indexPrefix)+32)
	prefix = append(prefix, indexPrefix...)
	prefix = append(prefix, hash[:]...)

	it := s.db.NewIterator(prefix, nil)
	defer it.Release()

	best := uint64(0)
	haveBest := false
	for it.Next() {
		key := it.Key()
		if len(key) != len(prefix)+8 {
			return nil, 0, false, fmt.Errorf("%w: index key %x has the wrong length", ErrRechargeStoreCorrupt, key)
		}
		h := binary.BigEndian.Uint64(key[len(prefix):])
		if h > elaHeight {
			break
		}
		best, haveBest = h, true
	}
	if err := it.Error(); err != nil {
		return nil, 0, false, err
	}
	if !haveBest {
		return nil, 0, false, nil
	}

	key := storeKey(dataPrefix, hash, best)
	ok, err := s.db.Has(key)
	if err != nil {
		return nil, 0, false, err
	}
	if !ok {
		return nil, 0, false, fmt.Errorf("%w: index entry for %x at ELA height %d has no data record", ErrRechargeStoreCorrupt, hash, best)
	}
	value, err = s.db.Get(key)
	if err != nil {
		return nil, 0, false, err
	}
	return value, best, true, nil
}

// RechargeAt returns the recharge record of elaHash as of elaHeight.
//
// It returns ErrRechargeElaTxFailed if the tx is marked failed at or before that
// height, and ErrRechargeUnavailable when it is simply unknown there. It never
// returns an empty result to mean "unknown".
func (s *RechargeStore) RechargeAt(elaHeight uint64, elaHash string) (RechargeDatas, *big.Int, error) {
	if err := s.requireCovered(elaHeight); err != nil {
		return nil, nil, err
	}
	hash, err := normaliseElaHash(elaHash)
	if err != nil {
		return nil, nil, err
	}
	failed, _, found, err := s.lookupAt(keyFailedData, keyFailedIndex, elaHeight, hash)
	if err != nil {
		return nil, nil, err
	}
	if found && len(failed) > 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrRechargeElaTxFailed, elaHash)
	}
	value, at, ok, err := s.lookupAt(keyRechargeData, keyRechargeIndex, elaHeight, hash)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s at ELA height %d", ErrRechargeUnavailable, elaHash, elaHeight)
	}
	record := new(storedRecharge)
	if err := rlp.DecodeBytes(value, record); err != nil {
		return nil, nil, fmt.Errorf("spv: decoding recharge record for %s at ELA height %d: %w", elaHash, at, err)
	}
	datas := make(RechargeDatas, len(record.Fees))
	totalFee := big.NewInt(0)
	for i := range datas {
		datas[i] = &RechargeData{
			TargetAddress: record.Addresses[i],
			TargetAmount:  record.Amounts[i],
			Fee:           record.Fees[i],
			TargetData:    record.Memos[i],
		}
		totalFee.Add(totalFee, record.Fees[i])
	}
	return datas, totalFee, nil
}

// IsFailedElaTxAt reports whether elaHash was marked failed at or before
// elaHeight.
//
// Absence is a real answer here, unlike for Recharge: the failed set is
// authoritative for the heights it covers, so "not marked" means "not failed".
// The coverage check is what keeps "the store has not seen this height yet" from
// being reported as "not failed".
func (s *RechargeStore) IsFailedElaTxAt(elaHeight uint64, elaHash string) (bool, error) {
	if err := s.requireCovered(elaHeight); err != nil {
		return false, err
	}
	hash, err := normaliseElaHash(elaHash)
	if err != nil {
		return false, err
	}
	value, _, found, err := s.lookupAt(keyFailedData, keyFailedIndex, elaHeight, hash)
	if err != nil {
		return false, err
	}
	return found && len(value) > 0, nil
}
