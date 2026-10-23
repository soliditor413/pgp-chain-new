package spv

import (
	"errors"

	"github.com/elastos/Elastos.ELA.SPV/util"
)

// errSPVNotReady is returned by the package level accessors when the SPV
// service has not been started yet.
//
// It must never be observed on the block execution path: a precompile that
// returns an error makes the *transaction* revert instead of aborting the
// block, which would silently fork stateRoot from a pgp node that had SPV
// running. The readiness gate on the download side (consensus/pgp) is what
// keeps legacy blocks away from InsertChain until Ready() is true; this error
// is the safety net for the case where that gate is bypassed.
var errSPVNotReady = errors.New("spv: service is not started")

// ready reports whether the SPV service is started and its header store is
// usable.
//
// Deliberately NOT exported. It looks like a natural gate for "may we execute
// legacy blocks yet?", and that use is actively harmful: legacy sync currently
// works with SPV never started (nothing in the synced range touches
// 0x3e8..0x3f3), so gating execution on this would stop the whole pipeline for
// a condition that does not block it. The intended behaviour is to fail loudly
// on the specific block that needs SPV data, not to refuse every block in
// advance.
//
// pgp-chain had no readiness check at all: every precompile dereferenced the
// package level SpvService directly, so a node that had not finished starting
// SPV panicked mid-block instead of returning an error.
func ready() bool {
	s := SpvService
	if s == nil || s.SPVService == nil {
		return false
	}
	return s.HeaderStore() != nil
}

// GetELAHeader returns the ELA main chain header at the given height.
//
// Nil safe counterpart of (*Service).GetELAHeader; use this one from code that
// may run before NewService. Returning an error (instead of panicking) is what
// turns "SPV is not up" into a failed block at a known height.
func GetELAHeader(height uint32) (*util.Header, error) {
	if !ready() {
		return nil, errSPVNotReady
	}
	return SpvService.GetELAHeader(height)
}

// GetHeaderBest returns the ELA main chain header at the SPV head.
//
// WARNING: this is a live-head read. It exists for the 0x3f0
// `getMainChainLatestHeight` precompile, whose *purpose* is to expose the
// current ELA height, but it is not replay safe: see the CONSENSUS-RISK notes in
// legacy_state_view.go and GetSpvHeight.
func GetHeaderBest() (*util.Header, error) {
	if !ready() {
		return nil, errSPVNotReady
	}
	return SpvService.HeaderStore().GetBest()
}
