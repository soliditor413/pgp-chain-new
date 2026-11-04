package params

import (
	"math"
	"math/big"
	"testing"
)

func TestApplyPGPForkHeightFillsEveryBlockFork(t *testing.T) {
	cfg := &ChainConfig{ChainID: big.NewInt(860621), Legacy: &LegacyConfig{ForkHeight: 642}}

	cfg.ApplyPGPForkHeight()

	for _, fork := range cfg.pgpDerivedBlockForks() {
		if *fork == nil {
			t.Fatal("a block fork was left nil")
		}
		if (*fork).Uint64() != 643 {
			t.Fatalf("fork scheduled at %v, want 643", *fork)
		}
	}
}

// The sentinel matters: math.MaxUint64+1 wraps to zero, and a fork at block 0
// means "active since genesis" -- the opposite of "never".
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
	if cfg.PlatoBlock == nil || cfg.PlatoBlock.Uint64() != 643 {
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
