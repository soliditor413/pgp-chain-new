package spv

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/elastos/Elastos.ELA.SPV/interface/iutil"
	"github.com/elastos/Elastos.ELA/core/transaction"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

// rechargeTarget30115 is the esc address carried by the cross chain output of
// the ELA deposit that pgp block 30115 redeems. It is NOT the ELA side payee
// (that is XNQWEZ7aqNyJHvav8j8tNo2ZQypuTsWQk6, derived from the pgp genesis
// hash); it is the EVM address the mint is credited to.
const rechargeTarget30115 = "0xa20f5A22eF423b0e5c2Db79A5475D9512d989971"

// realElaTx30115 deserialises the mainnet ELA deposit tx that pgp block 30115
// redeems, through the only correct entry point.
//
// Elastos.ELA's transaction.BaseTransaction.Deserialize does NOT consume the
// version/txType flag bytes: it goes straight to DeserializeUnsigned, which
// reads one byte as the payload version and dispatches on the *already set*
// tx.txType. A freshly allocated BaseTransaction therefore has txType = 0
// (CoinBase) and reads 0x09 as its payload version, which is wrong on both
// counts. transaction.GetTransactionByBytes is the entry point that consumes
// the flag bytes and sets version/txType before any payload dispatch.
func realElaTx30115(t *testing.T) *iutil.Tx {
	t.Helper()
	raw, err := hex.DecodeString(elaRechargeTx30115Hex)
	if err != nil {
		t.Fatalf("bad fixture hex: %v", err)
	}
	r := bytes.NewReader(raw)
	tx, err := transaction.GetTransactionByBytes(r)
	if err != nil {
		t.Fatalf("reading the ELA tx header: %v", err)
	}
	if err := tx.Deserialize(r); err != nil {
		t.Fatalf("deserialising the ELA deposit tx: %v", err)
	}
	return iutil.NewTx(tx)
}

// TestApplyElaBlockDecodesRealRechargeTx is the end-to-end guard on the write
// path: a real ELA block carrying a real TransferCrossChainAsset tx must produce
// exactly one recharge record with the values pgp block 30115 was built against.
//
// Every other test in this package feeds either no transactions or an
// undecodable stub, so without this one the whole decode path could be broken
// and the suite would stay green. It was, in fact: decodeElaTransaction
// allocated a bare BaseTransaction and called Deserialize on it, which fails on
// any real transaction.
//
// The numbers below are the ones ELAMinter.Recharge was called with for block
// 30115, cross checked against the ELA JSON-RPC verbose output of the same tx
// (/tmp/ela-groundtruth/tx-1b7e2877.verbose.json):
//
//	output[0]: value 10,150,000 to the pgp cross chain address, targetAmount
//	           10,000,000, so fee = output.Value - targetAmount = 150,000
//	output[1]: 110,893,470 of a different asset back to the sender (change),
//	           NOT a cross chain output and must not become a record
func TestApplyElaBlockDecodesRealRechargeTx(t *testing.T) {
	s := newTestStore()
	if err := ApplyElaBlock(s, elaBlockAt(elaHeight30115, realElaTx30115(t))); err != nil {
		t.Fatalf("ApplyElaBlock: %v", err)
	}

	datas, fee, err := s.RechargeAt(elaHeight30115, elaHash30115)
	if err != nil {
		t.Fatalf("RechargeAt: %v", err)
	}
	if len(datas) != 1 {
		t.Fatalf("got %d recharge records, want 1 (the change output must not count)", len(datas))
	}
	d := datas[0]
	if d.TargetAmount.String() != "10150000" {
		t.Errorf("TargetAmount = %s, want 10150000 (the output value, not the payload target)", d.TargetAmount)
	}
	if fee.String() != "150000" {
		t.Errorf("total fee = %s, want 150000", fee)
	}
	if d.Fee.String() != "150000" {
		t.Errorf("per record fee = %s, want 150000", d.Fee)
	}
	if d.TargetAddress != ethCommon.HexToAddress(rechargeTarget30115) {
		t.Errorf("TargetAddress = %s, want %s", d.TargetAddress.Hex(), rechargeTarget30115)
	}
	if len(d.TargetData) != 0 {
		t.Errorf("TargetData = %x, want empty", d.TargetData)
	}
}

// TestDecodeElaTransactionKeepsTxType pins the specific failure that made the
// test above necessary: the decoded tx must keep its type and hash, because both
// the store key (hash) and the recharge branch (type) depend on them.
func TestDecodeElaTransactionKeepsTxType(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}
	if tx.Hash().String() != elaHash30115 {
		t.Errorf("hash = %s, want %s", tx.Hash().String(), elaHash30115)
	}
	if tx.TxType().Name() != "TransferCrossChainAsset" {
		t.Errorf("txType = %s, want TransferCrossChainAsset", tx.TxType().Name())
	}
}
