// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package parlia

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// voteChain answers only the two lookups the guarded paths make before they are
// supposed to stop, and counts the snapshot walk separately: a legacy block must
// not start one.
type voteChain struct {
	consensus.ChainHeaderReader
	verified       *types.Header
	getHeaderCalls int
}

func (c *voteChain) GetVerifiedBlockByHash(common.Hash) *types.Header { return c.verified }

func (c *voteChain) GetHeaderByHash(common.Hash) *types.Header {
	c.getHeaderCalls++
	return nil
}

// snapshot() reaches for GetHeader (hash+number) when it walks backwards, so a
// stub that only overrides GetHeaderByHash panics inside the embedded nil
// interface instead of telling us whether the walk started.
func (c *voteChain) GetHeader(common.Hash, uint64) *types.Header {
	c.getHeaderCalls++
	return nil
}

func (c *voteChain) GetHeaderByNumber(uint64) *types.Header { return nil }

func (c *voteChain) Config() *params.ChainConfig { return nil }

func guardParlia(legacy *params.LegacyConfig) *Parlia {
	return &Parlia{
		chainConfig: attestationConfig(legacy),
		config:      &params.ParliaConfig{},
		// snapshot() starts with these; nil ones panic before the test could
		// observe anything.
		recentSnaps: lru.NewCache[common.Hash, *Snapshot](16),
		signatures:  lru.NewCache[common.Hash, common.Address](16),
		db:          rawdb.NewMemoryDatabase(),
	}
}

// TestIsActiveValidatorAtSkipsLegacyBlocks covers the vote manager's poll. It
// asks this question with whatever the head currently is, which during a legacy
// era is a block below the anchor — snapshot(head-1) then misses the anchor
// (matched by exact number) and walks into the legacy genesis, where
// parseValidators rejects the PBFT extra. The poll used to log an error every
// few seconds until the fork.
func TestIsActiveValidatorAtSkipsLegacyBlocks(t *testing.T) {
	const fork = uint64(100)
	p := guardParlia(&params.LegacyConfig{ForkHeight: fork})

	t.Run("legacy block", func(t *testing.T) {
		chain := &voteChain{}
		header := &types.Header{Number: new(big.Int).SetUint64(fork - 50), ParentHash: common.HexToHash("0x01")}
		if p.IsActiveValidatorAt(chain, header, nil) {
			t.Fatal("a pre-fork block must not report an active Parlia validator")
		}
		if chain.getHeaderCalls != 0 {
			t.Fatalf("snapshot chain walked %d times, want 0", chain.getHeaderCalls)
		}
	})

	t.Run("fork block", func(t *testing.T) {
		// The guard must not swallow post-fork blocks: reaching the snapshot
		// lookup is what separates "no Parlia set yet" from "no set found".
		chain := &voteChain{}
		header := &types.Header{Number: new(big.Int).SetUint64(fork + 1), ParentHash: common.HexToHash("0x02")}
		p.IsActiveValidatorAt(chain, header, nil)
		if chain.getHeaderCalls == 0 {
			t.Fatal("a post-fork block must still be looked up")
		}
	})
}

// TestVerifyVoteRejectsLegacyTarget: a vote can only target a Parlia block, and
// rejecting a legacy target also keeps VerifyVote from asking for
// targetNumber-1, one below the anchor.
func TestVerifyVoteRejectsLegacyTarget(t *testing.T) {
	const fork = uint64(100)
	p := guardParlia(&params.LegacyConfig{ForkHeight: fork})

	header := &types.Header{Number: new(big.Int).SetUint64(fork - 10), ParentHash: common.HexToHash("0x03")}
	chain := &voteChain{verified: header}
	vote := &types.VoteEnvelope{Data: &types.VoteData{
		SourceNumber: fork - 20,
		SourceHash:   common.HexToHash("0x04"),
		TargetNumber: fork - 10,
		TargetHash:   header.Hash(),
	}}

	err := p.VerifyVote(chain, vote)
	if err == nil || !strings.Contains(err.Error(), "pre-fork") {
		t.Fatalf("want a pre-fork rejection, got %v", err)
	}
	if chain.getHeaderCalls != 0 {
		t.Fatalf("snapshot chain walked %d times, want 0", chain.getHeaderCalls)
	}
}
