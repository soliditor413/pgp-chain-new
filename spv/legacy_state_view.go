package spv

import (
	"errors"
	"math/big"
	"sync"
)

// ErrLegacyStateViewMissing is returned by every replay-time read while no
// LegacyStateView has been installed.
//
// It is deliberately an error and never a zero value: a zero value ("not
// completed", "no recharge data", "not failed") silently changes the execution
// result of a block that depends on the real answer, which shows up as a state
// root mismatch much later and in the wrong place. An error stops the node on
// the exact block instead.
var ErrLegacyStateViewMissing = errors.New("spv: legacy state view is not installed")

// LegacyStateView is the replay-time view of the pgp-chain external state.
//
// SCOPE: this interface only exposes ELA chain data; this chain's own state must
// not be read through it, that is the StateDB's job (core/state_transition.go and
// the precompiles hold a vm.StateDB, spv never does).
//
// pgp-chain read some of its SPV data from the *live* node head (the IPC
// eth_call in spv/elaminter.go:113, HeaderStore().GetBest(), and the
// spvTransactiondb cache), which is not reproducible during a replay. Every such
// read must be pinned to the ELA height recorded in the block being replayed:
// pgp-chain/consensus/pbft/pbft.go:492 stores it as header.Nonce
// (types.EncodeNonce(GetSpvHeight())).
//
// Contract for every implementation:
//   - never return a zero value to mean "I do not know". Return an error.
//   - the answers must depend only on (txHash, ELA height), never on the node's
//     current head or sync progress.
type LegacyStateView interface {
	// ELAHeight is the ELA main chain height observed when the block was
	// produced.
	//
	// 0 is a legitimate value (a block sealed before the first ELA anchor), so
	// the "no view installed" case is reported as an error instead of as 0.
	// The previous shape (plain uint32 plus a package-level
	// ELAHeightForReplay helper) relied on the caller remembering to use the
	// wrapper; this interface makes forgetting impossible.
	ELAHeight() (uint32, error)

	// IsMainTxCompleted is deliberately NOT part of this interface.
	//
	// The ELAMinter at 0x...0064 is a PGP-side contract: pgp-chain asked
	// "has this ELA hash been completed?" by IPC-calling *its own node* at the
	// latest block (elaminter.go:113), so the answer is this chain's state, not
	// ELA data. It needs no ELA height, no Nonce and no view, so it does not
	// belong here; the correct home is core/state_transition.go, which already
	// holds the StateDB of the block being executed and can do
	// st.state.GetState(params.ELAMINTER, slot) without importing spv.

	// Recharge returns the recharge record of an ELA main chain tx hash and the
	// total fee, replacing the four spvTransactiondb keys
	// ("Fee"/"Address"/"Output"/"Input") that FindOutputFeeAndaddressByTxHash,
	// GetRechargeDataByTxhash and FindOutRechargeInput read today.
	Recharge(txHash string) (RechargeDatas, *big.Int, error)

	// IsFailedElaTx reports whether the ELA main chain tx was returned or
	// failed. It replaces the in-memory failedTxList plus the KV scan in
	// IsFailedElaTx; an in-memory cache is not a valid excuse to leave a read on
	// the execution path unpinned.
	IsFailedElaTx(txHash string) (bool, error)
}

// nullStateView is the placeholder installed until the ELA-height alignment
// layer is wired in.
//
// Every data method returns ErrLegacyStateViewMissing instead of a zero value.
// While it is installed, any block that touches a legacy SPV read will fail to
// execute and the node will stop on that height with an error naming the read;
// that is the intended behaviour until the real view exists.
type nullStateView struct{}

func (nullStateView) ELAHeight() (uint32, error) { return 0, ErrLegacyStateViewMissing }

func (nullStateView) Recharge(txHash string) (RechargeDatas, *big.Int, error) {
	return nil, nil, ErrLegacyStateViewMissing
}

func (nullStateView) IsFailedElaTx(txHash string) (bool, error) {
	return false, ErrLegacyStateViewMissing
}

var (
	stateViewMu sync.RWMutex
	stateView   LegacyStateView // nil means "no view installed"
)

// CurrentRechargeView returns the view pinned for the block currently being
// executed, or nil when none has been installed.
//
// Callers on the block execution path (the legacy precompiles) MUST treat nil as
// a hard error and MUST NOT fall back to "the last view" or to ELA height 0.
// The reason is that a missing view means the block execution entry point never
// called InstallHeightView, and silently reusing the previous block's view would
// answer with the *previous* ELA height — a wrong stateRoot with no error
// anywhere. eth_call / eth_estimateGas are exactly this case: their simulated
// BlockContext carries no Nonce, so they never install a view and must fail
// loudly instead of guessing.
func CurrentRechargeView() LegacyStateView {
	stateViewMu.RLock()
	defer stateViewMu.RUnlock()
	return stateView
}

// StateView is the nil-safe form of CurrentRechargeView.
//
// It never returns nil: when no view is installed it returns a placeholder whose
// every method fails with ErrLegacyStateViewMissing. Use this from code that
// wants "fail with an error" rather than "panic on a nil interface";
// CurrentRechargeView is for callers that must distinguish the two.
func StateView() LegacyStateView {
	if v := CurrentRechargeView(); v != nil {
		return v
	}
	return nullStateView{}
}

// SetStateView installs the legacy state view. Passing nil uninstalls it, which
// makes CurrentRechargeView return nil and StateView return nullStateView.
func SetStateView(v LegacyStateView) {
	stateViewMu.Lock()
	defer stateViewMu.Unlock()
	stateView = v
}

// LegacyStateViewInstalled reports whether a real view has been installed.
func LegacyStateViewInstalled() bool {
	return CurrentRechargeView() != nil
}
