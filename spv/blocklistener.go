package spv

import (
	"bytes"
	"sync"
	"sync/atomic"

	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA.SPV/util"

	"github.com/elastos/Elastos.ELA/core/transaction"
	elacom "github.com/elastos/Elastos.ELA/core/types/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// elaProgressInterval is how often NotifyBlock reports that the ELA main chain is
// still advancing. See the log line for why it exists at all.
const elaProgressInterval = 10000

// testHookBeforeAuxBlockStore is called inside NotifyBlock's critical section,
// between advancing blockNumber and recording the aux block. It is nil in
// production.
//
// The hook must not call back into the listener — BlockHeight, StoreAuxBlock or
// anything else that takes l.mu. l.mu is held at this point and sync.Mutex is
// not reentrant, so such a callback is a hard deadlock whose symptom is a test
// hanging until the 10 minute timeout rather than failing with a message.
//
// It exists because the torn (height, aux block) pair it guards against is only
// reachable from a window two field writes wide, which the race detector finds
// but a plain `go test ./spv/` does not: with the mutex removed,
// TestNotifyBlockSerialisesHeightAndAuxBlock passes 30 runs out of 30 without
// -race. Parking a writer inside the window instead makes the window observable
// with the ordinary test runner, so the suite protects the mutex on its own.
var testHookBeforeAuxBlockStore func()

type auxParam struct {
	block  *util.Block
	height uint32
}

func (param *auxParam) clean() {
	param.height = 0
	param.block = nil
}

type BlockListener struct {
	// mu guards blockNumber, param and handle. NotifyBlock is driven by the SPV
	// sdk's notification goroutine while BlockHeight is read from the block
	// execution path (nextturn_dposinfo.go) and from onBlockHandled's own
	// re-entry, so the read-decide-write sequence "is this height newer than the
	// last one, and if so remember its aux block" has to be one atomic step.
	// Without it two notifications landing together can both pass the
	// blockNumber comparison and the later one can pair a height with the other
	// block's aux param.
	mu                   sync.Mutex
	blockNumber          uint32
	param                auxParam
	handle               func(block interface{}) error
	dynamicArbiterHeight uint64

	// Cumulative counters reported with the progress line. They exist because
	// an empty bloom filter is indistinguishable from a healthy sync in every
	// other signal: coverage advances, no error is logged, and the recharge
	// store simply stays empty. "txs" going up is the proof that the filter
	// installed by transactionListener actually matches something.
	txsSeen           atomic.Uint64
	crossChainTxsSeen atomic.Uint64
}

func (l *BlockListener) GetDynamicArbiterHeight() uint64 {
	return atomic.LoadUint64(&l.dynamicArbiterHeight)
}

// countCrossChainTxs reports how many of the block's transactions are
// TransferCrossChainAsset. A transaction that fails to decode is not counted;
// ApplyElaBlock reports that failure on its own, and duplicating it here would
// just double the log volume during a bad stretch.
func countCrossChainTxs(block *util.Block) uint64 {
	var count uint64
	for _, t := range block.Transactions {
		if t == nil {
			continue
		}
		tx, err := decodeElaTransaction(t)
		if err != nil {
			continue
		}
		if tx.TxType() == elacom.TransferCrossChainAsset {
			count++
		}
	}
	return count
}

// NotifyBlock is the ELA listener callback.
//
// Every block is indexed into the recharge store here, before anything else can
// observe its height, because that store is the only source the 0x3F1
// getMainChainRechargeData precompile reads and block 30115 is the first block
// that needs it. Indexing happens for synced blocks too (not just isCurrent), so
// a node that syncs the ELA chain while replaying legacy blocks still ends up
// with the records those blocks ask about.
//
// This is the ONLY call site, deliberately:
//   - it sits above the isCurrent/dynamicArbiterHeight branches, so coverage
//     tracks what has been *applied* rather than what the arbiter schedule
//     happens to care about — skipping the gated blocks would leave holes in the
//     height index;
//   - it is nil-checked, so blocks that arrive before SpvDbInit has run are
//     skipped quietly instead of logging one error per block;
//   - an earlier port called ApplyElaBlock a second time inside the isCurrent
//     branch, which decoded every live block twice for no benefit.
//
// A failure is logged and the sync continues. The ELA block is already on the ELA
// chain, so stopping here would just trade one stuck node for another; the BSC
// block anchored on that ELA height will fail loudly with
// ErrRechargeHeightNotCovered when it is executed, which is the behaviour we
// want.
func (l *BlockListener) NotifyBlock(block *util.Block, isCurrent bool) {
	if block != nil {
		txs := uint64(len(block.Transactions))
		crossChainTxs := countCrossChainTxs(block)
		txsSoFar := l.txsSeen.Add(txs)
		crossChainTxsSoFar := l.crossChainTxsSeen.Add(crossChainTxs)

		if store := GetRechargeStore(); store != nil {
			if err := ApplyElaBlock(store, block); err != nil {
				log.Error("failed to index ELA block into the recharge store; "+
					"any BSC block anchored on this ELA height will fail with ErrRechargeHeightNotCovered",
					"elaHeight", block.Height, "err", err)
			}
		}

		// The progress line is deliberately outside the store check above. The
		// question it answers — "is the bloom filter matching anything?" — is
		// independent of whether indexing is wired, and a store that failed to
		// open (SpvDbInit returns early on a leveldb error, and NewService then
		// starts syncing anyway) would otherwise suppress the only signal that
		// distinguishes "syncing fine" from "syncing nothing".
		if block.Height%elaProgressInterval == 0 {
			// The ELA sub-package logs are attached at Warn, so without this a
			// node syncing 2.3M main chain blocks reports nothing between
			// start-up and the first reorg or error. One line every 10k blocks
			// is ~230 lines for a full sync and is the only way to tell "still
			// syncing" from "stuck" from the log alone.
			//
			// txs/crossChainTxs are cumulative. If txs stays at 0 while
			// elaHeight climbs, the bloom filter has no address (see
			// transactionListener) and every recharge record is missing.
			log.Info("ELA main chain sync progress", "elaHeight", block.Height,
				"txs", txsSoFar, "crossChainTxs", crossChainTxsSoFar,
				"indexing", GetRechargeStore() != nil, "current", isCurrent)
		}
	}

	if isCurrent {
		// One critical section: decide whether this block advances the live
		// head, record it, and read back the aux block the arbiter schedule
		// will be evaluated against. The reads and writes below used to be a
		// bare sequence on unguarded fields.
		l.mu.Lock()
		advanced := block.Height > l.blockNumber
		if advanced {
			l.blockNumber = block.Height
			if hook := testHookBeforeAuxBlockStore; hook != nil {
				hook()
			}
			l.storeAuxBlockLocked(block)
		}
		height, aux := l.blockNumber, l.param.block
		due := uint64(height) >= l.GetDynamicArbiterHeight()
		l.mu.Unlock()

		// A duplicate height returns before l.handle below, exactly as the
		// original did: the callback is for blocks that moved the head.
		if !advanced {
			log.Warn("BlockListener handle block ", "height", height)
			return
		}

		log.Info("BlockListener handle block ", "height", height, "l.dynamicArbiterHeight ", l.GetDynamicArbiterHeight())

		// The arbiter-schedule gate also returns before l.handle, as in
		// pgp-chain: below dynamicArbiterHeight the live block is recorded but
		// the consensus-mode switch and the registered callback are not run.
		if !due {
			return
		}

		// onBlockHandled is deliberately called after the unlock, and it has to
		// be the whole callback rather than a lock-free BlockHeight inside it:
		// onBlockHandled re-enters this listener along three paths, every one of
		// which would self-deadlock on the non-reentrant mutex.
		//   1. InitNextTurnDposInfo -> BlockHeight()
		//      (blocklistener.go, the guard before GetNextArbiters)
		//   2. SpvIsWorkingHeight -> BlockHeight(), when nextTurnDposInfo != nil
		//      (nextturn_dposinfo.go:48)
		//   3. GetCurrentConsensusMode -> GetSpvHeight -> BlockHeight(), the
		//      fallback taken when HeaderStore().GetBest() errors
		//      (nextturn_dposinfo.go:148)
		// So the boundary is "the listener's own state is consistent while the
		// lock is held", not "the height onBlockHandled sees equals the block it
		// was given" — it re-reads the live head by design, which may already be
		// a block ahead.
		l.onBlockHandled(aux)
	}

	if handle := l.handleFunc(); handle != nil {
		handle(block)
	}

	// pgp-chain also broadcast dpos.ETOnSPVHeight here (blocklistener.go:65).
	// That event bus is not ported yet; see the TODO in onBlockHandled.
}

func (l *BlockListener) BlockHeight() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.blockNumber
}

