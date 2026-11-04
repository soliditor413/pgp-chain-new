package params

import (
	"math"
	"math/big"
)

// pgpDerivedBlockForks returns the BSC-only block forks that a pgp chain has to
// schedule from its Legacy.ForkHeight.
//
// Rationale for ForkHeight+1 rather than ForkHeight: the legacy segment ends at
// ForkHeight-1 and the Parlia engine takes over at ForkHeight. Putting every
// BSC fork on the block *after* the switch keeps the whole legacy history --
// including the fork block's own parent lookups -- on the pre-Luban rules,
// which is what pgp-chain actually ran.
//
// Time-based forks (KeplerTime, FeynmanTime, ...) are deliberately absent: they
// are compared against header timestamps, and a block height cannot be turned
// into a timestamp before that block exists. Scheduling them needs either the
// real fork timestamp or a rule stated in terms of the fork block's time; see
// docs/design/pgp-system-contract-deploy-design.md.
func (c *ChainConfig) pgpDerivedBlockForks() []**big.Int {
	return []**big.Int{
		&c.RamanujanBlock,
		&c.NielsBlock,
		&c.MirrorSyncBlock,
		&c.BrunoBlock,
		&c.EulerBlock,
		&c.GibbsBlock,
		&c.NanoBlock,
		&c.MoranBlock,
		&c.PlanckBlock,
		&c.LubanBlock,
		&c.PlatoBlock,
		&c.HertzBlock,
		&c.HertzfixBlock,
	}
}

// ApplyPGPForkHeight fills in the BSC-only block forks that are still nil, using
// Legacy.ForkHeight+1.
//
// It is idempotent and never overwrites a value that the configuration already
// carries: an operator that pins lubanBlock explicitly keeps their number. It
// is a no-op when the chain has no legacy section, or when ForkHeight is still
// the "never fork" sentinel (math.MaxUint64) -- in that case ForkHeight+1 would
// wrap to zero and silently activate every BSC fork at genesis, which is the
// exact opposite of what the sentinel means.
func (c *ChainConfig) ApplyPGPForkHeight() {
	if c == nil || c.Legacy == nil {
		return
	}
	fh := c.Legacy.ForkHeight
	if fh == 0 || fh == math.MaxUint64 {
		return
	}
	next := new(big.Int).SetUint64(fh + 1)
	for _, fork := range c.pgpDerivedBlockForks() {
		if *fork == nil {
			*fork = new(big.Int).Set(next)
		}
	}
}
