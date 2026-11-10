// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package params

import (
	"math"
	"math/big"
	"strings"
	"testing"
)

// TestCheckDeployHeight pins the rules on Legacy.SystemContractDeployHeight.
//
// The one that matters most is D <= ForkHeight-2. The fork block reads the
// validator set out of 0x1000, so a deploy height at the fork or after it means
// the very first Parlia block runs against empty accounts: every read returns
// nothing, the block reverts, and the chain stops. The spare block below the
// fork is what lets an operator retry instead of being stuck.
func TestCheckDeployHeight(t *testing.T) {
	tests := []struct {
		name      string
		fork      uint64
		deploy    uint64
		wantError string // empty means "must be accepted"
	}{
		{"no injection on an unforked chain", math.MaxUint64, 0, ""},
		// F-18: a forked chain without an injection has no initialisation path
		// left, so it is a config error rather than a silently broken chain.
		{"no injection on a forked chain", 100, 0, "requires legacy.systemContractDeployHeight"},
		{"normal", 100, 10, ""},
		{"tightest legal", 100, 98, ""},
		{"two blocks below the fork", 4, 2, ""},
		{"before EIP-150", 10, 1, "before EIP-150"},
		{"at the fork", 100, 100, "forkHeight-2"},
		{"one above the tightest legal", 100, 99, "forkHeight-2"},
		{"past the fork", 100, 101, "forkHeight-2"},
		{"no fork height", math.MaxUint64, 10, "requires a fixed legacy.forkHeight"},
		{"fork height zero", 0, 10, "requires a fixed legacy.forkHeight"},
		{"fork height too small", 2, 2, "leaves no room"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ChainConfig{
				ChainID: big.NewInt(1),
				Legacy: &LegacyConfig{
					ForkHeight:                 tc.fork,
					SystemContractDeployHeight: tc.deploy,
				},
			}
			err := cfg.checkDeployHeight()
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("forkHeight %d deployHeight %d: unexpected error %v", tc.fork, tc.deploy, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("forkHeight %d deployHeight %d: accepted, want an error mentioning %q", tc.fork, tc.deploy, tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error %q does not mention %q", err, tc.wantError)
			}
		})
	}
}

// TestCheckConfigForkOrderChecksDeployHeight makes sure the rule is actually
// reachable: CheckConfigForkOrder returns early for non-BSC configs, and a
// legacy chain without Parlia is one of those, so a check placed after that
// return would never run on the chain that needs it.
func TestCheckConfigForkOrderChecksDeployHeight(t *testing.T) {
	cfg := &ChainConfig{
		ChainID: big.NewInt(1),
		Legacy: &LegacyConfig{
			ForkHeight:                 100,
			SystemContractDeployHeight: 100, // at the fork: illegal
		},
	}
	if !cfg.IsNotInBSC() {
		t.Fatalf("fixture is not the non-BSC shape this test is about")
	}
	err := cfg.CheckConfigForkOrder()
	if err == nil || !strings.Contains(err.Error(), "systemContractDeployHeight") {
		t.Fatalf("CheckConfigForkOrder = %v, want the deploy-height error even for a non-BSC config", err)
	}
}
