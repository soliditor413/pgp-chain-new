package spv

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

const (
	testElaHash  = "c4a3d5dc09808adb4e01fa805f722c3c34f41ca57496c299d17b5e0477b2b056"
	testElaHash2 = "00000000000000000000000000000000000000000000000000000000000000aa"
)

func newTestStore() *RechargeStore {
	return NewRechargeStore(memorydb.New())
}

// elaHashForHeight gives every test height a distinct, deterministic ELA block
// hash so the reorg detector has something to compare.
func elaHashForHeight(h uint64) common.Hash {
	return common.BigToHash(new(big.Int).SetUint64(h + 1))
}

// storeRechargeAtReplayHeight writes a recharge record and then marks the ELA
// height as applied, which is what the listener does (via ApplyElaBlock).
func storeRechargeAtReplayHeight(t *testing.T, s *RechargeStore, h uint64, elaHash string, datas RechargeDatas) {
	t.Helper()
	if err := s.PutRecharge(h, elaHash, datas); err != nil {
		t.Fatalf("PutRecharge at %d: %v", h, err)
	}
	if err := s.AdvanceCoverage(h, elaHashForHeight(h)); err != nil {
		t.Fatalf("AdvanceCoverage to %d: %v", h, err)
	}
}

func rechargeFixture(fee, amount int64) RechargeDatas {
	return RechargeDatas{
		&RechargeData{
			TargetAddress: common.HexToAddress("0x1111111111111111111111111111111111111111"),
			TargetAmount:  big.NewInt(amount),
			Fee:           big.NewInt(fee),
			TargetData:    []byte("memo"),
		},
		&RechargeData{
			TargetAddress: common.HexToAddress("0x2222222222222222222222222222222222222222"),
			TargetAmount:  big.NewInt(amount * 2),
			Fee:           big.NewInt(fee * 2),
			TargetData:    []byte("memo2"),
		},
	}
}

// TestRechargeStoreRoundTrip pins the basic contract: what goes in at ELA height
// H comes back unchanged, and the total fee is the sum of the entries.
func TestRechargeStoreRoundTrip(t *testing.T) {
	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))

	datas, totalFee, err := s.RechargeAt(100, testElaHash)
	if err != nil {
		t.Fatalf("RechargeAt: %v", err)
	}
	if len(datas) != 2 {
		t.Fatalf("len(datas) = %d, want 2", len(datas))
	}
	if datas[0].Fee.Cmp(big.NewInt(3)) != 0 || datas[1].Fee.Cmp(big.NewInt(6)) != 0 {
		t.Fatalf("fees = %v/%v, want 3/6", datas[0].Fee, datas[1].Fee)
	}
	if datas[0].TargetAmount.Cmp(big.NewInt(10)) != 0 || datas[1].TargetAmount.Cmp(big.NewInt(20)) != 0 {
		t.Fatalf("amounts = %v/%v, want 10/20", datas[0].TargetAmount, datas[1].TargetAmount)
	}
	if string(datas[0].TargetData) != "memo" || string(datas[1].TargetData) != "memo2" {
		t.Fatalf("memos = %q/%q", datas[0].TargetData, datas[1].TargetData)
	}
	if totalFee.Cmp(big.NewInt(9)) != 0 {
		t.Fatalf("totalFee = %v, want 9", totalFee)
	}
	// The 0x form must address the same record as the bare form.
	if _, _, err := s.RechargeAt(100, "0x"+testElaHash); err != nil {
		t.Fatalf("RechargeAt with 0x prefix: %v", err)
	}
}

// TestRechargeStoreAnswersAsOfHeight is the property the whole store exists for:
// a read at height H must see exactly the records written at heights <= H.
func TestRechargeStoreAnswersAsOfHeight(t *testing.T) {
	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))
	// Simulate the listener having applied ELA blocks up to 5000 with no further
	// recharges; without this a read at 101 is *uncovered*, not *unknown*.
	if err := s.AdvanceCoverage(5000, elaHashForHeight(5000)); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}

	// Before the record existed: unknown, and unknown is an error, not an empty
	// RechargeDatas that a caller could mistake for "zero recharge".
	for _, h := range []uint64{0, 1, 99} {
		datas, fee, err := s.RechargeAt(h, testElaHash)
		if !errors.Is(err, ErrRechargeUnavailable) {
			t.Fatalf("RechargeAt(%d) error = %v, want ErrRechargeUnavailable", h, err)
		}
		if datas != nil || fee != nil {
			t.Fatalf("RechargeAt(%d) returned (%v, %v) alongside the error", h, datas, fee)
		}
	}

	for _, h := range []uint64{100, 101, 5000} {
		datas, _, err := s.RechargeAt(h, testElaHash)
		if err != nil {
			t.Fatalf("RechargeAt(%d): %v", h, err)
		}
		if len(datas) == 0 {
			t.Fatalf("RechargeAt(%d) returned no data", h)
		}
	}
}

