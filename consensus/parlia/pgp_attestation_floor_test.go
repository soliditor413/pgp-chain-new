package parlia

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// TestMinAttestationBlockNumber pins the two floors that decide when a block may
// carry a vote attestation.
//
// The case that matters is the fork boundary: assembling at the first Parlia
// block asks snapshot() for ForkHeight-2, one block below the anchor, and the
// anchor is only recognised by exact number - so the walk continues into the
// legacy genesis and parseValidators fails there. Skipping that single block is
// what keeps the fork from stalling on its first block.
func TestMinAttestationBlockNumber(t *testing.T) {
	cases := []struct {
		name   string
		legacy *params.LegacyConfig
		want   uint64
	}{
		{"no legacy section (BSC mainnet, chapel)", nil, 3},
		{"legacy but no fork scheduled", &params.LegacyConfig{ForkHeight: math.MaxUint64}, 3},
		{"fork at genesis", &params.LegacyConfig{ForkHeight: 0}, 3},
		{"fork too early for the floor to matter", &params.LegacyConfig{ForkHeight: 1}, 3},
		{"fork exactly at the floor", &params.LegacyConfig{ForkHeight: 2}, 3},
		{"fork just above the floor", &params.LegacyConfig{ForkHeight: 3}, 4},
		{"fork like a devnet", &params.LegacyConfig{ForkHeight: 642}, 643},
	}
	for _, c := range cases {
		p := &Parlia{chainConfig: &params.ChainConfig{ChainID: big.NewInt(1), Legacy: c.legacy}}
		if got := p.minAttestationBlockNumber(); got != c.want {
			t.Errorf("%s: minAttestationBlockNumber() = %d, want %d", c.name, got, c.want)
		}
	}
}
