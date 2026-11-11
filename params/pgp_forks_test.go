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
// survive the same fork-order validation a genesis file does.
//
// On the fork checksum: this config does carry time forks now, and that does not
// move it. core/forkid drops every time fork at or before genesis
// (forkid.go:315-317), which is exactly the 0-valued group, and the block forks
// pinned at MaxInt64 short-circuit NewID before it ever looks at time forks. So
// the checksum stays crc32(genesis) and Next stays the MaxInt64 sentinel - what
// this test pins instead is the *shape* of the schedule, because the shape is
// what CheckConfigForkOrder enforces and what is easy to break by hand.
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

	// The 0-valued group is a prefix of the fork order: the order check compares
	// timestamps, so any fork pinned at 0 *after* one pinned at MaxInt64 reads as
	// a rewind. Haber sits after Cancun in that order and shares its activation
	// time on BSC mainnet, which is why it belongs to the deferred group even
	// though its own effect (the 0x100 precompile) is execution-layer.
	for _, f := range []struct {
		name string
		got  *uint64
	}{
		{"keplerTime", cfg.KeplerTime},
		{"feynmanTime", cfg.FeynmanTime},
		{"feynmanFixTime", cfg.FeynmanFixTime},
	} {
		if f.got == nil || *f.got != 0 {
			t.Errorf("%s = %v, want 0 (execution-layer fork active since genesis)", f.name, f.got)
		}
	}
	for _, f := range []struct {
		name string
		got  *uint64
	}{
		{"cancunTime", cfg.CancunTime},
		{"haberTime", cfg.HaberTime},
		{"bohrTime", cfg.BohrTime},
		{"pragueTime", cfg.PragueTime},
		{"lorentzTime", cfg.LorentzTime},
		{"jennerTime", cfg.JennerTime},
	} {
		if f.got == nil || *f.got != math.MaxInt64 {
			t.Errorf("%s = %v, want the MaxInt64 sentinel (deferred until the fork height is pinned)", f.name, f.got)
		}
	}
	// Blob schedule entries are mandatory once a blob fork is configured.
	if cfg.BlobScheduleConfig == nil || cfg.BlobScheduleConfig.Cancun == nil {
		t.Fatal("CancunTime is set, so BlobScheduleConfig must carry a Cancun entry")
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
