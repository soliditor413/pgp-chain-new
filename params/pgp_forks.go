package params

import (
	"math"
	"math/big"
)

// pgpDerivedBlockForks returns the BSC-only block forks that a pgp chain has to
// schedule from its Legacy.ForkHeight, paired with the offset from that height.
//
// Luban sits at ForkHeight itself: it is the fork that changes what a validator
// set *is* (BLS vote addresses, the extra-data layout), and the fork block is
// the first one Parlia produces, so it has to be read with the post-Luban rules
// from the start. Every other BSC fork sits one block later -- none of them is
// needed to produce the fork block, and keeping them off it keeps the switch
// itself minimal.
//
// Time-based forks (KeplerTime, FeynmanTime, ...) are deliberately absent: they
// are compared against header timestamps, and a block height cannot be turned
// into a timestamp before that block exists. Scheduling them needs either the
// real fork timestamp or a rule stated in terms of the fork block's time; see
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
		{&c.RamanujanBlock, 1},
		{&c.NielsBlock, 1},
		{&c.MirrorSyncBlock, 1},
		{&c.BrunoBlock, 1},
		{&c.EulerBlock, 1},
		{&c.GibbsBlock, 1},
		{&c.NanoBlock, 1},
		{&c.MoranBlock, 1},
		{&c.PlanckBlock, 1},
		{&c.PlatoBlock, 1},
		{&c.HertzBlock, 1},
		{&c.HertzfixBlock, 1},
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
