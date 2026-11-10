package spv

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA.SPV/util"
	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/common/config"
	elatx "github.com/elastos/Elastos.ELA/core/transaction"
	"github.com/elastos/Elastos.ELA/core/types/functions"
	elaCrypto "github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/elanet/filter"

	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

var (
	dataDir            = "./"
	SpvService         *Service
	transactionDBMutex sync.RWMutex
	spvTransactiondb   *leveldb.Database

	// failedTxList and failedMutex are gone on purpose.
	//
	// pgp-chain kept the "this ELA tx failed" marks in a map keyed by the *live*
	// BSC block number (ipcClient.CurrentBlockNumber() at the moment the failure
	// was noticed) and mirrored them into 8-byte-height KV buckets. Neither key
	// is the ELA height of the block being replayed, so both answers drifted with
	// the node's head. RechargeStore.MarkFailedElaTx replaces them with a mark
	// keyed by (ELA height, tx hash).
	//
	// consensusMode used to live here too; it moved to nextturn_dposinfo.go so it
	// sits next to producerStateMu, the lock that guards it.

	// PbftEngine is the legacy PBFT engine handle. pgp-chain declared it as
	// consensus.IPbftEngine (pgp-chain/consensus/consensus.go:137); bsc has no
	// PBFT engine yet, so the narrow view it actually needs is declared below
	// and it stays nil until the legacy consensus layer is ported.
	PbftEngine PbftEngineView
	stopChn    = make(chan struct{})

	ErrMainTxHashPresence  = errors.New("main txhash presence")
	ErrMainTxHashCompleted = errors.New("ELAMinter: already completed")

	// errUnimplemented marks a read path whose pgp-chain dependency
	// (smallcrosstx / withdrawfailedtx) has not been ported yet.
	errUnimplemented = errors.New("spv: not ported yet")
)

// PbftEngineView is the narrow view of the legacy PBFT engine that the SPV read
// paths still need.
//
// pgp-chain declared this as consensus.IPbftEngine (pgp-chain/consensus/consensus.go:137)
// and read the ELA anchor height straight off *types.Block.Nonce(). bsc's
// core/types transitively requires cgo (it imports prysm's BLS bindings), which
// would drag the whole cgo toolchain into this package, so the block
// dereference is left to whoever implements this interface.
//
// The head-based accessor (CurrentBlock().Nonce()) is deliberately absent: it is
// a live-head read and must not reach an execution result. See GetArbiters.
type PbftEngineView interface {
	// SpvHeightAt is the ELA height anchored in the block at the given pgp
	// height, or 0 when the block is unknown.
	SpvHeightAt(height uint64) uint64
	// Producers returns the statically configured producer set, used before
	// the first ELA-anchored height is known.
	Producers() []string
}

const (
	databaseCache int = 768

	handles = 16

	//Unprocessed refill transaction index prefix
	UnTransaction string = "UnT-"

	// missingNumber is returned by GetBlockNumber if no header with the
	// given block hash has been stored in the database
	missingNumber = uint64(0xffffffffffffffff)

	//Cross-chain exchange rate
	rate int64 = 1

	//Cross-chain recharge unprocessed transaction index
	UnTransactionIndex = "UnTI"

	//Cross-chain recharge unprocessed transaction seek
	UnTransactionSeek = "UnTS"

	// Fixed number of extra-data prefix bytes reserved for signer vanity
	ExtraVanity = 32

	// Fixed number of extra-data suffix bytes reserved for signer seal
	ExtraSeal = 65

	// Fixed height of ela chain height with LitterEnd encode
	ExtraElaHeight = 8

	GASLimtScale = 10
	blockDiff    = 6
)

// IsOnlyCRConsensus selects which half of GetArbiters' answer counts as the
// producer set: the CR (convenor) arbiters when true, the normal arbiters when
// false.
//
// It is a const, not a var, and that is the whole point (audit F-16). Whether a
// legacy block is valid must not depend on a package-level variable that some
// future caller can flip: two nodes that disagree about this flag would disagree
// about the validity of every legacy block after the ELA height where the two
// arbiter sets differ, and nothing in the block records which convention was
// meant. It was a var until now and had no assignment outside this declaration,
// so making it const costs nothing and removes the ability to introduce exactly
// that divergence.
const IsOnlyCRConsensus = true

