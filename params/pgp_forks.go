package params

import (
	"math"
	"math/big"
)

// pgpDerivedBlockForks returns the BSC-only block forks that a pgp chain has to
// schedule from its Legacy.ForkHeight, paired with the offset from that height.
//
// Every one of them sits at ForkHeight itself. The fork block is the first one
// Parlia produces, so it is also the first block whose rules have to be the
// post-switch ones: Luban decides what a validator set is (BLS vote addresses,
// the extra-data layout), Plato enables the finality-reward call, and the rest
// gate precompiles and gas rules that the fork block must already agree on.
//
// Time-based forks (KeplerTime, FeynmanTime, ...) are deliberately absent: they
// are compared against header timestamps, and a block height cannot be turned
// into a timestamp before that block exists. See
// docs/design/pgp-system-contract-deploy-design.md.
func (c *ChainConfig) pgpDerivedBlockForks() []struct {
	target **big.Int
	offset uint64
} {
	return []struct {
		target **big.Int
		offset uint64
	}{
		{&c.LubanBlock, 0},
		{&c.RamanujanBlock, 0},
		{&c.NielsBlock, 0},
		{&c.MirrorSyncBlock, 0},
		{&c.BrunoBlock, 0},
		{&c.EulerBlock, 0},
		{&c.GibbsBlock, 0},
		{&c.NanoBlock, 0},
		{&c.MoranBlock, 0},
		{&c.PlanckBlock, 0},
		{&c.PlatoBlock, 0},
		{&c.HertzBlock, 0},
		{&c.HertzfixBlock, 0},
	}
}

// ApplyPGPForkHeight fills in the BSC-only block forks that are still nil,
// scheduling Luban at Legacy.ForkHeight and the rest one block later.
//
// It is idempotent and never overwrites a value that the configuration already
// carries: an operator that pins lubanBlock explicitly keeps their number. It
// is a no-op when the chain has no legacy section, or when ForkHeight is still
// the "never fork" sentinel (math.MaxUint64) -- in that case the offsets would
// wrap around and silently activate BSC forks at genesis, which is the exact
// opposite of what the sentinel means.
func (c *ChainConfig) ApplyPGPForkHeight() {
	if c == nil || c.Legacy == nil {
		return
	}
	fh := c.Legacy.ForkHeight
	if fh == 0 || fh == math.MaxUint64 {
		return
	}
	for _, fork := range c.pgpDerivedBlockForks() {
		if *fork.target == nil {
			*fork.target = new(big.Int).SetUint64(fh + fork.offset)
		}
	}
}