// StoreAuxBlock satisfies the SPV BlockListener interface. It has no caller
// outside NotifyBlock today, but it is exported through that interface, so it
// takes the lock itself rather than relying on its callers.
func (l *BlockListener) StoreAuxBlock(block interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.storeAuxBlockLocked(block)
}

// storeAuxBlockLocked does the work of StoreAuxBlock. The caller must hold l.mu.
func (l *BlockListener) storeAuxBlockLocked(block interface{}) {
	b, ok := block.(*util.Block)
	if !ok || b == nil {
		log.Error("BlockListener StoreAuxBlock: unexpected block type")
		return
	}

	l.param = auxParam{
		b,
		b.Height,
	}
}

// RegisterFunc installs the block callback. RegisterFunc runs at start-up,
// before the SPV service is started, so the write cannot overlap a
// notification in practice; it takes the lock anyway so that the read in
// NotifyBlock is not a data race for the race detector.
func (l *BlockListener) RegisterFunc(handleFunc func(block interface{}) error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.handle = handleFunc
}

// handleFunc returns the registered callback, or nil when none was registered.
func (l *BlockListener) handleFunc() func(block interface{}) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.handle
}

// onBlockHandled keeps only the consensus mode switch of the original
// pgp-chain implementation (blocklistener.go:86-123). The producer
// re-initialisation posts and the dpos / dpos_msg event notifications are
// write-path / broadcast concerns and are not ported yet.
func (l *BlockListener) onBlockHandled(block interface{}) {
	isWorkingHeight := SpvIsWorkingHeight()
	isNexturnBlock := IsNexturnBlock(block)
	// The nil checks read a snapshot rather than testing the global directly,
	// and InitNextTurnDposInfo is called outside any lock: it reads the same
	// state through the same accessors and would self-deadlock if the lock were
	// held here. Two concurrent passes both reaching InitNextTurnDposInfo is
	// harmless — it is idempotent, isSameNexturnArbiters drops the second write.
	if nextTurnInfo() == nil {
		InitNextTurnDposInfo()
	} else if !isWorkingHeight {
		if isNexturnBlock {
			log.Info("------------------ force change next turn arbiters-----------")
		} else {
			InitNextTurnDposInfo()
		}
	}
	// TODO(spv-porter): pgp-chain notified dpos.ETNextProducers with
	// DumpNextDposInfo() here (blocklistener.go:100).
	if nextTurnInfo() == nil {
		return
	}
	nowConsensus := GetCurrentConsensusMode()
	previousMode := currentConsensusMode()
	if previousMode == spv.POW && nowConsensus == spv.DPOS {
		log.Info("----------turn to Dpos mode------------")
		// TODO(spv-porter): SpvService.mux.Post(events.InitCurrentProducers{})
		InitNextTurnDposInfo()
		// TODO(spv-porter): notify dpos_msg.ETESCStateChanged with ChainState_DPOS.
	} else if previousMode == spv.DPOS && nowConsensus == spv.POW {
		log.Info("----------turn to POW mode------------")
		// TODO(spv-porter): SpvService.mux.Post(events.InitCurrentProducers{})
		InitNextTurnDposInfo()
		// TODO(spv-porter): notify dpos_msg.ETESCStateChanged with ChainState_POW.
	}
	isWorkingHeight = SpvIsWorkingHeight()
	if isWorkingHeight && nowConsensus != spv.POW {
		// TODO(spv-porter): SpvService.mux.Post(events.InitCurrentProducers{})
	}
	setConsensusMode(nowConsensus)
	log.Info("current consensus mode", "mode", currentConsensusMode(), "isNexturnBlock", isNexturnBlock, "isWorkingHeight", isWorkingHeight)
}