const (
	ChainState_DPOS = iota
	ChainState_POW
	ChainState_Error
)

// The ELA main chains this node can follow. These are the canonical spellings;
// NormalizeActiveNet maps the accepted aliases onto them.
const (
	NetMainnet = "mainnet"
	NetTestnet = "testnet"
	NetRegnet  = "regnet"
)

// NormalizeActiveNet maps a user-supplied network name onto one of the three
// canonical names. The empty string means mainnet, because that is what a node
// with no flag ran before this existed and changing it would resync every
// existing datadir.
//
// Unknown names are an error, deliberately. The switch this replaces ended in
// `default: &config.DefaultParams`, i.e. anything it did not recognise - a typo,
// "chapel", a new network - silently followed the ELA main chain. A node that
// syncs the wrong chain looks healthy from the outside: it connects, it downloads
// headers, it just never agrees with the network about the arbiter set. Refusing
// to start is the cheap version of that discovery.
func NormalizeActiveNet(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "mainnet", "main", "m":
		return NetMainnet, nil
	case "testnet", "test", "t":
		return NetTestnet, nil
	case "regnet", "reg", "r":
		return NetRegnet, nil
	// Legacy aliases. pgp-chain mapped --goerli to RegNet ("g") and this fork
	// carried a misspelled "goreli" arm; both still work so an existing start
	// script does not break, but they mean regnet, not some fourth network.
	case "goerli", "goreli", "g":
		return NetRegnet, nil
	default:
		return "", fmt.Errorf("spv: unknown ELA network %q; want mainnet, testnet or regnet", name)
	}
}

// activeNet is the normalised network the running service follows. Written once
// by NewService, read by callers that want to log or assert it.
var activeNet = NetMainnet

// ActiveNetName returns the normalised ELA network this process follows.
func ActiveNetName() string { return activeNet }

type Config struct {
	// DataDir is the data path to store db files peer addresses etc.
	DataDir string

	// ActiveNet indicates the ELA network to connect with.
	ActiveNet string

	// GenesisAddress is the address generated by the side chain genesis block.
	GenesisAddress string

	GenesisHash common.Uint256
}

type Service struct {
	spv.SPVService
	GenesisHash common.Uint256
	mux         *event.TypeMux
}

// Spv database initialization
func SpvDbInit(spvdataDir string, pledgeBillContract string, signer ethCommon.Address, client *rpc.Client) {
	db, err := leveldb.New(filepath.Join(spvdataDir, "spv_transaction_info.db"), databaseCache, handles, "eth/db/ela/", false)
	if err != nil {
		log.Error("spv Open db", "err", err)
		return
	}
	spvTransactiondb = db

	// The ELA-height-indexed recharge store lives in the same database, so one
	// batch covers a record, its index entry and the covered-through marker.
	//
	// This only makes the store *available*. Block execution still has to pin the
	// view to the block's ELA anchor with InstallHeightView(uint32(header.Nonce))
	// before running the legacy precompiles; that call is the one hook that
	// belongs to core (it holds the header) and is not wired yet. Until it is, the
	// installed view stays null and every legacy read fails loudly, which is the
	// intended behaviour for this phase.
	SetRechargeStore(NewRechargeStore(db))

	// The read path is only useful if the precompile that consumes it is real;
	// until this call the 0x3f1 address is a stub that charges its gas and fails.
	registerRechargePrecompile()

	// TODO(spv-porter): pgp-chain also wired the write path here
	//   ipcClient = ethclient.NewClient(client)
	//   pledgeBill.Init(db, &transactionDBMutex, pledgeBillContract, signer, ipcClient)
	// Neither the IPC ethclient (a self-loop back into this node) nor
	// pledgeBill is ported yet, so pledgeBillContract/signer/client are
	// currently unused and kept only so the call site stays source compatible.
	_ = pledgeBillContract
	_ = signer
	_ = client
}

