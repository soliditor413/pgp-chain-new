package spv

import (
	"bytes"
	"errors"
	"sort"
	"sync"

	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type NextTurnDPOSInfo struct {
	*payload.NextTurnDPOSInfo
}

var (
	// producerStateMu guards nextTurnDposInfo and consensusMode below.
	//
	// Both are written by onBlockHandled / InitNextTurnDposInfo / IsNexturnBlock,
	// which run on the Elastos SPV notification path. That path is not
	// single-threaded: the peer code starts one block handler goroutine per peer
	// (Elastos.ELA.SPV peer/peer.go, `go p.blockHandler()`), so two peers
	// delivering blocks at once can run two of these concurrently. The same two
	// variables are read by SpvIsWorkingHeight, GetWorkingHeight,
	// MainChainIsPowMode and GetTotalProducersCount from those goroutines and,
	// once the producer broadcast path is ported, from the peer set as well.
	//
	// nextTurnDposInfo is only ever published as a whole new struct and never
	// mutated in place, so a reader may take the pointer under RLock and read
	// its fields after unlocking; the slices it points at are frozen at
	// publication.
	//
	// Lock order: producerStateMu is a leaf. Its four accessors only lock and
	// read or write one pointer or one byte; the only thing any of them calls
	// while the lock is held is testHookProducerStateWriteLocked, which is nil
	// in production. Readers that need to call out snapshot first and call
	// afterwards — SpvIsWorkingHeight releases the lock before BlockHeight,
	// which takes BlockListener.mu — so the only lock edge in this package runs
	// transactionDBMutex -> l.mu and there is no cycle. Anything added inside a
	// critical section here has to preserve that; a call that takes l.mu (or
	// producerStateMu again) turns this into a deadlock.
	producerStateMu  sync.RWMutex
	nextTurnDposInfo *NextTurnDPOSInfo
	consensusMode    spv.ConsensusAlgorithm
	zero             = common.Hex2Bytes("000000000000000000000000000000000000000000000000000000000000000000")
)

// testHookProducerStateWriteLocked is called by the writers of the state above
// immediately after they take producerStateMu for writing. It is nil in
// production.
//
// It exists so a test can park a writer inside the write lock and check that the
// readers really do acquire the read lock: with the lock honoured they block,
// without it they do not. A pure race-detector test cannot make that distinction
// on a pointer and a byte, where the torn value is not observable.
//
// The hook must not touch anything that takes producerStateMu — a reader would
// block forever, and the mutex is not reentrant — nor anything that takes
// BlockListener.mu.
var testHookProducerStateWriteLocked func()

// nextTurnInfo returns the published producer set, or nil when none has been
// published yet. The returned pointer is safe to read without the lock; see
// producerStateMu.
func nextTurnInfo() *NextTurnDPOSInfo {
	producerStateMu.RLock()
	defer producerStateMu.RUnlock()
	return nextTurnDposInfo
}

// setNextTurnInfo publishes info as the current producer set.
func setNextTurnInfo(info *NextTurnDPOSInfo) {
	producerStateMu.Lock()
	if hook := testHookProducerStateWriteLocked; hook != nil {
		hook()
	}
	nextTurnDposInfo = info
	producerStateMu.Unlock()
}

// currentConsensusMode returns the cached ELA consensus algorithm.
func currentConsensusMode() spv.ConsensusAlgorithm {
	producerStateMu.RLock()
	defer producerStateMu.RUnlock()
	return consensusMode
}

// setConsensusMode records the ELA consensus algorithm the listener last saw.
func setConsensusMode(mode spv.ConsensusAlgorithm) {
	producerStateMu.Lock()
	if hook := testHookProducerStateWriteLocked; hook != nil {
		hook()
	}
	consensusMode = mode
	producerStateMu.Unlock()
}

const (
	CURRENT_PRODUCERS = "current_producers"
)

func GetTotalProducersCount() int {
	info := nextTurnInfo()
	if info == nil {
		return 0
	}
	count, err := SafeAdd(len(info.CRPublicKeys), len(info.DPOSPublicKeys))
	if err != nil {
		log.Error("SafeAdd error", "error", err)
		return 0
	}
	return count
}

// SpvIsWorkingHeight reports whether the live SPV head is past the next turn's
// working height.
//
// CONSENSUS-RISK: live-head read (BlockListener.BlockHeight()). It is only used
// by the block listener's consensus mode switch, never by block execution.
func SpvIsWorkingHeight() bool {
	info := nextTurnInfo()
	if info == nil {
		return false
	}
	// The producer snapshot is taken above and released before this call, so
	// producerStateMu is never held across BlockListener.mu.
	return currentSpvBlockHeight() > info.WorkingHeight
}

// currentSpvBlockHeight reports the listener's live height, and 0 when the SPV
// service has not been started.
//
// The nil guards are not decoration: SpvIsWorkingHeight reaches this with a
// published producer set during start-up ordering, and pgp-chain dereferenced
// SpvService unconditionally there.
func currentSpvBlockHeight() uint32 {
	if SpvService == nil {
		return 0
	}
	listener := SpvService.GetBlockListener()
	if listener == nil {
		return 0
	}
	return listener.BlockHeight()
}

func MainChainIsPowMode() bool {
	return currentConsensusMode() == spv.POW
}

func GetCRCPublicKeys(elaHeight uint64) [][]byte {
	crcArbiters, _, err := SpvService.GetArbiters(uint32(elaHeight))
	if err != nil {
		return nil
	}
	return crcArbiters
}

// GetProducers returns the producer set that was active at elaHeight, which is
// the ELA height anchored in the block being executed (header.Nonce).
//
// CONSENSUS-RISK: pgp-chain asked GetCurrentConsensusMode() here, i.e. the
// consensus algorithm at the *live* SPV head (nextturn_dposinfo.go:65). When
// replaying a historical block that answers for the wrong height, and this
// function is on the execution path (GetArbiters -> 0x3e8 precompile), so the
// mistake lands in stateRoot. It is pinned to elaHeight now.
func GetProducers(elaHeight uint64) ([][]byte, int, error) {
	producers := make([][]byte, 0)
	totalCount := 0
	if SpvService == nil {
		return producers, totalCount, errors.New("spv is not start")
	}
	if consensusModeAt(elaHeight) == spv.POW {
		// CONSENSUS-RISK: GetCurrentProducers reads the spvTransactiondb KV
		// store, which is external state fed by the SPV listener and therefore
		// not reproducible from the block alone.
		producers = GetCurrentProducers()
		sort.Slice(producers, func(i, j int) bool {
			return bytes.Compare(producers[i], producers[j]) < 0
		})
		totalCount = len(producers)
		return producers, totalCount, nil
	}
	crcArbiters, normalArbitrs, err := SpvService.GetArbiters(uint32(elaHeight))
	if err != nil {
		return producers, totalCount, err
	}
	if IsOnlyCRConsensus {
		normalArbitrs = make([][]byte, 0)
	} else {
		crcArbiters = make([][]byte, 0)
	}

	for _, arbiter := range crcArbiters {
		if len(arbiter) > 0 && bytes.Compare(zero, arbiter) != 0 {
			producers = append(producers, arbiter)
		}
	}
	for _, arbiter := range normalArbitrs {
		if len(arbiter) > 0 && bytes.Compare(zero, arbiter) != 0 {
			producers = append(producers, arbiter)
		}
	}
	totalCount, err = SafeAdd(len(crcArbiters), len(normalArbitrs))
	if err != nil {
		return nil, totalCount, err
	}
	sort.Slice(producers, func(i, j int) bool {
		return bytes.Compare(producers[i], producers[j]) < 0
	})
	return producers, totalCount, nil
}

// consensusModeAt returns the ELA consensus algorithm that was in force at the
// given ELA height.
//
// This is the replay-safe counterpart of GetCurrentConsensusMode, which asks
// the live SPV head. Anything reachable from block execution must use this one.
func consensusModeAt(elaHeight uint64) spv.ConsensusAlgorithm {
	if SpvService == nil {
		log.Error("Spv is not started")
		return spv.DPOS
	}
	mode, err := SpvService.GetConsensusAlgorithm(uint32(elaHeight))
	if err != nil {
		log.Error("consensusModeAt", "error", err, "elaHeight", elaHeight)
		return spv.DPOS
	}
	return mode
}

// GetSpvHeight returns the ELA height the SPV service is currently synced to.
//
// CONSENSUS-RISK: this is the *live* head (HeaderStore().GetBest()), not the
// ELA height of the block being executed. It must never influence an execution
// result; use LegacyStateView.ELAHeight() on the consensus path instead.
func GetSpvHeight() uint64 {
	if SpvService != nil && SpvService.GetBlockListener() != nil {
		header, err := SpvService.HeaderStore().GetBest()
		if err != nil {
			log.Error("SpvService getBest error", "error", err)
			return uint64(SpvService.GetBlockListener().BlockHeight())
		}
		return uint64(header.Height)
	}
	return 0
}

func GetWorkingHeight() uint32 {
	if info := nextTurnInfo(); info != nil {
		return info.WorkingHeight
	}
	return 0
}

func SetCurrentProducers(producers [][]byte) {
	if spvTransactiondb == nil {
		return
	}
	transactionDBMutex.Lock()
	defer transactionDBMutex.Unlock()
	b := new(bytes.Buffer)
	count := len(producers)
	if count == 0 {
		log.Error("not SetCurrentCRProducers crc arbitrator is empty")
		return
	}
	err := elacom.WriteVarUint(b, uint64(count))
	if err != nil {
		log.Error("[SetCurrentCRProducers]write count error", "error", err)
		return
	}

	for _, arbiter := range producers {
		if len(arbiter) > 0 && bytes.Compare(zero, arbiter) != 0 {
			err = elacom.WriteVarBytes(b, arbiter)
			if err != nil {
				log.Error("[SetCurrentCRProducers]WriteVarBytes error", "error", err)
				return
			}
		}
	}

	err = spvTransactiondb.Put([]byte(CURRENT_PRODUCERS), b.Bytes())
	if err != nil {
		log.Error("[setCurrentCRProducers] write db error", "error", err)
		return
	}
}

// GetCurrentProducers returns the producer set cached in the spv KV store.
//
// CONSENSUS-RISK: the KV store is external state written by the producer
// notification path (SetCurrentProducers), not by block execution. Replaying
// the same block on a node with a different SPV sync history can return a
// different set. Do not use it as the source of truth on the execution path.
func GetCurrentProducers() [][]byte {
	if spvTransactiondb == nil {
		return nil
	}
	transactionDBMutex.Lock()
	defer transactionDBMutex.Unlock()
	b, err := spvTransactiondb.Get([]byte(CURRENT_PRODUCERS))
	if err != nil {
		// CONSENSUS-RISK: falls back to the live SPV head via GetSpvHeight().
		spvHeight := GetSpvHeight()
		crcArbiters, normalArbitrs, err := SpvService.GetArbiters(uint32(spvHeight))
		log.Error("[GetCurrentCRProducers] read db error", "error", err)
		if IsOnlyCRConsensus {
			return crcArbiters
		} else {
			return normalArbitrs
		}
	}
	if b == nil {
		return nil
	}
	producers := make([][]byte, 0)
	reader := bytes.NewReader(b)
	count, err := elacom.ReadVarUint(reader, 0)
	for i := 0; i < int(count); i++ {
		arbiter, err := elacom.ReadVarBytes(reader, 33, "arbiter")
		if err != nil {
			log.Error("[GetCurrentCRProducers] read arbiter error", "error", err)
			return nil
		}
		if len(arbiter) > 0 && bytes.Compare(zero, arbiter) != 0 {
			producers = append(producers, arbiter)
		}
	}
	return producers
}
