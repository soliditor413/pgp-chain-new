package spv

import (
	"bytes"
	"sync/atomic"

	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA.SPV/util"

	"github.com/elastos/Elastos.ELA/core/transaction"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type auxParam struct {
	block  *util.Block
	height uint32
}

func (param *auxParam) clean() {
	param.height = 0
	param.block = nil
}

type BlockListener struct {
	blockNumber          uint32
	param                auxParam
	handle               func(block interface{}) error
	dynamicArbiterHeight uint64
}

func (l *BlockListener) GetDynamicArbiterHeight() uint64 {
	return atomic.LoadUint64(&l.dynamicArbiterHeight)
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
// A failure is logged and the sync continues. The ELA block is already on the ELA
// chain, so stopping here would just trade one stuck node for another; the BSC
// block anchored on that ELA height will fail loudly with
// ErrRechargeHeightNotCovered when it is executed, which is the behaviour we
// want.
func (l *BlockListener) NotifyBlock(block *util.Block, isCurrent bool) {
	if block != nil {
		if store := GetRechargeStore(); store != nil {
			if err := ApplyElaBlock(store, block); err != nil {
				log.Error("failed to index ELA block into the recharge store; "+
					"any BSC block anchored on this ELA height will fail with ErrRechargeHeightNotCovered",
					"elaHeight", block.Height, "err", err)
			}
		}
	}

	if isCurrent {
		if block.Height <= l.blockNumber {
			log.Warn("BlockListener handle block ", "height", l.blockNumber)
			return
		}
		l.blockNumber = block.Height
		l.StoreAuxBlock(block)

		// Recharge records are written for every ELA block, and deliberately
		// before the dynamicArbiterHeight early return below: coverage has to
		// track what has been *applied*, not what the arbiter schedule happens to
		// care about. Skipping those blocks would leave holes in the height index.
		//
		// An error here is logged and not retried; ApplyElaBlock refuses to advance
		// coverage on failure, so every later read at or above this height fails
		// loudly instead of reading a half-written block.
		if err := ApplyElaBlock(GetRechargeStore(), block); err != nil {
			log.Error("applying ELA block to the recharge store failed", "height", block.Height, "err", err)
		}

		log.Info("BlockListener handle block ", "height", l.blockNumber, "l.dynamicArbiterHeight ", l.dynamicArbiterHeight)

		if uint64(l.blockNumber) < l.dynamicArbiterHeight {
			return
		}

		l.onBlockHandled(l.param.block)
	}

	if l.handle != nil {
		l.handle(block)
	}

	// pgp-chain also broadcast dpos.ETOnSPVHeight here (blocklistener.go:65).
	// That event bus is not ported yet; see the TODO in onBlockHandled.
}

func (l *BlockListener) BlockHeight() uint32 {
	return l.blockNumber
}

func (l *BlockListener) StoreAuxBlock(block interface{}) {
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

func (l *BlockListener) RegisterFunc(handleFunc func(block interface{}) error) {
	l.handle = handleFunc
}

// onBlockHandled keeps only the consensus mode switch of the original
// pgp-chain implementation (blocklistener.go:86-123). The producer
// re-initialisation posts and the dpos / dpos_msg event notifications are
// write-path / broadcast concerns and are not ported yet.
func (l *BlockListener) onBlockHandled(block interface{}) {
	isWorkingHeight := SpvIsWorkingHeight()
	isNexturnBlock := IsNexturnBlock(block)
	if nextTurnDposInfo == nil {
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
	if nextTurnDposInfo == nil {
		return
	}
	nowConsensus := GetCurrentConsensusMode()
	if consensusMode == spv.POW && nowConsensus == spv.DPOS {
		log.Info("----------turn to Dpos mode------------")
		// TODO(spv-porter): SpvService.mux.Post(events.InitCurrentProducers{})
		InitNextTurnDposInfo()
		// TODO(spv-porter): notify dpos_msg.ETESCStateChanged with ChainState_DPOS.
	} else if consensusMode == spv.DPOS && nowConsensus == spv.POW {
		log.Info("----------turn to POW mode------------")
		// TODO(spv-porter): SpvService.mux.Post(events.InitCurrentProducers{})
		InitNextTurnDposInfo()
		// TODO(spv-porter): notify dpos_msg.ETESCStateChanged with ChainState_POW.
	}
	isWorkingHeight = SpvIsWorkingHeight()
	if isWorkingHeight && nowConsensus != spv.POW {
		// TODO(spv-porter): SpvService.mux.Post(events.InitCurrentProducers{})
	}
	consensusMode = nowConsensus
	log.Info("current consensus mode", "mode", consensusMode, "isNexturnBlock", isNexturnBlock, "isWorkingHeight", isWorkingHeight)
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

	nextTurnDposInfo = &NextTurnDPOSInfo{
		&payload.NextTurnDPOSInfo{
			WorkingHeight:  payloadData.WorkingHeight,
			CRPublicKeys:   payloadData.CRPublicKeys,
			DPOSPublicKeys: payloadData.DPOSPublicKeys,
		},
	}

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

	if GetCurrentConsensusMode() == spv.POW && len(crcArbiters) == 0 && len(normalArbiters) == 0 {
		log.Info("current consensus is pow and next turn is pow", "consensusMode", consensusMode)
		if consensusMode == spv.DPOS {
			DumpNextDposInfo()
		}
		return
	}

	if isSameNexturnArbiters(workingHeight, crcArbiters, normalArbiters) {
		return
	}
	nextTurnDposInfo = &NextTurnDPOSInfo{
		&payload.NextTurnDPOSInfo{
			WorkingHeight:  workingHeight,
			CRPublicKeys:   crcArbiters,
			DPOSPublicKeys: normalArbiters,
		},
	}

	// TODO(spv-porter): pgp-chain notified dpos.ETNextProducers here.
	DumpNextDposInfo()
}

func isSameNexturnArbiters(workingHeight uint32, crcArbiters, normalArbiters [][]byte) bool {
	if nextTurnDposInfo == nil {
		return false
	}
	if nextTurnDposInfo.WorkingHeight != workingHeight {
		return false
	}
	if len(crcArbiters) != len(nextTurnDposInfo.CRPublicKeys) {
		return false
	}
	if len(normalArbiters) != len(nextTurnDposInfo.DPOSPublicKeys) {
		return false
	}
	for index, v := range crcArbiters {
		if !bytes.Equal(v, nextTurnDposInfo.CRPublicKeys[index][:]) {
			return false
		}
	}
	for index, v := range normalArbiters {
		if !bytes.Equal(v, nextTurnDposInfo.DPOSPublicKeys[index][:]) {
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
	if nextTurnDposInfo == nil {
		nextTurnDposInfo = &NextTurnDPOSInfo{
			&payload.NextTurnDPOSInfo{
				WorkingHeight:  0,
				CRPublicKeys:   make([][]byte, 0),
				DPOSPublicKeys: make([][]byte, 0),
			},
		}
		return peers
	}
	for _, arbiter := range nextTurnDposInfo.CRPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
		log.Info(common.Bytes2Hex(arbiter) + "\n")
	}
	log.Info("-------------------DPOSPublicKeys---------------")
	for _, arbiter := range nextTurnDposInfo.DPOSPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
		log.Info(common.Bytes2Hex(arbiter) + "\n")
	}

	log.Info("work height", "height", nextTurnDposInfo.WorkingHeight, "activeCount", len(peers), "count", GetTotalProducersCount())
	return peers
}

func GetNextTurnPeers() []peer.PID {
	peers := make([]peer.PID, 0)
	if nextTurnDposInfo == nil {
		return peers
	}
	for _, arbiter := range nextTurnDposInfo.CRPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
	}
	for _, arbiter := range nextTurnDposInfo.DPOSPublicKeys {
		if len(arbiter) > 0 {
			var pid peer.PID
			copy(pid[:], arbiter)
			peers = append(peers, pid)
		}
	}

	return peers
}
