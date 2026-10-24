package spv

import (
	"errors"
	"io"
	"testing"

	"github.com/elastos/Elastos.ELA.SPV/util"
	elacommon "github.com/elastos/Elastos.ELA/common"
	elacom "github.com/elastos/Elastos.ELA/core/types/common"
)

// fakeHeader stands in for util.BlockHeader, which is an interface: a zero
// util.Header panics on Hash().
type fakeHeader struct{ hash elacommon.Uint256 }

func (h fakeHeader) Previous() elacommon.Uint256   { return elacommon.Uint256{} }
func (h fakeHeader) Bits() uint32                  { return 0 }
func (h fakeHeader) MerkleRoot() elacommon.Uint256 { return elacommon.Uint256{} }
func (h fakeHeader) Hash() elacommon.Uint256       { return h.hash }
func (h fakeHeader) PowHash() elacommon.Uint256    { return h.hash }
func (h fakeHeader) Serialize(w io.Writer) error   { return nil }
func (h fakeHeader) Deserialize(r io.Reader) error { return nil }

// emptyTx serialises to nothing, which makes the ELA deserializer fail on the
// first read. It stands in for a corrupt transaction in a block.
type emptyTx struct{}

func (emptyTx) Hash() elacommon.Uint256             { return elacommon.Uint256{} }
func (emptyTx) Serialize(w io.Writer) error         { return nil }
func (emptyTx) Deserialize(r io.Reader) error       { return nil }
func (emptyTx) MatchFilter(filter util.Filter) bool { return true }

func elaBlockAt(height uint32, txs ...util.Transaction) *util.Block {
	return &util.Block{
		Header: util.Header{
			BlockHeader: fakeHeader{hash: elacommon.Uint256{1}},
			Height:      height,
		},
		Transactions: txs,
	}
}

func TestApplyElaBlockRejectsMissingInputs(t *testing.T) {
	if err := ApplyElaBlock(nil, elaBlockAt(1)); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("ApplyElaBlock with a nil store = %v, want ErrRechargeStoreNotInitialised", err)
	}
	if err := ApplyElaBlock(newTestStore(), nil); err == nil {
		t.Fatal("ApplyElaBlock accepted a nil block")
	}
}

// TestApplyElaBlockAdvancesCoverage is the ordering guarantee: an ELA block with
// no recharge transactions still moves the coverage marker, otherwise a quiet
// stretch of the ELA chain would make every later read fail.
func TestApplyElaBlockAdvancesCoverage(t *testing.T) {
	s := newTestStore()

	// Nothing applied yet: block 30115 must refuse to answer.
	if _, _, err := s.RechargeAt(30115, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("before applying, RechargeAt = %v, want ErrRechargeHeightNotCovered", err)
	}

	if err := ApplyElaBlock(s, elaBlockAt(30115)); err != nil {
		t.Fatalf("ApplyElaBlock: %v", err)
	}
	if covered, found, err := s.CoveredThrough(); err != nil || !found || covered != 30115 {
		t.Fatalf("CoveredThrough = (%d, %v, %v), want (30115, true, nil)", covered, found, err)
	}
	// Covered and simply empty: the stored-and-verified answer is "no record",
	// not "not synced".
	if _, _, err := s.RechargeAt(30115, testElaHash); !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("after applying, RechargeAt = %v, want ErrRechargeUnavailable", err)
	}
}

// TestApplyElaBlockFailedTxDoesNotAdvanceCoverage pins the fail-closed rule: a
// block whose transaction cannot be decoded aborts the whole block, and coverage
// must stay where it was. Skipping the tx and advancing would turn a decode
// failure into a silently missing record, i.e. a wrong answer.
func TestApplyElaBlockFailedTxDoesNotAdvanceCoverage(t *testing.T) {
	s := newTestStore()
	if err := ApplyElaBlock(s, elaBlockAt(10)); err != nil {
		t.Fatalf("ApplyElaBlock(10): %v", err)
	}

	err := ApplyElaBlock(s, elaBlockAt(11, emptyTx{}))
	if err == nil {
		t.Fatal("ApplyElaBlock accepted a block with an undecodable transaction")
	}
	if covered, _, _ := s.CoveredThrough(); covered != 10 {
		t.Fatalf("coverage moved to %d after a failed block, want it to stay at 10", covered)
	}
}

