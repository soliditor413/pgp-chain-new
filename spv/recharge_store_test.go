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

var testElaBlockHash = common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")

func newTestStore() *RechargeStore {
	return NewRechargeStore(memorydb.New())
}

// putAt writes a recharge record and marks the ELA block applied, which is what
// the listener does per block (PutRecharge ... AdvanceCoverage, in that order).
func putAt(t *testing.T, s *RechargeStore, height uint64, hash string, datas RechargeDatas) {
	t.Helper()
	if err := s.PutRecharge(height, hash, datas); err != nil {
		t.Fatalf("PutRecharge(%d): %v", height, err)
	}
	if err := s.AdvanceCoverage(height, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage(%d): %v", height, err)
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
	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))

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
	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))
	// Simulate the listener having applied ELA blocks up to 5000 with no further
	// recharges; without this a read at 101 is *uncovered*, not *unknown*.
	if err := s.AdvanceCoverage(5000, testElaBlockHash); err != nil {
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
	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))
	putAt(t, s, 200, testElaHash, rechargeFixture(7, 20))
	if err := s.AdvanceCoverage(300, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}

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
// ELA height 0 is deliberately included: there is no special case for it, because
// pgp-chain had no height dimension on the recharge path and inventing one would
// be the same fabrication the store exists to prevent.
func TestRechargeStoreRefusesUncoveredHeight(t *testing.T) {
	s := newTestStore()

	for _, h := range []uint64{0, 5} {
		if _, _, err := s.RechargeAt(h, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
			t.Fatalf("RechargeAt(%d) on an empty store = %v, want ErrRechargeHeightNotCovered", h, err)
		}
		if _, err := s.IsFailedElaTxAt(h, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
			t.Fatalf("IsFailedElaTxAt(%d) on an empty store = %v, want ErrRechargeHeightNotCovered", h, err)
		}
	}

	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))
	if _, _, err := s.RechargeAt(1000, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("RechargeAt(1000) error = %v, want ErrRechargeHeightNotCovered", err)
	}
	if covered, found, err := s.CoveredThrough(); err != nil || !found || covered != 100 {
		t.Fatalf("CoveredThrough = (%d, %v, %v), want (100, true, nil)", covered, found, err)
	}
}

// TestPutRechargeDoesNotAdvanceCoverage pins the ordering rule: a record written
// mid-block must not make the block look covered.
func TestPutRechargeDoesNotAdvanceCoverage(t *testing.T) {
	s := newTestStore()
	if err := s.PutRecharge(100, testElaHash, rechargeFixture(3, 10)); err != nil {
		t.Fatalf("PutRecharge: %v", err)
	}
	if _, found, err := s.CoveredThrough(); err != nil || found {
		t.Fatalf("CoveredThrough after a bare PutRecharge = (found=%v, err=%v), want (false, nil)", found, err)
	}
	if _, _, err := s.RechargeAt(100, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("RechargeAt before AdvanceCoverage = %v, want ErrRechargeHeightNotCovered", err)
	}
}

