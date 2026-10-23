package spv

import (
	"bytes"
	"errors"
	"sort"

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
	nextTurnDposInfo *NextTurnDPOSInfo
	zero             = common.Hex2Bytes("000000000000000000000000000000000000000000000000000000000000000000")
)

const (
	CURRENT_PRODUCERS = "current_producers"
)

func GetTotalProducersCount() int {
	if nextTurnDposInfo == nil {
		return 0
	}
	count, err := SafeAdd(len(nextTurnDposInfo.CRPublicKeys), len(nextTurnDposInfo.DPOSPublicKeys))
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
	if nextTurnDposInfo != nil {
		return SpvService.GetBlockListener().BlockHeight() > nextTurnDposInfo.WorkingHeight
	}
	return false
}

func MainChainIsPowMode() bool {
	return consensusMode == spv.POW
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
	if nextTurnDposInfo != nil {
		return nextTurnDposInfo.WorkingHeight
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
