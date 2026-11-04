package params

import (
	"math"
	"math/big"
	"testing"
)

func TestApplyPGPForkHeightSchedulesLubanAtTheFork(t *testing.T) {
	cfg := &ChainConfig{ChainID: big.NewInt(860621), Legacy: &LegacyConfig{ForkHeight: 642}}

	cfg.ApplyPGPForkHeight()

	// Luban defines what a validator set is (BLS vote addresses, the extra-data
	// layout), and the fork block is the first one Parlia produces, so it has to
	// be active there rather than a block later.
	if cfg.LubanBlock == nil || cfg.LubanBlock.Uint64() != 642 {
		t.Fatalf("lubanBlock = %v, want 642", cfg.LubanBlock)
	}
	for _, fork := range cfg.pgpDerivedBlockForks() {
		if *fork.target == nil {
			t.Fatal("a block fork was left nil")
		}
		want := uint64(642) + fork.offset
		if (*fork.target).Uint64() != want {
			t.Fatalf("fork scheduled at %v, want %d", *fork.target, want)
		}
	}
}

// The sentinel matters: incrementing math.MaxUint64 wraps to zero, and a fork at
// block 0 means "active since genesis" -- the opposite of "never".
func TestApplyPGPForkHeightIgnoresTheSentinel(t *testing.T) {
	for _, height := range []uint64{0, math.MaxUint64} {
		cfg := &ChainConfig{Legacy: &LegacyConfig{ForkHeight: height}}
		cfg.ApplyPGPForkHeight()
		if cfg.LubanBlock != nil {
			t.Fatalf("ForkHeight %d must leave the forks unscheduled, got luban=%v", height, cfg.LubanBlock)
		}
	}
}

// An operator who pinned a fork keeps it; only the gaps are filled.
func TestApplyPGPForkHeightKeepsPinnedValues(t *testing.T) {
	cfg := &ChainConfig{
		LubanBlock: big.NewInt(100),
		Legacy:     &LegacyConfig{ForkHeight: 642},
	}

	cfg.ApplyPGPForkHeight()

	if cfg.LubanBlock.Uint64() != 100 {
		t.Fatalf("pinned lubanBlock was overwritten: %v", cfg.LubanBlock)
	}
	if cfg.PlatoBlock == nil || cfg.PlatoBlock.Uint64() != 642 {
		t.Fatalf("unpinned platoBlock not filled: %v", cfg.PlatoBlock)
	}
}

func TestApplyPGPForkHeightWithoutLegacyIsANoop(t *testing.T) {
	cfg := &ChainConfig{}
	cfg.ApplyPGPForkHeight()
	if cfg.LubanBlock != nil {
		t.Fatal("a chain without a legacy section must not gain forks")
	}
}

// Idempotent: the second call must not move anything.
func TestApplyPGPForkHeightIsIdempotent(t *testing.T) {
	cfg := &ChainConfig{Legacy: &LegacyConfig{ForkHeight: 7}}
	cfg.ApplyPGPForkHeight()
	first := cfg.LubanBlock.Uint64()
	cfg.ApplyPGPForkHeight()
	if cfg.LubanBlock.Uint64() != first {
		t.Fatal("second call moved a fork")
	}
}

// The built-in pgp config is what every mainnet node starts from, so it has to
// survive the same fork-order validation a genesis file does. The time-based
// forks stay nil: gatherForks counts any non-nil value -- MaxUint64 included --
// into the EIP-2124 checksum, and this config's checksum has to keep matching
// pgp-chain's own client.
func TestPGPChainConfigPassesForkOrder(t *testing.T) {
	if err := PGPChainConfig.CheckConfigForkOrder(); err != nil {
		t.Fatalf("PGPChainConfig fails fork-order validation: %v", err)
	}
	if PGPChainConfig.FeynmanTime != nil {
		t.Fatal("FeynmanTime must stay nil until launch, or the fork checksum changes")
	}
}
