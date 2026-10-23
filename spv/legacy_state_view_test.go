package spv

import (
	"errors"
	"math/big"
	"testing"
)

// fakeStateView is a fully populated view, used to prove the call sites really
// route through LegacyStateView instead of the local KV store or the live head.
type fakeStateView struct {
	height   uint32
	recharge map[string]RechargeDatas
	failed   map[string]bool
	err      error
}

func (f *fakeStateView) ELAHeight() (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.height, nil
}

func (f *fakeStateView) Recharge(txHash string) (RechargeDatas, *big.Int, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	data, ok := f.recharge[txHash]
	if !ok {
		return nil, nil, errors.New("no recharge record for " + txHash)
	}
	return data, big.NewInt(7), nil
}

func (f *fakeStateView) IsFailedElaTx(txHash string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.failed[txHash], nil
}

// TestNullStateViewNeverReturnsZeroValue pins the contract that matters most:
// the placeholder must fail, never answer. A zero value here ("height 0", "no
// recharge data", "not failed") silently changes the result of a replayed block
// instead of stopping on it.
//
// ELAHeight is the one that bit us: a plain uint32 forced every caller through a
// convention ("use the wrapper, not the method"), and the convention is exactly
// what gets forgotten. It now returns an error like the rest.
func TestNullStateViewNeverReturnsZeroValue(t *testing.T) {
	SetStateView(nil)
	defer SetStateView(nil)

	if LegacyStateViewInstalled() {
		t.Fatal("LegacyStateViewInstalled() = true right after SetStateView(nil)")
	}

	v := StateView()
	if h, err := v.ELAHeight(); !errors.Is(err, ErrLegacyStateViewMissing) || h != 0 {
		t.Fatalf("ELAHeight = (%d, %v), want (0, ErrLegacyStateViewMissing)", h, err)
	}
	if _, _, err := v.Recharge("0xdead"); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("Recharge error = %v, want ErrLegacyStateViewMissing", err)
	}
	if _, err := v.IsFailedElaTx("0xdead"); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("IsFailedElaTx error = %v, want ErrLegacyStateViewMissing", err)
	}
}

func TestELAHeightWithRealView(t *testing.T) {
	SetStateView(&fakeStateView{height: 4242})
	defer SetStateView(nil)

	if !LegacyStateViewInstalled() {
		t.Fatal("LegacyStateViewInstalled() = false with a real view")
	}
	h, err := StateView().ELAHeight()
	if err != nil {
		t.Fatalf("ELAHeight error = %v", err)
	}
	if h != 4242 {
		t.Fatalf("ELAHeight = %d, want 4242", h)
	}

	// A real view may legitimately pin height 0 (a block sealed before the first
	// ELA anchor); that must not be confused with "no view installed".
	SetStateView(&fakeStateView{height: 0})
	if h, err := StateView().ELAHeight(); err != nil || h != 0 {
		t.Fatalf("ELAHeight = (%d, %v), want (0, nil)", h, err)
	}

	// An erroring view must not degrade into a usable height either.
	SetStateView(&fakeStateView{err: errors.New("boom")})
	if _, err := StateView().ELAHeight(); err == nil {
		t.Fatal("expected the view error to propagate from ELAHeight")
	}
}

// TestGetArbitersRefusesWithoutView covers the first live-head read point: it
// used to take the ELA height off the live head and is called by the 0x3e8
// precompile.
func TestGetArbitersRefusesWithoutView(t *testing.T) {
	SetStateView(nil)
	defer SetStateView(nil)

	if _, _, err := GetArbiters(); !errors.Is(err, ErrLegacyStateViewMissing) {
		t.Fatalf("GetArbiters error = %v, want ErrLegacyStateViewMissing", err)
	}
}
