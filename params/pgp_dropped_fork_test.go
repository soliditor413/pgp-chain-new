// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package params

import (
	"math"
	"math/big"
	"testing"
)

// TestDroppedLegacyForkKeepsForkOrderLegal answers a configuration question:
// can a fork that only exists for BSC's history be left out entirely, while the
// modern ones are scheduled at the pgp fork height?
//
// The order check treats a fork pinned at the sentinel as absent *and optional*
// (params/config.go, "neither requires its predecessors nor constrains its
// successors"), so the answer is yes -- but only when it is pinned at the
// sentinel. nil is not the same thing: nil means "not configured", and the check
// keeps rejecting a chain that skips a required fork.
func TestDroppedLegacyForkKeepsForkOrderLegal(t *testing.T) {
	const fork = uint64(64)

	base := func() *ChainConfig {
		cfg := &ChainConfig{
			ChainID:             big.NewInt(860621),
			HomesteadBlock:      big.NewInt(1),
			EIP150Block:         big.NewInt(2),
			EIP155Block:         big.NewInt(3),
			EIP158Block:         big.NewInt(3),
			ByzantiumBlock:      big.NewInt(3),
			ConstantinopleBlock: big.NewInt(3),
			PetersburgBlock:     big.NewInt(3),
			IstanbulBlock:       big.NewInt(3),
			BerlinBlock:         big.NewInt(3),
			LondonBlock:         big.NewInt(3),
			KeplerTime:          newUint64(0),
			FeynmanTime:         newUint64(0),
			FeynmanFixTime:      newUint64(0),
			Parlia:              &ParliaConfig{},
			Legacy: &LegacyConfig{
				ForkHeight:                 fork,
				SystemContractDeployHeight: fork - 2,
			},
		}
		return cfg
	}

	// Baseline: nothing pinned, everything derived.
	all := base()
	all.ApplyPGPForkHeight()
	if err := all.CheckConfigForkOrder(); err != nil {
		t.Fatalf("all forks derived: %v", err)
	}
	if all.EulerBlock == nil || all.EulerBlock.Uint64() != fork {
		t.Fatalf("derivation should have scheduled eulerBlock: %v", all.EulerBlock)
	}

	// Dropped: euler pinned at the sentinel instead of being derived.
	drop := base()
	drop.EulerBlock = big.NewInt(math.MaxInt64)
	drop.ApplyPGPForkHeight()
	if err := drop.CheckConfigForkOrder(); err != nil {
		t.Fatalf("a fork pinned at the sentinel must not constrain its successors: %v", err)
	}
	if drop.EulerBlock.Int64() != math.MaxInt64 {
		t.Fatalf("ApplyPGPForkHeight overwrote a pinned sentinel: %v", drop.EulerBlock)
	}
	if drop.LubanBlock == nil || drop.LubanBlock.Uint64() != fork {
		t.Fatalf("luban must still be derived: %v", drop.LubanBlock)
	}
}