func IsNexturnBlock(block interface{}) bool {
	b, ok := block.(*util.Block)
	if !ok || b == nil {
		return false
	}
	var tx transaction.BaseTransaction
	for _, t := range b.Transactions {
		buf := new(bytes.Buffer)
		t.Serialize(buf)
		r := bytes.NewReader(buf.Bytes())
		tx = transaction.BaseTransaction{}
		tx.Deserialize(r)
		if tx.IsNextTurnDPOSInfoTx() {
			break
		}
	}

	if !tx.IsNextTurnDPOSInfoTx() {
		log.Info("received not next turn block", "height", b.Height)
		return false
	}

	payloadData, ok := tx.Payload().(*payload.NextTurnDPOSInfo)
	if !ok || payloadData == nil {
		log.Error("received next turn block with unexpected payload", "height", b.Height)
		return false
	}

	if IsOnlyCRConsensus {
		payloadData.DPOSPublicKeys = make([][]byte, 0)
	} else {
		payloadData.CRPublicKeys = make([][]byte, 0)
	}

	setNextTurnInfo(&NextTurnDPOSInfo{
		&payload.NextTurnDPOSInfo{
			WorkingHeight:  payloadData.WorkingHeight,
			CRPublicKeys:   payloadData.CRPublicKeys,
			DPOSPublicKeys: payloadData.DPOSPublicKeys,
		},
	})

	return true
}

