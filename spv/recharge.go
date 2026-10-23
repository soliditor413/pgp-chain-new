package spv

import (
	"errors"
	"math/big"
	"strings"

	"github.com/elastos/Elastos.ELA/common"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

type RechargeData struct {
	TargetAddress ethCommon.Address
	TargetAmount  *big.Int
	Fee           *big.Int
	TargetData    []byte
}

type RechargeDatas []*RechargeData

// GetRechargeDataByTxhash returns every recharge output of an ELA main chain tx
// plus the total fee.
//
// CONSENSUS-RISK: pgp-chain read four flat KV keys written by the SPV listener
// ("<hash>Fee"/"Address"/"Output"/"Input") and multiplied the ELA fixed64 values
// by `rate` at read time. The result therefore depended on how far the listener
// had synced, not on the block being executed, and it backed the 0x3f1
// getMainChainRechargeData precompile, so that leak reached stateRoot.
//
// It now delegates to the installed LegacyStateView, i.e. to the ELA-height
// indexed RechargeStore pinned to the block's header.Nonce. A missing record, a
// failed tx and an uncovered ELA height are all errors, never an empty result.
func GetRechargeDataByTxhash(elaHash string) (RechargeDatas, *big.Int, error) {
	return StateView().Recharge(trimHexPrefix(elaHash))
}

// BuildRechargeDatas converts the comma separated payload strings that pgp-chain
// persisted into the flat KV layout into RechargeDatas.
//
// This is the *write* side of the conversion and the only place where the ELA
// fixed64 values are scaled by `rate`. The read side (RechargeStore) returns the
// already-scaled wei values, so the multiplication cannot drift between a node
// that stored a record and a node replaying it.
//
// pgp-chain did the same conversion inside GetRechargeDataByTxhash and again in
// SavePayloadInfo; keeping one copy removes the chance of the two diverging.
func BuildRechargeDatas(feeValues, addrss, outputs, memos string) (RechargeDatas, error) {
	addrs := strings.Split(addrss, ",")
	fees := strings.Split(feeValues, ",")
	amounts := strings.Split(outputs, ",")
	targetMemos := strings.Split(memos, ",")
	if len(addrs) != len(fees) || len(fees) != len(amounts) || len(amounts) != len(targetMemos) {
		return nil, errors.New("spv: recharge payload has mismatched field counts")
	}
	y := new(big.Int).SetInt64(rate)
	datas := make(RechargeDatas, 0, len(fees))
	for i := range fees {
		if !ethCommon.IsHexAddress(addrs[i]) {
			return nil, errors.New("spv: error esc address " + addrs[i])
		}
		f, err := common.StringToFixed64(fees[i])
		if err != nil {
			return nil, err
		}
		o, err := common.StringToFixed64(amounts[i])
		if err != nil {
			return nil, err
		}
		datas = append(datas, &RechargeData{
			TargetAddress: ethCommon.HexToAddress(addrs[i]),
			TargetAmount:  new(big.Int).Mul(new(big.Int).SetInt64(o.IntValue()), y),
			Fee:           new(big.Int).Mul(new(big.Int).SetInt64(f.IntValue()), y),
			TargetData:    []byte(targetMemos[i]),
		})
	}
	return datas, nil
}
