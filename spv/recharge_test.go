package spv

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestBuildRechargeDatas pins the only place where the ELA fixed64 payload
// values are scaled by `rate`. pgp-chain did this multiplication at read time in
// two different functions; doing it once at write time is what keeps a node that
// stored a record and a node replaying it from disagreeing.
func TestBuildRechargeDatas(t *testing.T) {
	datas, err := BuildRechargeDatas(
		"0.00000001,0.00000002",
		"0x1111111111111111111111111111111111111111,0x2222222222222222222222222222222222222222",
		"0.00000003,0.00000004",
		"memo1,memo2",
	)
	if err != nil {
		t.Fatalf("BuildRechargeDatas: %v", err)
	}
	if len(datas) != 2 {
		t.Fatalf("len(datas) = %d, want 2", len(datas))
	}
	if datas[0].TargetAddress != common.HexToAddress("0x1111111111111111111111111111111111111111") {
		t.Fatalf("address[0] = %v", datas[0].TargetAddress)
	}
	// Fixed64 0.00000001 -> 1 satoshi, scaled by rate (1 today).
	wantFee := new(big.Int).Mul(big.NewInt(1), big.NewInt(rate))
	if datas[0].Fee.Cmp(wantFee) != 0 {
		t.Fatalf("fee[0] = %v, want %v", datas[0].Fee, wantFee)
	}
	wantAmount := new(big.Int).Mul(big.NewInt(3), big.NewInt(rate))
	if datas[0].TargetAmount.Cmp(wantAmount) != 0 {
		t.Fatalf("amount[0] = %v, want %v", datas[0].TargetAmount, wantAmount)
	}
	if string(datas[1].TargetData) != "memo2" {
		t.Fatalf("memo[1] = %q", datas[1].TargetData)
	}

	// A payload whose field counts disagree must be rejected rather than
	// silently truncated to the shortest column.
	if _, err := BuildRechargeDatas("1", "0x1111111111111111111111111111111111111111", "1,2", "m"); err == nil {
		t.Fatal("BuildRechargeDatas accepted mismatched field counts")
	}
	if _, err := BuildRechargeDatas("1", "not-an-address", "1", "m"); err == nil {
		t.Fatal("BuildRechargeDatas accepted an invalid esc address")
	}
}

// TestRechargeReadPointsRefuseWithoutView is the regression test for the three
// read points that used to fabricate zero values: FindOutputFeeAndaddressByTxHash
// returned (0, 0x0, 0), FindOutRechargeInput returned an empty memo and
// IsFailedElaTx returned false. With no view installed all three must fail, and
// the answers must be a zero-value result *plus* the error, never a bare zero.
func TestRechargeReadPointsRefuseWithoutView(t *testing.T) {
	SetStateView(nil)
	defer SetStateView(nil)

	fee, addr, amount, err := FindOutputFeeAndaddressByTxHash(testElaHash)
	if !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("FindOutputFeeAndaddressByTxHash error = %v, want ErrLegacyStateViewMissing", err)
	}
	if fee != nil || amount != nil || addr != (common.Address{}) {
		t.Fatalf("FindOutputFeeAndaddressByTxHash returned data alongside the error: %v %v %v", fee, addr, amount)
	}

	input, err := FindOutRechargeInput(testElaHash)
	if !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("FindOutRechargeInput error = %v, want ErrLegacyStateViewMissing", err)
	}
	if input != nil {
		t.Fatalf("FindOutRechargeInput returned %x alongside the error", input)
	}

	if _, err := IsFailedElaTx(testElaHash); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("IsFailedElaTx error = %v, want ErrLegacyStateViewMissing", err)
	}
}

// TestRechargeReadPointsUseStore proves the three read points really go through
// the ELA-height-indexed store rather than a second, flat source.
func TestRechargeReadPointsUseStore(t *testing.T) {
	s := newTestStore()
	if err := s.PutRecharge(100, testElaHash, rechargeFixture(3, 10)); err != nil {
		t.Fatalf("PutRecharge: %v", err)
	}
	if err := s.AdvanceCoverage(150); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	SetStateView(s.HeightView(120))
	defer SetStateView(nil)

	fee, addr, amount, err := FindOutputFeeAndaddressByTxHash(testElaHash)
	if err != nil {
		t.Fatalf("FindOutputFeeAndaddressByTxHash: %v", err)
	}
	if fee.Cmp(big.NewInt(3)) != 0 || amount.Cmp(big.NewInt(10)) != 0 {
		t.Fatalf("fee/amount = %v/%v, want 3/10 (the *first* output)", fee, amount)
	}
	if addr != common.HexToAddress("0x1111111111111111111111111111111111111111") {
		t.Fatalf("addr = %v", addr)
	}

	input, err := FindOutRechargeInput(testElaHash)
	if err != nil {
		t.Fatalf("FindOutRechargeInput: %v", err)
	}
	if string(input) != "memo" {
		t.Fatalf("memo = %q, want memo", input)
	}

	// A tx the store has never seen is an error, not a zero.
	if _, _, _, err := FindOutputFeeAndaddressByTxHash(testElaHash2); !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("unknown tx error = %v, want ErrRechargeUnavailable", err)
	}
}

// TestGetRechargeDataByTxhashUsesStore pins that the 0x3f1 entry point hands back
// the ELA-height-pinned record and the summed fee.
func TestGetRechargeDataByTxhashUsesStore(t *testing.T) {
	SetStateView(nil)
	if _, _, err := GetRechargeDataByTxhash(testElaHash); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("with no view error = %v, want ErrLegacyStateViewMissing", err)
	}

	s := newTestStore()
	// Version 1 at ELA height 100, version 2 at 200; the view decides which one.
	if err := s.PutRecharge(100, "0x"+testElaHash, rechargeFixture(3, 10)); err != nil {
		t.Fatalf("PutRecharge at 100: %v", err)
	}
	if err := s.PutRecharge(200, testElaHash, rechargeFixture(7, 20)); err != nil {
		t.Fatalf("PutRecharge at 200: %v", err)
	}

	SetStateView(s.HeightView(150))
	datas, totalFee, err := GetRechargeDataByTxhash(testElaHash)
	if err != nil {
		t.Fatalf("GetRechargeDataByTxhash: %v", err)
	}
	if datas[0].Fee.Cmp(big.NewInt(3)) != 0 || totalFee.Cmp(big.NewInt(9)) != 0 {
		t.Fatalf("fee/total = %v/%v, want 3/9", datas[0].Fee, totalFee)
	}

	SetStateView(s.HeightView(200))
	datas, totalFee, err = GetRechargeDataByTxhash("0x" + testElaHash)
	if err != nil {
		t.Fatalf("GetRechargeDataByTxhash at 200: %v", err)
	}
	if datas[0].Fee.Cmp(big.NewInt(7)) != 0 || totalFee.Cmp(big.NewInt(21)) != 0 {
		t.Fatalf("fee/total at 200 = %v/%v, want 7/21", datas[0].Fee, totalFee)
	}
	SetStateView(nil)
}