// TestRechargeStoreVersionsByHeight covers a corrected re-observation: the same
// ELA tx stored again at a later height must not retroactively change what an
// earlier height saw.
func TestRechargeStoreVersionsByHeight(t *testing.T) {
	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))
	storeRechargeAtReplayHeight(t, s, 200, testElaHash, rechargeFixture(7, 20))

	datas, _, err := s.RechargeAt(150, testElaHash)
	if err != nil {
		t.Fatalf("RechargeAt(150): %v", err)
	}
	if datas[0].Fee.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("fee at 150 = %v, want the 100-height value 3", datas[0].Fee)
	}

	datas, _, err = s.RechargeAt(200, testElaHash)
	if err != nil {
		t.Fatalf("RechargeAt(200): %v", err)
	}
	if datas[0].Fee.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("fee at 200 = %v, want the 200-height value 7", datas[0].Fee)
	}
}

// TestRechargeStoreRefusesUncoveredHeight is the second failure-mode test: a
// store that has only seen ELA height 100 must not answer for height 1000, which
// it would otherwise do by reporting every tx as unknown and not-failed.
//
// ELA height 0 gets no special treatment: pgp-chain had no height dimension on
// this path, so there is no height-0 behaviour to copy, and an empty store must
// refuse height 0 exactly like any other height.
func TestRechargeStoreRefusesUncoveredHeight(t *testing.T) {
	s := newTestStore()

	for _, h := range []uint64{0, 5, 30115} {
		if _, _, err := s.RechargeAt(h, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
			t.Fatalf("RechargeAt(%d) on an empty store error = %v, want ErrRechargeHeightNotCovered", h, err)
		}
		if _, err := s.IsFailedElaTxAt(h, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
			t.Fatalf("IsFailedElaTxAt(%d) on an empty store error = %v, want ErrRechargeHeightNotCovered", h, err)
		}
	}

	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))
	if _, _, err := s.RechargeAt(1000, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("RechargeAt(1000) error = %v, want ErrRechargeHeightNotCovered", err)
	}
	if height, covered, err := s.CoveredThrough(); err != nil || !covered || height != 100 {
		t.Fatalf("CoveredThrough = (%d, %v, %v), want (100, true, nil)", height, covered, err)
	}
}

// TestRechargeStoreCoveragePresence pins the difference between "never advanced"
// and "advanced to height 0". Collapsing the two into a single 0 is how an
// unsynced node would start answering "no record" for everything.
func TestRechargeStoreCoveragePresence(t *testing.T) {
	s := newTestStore()
	if height, covered, err := s.CoveredThrough(); err != nil || covered || height != 0 {
		t.Fatalf("CoveredThrough on a fresh store = (%d, %v, %v), want (0, false, nil)", height, covered, err)
	}

	if err := s.AdvanceCoverage(0, common.HexToHash(testElaHash2)); err != nil {
		t.Fatalf("AdvanceCoverage(0): %v", err)
	}
	if height, covered, err := s.CoveredThrough(); err != nil || !covered || height != 0 {
		t.Fatalf("CoveredThrough after height 0 = (%d, %v, %v), want (0, true, nil)", height, covered, err)
	}
	// Covered at height 0 means "ELA block 0 applied", so a read there is a real
	// "no such record" rather than "I have not synced".
	if _, _, err := s.RechargeAt(0, testElaHash); !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("RechargeAt(0) after covering height 0 = %v, want ErrRechargeUnavailable", err)
	}
}