// initElaTransactionFunctions wires Elastos.ELA's transaction factory hooks.
//
// Elastos.ELA dispatches transaction construction and deserialisation through
// package level function variables in core/types/functions. An ELA node fills
// them in from its own start-up: cmd/ela.go and pgp-chain's cmd/pgp/config.go:90
// both assign all four before touching any config. Nothing in this tree imports
// that start-up path, so without this the very first thing NewService does —
// chainParams.Sterilize() -> core.GenesisBlock() -> functions.CreateTransaction —
// calls a nil function and takes the node down with a SIGSEGV.
//
// It also resets config.DefaultParams. Sterilize mutates it in place (it
// multiplies DPoSConfiguration.SignTolerance by time.Second, converts the
// address strings to program hashes and builds GenesisBlock), so calling it on
// an already-sterilised value compounds. Taking a fresh copy each time makes
// NewService safe to call more than once in one process — which the tests do.
func initElaTransactionFunctions() {
	functions.GetTransactionByTxType = elatx.GetTransaction
	functions.GetTransactionByBytes = elatx.GetTransactionByBytes
	functions.CreateTransaction = elatx.CreateTransaction
	functions.GetTransactionParameters = elatx.GetTransactionparameters

	config.DefaultParams = *config.GetDefaultParams()
}

// Spv service initialization
func NewService(cfg *Config, tmux *event.TypeMux, dynamicArbiterHeight uint64) (*Service, error) {
	initElaTransactionFunctions()

	// Resolve the network before touching any params. An unknown name must not
	// reach the switch below, where the old default arm turned it into mainnet.
	net, err := NormalizeActiveNet(cfg.ActiveNet)
	if err != nil {
		return nil, err
	}
	activeNet = net

	var chainParams *config.Configuration
	switch net {
	case NetTestnet:
		chainParams = config.DefaultParams.TestNet()
	case NetRegnet:
		chainParams = config.DefaultParams.RegNet()
	default:
		chainParams = &config.DefaultParams
	}

	// Note what the three arms above do: TestNet() and RegNet() write into their
	// receiver and return it, and the receiver here is the package-level
	// config.DefaultParams. That mutation is intentional and matches upstream
	// (pgp-chain/cmd/pgp/main.go:419-431 did the same through SetConfig).
	//
	// It is also load-bearing: ELA's own transaction checks read
	// config.DefaultParams.CheckAddressHeight (core/transaction/
	// transactionchecker.go:682, blockchain/txvalidator.go:534), so leaving the
	// global on mainnet while the service runs testnet params would make the
	// library disagree with itself. A process follows exactly one ELA network.
	//
	// initElaTransactionFunctions() resets the global at the top of every call,
	// so a second NewService does not compound the first one's Sterilize (it
	// multiplies SignTolerance by time.Second in place).
	spvCfg := &spv.Config{
		DataDir:             cfg.DataDir,
		FilterType:          filter.FTReturnSidechainDepositCoinFilter,
		OnRollback:          nil, // Not implemented yet
		GenesisBlockAddress: cfg.GenesisAddress,
	}
	ResetConfigWithReflect(chainParams, spvCfg)
	chainParams.Sterilize()
	spvCfg.ChainParams = chainParams

	spvCfg.PermanentPeers = chainParams.PermanentPeers
	dataDir = cfg.DataDir
	spvCfg.NodeVersion = "PGP_1.9.7"

	// TODO(spv-porter): pgp-chain called initLog(cfg.DataDir) here. That helper
	// lived in spv_log.go and pulled in Elastos.ELA.SPV/wallet/store (and thus
	// the sqlite driver) only to attach loggers to the ELA sub packages, so it
	// is intentionally not ported.

	service, err := spv.NewSPVService(spvCfg)
	if err != nil {
		log.Error("Spv New DPOS SPVService: ", "err", err)
		return nil, err
	}

	SpvService = &Service{service, cfg.GenesisHash, tmux}

	// The transaction listener is what puts an address into the SPV bloom
	// filter, so it has to be registered before anything asks the ELA chain for
	// merkle blocks. Without it the peer sends merkle blocks that match nothing,
	// every block arrives with an empty transaction list, and the recharge store
	// fills with nothing while reporting full coverage — silently.
	//
	// Resolve the address first: a bad genesis address must fail here rather
	// than leave a half-registered service behind.
	programHash, err := common.Uint168FromAddress(cfg.GenesisAddress)
	if err != nil {
		log.Error("Spv resolve the ELA filter address: ", "address", cfg.GenesisAddress, "err", err)
		return nil, err
	}
	err = service.RegisterTransactionListener(&transactionListener{
		address: cfg.GenesisAddress,
		service: service,
	})
	if err != nil {
		log.Error("Spv Register Transaction Listener: ", "err", err)
		return nil, err
	}
	// One line that makes "did the filter get installed, and for what" visible
	// in the log, so this failure can never again cost a full sync to diagnose.
	log.Info("pgp: registered the ELA filter address",
		"address", cfg.GenesisAddress, "programHash", programHash.String())

	// And one line that proves the registration actually reached the filter the
	// peers will be sent. Registering can succeed while producing an empty
	// filter, and every downstream signal is blind to that; see
	// filter_diagnostic.go.
	logTxFilterState(service, cfg.GenesisAddress, programHash)

	// The block listener is part of the read path, not the write path:
	// GetSpvHeight, SpvIsWorkingHeight and the consensus mode switch all go
	// through it, so it is registered even in this read-only port.
	err = service.RegisterBlockListener(&BlockListener{
		dynamicArbiterHeight: dynamicArbiterHeight,
	})
	if err != nil {
		log.Error("Spv Register BlockListener: ", "err", err)
		return nil, err
	}

	// TODO(spv-porter): pgp-chain registered two more listeners here and neither
	// is ported yet:
	//   - &listener{...}'s *write* path: SavePayloadInfo -> SendTransaction, and
	//     the genesis extra-data scan that populated blocksigner.Signers and
	//     blocksigner.SelfIsProducer. The address-carrying read-only
	//     transactionListener above is registered, so the bloom filter is
	//     populated; what is missing is the code that submits ELA transactions.
	//   - &pledgeBill.PledgeBillListener{Service: service}

	return SpvService, nil
}

