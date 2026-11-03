// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eip1559

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// pgpForkConfig builds a chain shaped like the pgp fork: Parlia is present (so
// the BSC base-fee rule applies and IsInBSC() is true), London is reported
// active from londonBlock, and blocks below forkHeight belong to the legacy
// pgp history.
func pgpForkConfig(londonBlock int64, forkHeight uint64) *params.ChainConfig {
	cfg := copyConfig(params.TestChainConfig)
	cfg.Ethash = nil
	cfg.Parlia = &params.ParliaConfig{}
	cfg.LondonBlock = big.NewInt(londonBlock)
	cfg.Legacy = &params.LegacyConfig{ForkHeight: forkHeight}
	return cfg
}

// TestPGPLegacyParentWithoutBaseFee is the regression test for the fork block
// being rejected by every node except the one that mined it.
//
// pgp-chain's legacy headers have no baseFee field, so the parent of the first
// Parlia block has BaseFee == nil while the chain config reports that height as
// London. VerifyEIP1559Header used to treat that as a malformed parent and
// return "parent header is missing baseFee", which made block ForkHeight
// unimportable: the miner wrote it (WriteBlockWithState does not re-verify) and
// every follower dropped it, so the chain froze at ForkHeight.
//
// Both layouts that matter are covered: londonBlock 0 (the devnet, where every
// legacy height claims to be London) and londonBlock 3 with a high fork height
// (pgp mainnet, where the legacy history spans from block 3 to ForkHeight-1).
func TestPGPLegacyParentWithoutBaseFee(t *testing.T) {
	for _, tc := range []struct {
		name        string
		londonBlock int64
		forkHeight  uint64
	}{
		{"devnet-london-at-genesis", 0, 20},
		{"mainnet-shaped", 3, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pgpForkConfig(tc.londonBlock, tc.forkHeight)
			// The legacy parent: no BaseFee at all, which is what pgp-chain
			// actually produced.
			parent := &types.Header{
				Number:   new(big.Int).SetUint64(tc.forkHeight - 1),
				GasLimit: 30000000,
			}
			header := &types.Header{
				Number:   new(big.Int).SetUint64(tc.forkHeight),
				GasLimit: 30000000,
				BaseFee:  new(big.Int), // InitialBaseFeeForBSC == 0
			}
			if err := VerifyEIP1559Header(cfg, parent, header); err != nil {
				t.Fatalf("fork block rejected against a legacy parent: %v", err)
			}
		})
	}
}

// TestPGPLegacyParentBaseFeeIsStillConstrained makes sure the exemption above
// is a missing-field allowance and not a hole: a fork block that invents a
// non-zero baseFee is still rejected, because CalcBaseFee returns the BSC
// constant (zero) for a legacy parent.
func TestPGPLegacyParentBaseFeeIsStillConstrained(t *testing.T) {
	const fork = 20
	cfg := pgpForkConfig(0, fork)
	parent := &types.Header{Number: big.NewInt(fork - 1), GasLimit: 30000000}
	header := &types.Header{
		Number:   big.NewInt(fork),
		GasLimit: 30000000,
		BaseFee:  big.NewInt(1),
	}
	err := VerifyEIP1559Header(cfg, parent, header)
	if err == nil {
		t.Fatal("a fork block with baseFee 1 against a legacy parent must be rejected")
	}
	if !strings.Contains(err.Error(), "invalid baseFee") {
		t.Fatalf("wrong rejection reason: %v", err)
	}
}

// TestPGPParentWithoutBaseFeeAfterFork guards the scope of the exemption: from
// ForkHeight onwards the parent is a Parlia block, so a nil baseFee there is a
// genuine malformation and must still fail. Without this case the change would
// be indistinguishable from deleting the check.
func TestPGPParentWithoutBaseFeeAfterFork(t *testing.T) {
	const fork = 20
	cfg := pgpForkConfig(0, fork)
	parent := &types.Header{Number: big.NewInt(fork + 5), GasLimit: 30000000}
	header := &types.Header{
		Number:   big.NewInt(fork + 6),
		GasLimit: 30000000,
		BaseFee:  new(big.Int),
	}
	err := VerifyEIP1559Header(cfg, parent, header)
	if err == nil {
		t.Fatal("a Parlia block with a nil baseFee parent must be rejected")
	}
	if !strings.Contains(err.Error(), "parent header is missing baseFee") {
		t.Fatalf("wrong rejection reason: %v", err)
	}
}

// TestBSCConfigUnaffectedByPGPLegacyExemption pins that chains without a Legacy
// config (BSC mainnet, chapel, rialto) keep the strict rule: IsPGPLegacy is
// false when Legacy is nil, so the parent baseFee check is unchanged there.
func TestBSCConfigUnaffectedByPGPLegacyExemption(t *testing.T) {
	cfg := config() // Parlia present, LondonBlock 5, Legacy nil
	parent := &types.Header{Number: big.NewInt(9), GasLimit: 30000000}
	header := &types.Header{
		Number:   big.NewInt(10),
		GasLimit: 30000000,
		BaseFee:  new(big.Int),
	}
	err := VerifyEIP1559Header(cfg, parent, header)
	if err == nil {
		t.Fatal("a BSC chain must still reject a nil parent baseFee")
	}
	if !strings.Contains(err.Error(), "parent header is missing baseFee") {
		t.Fatalf("wrong rejection reason: %v", err)
	}
}
