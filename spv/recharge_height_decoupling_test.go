package spv

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// The ELA tx behind pgp block 30115, and the ELA height it sits at. See
// ela_hash_order_test.go for how the hash was pinned.
const (
	elaHash30115Hex = "0x1b7e2877238323b58f5240bcf5e6797df3aae5684142c075572e145ee3d25898"
	elaHeight30115  = 2076538
	// header.Nonce of pgp block 30115. pgp-chain would have pinned every read of
	// that block to this height (consensus/pbft/pbft.go:492).
	pgpNonce30115 = 2076267
)

func recharge30115Fixture() RechargeDatas {
	return RechargeDatas{
		&RechargeData{
			TargetAddress: common.HexToAddress("0xa20f5A22eF423b0e5c2Db79A5475D9512d989971"),
			TargetAmount:  big.NewInt(10150000),
			Fee:           big.NewInt(150000),
			TargetData:    nil,
		},
	}
}

// TestRechargeLookupIgnoresPinnedHeight is the regression guard for a bug that
// would have rejected a block the real chain accepted.
//
// pgp block 30115 stamps header.Nonce 2,076,267, yet the ELA tx it redeems sits
// at ELA height 2,076,538. The block executed successfully on mainnet: its header
// records gasUsed 0, which pgp-chain only produces when the gateway call returns
// no VM error (core/state_transition.go:325), and its logsBloom is non-empty.
//
// A height-capped lookup (RechargeAt with the pinned height) returns
// ErrRechargeUnavailable for exactly this record and turns a valid block into a
// rejected one. The view must answer by tx hash alone.
func TestRechargeLookupIgnoresPinnedHeight(t *testing.T) {
	s := newTestStore()
	putAt(t, s, elaHeight30115, elaHash30115Hex, recharge30115Fixture())

	v := s.HeightView(pgpNonce30115)

	// The pinned height is still reported, for the arbiters read.
	if got, err := v.ELAHeight(); err != nil || got != pgpNonce30115 {
		t.Fatalf("ELAHeight() = %d, %v; want %d (the pin is kept for GetArbiters)", got, err, pgpNonce30115)
	}

	datas, totalFee, err := v.Recharge(elaHash30115Hex)
	if err != nil {
		t.Fatalf("Recharge at the pinned height %d must still find a record written at %d: %v\n"+
			"A height-capped read here rejects pgp block 30115, which the real chain accepted.",
			pgpNonce30115, elaHeight30115, err)
	}
	if len(datas) != 1 {
		t.Fatalf("got %d recharge entries, want 1", len(datas))
	}
	if got, want := datas[0].TargetAmount, big.NewInt(10150000); got.Cmp(want) != 0 {
		t.Errorf("targetAmount = %s, want %s", got, want)
	}
	if got, want := datas[0].Fee, big.NewInt(150000); got.Cmp(want) != 0 {
		t.Errorf("fee = %s, want %s", got, want)
	}
	if got, want := totalFee, big.NewInt(150000); got.Cmp(want) != 0 {
		t.Errorf("totalFee = %s, want %s", got, want)
	}
	if got, want := datas[0].TargetAddress, common.HexToAddress("0xa20f5A22eF423b0e5c2Db79A5475D9512d989971"); got != want {
		t.Errorf("targetAddress = %s, want %s", got, want)
	}
}

// TestRechargeLookupTakesNewestRecord covers the reorg case: the same ELA tx hash
// can be written at two heights. With no upper bound on the read, the newest
// write is the answer, not the oldest and not an error.
func TestRechargeLookupTakesNewestRecord(t *testing.T) {
	s := newTestStore()

	older := RechargeDatas{&RechargeData{
		TargetAddress: common.HexToAddress("0x1111111111111111111111111111111111111111"),
		TargetAmount:  big.NewInt(1),
		Fee:           big.NewInt(2),
	}}
	newer := RechargeDatas{&RechargeData{
		TargetAddress: common.HexToAddress("0x2222222222222222222222222222222222222222"),
		TargetAmount:  big.NewInt(30),
		Fee:           big.NewInt(40),
	}}

	putAt(t, s, 1000, elaHash30115Hex, older)
	putAt(t, s, 2000, elaHash30115Hex, newer)

	// Pinned below both heights: must still see the newest.
	datas, _, err := s.HeightView(999).Recharge(elaHash30115Hex)
	if err != nil {
		t.Fatalf("Recharge: %v", err)
	}
	if got, want := datas[0].TargetAmount, big.NewInt(30); got.Cmp(want) != 0 {
		t.Fatalf("targetAmount = %s, want %s (the newest write at height 2000)", got, want)
	}
}

// TestRechargeAnyAbsenceIsAnError pins the "unknown is not empty" rule.
//
// pgp-chain never treated a missing record as "no recharge": its fallback path
// (TrySetRechargeDataFromSpvService) ended in an error, which the 0x3f1 precompile
// turned into a failing call. Returning an empty slice here would make 0x3f1
// succeed with N=0, ELAMinter would mark the ELA hash completed without minting,
// and the block would look fine while being wrong.
func TestRechargeAnyAbsenceIsAnError(t *testing.T) {
	s := newTestStore()
	putAt(t, s, 5000, testElaHash, rechargeFixture(1, 2))

	_, _, err := s.RechargeAny("0x00000000000000000000000000000000000000000000000000000000000000ff")
	if !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("absent record: err = %v, want ErrRechargeUnavailable", err)
	}
}

// TestRechargeAnyNeedsAnAdvancedStore keeps the one coverage check that still
// makes sense without a height: a store that has never been advanced cannot
// answer anything, and saying "not found" would be a lie.
func TestRechargeAnyNeedsAnAdvancedStore(t *testing.T) {
	s := newTestStore()
	if err := s.PutRecharge(100, elaHash30115Hex, recharge30115Fixture()); err != nil {
		t.Fatalf("PutRecharge: %v", err)
	}
	// Deliberately no AdvanceCoverage.
	_, _, err := s.RechargeAny(elaHash30115Hex)
	if !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("unadvanced store: err = %v, want ErrRechargeHeightNotCovered", err)
	}
}

// TestIsFailedElaTxIgnoresPinnedHeight mirrors the recharge test for the failed
// marker. pgp-chain answered this from a per-hash SPV query
// (HaveRetSideChainDepositCoinTx), so a height cap has the same defect.
func TestIsFailedElaTxIgnoresPinnedHeight(t *testing.T) {
	s := newTestStore()
	putAt(t, s, elaHeight30115, testElaHash, rechargeFixture(1, 2))
	if err := s.MarkFailedElaTx(elaHeight30115, testElaHash2); err != nil {
		t.Fatalf("MarkFailedElaTx: %v", err)
	}
	if err := s.AdvanceCoverage(elaHeight30115, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}

	v := s.HeightView(pgpNonce30115)

	failed, err := v.IsFailedElaTx(testElaHash2)
	if err != nil {
		t.Fatalf("IsFailedElaTx: %v", err)
	}
	if !failed {
		t.Fatalf("IsFailedElaTx(%s) = false, want true: the marker is at height %d, "+
			"above the pinned %d", testElaHash2, elaHeight30115, pgpNonce30115)
	}

	// Absence is a real answer for this one.
	failed, err = v.IsFailedElaTx(testElaHash)
	if err != nil {
		t.Fatalf("IsFailedElaTx: %v", err)
	}
	if failed {
		t.Fatalf("IsFailedElaTx(%s) = true, want false", testElaHash)
	}
}
