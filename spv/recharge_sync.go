package spv

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	"github.com/elastos/Elastos.ELA.SPV/util"
	elacommon "github.com/elastos/Elastos.ELA/common"
	elatx "github.com/elastos/Elastos.ELA/core/transaction"
	elacom "github.com/elastos/Elastos.ELA/core/types/common"
	"github.com/elastos/Elastos.ELA/core/types/outputpayload"
	"github.com/elastos/Elastos.ELA/core/types/payload"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

// ApplyElaBlock is the only writer of recharge records. It replaces pgp-chain's
// SavePayloadInfo / saveOutputPayload / OnReceivedRechargeTx triple, with two
// differences that matter:
//
//  1. the record is keyed by the ELA block height it was observed in, not by the
//     tx hash alone, so a replay of a block anchored at ELA height H sees exactly
//     the records that existed at H;
//  2. the coverage marker is advanced only after every recharge tx of the block
//     has been written, never per transaction. "Block received" and "block's
//     recharge records written" are different states; exposing the first as
//     covered would let a reader observe a half-written block and report
//     ErrRechargeUnavailable instead of ErrRechargeHeightNotCovered.
//
// It deliberately does no networking: everything it needs is in the block.
// pgp-chain's equivalent path could fall back to SpvService.GetTransaction, which
// is precisely the read that made the 0x3F1 precompile unreproducible.
//
// A malformed recharge tx aborts the call *without* advancing coverage: the node
// then fails every read at that height and stops, which is the intended
// behaviour. It does not skip the tx and carry on, because a silently skipped
// record is a wrong answer rather than a missing one.
func ApplyElaBlock(store *RechargeStore, block *util.Block) error {
	if store == nil {
		return ErrRechargeStoreNotInitialised
	}
	if block == nil {
		return errors.New("spv: ApplyElaBlock called with a nil ELA block")
	}
	height := uint64(block.Height)
	for i, t := range block.Transactions {
		if t == nil {
			continue
		}
		tx, err := decodeElaTransaction(t)
		if err != nil {
			return fmt.Errorf("spv: decoding ELA tx %d of block %d: %w", i, height, err)
		}
		if tx.TxType() != elacom.TransferCrossChainAsset {
			continue
		}
		datas, err := rechargeDatasOf(tx)
		if err != nil {
			return fmt.Errorf("spv: ELA tx %s at height %d: %w", tx.Hash().String(), height, err)
		}
		if len(datas) == 0 {
			// A TransferCrossChainAsset tx with no cross-chain output carries no
			// recharge data; pgp-chain stored nothing for it either.
			continue
		}
		if err := store.PutRecharge(height, tx.Hash().String(), datas); err != nil {
			return fmt.Errorf("spv: storing recharge for ELA tx %s at height %d: %w", tx.Hash().String(), height, err)
		}
	}
	return store.AdvanceCoverage(height, elaBlockHash(block))
}

// elaBlockHash converts the ELA block hash to the representation the store keeps
// for reorg forensics.
func elaBlockHash(block *util.Block) ethCommon.Hash {
	return ethCommon.BytesToHash(block.Hash().Bytes())
}

// decodeElaTransaction turns the SPV layer's opaque transaction into the full ELA
// transaction, which is where TxType/Payload/Outputs live. util.Transaction only
// exposes Hash/Serialize/Deserialize/MatchFilter.
func decodeElaTransaction(t util.Transaction) (*elatx.BaseTransaction, error) {
	buf := new(bytes.Buffer)
	if err := t.Serialize(buf); err != nil {
		return nil, err
	}
	tx := new(elatx.BaseTransaction)
	if err := tx.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		return nil, err
	}
	return tx, nil
}

// rechargeDatasOf extracts the recharge entries of a TransferCrossChainAsset tx.
//
// Two payload layouts exist and pgp-chain handled both:
//   - payload version >= 1: the amounts live on the outputs themselves
//     (outputpayload.CrossChainOutput), fee = output value - target amount;
//   - payload version 0: CrossChainAmounts/CrossChainAddresses on the payload,
//     fee = output value - cross chain amount, memo always empty.
func rechargeDatasOf(tx *elatx.BaseTransaction) (RechargeDatas, error) {
	if tx.PayloadVersion() >= payload.TransferCrossChainVersionV1 {
		return rechargeDatasFromOutputs(tx)
	}
	return rechargeDatasFromLegacyPayload(tx)
}

func rechargeDatasFromOutputs(tx *elatx.BaseTransaction) (RechargeDatas, error) {
	datas := make(RechargeDatas, 0)
	for _, out := range tx.Outputs() {
		if out == nil || out.Type != elacom.OTCrossChain {
			continue
		}
		op, ok := out.Payload.(*outputpayload.CrossChainOutput)
		if !ok || op == nil {
			return nil, errors.New("cross chain output carries an unexpected payload")
		}
		if err := op.Validate(); err != nil {
			return nil, fmt.Errorf("invalid cross chain output payload: %w", err)
		}
		fee, err := SafeFixed64Minus(out.Value, op.TargetAmount)
		if err != nil {
			return nil, err
		}
		entry, err := newRechargeData(op.TargetAddress, out.Value, fee, op.TargetData)
		if err != nil {
			return nil, err
		}
		datas = append(datas, entry)
	}
	return datas, nil
}

func rechargeDatasFromLegacyPayload(tx *elatx.BaseTransaction) (RechargeDatas, error) {
	version := tx.PayloadVersion()
	p := new(payload.TransferCrossChainAsset)
	if err := p.Deserialize(bytes.NewReader(tx.Payload().Data(version)), version); err != nil {
		return nil, fmt.Errorf("decoding TransferCrossChainAsset payload: %w", err)
	}
	outputs := tx.Outputs()
	if len(p.CrossChainAmounts) > len(outputs) {
		return nil, fmt.Errorf("payload lists %d cross chain amounts but the tx has %d outputs",
			len(p.CrossChainAmounts), len(outputs))
	}
	datas := make(RechargeDatas, 0, len(p.CrossChainAmounts))
	for i, target := range p.CrossChainAmounts {
		out := outputs[i]
		if out == nil {
			return nil, fmt.Errorf("nil output at index %d", i)
		}
		fee, err := SafeFixed64Minus(out.Value, target)
		if err != nil {
			return nil, err
		}
		entry, err := newRechargeData(p.CrossChainAddresses[i], out.Value, fee, nil)
		if err != nil {
			return nil, err
		}
		datas = append(datas, entry)
	}
	return datas, nil
}

// newRechargeData converts the ELA fixed64 values to wei and validates the esc
// address.
//
// pgp-chain did the `* rate` multiplication at *read* time in two separate
// functions, and validated the address at read time too. Doing both here means
// the multiplication and the validation exist exactly once, and a malformed esc
// address stops the listener instead of becoming a plausible-looking zero address
// on the execution path.
func newRechargeData(address string, amount, fee elacommon.Fixed64, data []byte) (*RechargeData, error) {
	if !ethCommon.IsHexAddress(address) {
		return nil, fmt.Errorf("esc address %q is not a hex address", address)
	}
	scale := big.NewInt(rate)
	return &RechargeData{
		TargetAddress: ethCommon.HexToAddress(address),
		TargetAmount:  new(big.Int).Mul(big.NewInt(int64(amount)), scale),
		Fee:           new(big.Int).Mul(big.NewInt(int64(fee)), scale),
		TargetData:    data,
	}, nil
}