func (s *Service) GetDatabase() *leveldb.Database {
	return spvTransactiondb
}

func (s *Service) VerifyElaHeader(hash *common.Uint256) error {
	blockChain := s.HeaderStore()
	_, err := blockChain.Get(hash)
	if err != nil {
		return errors.New("[VerifyElaHeader] Verify ela header failed.")
	}
	return nil
}

func (s *Service) GetELAHeader(height uint32) (*util.Header, error) {
	return s.HeaderStore().GetByHeight(height)
}

// trimHexPrefix drops a leading "0x".
//
// pgp-chain indexed s[0:2] directly in every hash-handling helper, which panics
// on a hash shorter than two characters. Those inputs come straight from
// precompile calldata, so a malformed one used to take the node down mid-block.
func trimHexPrefix(s string) string {
	if len(s) >= 2 && s[0:2] == "0x" {
		return s[2:]
	}
	return s
}

func GetUnTransactionNum(db DatabaseReader, Prefix string) uint64 {
	data, _ := db.Get([]byte(Prefix))
	if len(data) != 8 {
		return missingNumber
	}
	return binary.BigEndian.Uint64(data)
}

// DatabaseReader wraps the Get method of a backing data store.
type DatabaseReader interface {
	Get(key []byte) (value []byte, err error)
}

// FindOutputFeeAndaddressByTxHash returns the esc address, the recharge amount
// and the fee of the *first* output of an ELA recharge tx.
//
// CONSENSUS-RISK: this used to read the spvTransactiondb flat KV keys directly
// and returned (0, 0x0, 0) on every error path, so "the record is missing",
// "the tx failed" and "the recharge really is zero" were the same answer. It now
// goes through the height-indexed LegacyStateView, which fails instead of
// answering a zero, and takes no ELA height argument because the installed view
// is already pinned to the height of the block being executed.
func FindOutputFeeAndaddressByTxHash(transactionHash string) (*big.Int, ethCommon.Address, *big.Int, error) {
	datas, _, err := StateView().Recharge(trimHexPrefix(transactionHash))
	if err != nil {
		return nil, ethCommon.Address{}, nil, err
	}
	if len(datas) == 0 {
		return nil, ethCommon.Address{}, nil, fmt.Errorf("%w: %s", ErrRechargeUnavailable, transactionHash)
	}
	first := datas[0]
	return new(big.Int).Set(first.Fee), first.TargetAddress, new(big.Int).Set(first.TargetAmount), nil
}