// TestRechargeStorePutDoesNotAdvanceCoverage pins the ordering rule: writing a
// record must not publish the height as covered, because the same block may still
// have more records to write. A reader must not be able to see a half-written
// block as "covered".
func TestRechargeStorePutDoesNotAdvanceCoverage(t *testing.T) {
	s := newTestStore()
	if err := s.PutRecharge(30115, testElaHash, rechargeFixture(3, 10)); err != nil {
		t.Fatalf("PutRecharge: %v", err)
	}
	if _, _, err := s.RechargeAt(30115, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("RechargeAt before AdvanceCoverage = %v, want ErrRechargeHeightNotCovered", err)
	}

	if err := s.AdvanceCoverage(30115, elaHashForHeight(30115)); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	if _, _, err := s.RechargeAt(30115, testElaHash); err != nil {
		t.Fatalf("RechargeAt after AdvanceCoverage: %v", err)
	}
}

// TestRechargeStoreAdvanceCoverage pins that a quiet ELA block (one with no
// recharges) still moves coverage, otherwise every later read would fail.
func TestRechargeStoreAdvanceCoverage(t *testing.T) {
	s := newTestStore()
	if err := s.AdvanceCoverage(50, elaHashForHeight(50)); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	if failed, err := s.IsFailedElaTxAt(50, testElaHash); err != nil || failed {
		t.Fatalf("IsFailedElaTxAt = (%v, %v), want (false, nil)", failed, err)
	}
	// Coverage never moves backwards.
	if err := s.AdvanceCoverage(10, elaHashForHeight(10)); err != nil {
		t.Fatalf("AdvanceCoverage(10): %v", err)
	}
	if height, _, _ := s.CoveredThrough(); height != 50 {
		t.Fatalf("CoveredThrough = %d after a backwards advance, want 50", height)
	}
}

// TestRechargeStoreReorgKeepsData covers the ELA reorg policy: the same height
// with a different block hash is logged, not rejected, and the previously written
// records stay readable.
func TestRechargeStoreReorgKeepsData(t *testing.T) {
	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))

	// Same height, different ELA block hash.
	if err := s.AdvanceCoverage(100, elaHashForHeight(999)); err != nil {
		t.Fatalf("AdvanceCoverage with a different block hash: %v", err)
	}
	if _, _, err := s.RechargeAt(100, testElaHash); err != nil {
		t.Fatalf("RechargeAt after the reorg warning: %v", err)
	}
}

// TestRechargeStoreFailedMark covers the failed half of the store, which replaces
// the in-memory failedTxList.
func TestRechargeStoreFailedMark(t *testing.T) {
	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))
	if err := s.MarkFailedElaTx(120, testElaHash); err != nil {
		t.Fatalf("MarkFailedElaTx: %v", err)
	}
	if err := s.AdvanceCoverage(120, elaHashForHeight(120)); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}

	// Before the mark the recharge is readable and the tx is not failed.
	if failed, err := s.IsFailedElaTxAt(110, testElaHash); err != nil || failed {
		t.Fatalf("IsFailedElaTxAt(110) = (%v, %v), want (false, nil)", failed, err)
	}
	if _, _, err := s.RechargeAt(110, testElaHash); err != nil {
		t.Fatalf("RechargeAt(110): %v", err)
	}

	// From the mark onwards both reads change together.
	if failed, err := s.IsFailedElaTxAt(120, testElaHash); err != nil || !failed {
		t.Fatalf("IsFailedElaTxAt(120) = (%v, %v), want (true, nil)", failed, err)
	}
	if _, _, err := s.RechargeAt(120, testElaHash); !errors.Is(err, ErrRechargeElaTxFailed) {
		t.Fatalf("RechargeAt(120) error = %v, want ErrRechargeElaTxFailed", err)
	}

	// A different tx is unaffected.
	if failed, err := s.IsFailedElaTxAt(120, testElaHash2); err != nil || failed {
		t.Fatalf("IsFailedElaTxAt for another tx = (%v, %v), want (false, nil)", failed, err)
	}
}