func InitNextTurnDposInfo() {
	if SpvService == nil {
		log.Info("init next turn dpos info skipped", "reason", "spv service is not started")
		return
	}
	listener, ok := SpvService.GetBlockListener().(*BlockListener)
	if !ok || listener == nil {
		log.Info("init next turn dpos info skipped", "reason", "block listener is not a *BlockListener")
		return
	}
	dynamicArbiterHeight := listener.GetDynamicArbiterHeight()
	if uint64(listener.BlockHeight()) < dynamicArbiterHeight {
		log.Info("init next turn dpos info error", "height", listener.BlockHeight(), "dynamicArbiterHeight", dynamicArbiterHeight)
		return
	}
	workingHeight, crcArbiters, normalArbiters, err := SpvService.GetNextArbiters()
	if err != nil {
		log.Error("GetNextArbiters error", "err", err.Error())
		return
	}
	if IsOnlyCRConsensus {
		normalArbiters = make([][]byte, 0)
	} else {
		crcArbiters = make([][]byte, 0)
	}

	mode := currentConsensusMode()
	if GetCurrentConsensusMode() == spv.POW && len(crcArbiters) == 0 && len(normalArbiters) == 0 {
		log.Info("current consensus is pow and next turn is pow", "consensusMode", mode)
		if mode == spv.DPOS {
			DumpNextDposInfo()
		}
		return
	}

	if isSameNexturnArbiters(workingHeight, crcArbiters, normalArbiters) {
		return
	}
	setNextTurnInfo(&NextTurnDPOSInfo{
		&payload.NextTurnDPOSInfo{
			WorkingHeight:  workingHeight,
			CRPublicKeys:   crcArbiters,
			DPOSPublicKeys: normalArbiters,
		},
	})

	// TODO(spv-porter): pgp-chain notified dpos.ETNextProducers here.
	DumpNextDposInfo()
}

func isSameNexturnArbiters(workingHeight uint32, crcArbiters, normalArbiters [][]byte) bool {
	info := nextTurnInfo()
	if info == nil {
		return false
	}
	if info.WorkingHeight != workingHeight {
		return false
	}
	if len(crcArbiters) != len(info.CRPublicKeys) {
		return false
	}
	if len(normalArbiters) != len(info.DPOSPublicKeys) {
		return false
	}
	for index, v := range crcArbiters {
		if !bytes.Equal(v, info.CRPublicKeys[index][:]) {
			return false
		}
	}
	for index, v := range normalArbiters {
		if !bytes.Equal(v, info.DPOSPublicKeys[index][:]) {
			return false
		}
	}
	return true
}

// GetCurrentConsensusMode returns the ELA consensus algorithm at the *live*
// SPV head.
//
// CONSENSUS-RISK: live-head read, via GetSpvHeight() -> HeaderStore().GetBest().
// It drives the block listener's mode switch, which is correct for a running
// node but not reproducible during a replay. Anything on the block execution
// path must use consensusModeAt(elaHeight) instead.
func GetCurrentConsensusMode() spv.ConsensusAlgorithm {
	if SpvService == nil {
		log.Error("Spv is not started")
		return spv.DPOS
	}
	spvHeight := uint32(GetSpvHeight())
	mode, err := SpvService.GetConsensusAlgorithm(spvHeight)
	log.Info("GetCurrentConsensusMode", "error", err, "spvHeight", spvHeight, "Mode", mode)
	if err != nil {
		return spv.DPOS
	}
	return mode
}

func DumpNextDposInfo() []peer.PID {
	log.Info("-------------------dump next turn aribiters---------------")
	log.Info("-------------------CRPublicKeys---------------")
	peers := make([]peer.PID, 0)
	info := nextTurnInfo()
	if info == nil {
		// Not a dump so much as an initialisation, which is what pgp-chain did
		// here too: publishing an empty set keeps later nil checks from
		// re-entering InitNextTurnDposInfo on every block.
		setNextTurnInfo(&NextTurnDPOSInfo{
			&payload.NextTurnDPOSInfo{
				WorkingHeight:  0,
				CRPublicKeys:   make([][]byte, 0),
				DPOSPublicKeys: make([][]byte, 0),
			},
		})
		return peers
	}
	for _, arbiter := range info.CRPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
		log.Info(common.Bytes2Hex(arbiter) + "\n")
	}
	log.Info("-------------------DPOSPublicKeys---------------")
	for _, arbiter := range info.DPOSPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
		log.Info(common.Bytes2Hex(arbiter) + "\n")
	}

	log.Info("work height", "height", info.WorkingHeight, "activeCount", len(peers), "count", GetTotalProducersCount())
	return peers
}

func GetNextTurnPeers() []peer.PID {
	peers := make([]peer.PID, 0)
	info := nextTurnInfo()
	if info == nil {
		return peers
	}
	for _, arbiter := range info.CRPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
	}
	for _, arbiter := range info.DPOSPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
	}

	return peers
}