// FindOutRechargeInput returns the recharge memo bytes of an ELA hash.
//
// CONSENSUS-RISK: same external-state read as FindOutputFeeAndaddressByTxHash; it
// used to fabricate an empty memo when the key was absent, which is
// indistinguishable from a recharge that genuinely carried no memo.
func FindOutRechargeInput(transactionHash string) ([]byte, error) {
	datas, _, err := StateView().Recharge(trimHexPrefix(transactionHash))
	if err != nil {
		return nil, err
	}
	if len(datas) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrRechargeUnavailable, transactionHash)
	}
	return datas[0].TargetData, nil
}

// IsFailedElaTx reports whether the ELA main chain tx was returned or failed.
//
// CONSENSUS-RISK: pgp-chain answered from an in-memory failedTxList (filled with
// the *live* BSC head at the moment the failure was noticed) plus a full KV scan
// plus a live ELASPV query. The first two are gone: they are replaced by the
// ELA-height-indexed store, and a height the store has not reached is an error
// rather than "not failed". The ELASPV query is a separate signal and is flagged
// below; it is the one live read left on this path.
func IsFailedElaTx(elaTx string) (bool, error) {
	elaTx = trimHexPrefix(elaTx)
	failed, err := StateView().IsFailedElaTx(elaTx)
	if err != nil {
		return false, err
	}
	if failed {
		return true, nil
	}

	// CONSENSUS-RISK (known gap): pgp-chain's second half of this answer was
	// SpvService.HaveRetSideChainDepositCoinTx, i.e. a question put to the live
	// SPV state rather than to the store. It is a different signal from the store's
	// failed marks (which come from the recharge listener), so it cannot simply be
	// dropped: a tx pgp considered failed would be considered fine here and the
	// precompile at 0x3f0/0x3f1 would return data for it, diverging stateRoot.
	//
	// TODO(spv-porter): this is a LIVE READ on the execution path. Replace it with
	// a height-indexed query once ELASPV can answer "was this tx returned as of
	// ELA height H"; until then it must stay, and it must stay visibly here rather
	// than be quietly folded into the store.
	//
	// Do not move the store's marks behind this call and do not treat the two as
	// equivalent: the store's marks come from the recharge listener, this one from
	// the ELA chain's own bookkeeping.
	//
	// TODO(spv-porter): replace with a height-indexed ELASPV query once the ELA side
	// exposes one. Removing it instead of replacing it would change which txs count
	// as failed, which is a consensus change, not a cleanup.
	hash, err := common.Uint256FromHexString(elaTx)
	if err != nil {
		return false, err
	}
	if SpvService == nil {
		return false, errors.New("SpvService is not initialized")
	}
	res := SpvService.HaveRetSideChainDepositCoinTx(*hash)
	log.Debug("HaveRetSideChainDepositCoinTx", "res", res)
	return res, nil
}

// GetArbiters returns the arbiter public keys (hex) of the block being
// executed.
//
// CONSENSUS-RISK: pgp-chain took the height from the *live* SPV head
// (PbftEngine.CurrentBlock().Nonce(), spv_module.go:1097), so the answer
// depended on how far SPV had synced at execution time rather than on the block
// itself. This function is called by the 0x3e8 `arbiters` precompile
// (pgp-chain/core/vm/contracts.go:700), so that leak went straight into
// stateRoot. The height now comes from LegacyStateView.
func GetArbiters() ([]string, int, error) {
	producers := make([]string, 0)
	elaHeight, err := StateView().ELAHeight()
	if err != nil {
		return producers, 0, err
	}
	if elaHeight == 0 {
		// Only reachable with a real view that pinned height 0 (a block sealed
		// before the first ELA anchor); that is exactly the case where pgp-chain
		// fell back to the statically configured producers.
		if PbftEngine == nil {
			return producers, 0, errors.New("pbftEngine is nil and the pinned ELA height is 0")
		}
		producers = PbftEngine.Producers()
		return producers, len(producers), nil
	}
	list, totalProducers, err := GetProducers(uint64(elaHeight))
	for _, p := range list {
		producers = append(producers, common.BytesToHexString(p))
	}
	return producers, totalProducers, err
}

// IsSmallCrossTxByData reports the raw tx / signatures carried by a small cross
// chain transaction payload.
//
// CONSENSUS-RISK: the 0x3f3 `verifySmallCrossTx` precompile calls this
// (pgp-chain/core/vm/contracts.go:1631). While it is a stub, any historical
// transaction that hit 0x3f3 produces a different result and therefore a
// different stateRoot. Do not replay a range containing such a transaction with
// this build.
//
// TODO(spv-porter): pgp-chain decoded smallcrosstx.SmallCrossTx here
// (pgp-chain/spv/spv_module.go:1111). The smallcrosstx package is not ported
// yet, so no small cross tx can be recognised and the caller always gets the
// "not a small cross tx" answer.
func IsSmallCrossTxByData(data []byte) (string, string, []string, uint64) {
	return "", "", nil, 0
}