// TestRechargeStoreRejectsBadHash keeps a truncated or non-hex hash from aliasing
// a real record.
func TestRechargeStoreRejectsBadHash(t *testing.T) {
	s := newTestStore()
	for _, hash := range []string{"", "0x", "dead", testElaHash[:63], testElaHash + "00", "0x" + repeatByte('z', 64)} {
		if err := s.PutRecharge(1, hash, rechargeFixture(1, 1)); err == nil {
			t.Fatalf("PutRecharge accepted %q", hash)
		}
		if _, _, err := s.RechargeAt(1, hash); err == nil {
			t.Fatalf("RechargeAt accepted %q", hash)
		}
		if _, err := s.IsFailedElaTxAt(1, hash); err == nil {
			t.Fatalf("IsFailedElaTxAt accepted %q", hash)
		}
	}
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// TestRechargeStoreDetectsCorruptIndex covers an index entry whose data record
// vanished: the read must say so instead of reporting "unknown".
func TestRechargeStoreDetectsCorruptIndex(t *testing.T) {
	db := memorydb.New()
	s := NewRechargeStore(db)

	hash := common.HexToHash(testElaHash)
	if err := db.Put(storeKey(keyRechargeIndex, hash, 10), []byte{1}); err != nil {
		t.Fatalf("seeding index: %v", err)
	}
	if err := s.AdvanceCoverage(10, elaHashForHeight(10)); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	if _, _, err := s.RechargeAt(10, testElaHash); !errors.Is(err, ErrRechargeStoreCorrupt) {
		t.Fatalf("RechargeAt error = %v, want ErrRechargeStoreCorrupt", err)
	}
}

// TestHeightScopedView checks the LegacyStateView adapter: it is the seam every
// precompile sees, so it must answer as of its pinned height and fail when the
// store is missing.
func TestHeightScopedView(t *testing.T) {
	var unbound heightScopedView
	if _, err := unbound.ELAHeight(); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("ELAHeight on an unbound view = %v, want ErrRechargeStoreNotInitialised", err)
	}
	if _, _, err := unbound.Recharge(testElaHash); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("Recharge on an unbound view = %v, want ErrRechargeStoreNotInitialised", err)
	}
	if _, err := unbound.IsFailedElaTx(testElaHash); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("IsFailedElaTx on an unbound view = %v, want ErrRechargeStoreNotInitialised", err)
	}

	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))

	view := s.HeightView(100)
	if h, err := view.ELAHeight(); err != nil || h != 100 {
		t.Fatalf("ELAHeight = (%d, %v), want (100, nil)", h, err)
	}
	if _, _, err := view.Recharge(testElaHash); err != nil {
		t.Fatalf("Recharge through the view: %v", err)
	}
	if failed, err := view.IsFailedElaTx(testElaHash); err != nil || failed {
		t.Fatalf("IsFailedElaTx through the view = (%v, %v), want (false, nil)", failed, err)
	}
}

// TestInstallHeightView covers the per-block lifecycle core/ drives: install,
// read, uninstall. Uninstall must leave CurrentRechargeView nil so a forgotten
// install on the next block fails loudly instead of reusing the old height.
func TestInstallHeightView(t *testing.T) {
	SetRechargeStore(nil)
	SetStateView(nil)
	if err := InstallHeightView(7); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("InstallHeightView without a store = %v, want ErrRechargeStoreNotInitialised", err)
	}

	s := newTestStore()
	storeRechargeAtReplayHeight(t, s, 100, testElaHash, rechargeFixture(3, 10))
	if err := s.AdvanceCoverage(120, elaHashForHeight(120)); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	SetRechargeStore(s)
	defer func() {
		SetRechargeStore(nil)
		SetStateView(nil)
	}()

	if CurrentRechargeView() != nil {
		t.Fatal("CurrentRechargeView must be nil before any install")
	}
	if err := InstallHeightView(100); err != nil {
		t.Fatalf("InstallHeightView: %v", err)
	}
	if CurrentRechargeView() == nil {
		t.Fatal("CurrentRechargeView is nil right after a successful install")
	}
	if h, err := CurrentRechargeView().ELAHeight(); err != nil || h != 100 {
		t.Fatalf("installed ELAHeight = (%d, %v), want (100, nil)", h, err)
	}
	if _, _, err := CurrentRechargeView().Recharge(testElaHash); err != nil {
		t.Fatalf("installed view Recharge: %v", err)
	}

	// A second install replaces the view rather than accumulating.
	if err := InstallHeightView(99); err != nil {
		t.Fatalf("InstallHeightView(99): %v", err)
	}
	if _, _, err := CurrentRechargeView().Recharge(testElaHash); !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("Recharge as of 99 = %v, want ErrRechargeUnavailable", err)
	}

	UninstallHeightView()
	if CurrentRechargeView() != nil {
		t.Fatal("CurrentRechargeView is not nil after UninstallHeightView")
	}
	// The nil-safe facade still answers, with an error rather than a panic.
	if _, _, err := StateView().Recharge(testElaHash); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("StateView().Recharge after uninstall = %v, want ErrLegacyStateViewMissing", err)
	}
}
