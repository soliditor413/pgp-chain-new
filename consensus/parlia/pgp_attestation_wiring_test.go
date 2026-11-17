// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package parlia

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// noVotes is enough of a vote pool to get past the precondition check. Nothing
// here ever asks it for votes: the block under test is stopped by the floor, or
// stops on the missing parent.
type noVotes struct{}

func (noVotes) FetchVotesByBlockHash(common.Hash, uint64) []*types.VoteEnvelope { return nil }

// countingChain answers GetHeaderByHash with nil and counts the calls. The
// embedded interface is deliberately left nil: any other method the assembly
// reaches for panics, which is exactly the "it should never have walked that
// far" signal these tests are after.
type countingChain struct {
	consensus.ChainHeaderReader
	getHeaderCalls int
}

func (c *countingChain) GetHeaderByHash(common.Hash) *types.Header {
	c.getHeaderCalls++
	return nil
}

func attestationConfig(legacy *params.LegacyConfig) *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:    big.NewInt(860621),
		LubanBlock: big.NewInt(40),
		Parlia:     &params.ParliaConfig{},
		Legacy:     legacy,
	}
}

// TestAssembleVoteAttestationStopsAtTheForkBlock is the wiring half of
// TestMinAttestationBlockNumber: the floor has to actually short-circuit before
// assembly touches the chain.
//
// At the first Parlia block the parent is a legacy header, so the vote search
// would ask snapshot() for ForkHeight-2 -- one below the exact-match anchor at
// ForkHeight-1 -- and walk into the legacy genesis, where parseValidators finds
// no validator records. Reporting that as an assembly error on the fork block is
// what this guard removes.
func TestAssembleVoteAttestationStopsAtTheForkBlock(t *testing.T) {
	const fork = uint64(100)
	p := &Parlia{
		chainConfig: attestationConfig(&params.LegacyConfig{ForkHeight: fork}),
		VotePool:    noVotes{},
	}

	tests := []struct {
		name      string
		number    uint64
		wantCalls int
		wantErr   string
	}{
		{"last legacy block", fork - 1, 0, ""},
		{"first parlia block", fork, 0, ""},
		{"one past the fork", fork + 1, 1, "parent not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := &countingChain{}
			header := &types.Header{Number: new(big.Int).SetUint64(tc.number), ParentHash: common.HexToHash("0x01")}

			err := p.assembleVoteAttestation(chain, header)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("block %d should be skipped silently, got %v", tc.number, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("block %d should proceed and fail with %q, got %v", tc.number, tc.wantErr, err)
			}
			if chain.getHeaderCalls != tc.wantCalls {
				t.Fatalf("block %d: chain touched %d times, want %d (0 means the floor short-circuited)",
					tc.number, chain.getHeaderCalls, tc.wantCalls)
			}
		})
	}
}

// TestAssembleVoteAttestationWithoutLegacySection is the regression for the
// panic the first version of this guard caused: chains without a legacy era
// (BSC mainnet, chapel) have Legacy == nil and Luban already active, so reading
// Legacy.ForkHeight before checking for nil killed block assembly there.
func TestAssembleVoteAttestationWithoutLegacySection(t *testing.T) {
	// Luban 已经在创世生效：这才是 BSC 主网/chapel 的形态（没有 legacy 段，
	// 所以 Luban 的块高早就过了）。设成 40 会让 block 3 在第一个前置条件
	// （!IsLuban）就返回，测不到 floor。
	cfg := attestationConfig(nil)
	cfg.LubanBlock = big.NewInt(0)
	p := &Parlia{chainConfig: cfg, VotePool: noVotes{}}

	// Floor 3 inherited from BSC: blocks 1 and 2 have nothing to vote on and
	// snapshot(number-1) would underflow at 1.
	for _, n := range []uint64{1, 2} {
		chain := &countingChain{}
		header := &types.Header{Number: new(big.Int).SetUint64(n), ParentHash: common.HexToHash("0x01")}
		if err := p.assembleVoteAttestation(chain, header); err != nil {
			t.Fatalf("block %d should be skipped, got %v", n, err)
		}
		if chain.getHeaderCalls != 0 {
			t.Fatalf("block %d: chain touched %d times, want 0", n, chain.getHeaderCalls)
		}
	}

	// Block 3 is the first one that may carry an attestation; it has to get past
	// the Legacy lookup without panicking.
	chain := &countingChain{}
	header := &types.Header{Number: big.NewInt(3), ParentHash: common.HexToHash("0x01")}
	err := p.assembleVoteAttestation(chain, header)
	if err == nil || !strings.Contains(err.Error(), "parent not found") {
		t.Fatalf("block 3 should proceed and stop on the missing parent, got %v", err)
	}
	if chain.getHeaderCalls != 1 {
		t.Fatalf("block 3: chain touched %d times, want 1", chain.getHeaderCalls)
	}
}
