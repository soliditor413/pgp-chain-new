// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package parlia

import (
	"math"
	"math/big"
	"testing"
)

// TestShouldInitContractOnlyAtBlockOne pins the predicate that decides whether
// Finalize/FinalizeAndAssemble initialise the system contracts.
//
// This is the chain-halting regression. Before the deploy-time injection
// existed, the predicate also fired at Legacy.ForkHeight, because that was
// where the contracts were created. Now they are created and initialised at
// Legacy.SystemContractDeployHeight, i.e. before the fork, so a second
// initContract at ForkHeight sends init() to contracts that are already
// initialised: every one of them reverts, applyTransaction returns, and the
// fork block fails. The chain stops at the fork, with no consensus bug to point
// at -- just a revert in a block nobody can produce.
//
// Block 1 must keep firing: it is a legacy block whose alloc has no code, so
// the call is a no-op, and replaying the history depends on that no-op.
func TestShouldInitContractOnlyAtBlockOne(t *testing.T) {
	p := &Parlia{}

	for _, tc := range []struct {
		name   string
		number *big.Int
		want   bool
	}{
		{"genesis", big.NewInt(0), false},
		{"block 1", big.NewInt(1), true},
		{"block 2", big.NewInt(2), false},
		{"last legacy block", big.NewInt(99), false},
		{"fork height", big.NewInt(100), false},
		{"past the fork", big.NewInt(101), false},
		{"nil number", nil, false},
	} {
		got := p.shouldInitContract(tc.number)
		// The last legacy block and the fork height matter most: those are the
		// two blocks the old predicate confused.
		if got != tc.want {
			t.Fatalf("%s: shouldInitContract(%v) = %v, want %v", tc.name, tc.number, got, tc.want)
		}
	}
}

// TestShouldInitContractIgnoresForkHeight guards the exact regression: build the
// predicate's inputs the way a real PGP chain does (ForkHeight set, not
// MaxUint64) and require that no height related to the fork returns true except
// 1. A refactor that reintroduces "init at the fork" passes the table above only
// by accident, never this one.
func TestShouldInitContractIgnoresForkHeight(t *testing.T) {
	p := &Parlia{}
	fork := uint64(642)

	for _, h := range []uint64{fork - 2, fork - 1, fork, fork + 1, fork + 2} {
		if p.shouldInitContract(new(big.Int).SetUint64(h)) {
			t.Fatalf("shouldInitContract(%d) = true with ForkHeight %d: the fork block would re-initialise "+
				"already-initialised contracts and revert", h, fork)
		}
	}
	if !p.shouldInitContract(big.NewInt(1)) {
		t.Fatalf("shouldInitContract(1) = false: the legacy block-1 no-op initialisation is part of the " +
			"history being replayed")
	}
	if fork == math.MaxUint64 {
		t.Fatalf("fixture error: fork height collides with the no-fork sentinel")
	}
}
