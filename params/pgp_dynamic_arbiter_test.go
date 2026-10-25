// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package params

import (
	"math/big"
	"testing"
)

// TestMainnetCarriesTheFlagDefaultDynamicArbiterHeight pins the value that the
// SPV listener gate is compared against.
//
// 2075778 is not an arbitrary number: it is the default of pgp-chain's
// --spv.arbiter.height flag (pgp-chain/cmd/utils/flags.go:808-812), and it is
// what every real mainnet node ran with, because no operator overrides a flag
// that they have no reason to touch. cmd/pgp/main.go:456-463 reads it back out
// of the chain config and hands it to spv.NewService.
//
// The number matters because the listener compares it against the ELA block
// height. Below it InitNextTurnDposInfo returns early and the POW->DPOS
// transition in onBlockHandled is unreachable; at or above it the listener
// starts maintaining the next-turn producer set. This port shipped
// math.MaxUint64 for a while, which put the boundary past any height the ELA
// chain will ever reach, so the gate never opened and every live block logged
// "l.dynamicArbiterHeight = 18446744073709551615".
func TestMainnetCarriesTheFlagDefaultDynamicArbiterHeight(t *testing.T) {
	if got := PGPChainConfig.DynamicArbiterHeight(); got != 2075778 {
		t.Fatalf("mainnet DynamicArbiterHeight = %d, want 2075778 "+
			"(the default of pgp-chain's --spv.arbiter.height)", got)
	}
	// The raw field has to carry it too, not just the accessor: a fresh datadir
	// persists the chain config as JSON, and a zero there would be re-read by
	// the next run.
	if PGPChainConfig.Legacy.DynamicArbiterHeight == 0 {
		t.Fatal("PGPChainConfig.Legacy.DynamicArbiterHeight is 0; " +
			"a fresh datadir would persist the zero and need the accessor fallback forever")
	}
	if DefaultTestnetDynamicArbiterHeight != 1496588 {
		t.Fatalf("testnet DynamicArbiterHeight = %d, want 1496588 "+
			"(pgp-chain cmd/utils/flags.go:1652-1654)", DefaultTestnetDynamicArbiterHeight)
	}
}

// TestDynamicArbiterHeightFallback covers the four inputs the accessor can see.
//
// The fallback is not cosmetic. A Legacy block whose DynamicArbiterHeight is
// zero would otherwise hand zero to the listener, which means "switch to DPOS at
// ELA height 0" — a state no pgp node was ever in. That is a different failure
// from the MaxUint64 one and just as silent.
//
// What this test is NOT about: a datadir created before the field existed. For
// the pgp genesis hash the stored config is never read back, because
// chainConfigOrDefault asks params.GetBuiltInChainConfig first and that returns
// PGPChainConfig itself (core/genesis.go:443, :543-552,
// params/config.go:603-616). An old datadir therefore picks the value up from
// this package on its own, and the zero this test feeds in comes from a
// hand-written config instead.
func TestDynamicArbiterHeightFallback(t *testing.T) {
	tests := []struct {
		name string
		cfg  *ChainConfig
		want uint64
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: 0,
		},
		{
			name: "not a legacy chain",
			cfg:  &ChainConfig{ChainID: big.NewInt(1)},
			want: 0,
		},
		{
			name: "legacy block present but the field is zero",
			cfg:  &ChainConfig{ChainID: big.NewInt(1), Legacy: &LegacyConfig{}},
			want: DefaultDynamicArbiterHeight,
		},
		{
			name: "operator override",
			cfg:  &ChainConfig{ChainID: big.NewInt(1), Legacy: &LegacyConfig{DynamicArbiterHeight: 300}},
			want: 300,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.DynamicArbiterHeight(); got != tt.want {
				t.Fatalf("DynamicArbiterHeight() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDynamicArbiterHeightIsNotAForkSwitch is the safety property behind putting
// this field under Legacy at all: it must not be mistaken for a rule boundary.
//
// ForkHeight is the only height in LegacyConfig that changes how blocks are
// validated, and it is MaxUint64 today, so every block number keeps taking the
// legacy branch regardless of the arbiter height.
func TestDynamicArbiterHeightIsNotAForkSwitch(t *testing.T) {
	cfg := *PGPChainConfig
	legacy := *PGPChainConfig.Legacy
	legacy.DynamicArbiterHeight = 1 // absurd, on purpose
	cfg.Legacy = &legacy

	for _, num := range []uint64{0, 1, 1000, 30115, 2075778, 10_000_000} {
		if !cfg.IsPGPLegacy(new(big.Int).SetUint64(num)) {
			t.Fatalf("block %d stopped being legacy after changing DynamicArbiterHeight", num)
		}
	}
}