// TestApplyElaBlockNilTransactionDoesNotAdvanceCoverage closes the one hole the
// decode-failure test leaves open.
//
// A nil entry looks harmless - there is no transaction there to lose - but the
// coverage marker is irreversible and cannot be backfilled: once a height is
// covered, every read at or below it is answered from this store forever. So
// advancing past a nil is the one mistake with no recovery path, and it must be
// as loud as an undecodable transaction, not skipped.
func TestApplyElaBlockNilTransactionDoesNotAdvanceCoverage(t *testing.T) {
	s := newTestStore()
	if err := ApplyElaBlock(s, elaBlockAt(20)); err != nil {
		t.Fatalf("ApplyElaBlock(20): %v", err)
	}

	if err := ApplyElaBlock(s, elaBlockAt(21, nil)); err == nil {
		t.Fatal("ApplyElaBlock advanced coverage over a nil transaction")
	}
	if covered, _, _ := s.CoveredThrough(); covered != 20 {
		t.Fatalf("coverage moved to %d after a block with a nil transaction, want it to stay at 20", covered)
	}
}

// a plausible-looking zero address that the precompile would happily return.
// TestNewRechargeDataValidatesAddress keeps a malformed esc address from becoming
// a plausible-looking zero address that the precompile would happily return.
func TestNewRechargeDataValidatesAddress(t *testing.T) {
	if _, err := newRechargeData("not-an-address", 1, 1, nil); err == nil {
		t.Fatal("newRechargeData accepted an invalid esc address")
	}

	data, err := newRechargeData("0x1111111111111111111111111111111111111111", 10, 3, []byte("m"))
	if err != nil {
		t.Fatalf("newRechargeData: %v", err)
	}
	if data.TargetAddress.Hex() != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("address = %v", data.TargetAddress)
	}
	if data.TargetAmount.Int64() != 10 || data.Fee.Int64() != 3 {
		t.Fatalf("amount/fee = %v/%v, want 10/3 scaled by rate", data.TargetAmount, data.Fee)
	}
	if string(data.TargetData) != "m" {
		t.Fatalf("memo = %q", data.TargetData)
	}
}

// TestApplyElaTransactionIndexesRealTx covers the transaction-listener path: a
// single decoded tx is enough to write the record, and it must NOT move the
// coverage marker on its own.
func TestApplyElaTransactionIndexesRealTx(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}

	s := newTestStore()
	if err := ApplyElaTransaction(s, elaHeight30115, tx); err != nil {
		t.Fatalf("ApplyElaTransaction: %v", err)
	}

	// One transaction cannot prove its block complete, so the read must still be
	// gated on coverage: that is the whole reason AdvanceCoverage lives in
	// ApplyElaBlock.
	if _, _, err := s.RechargeAt(elaHeight30115, elaHash30115); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("RechargeAt before AdvanceCoverage = %v, want ErrRechargeHeightNotCovered", err)
	}

	if err := s.AdvanceCoverage(elaHeight30115, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	datas, fee, err := s.RechargeAt(elaHeight30115, elaHash30115)
	if err != nil {
		t.Fatalf("RechargeAt after AdvanceCoverage: %v", err)
	}
	if len(datas) != 1 {
		t.Fatalf("got %d recharge records, want 1", len(datas))
	}
	if fee.String() != "150000" {
		t.Fatalf("fee = %s, want 150000", fee)
	}
	if datas[0].TargetAddress.Hex() != rechargeTarget30115 {
		t.Fatalf("target = %s, want %s", datas[0].TargetAddress.Hex(), rechargeTarget30115)
	}
}

// TestApplyElaTransactionSkipsNonCrossChainTx pins the type filter: a tx of any
// other type must be a no-op rather than an error, otherwise the listener would
// log a failure for every ordinary ELA transaction it is handed.
func TestApplyElaTransactionSkipsNonCrossChainTx(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}
	tx.SetTxType(elacom.TransferAsset)

	s := newTestStore()
	if err := ApplyElaTransaction(s, elaHeight30115, tx); err != nil {
		t.Fatalf("ApplyElaTransaction: %v", err)
	}
	if err := s.AdvanceCoverage(elaHeight30115, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	if _, _, err := s.RechargeAt(elaHeight30115, elaHash30115); !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("RechargeAt = %v, want ErrRechargeUnavailable (nothing stored)", err)
	}
}

// TestApplyElaTransactionRejectsMissingStore keeps the listener from silently
// dropping records when SpvDbInit has not run.
func TestApplyElaTransactionRejectsMissingStore(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}
	if err := ApplyElaTransaction(nil, elaHeight30115, tx); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("ApplyElaTransaction(nil store) = %v, want ErrRechargeStoreNotInitialised", err)
	}
	// A nil tx is not an error: it means "nothing to index".
	if err := ApplyElaTransaction(newTestStore(), elaHeight30115, nil); err != nil {
		t.Fatalf("ApplyElaTransaction(nil tx) = %v, want nil", err)
	}
}
