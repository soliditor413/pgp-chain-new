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
	// The production path applies ApplyPGPForkHeight before validating
	// (core/genesis.go), so validating the bare literal checks a configuration
	// that never runs. Copy first: Apply mutates, and the global must stay
	// pristine for the other tests in this package.
	cfg := *PGPChainConfig
	cfg.ApplyPGPForkHeight()
	if err := cfg.CheckConfigForkOrder(); err != nil {
		t.Fatalf("PGPChainConfig fails fork-order validation: %v", err)
	}
	if PGPChainConfig.FeynmanTime != nil {
		t.Fatal("FeynmanTime must stay nil until launch, or the fork checksum changes")
	}
}

// TestForkOrderTreatsSentinelAsUnscheduled pins the rule that lets Luban sit at
// math.MaxInt64 while Plato sits at ForkHeight: a fork at or above the sentinel
// is "never", so it neither requires its predecessors nor constrains its
// successors.
func TestForkOrderTreatsSentinelAsUnscheduled(t *testing.T) {
	const f = uint64(642)
	// The whole pre-Luban chain has to be present. CheckConfigForkOrder skips
	// the first missing non-optional fork, so a config that starts at Planck
	// reports "gibbsBlock not enabled" before it ever reaches Luban.
	base := func() *ChainConfig {
		return &ChainConfig{
			ChainID:         big.NewInt(1),
			MirrorSyncBlock: new(big.Int).SetUint64(f - 6),
			BrunoBlock:      new(big.Int).SetUint64(f - 5),
			EulerBlock:      new(big.Int).SetUint64(f - 4),
			GibbsBlock:      new(big.Int).SetUint64(f - 3),
			PlanckBlock:     new(big.Int).SetUint64(f - 2),
			PlatoBlock:      new(big.Int).SetUint64(f),
			// Without Parlia, IsNotInBSC is true and CheckConfigForkOrder
			// returns nil on its second line: every assertion below would pass
			// without checking anything.
			Parlia: &ParliaConfig{},
		}
	}

	// Scheduled never + successors scheduled at F: accepted.
	ok := base()
	if ok.IsNotInBSC() {
		t.Fatal("this test is vacuous: the config is not in BSC, so CheckConfigForkOrder returns nil " +
			"immediately and the sentinel rule is never exercised")
	}
	ok.LubanBlock = big.NewInt(math.MaxInt64)
	if err := ok.CheckConfigForkOrder(); err != nil {
		t.Fatalf("luban at the sentinel must not constrain plato: %v", err)
	}

	// The negative control: a fork that is merely ABSENT must still be
	// rejected. If this passes, the sentinel rule has quietly disabled the
	// skip detection it was supposed to leave alone.
	missing := base()
	if err := missing.CheckConfigForkOrder(); err == nil {
		t.Fatal("a nil luban fork must still be rejected when plato is scheduled")
	}

	// A real luban before plato is the normal case and must still pass.
	normal := base()
	normal.LubanBlock = new(big.Int).SetUint64(f - 1)
	if err := normal.CheckConfigForkOrder(); err != nil {
		t.Fatalf("normal luban ordering was rejected: %v", err)
	}
}
