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
	it "github.com/elastos/Elastos.ELA/core/types/interfaces"
	"github.com/elastos/Elastos.ELA/core/types/outputpayload"
	"github.com/elastos/Elastos.ELA/core/types/payload"

	ethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
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
		if err := ApplyElaTransaction(store, height, tx); err != nil {
			return fmt.Errorf("spv: applying ELA tx %d of block %d: %w", i, height, err)
		}
	}
	return store.AdvanceCoverage(height, elaBlockHash(block))
}

// ApplyElaTransaction indexes one already-decoded ELA transaction at elaHeight.
// It does not touch the coverage marker; only AdvanceCoverage (i.e. ApplyElaBlock)
// may do that, because a single transaction cannot prove its block complete.
//
// It exists for the transaction listener, which is handed a decoded
// it.Transaction by the SPV layer and would otherwise have to serialise it only
// to deserialise it again. Both entries therefore converge on the same "write
// one record" step, while ApplyElaBlock keeps ownership of coverage.
//
// A transaction that is not a TransferCrossChainAsset, or one that carries no
// cross-chain output, is not an error: it simply has no recharge record. That
// mirrors ApplyElaBlock's loop, which skipped exactly these.
func ApplyElaTransaction(store *RechargeStore, elaHeight uint64, tx it.Transaction) error {
	if store == nil {
		return ErrRechargeStoreNotInitialised
	}
	if tx == nil {
		return nil
	}
	if tx.TxType() != elacom.TransferCrossChainAsset {
		return nil
	}
	datas, err := rechargeDatasOf(tx)
	if err != nil {
		return fmt.Errorf("spv: ELA tx %s at height %d: %w", tx.Hash().String(), elaHeight, err)
	}
	// One line per cross-chain output. A TransferCrossChainAsset carrying
	// recharge data is rare enough over the whole ELA chain (hundreds, not
	// millions) that logging it at Info is what makes an SPV disagreement
	// debuggable after the fact: the record this writes is what the 0x3f1
	// precompile later hands to a Recharge, so when a block executes
	// differently from the reference, these lines say whether the ELA side
	// delivered the same amounts.
	for _, data := range datas {
		log.Info("pgp: ELA recharge indexed", "elaHeight", elaHeight, "elaTx", tx.Hash().String(),
			"targetAddress", data.TargetAddress, "targetAmount", data.TargetAmount, "fee", data.Fee)
	}
	if len(datas) == 0 {
		// A TransferCrossChainAsset tx with no cross-chain output carries no
		// recharge data; pgp-chain stored nothing for it either.
		return nil
	}
	if err := store.PutRecharge(elaHeight, tx.Hash().String(), datas); err != nil {
		return fmt.Errorf("spv: storing recharge for ELA tx %s at height %d: %w", tx.Hash().String(), elaHeight, err)
	}
	return nil
}

// elaBlockHash converts the ELA block hash to the representation the store keeps
// for reorg forensics. A block whose header has not been populated yields the zero
// hash, which the store treats as "unknown" and does not record.
func elaBlockHash(block *util.Block) ethCommon.Hash {
	if block == nil || block.BlockHeader == nil {
		return ethCommon.Hash{}
	}
	return ethCommon.BytesToHash(block.Hash().Bytes())
}

// decodeElaTransaction turns the SPV layer's opaque transaction into the full ELA
// transaction, which is where TxType/Payload/Outputs live. util.Transaction only
// exposes Hash/Serialize/Deserialize/MatchFilter.
//
// The re-decode MUST go through transaction.GetTransactionByBytes. Elastos.ELA's
// BaseTransaction.Deserialize goes straight to DeserializeUnsigned, which reads
// one byte as the *payload version* and then dispatches on the *already set*
// tx.txType. A bare `new(elatx.BaseTransaction)` therefore has txType 0
// (CoinBase), reads the 0x09 version byte as its payload version, and every
// version 9 transaction fails with
// "[Attribute error] Deserialize Unsupported attribute Description".
// GetTransactionByBytes consumes the version/txType flag bytes and hands back the
// concrete per-type transaction, which is also what keeps the payload type right.
func decodeElaTransaction(t util.Transaction) (it.Transaction, error) {
	buf := new(bytes.Buffer)
	if err := t.Serialize(buf); err != nil {
		return nil, err
	}
	r := bytes.NewReader(buf.Bytes())
	tx, err := elatx.GetTransactionByBytes(r)
	if err != nil {
		return nil, err
	}
	if err := tx.Deserialize(r); err != nil {
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
func rechargeDatasOf(tx it.Transaction) (RechargeDatas, error) {
	if tx.PayloadVersion() >= payload.TransferCrossChainVersionV1 {
		return rechargeDatasFromOutputs(tx)
	}
	return rechargeDatasFromLegacyPayload(tx)
}

func rechargeDatasFromOutputs(tx it.Transaction) (RechargeDatas, error) {
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

func rechargeDatasFromLegacyPayload(tx it.Transaction) (RechargeDatas, error) {
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