// VerifySmallCrossTx verifies the arbiter signatures carried by a small cross
// chain transaction.
//
// CONSENSUS-RISK: the 0x3f3 `verifySmallCrossTx` precompile calls this
// (pgp-chain/core/vm/contracts.go:1631). A *failed verification* must surface to
// the precompile as the 32-byte zero value with a nil error
// (pgp-chain returns `false32Byte, nil`, contracts.go:1635-1640), NOT as an
// error. evm.Call zeroes the forwarded gas whenever the returned error is not
// ErrExecutionReverted (evm.go:266-268), so turning "the signature does not
// verify" into an error burns the whole sub-call gas budget instead of just
// RequiredGas — a real fork. Keep the two cases apart:
//   - verification concluded "no"      -> (false, nil) -> precompile returns false32Byte
//   - the environment could not answer -> error        (e.g. errUnimplemented,
//     no pinned ELA height, arbiter set unavailable)
//
// TODO(spv-porter): blocked on smallcrosstx being ported.
func VerifySmallCrossTx(rawTxID, rawTx string, signatures []string,
	blockHeight uint64) (bool, error) {
	return false, errUnimplemented
}

// CONSENSUS-RISK: all precompile error/zero-value choices are gas decisions,
// not style. The rule, from kou-dou-ma's audit of pgp-chain: "the environment or
// the input is broken" -> error; "verification concluded no" -> zero value with
// a nil error. Every one of the 12 ELA precompiles must copy its own case
// verbatim.

func verifySmallCrossTxBySignature(rawTx string, signatures []string,
	blockHeight uint64) (bool, error) {
	var (
		arbiters [][]byte
		total    int
	)
	if PbftEngine == nil {
		return false, errors.New("PbftEngine is nil")
	}
	spvHeight := PbftEngine.SpvHeightAt(blockHeight)
	if spvHeight == 0 {
		// CONSENSUS-RISK: this used to fall back to GetSpvHeight(), i.e. the
		// live SPV head. A live-head fallback is the most invisible form of this
		// bug: correct whenever SPV is in sync, wrong only when it lags. If the
		// batch being replayed has no pinned height for this block, refuse.
		return false, errors.New("no ELA height pinned for the block at height " + strconv.FormatUint(blockHeight, 10))
	}
	producers, totalNum, err := GetProducers(spvHeight)
	if err != nil {
		return false, err
	}
	arbiters = producers
	total = totalNum
	buff, err := hex.DecodeString(rawTx)
	if err != nil {
		log.Error("VerifySmallCrossTx DecodeString raw error", "error", err)
		return false, err
	}
	count := 0
	for _, signature := range signatures {
		sig, err := hex.DecodeString(signature)
		if err != nil {
			log.Error("DecodeString signature error", "err", err)
			continue
		}
		for _, pbk := range arbiters {
			pubKey, err := elaCrypto.DecodePoint(pbk)
			if err != nil {
				log.Error("arbiter is error", "error", err)
				continue
			}
			err = elaCrypto.Verify(*pubKey, buff, sig)
			if err == nil {
				count++
				if count >= maxArbitersSign(total) {
					return true, nil
				}
				break
			}
		}
	}
	return false, nil
}

// maxArbitersSign is a verbatim copy of
// pgp-chain/smallcrosstx.GetMaxArbitersSign (smallcrosstx/small_cross_tx.go:130),
// duplicated here only until that package is ported.
func maxArbitersSign(total int) int {
	return total*2/3 + 1
}

// Close releases the SPV transaction database.
//
// pgp-chain dereferenced SpvService unconditionally, so closing a node that
// never started SPV panicked (spv_module.go:1219). It is nil safe now.
func Close() {
	if SpvService == nil {
		log.Info("spv service is not started, skip close")
		return
	}
	spvdb := SpvService.GetDatabase()
	if spvdb != nil {
		spvdb.Close()
		close(stopChn)
	}
}