// TestRechargeStoreAdvanceCoverage pins that a quiet ELA block (one with no
// recharges) still moves coverage, otherwise every later read would fail.
func TestRechargeStoreAdvanceCoverage(t *testing.T) {
	s := newTestStore()
	if err := s.AdvanceCoverage(50, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	if failed, err := s.IsFailedElaTxAt(50, testElaHash); err != nil || failed {
		t.Fatalf("IsFailedElaTxAt = (%v, %v), want (false, nil)", failed, err)
	}
	// Coverage never moves backwards.
	if err := s.AdvanceCoverage(10, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage(10): %v", err)
	}
	if covered, _, _ := s.CoveredThrough(); covered != 50 {
		t.Fatalf("CoveredThrough = %d after a backwards advance, want 50", covered)
	}
}

// TestCoveragePresenceDistinguishesNeverAdvanced pins the third return value of
// CoveredThrough. Collapsing "never advanced" and "advanced to ELA height 0" into
// a single 0 is how an unsynced node would start answering "no record" with
// confidence instead of failing.
func TestCoveragePresenceDistinguishesNeverAdvanced(t *testing.T) {
	s := newTestStore()
	if covered, found, err := s.CoveredThrough(); err != nil || found || covered != 0 {
		t.Fatalf("fresh store CoveredThrough = (%d, %v, %v), want (0, false, nil)", covered, found, err)
	}
	if _, _, err := s.RechargeAt(0, testElaHash); !errors.Is(err, ErrRechargeHeightNotCovered) {
		t.Fatalf("RechargeAt(0) on a fresh store = %v, want ErrRechargeHeightNotCovered", err)
	}

	if err := s.AdvanceCoverage(0, common.HexToHash("0xaa")); err != nil {
		t.Fatalf("AdvanceCoverage(0): %v", err)
	}
	if covered, found, err := s.CoveredThrough(); err != nil || !found || covered != 0 {
		t.Fatalf("after covering height 0, CoveredThrough = (%d, %v, %v), want (0, true, nil)", covered, found, err)
	}
	// Height 0 is now genuinely covered, so the answer is "no such record" rather
	// than "the store has not got there yet".
	if _, _, err := s.RechargeAt(0, testElaHash); !errors.Is(err, ErrRechargeUnavailable) {
		t.Fatalf("RechargeAt(0) after covering height 0 = %v, want ErrRechargeUnavailable", err)
	}
}

// TestAdvanceCoverageRemembersElaBlockHash covers the reorg forensic trail: the
// height -> ELA block hash mapping is kept, and a second write at the same height
// with a different hash is recorded rather than rejected.
func TestAdvanceCoverageRemembersElaBlockHash(t *testing.T) {
	db := memorydb.New()
	s := NewRechargeStore(db)

	first := common.HexToHash("0x01")
	if err := s.AdvanceCoverage(7, first); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	key := storeKey(keyElaBlockHash, common.Hash{}, 7)
	stored, err := db.Get(key)
	if err != nil {
		t.Fatalf("block hash entry missing: %v", err)
	}
	if common.BytesToHash(stored) != first {
		t.Fatalf("stored block hash = %v, want %v", common.BytesToHash(stored), first)
	}

	// Same height, different hash: the old data is kept and no error is raised
	// (a WARN is logged, which is what gets us a post-mortem trail).
	second := common.HexToHash("0x02")
	if err := s.AdvanceCoverage(7, second); err != nil {
		t.Fatalf("AdvanceCoverage with a different hash at the same height: %v", err)
	}
	stored, _ = db.Get(key)
	if common.BytesToHash(stored) != first {
		t.Fatalf("stored block hash was rewritten to %v, want the original %v", common.BytesToHash(stored), first)
	}
}

// TestRechargeStoreFailedMark covers the failed half of the store, which replaces
// the in-memory failedTxList.
func TestRechargeStoreFailedMark(t *testing.T) {
	s := newTestStore()
	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))
	if err := s.MarkFailedElaTx(120, testElaHash); err != nil {
		t.Fatalf("MarkFailedElaTx: %v", err)
	}
	if err := s.AdvanceCoverage(120, testElaBlockHash); err != nil {
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
	if err := s.AdvanceCoverage(1, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
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
	if err := s.AdvanceCoverage(10, testElaBlockHash); err != nil {
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
	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))
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

// TestInstallHeightView covers the two-step wiring: the view published to the
// precompiles is pinned to the block's ELA anchor, and uninstalling it makes the
// next reader fail instead of silently reusing the previous height.
func TestInstallHeightView(t *testing.T) {
	SetRechargeStore(nil)
	UninstallHeightView()
	defer func() {
		SetRechargeStore(nil)
		UninstallHeightView()
	}()

	if err := InstallHeightView(7); !errors.Is(err, ErrRechargeStoreNotInitialised) {
		t.Fatalf("InstallHeightView without a store = %v, want ErrRechargeStoreNotInitialised", err)
	}
	if CurrentRechargeView() != nil {
		t.Fatal("CurrentRechargeView() is non-nil with no store")
	}

	s := newTestStore()
	putAt(t, s, 100, testElaHash, rechargeFixture(3, 10))
	if err := s.AdvanceCoverage(120, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	SetRechargeStore(s)

	if err := InstallHeightView(100); err != nil {
		t.Fatalf("InstallHeightView: %v", err)
	}
	if v := CurrentRechargeView(); v == nil {
		t.Fatal("CurrentRechargeView() is nil right after InstallHeightView")
	}
	if h, err := StateView().ELAHeight(); err != nil || h != 100 {
		t.Fatalf("installed ELAHeight = (%d, %v), want (100, nil)", h, err)
	}
	if _, _, err := StateView().Recharge(testElaHash); err != nil {
		t.Fatalf("installed view Recharge: %v", err)
	}

	// Installing a second height must be independent, not cumulative: the
	// reported height moves to 99 ...
	if err := InstallHeightView(99); err != nil {
		t.Fatalf("InstallHeightView(99): %v", err)
	}
	if h, err := StateView().ELAHeight(); err != nil || h != 99 {
		t.Fatalf("installed ELAHeight = (%d, %v), want (99, nil)", h, err)
	}
	// ... while the recharge read still answers, because it is not pinned to that
	// height. A record written at ELA height 100 must stay visible from a view
	// pinned at 99; pgp block 30115 is the real-world case of the same gap
	// (header.Nonce 2,076,267 against a record at 2,076,538).
	if _, _, err := StateView().Recharge(testElaHash); err != nil {
		t.Fatalf("Recharge from a view pinned below the record: %v, want success", err)
	}

	// After Uninstall everything must fail: this is what stops a forgotten
	// InstallHeightView from silently answering with the previous block's height.
	UninstallHeightView()
	if CurrentRechargeView() != nil {
		t.Fatal("CurrentRechargeView() is non-nil after UninstallHeightView")
	}
	if _, err := StateView().ELAHeight(); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("ELAHeight after uninstall = %v, want ErrLegacyStateViewMissing", err)
	}
	if _, _, err := StateView().Recharge(testElaHash); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("Recharge after uninstall = %v, want ErrLegacyStateViewMissing", err)
	}
}
