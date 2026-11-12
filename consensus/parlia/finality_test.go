// Copyright 2017 The bnb-chain Authors
// This file is part of the bnb-chain library.
//
// The bnb-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The bnb-chain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the bnb-chain library. If not, see <http://www.gnu.org/licenses/>.

package parlia

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

type finalizedHeaderChain struct {
	cfg      *params.ChainConfig
	current  *types.Header
	byHash   map[common.Hash]*types.Header
	byNumber map[uint64]*types.Header
}

func (c *finalizedHeaderChain) Config() *params.ChainConfig {
	return c.cfg
}

func (c *finalizedHeaderChain) CurrentHeader() *types.Header {
	return c.current
}

func (c *finalizedHeaderChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	header := c.byHash[hash]
	if header != nil && header.Number.Uint64() == number {
		return header
	}
	return nil
}

func (c *finalizedHeaderChain) GetHeaderByNumber(number uint64) *types.Header {
	return c.byNumber[number]
}

func (c *finalizedHeaderChain) GetHeaderByHash(hash common.Hash) *types.Header {
	return c.byHash[hash]
}

func (c *finalizedHeaderChain) GenesisHeader() *types.Header {
	return c.byNumber[0]
}

func (c *finalizedHeaderChain) GetTd(hash common.Hash, number uint64) *big.Int {
	return big.NewInt(0)
}

func (c *finalizedHeaderChain) GetHighestVerifiedHeader() *types.Header {
	return c.current
}

func (c *finalizedHeaderChain) GetVerifiedBlockByHash(hash common.Hash) *types.Header {
	return c.byHash[hash]
}

type fixedVotePool struct {
	n int
}

func (v *fixedVotePool) FetchVotesByBlockHash(targetBlockHash common.Hash, sourceBlockNum uint64) []*types.VoteEnvelope {
	votes := make([]*types.VoteEnvelope, v.n)
	for i := range votes {
		votes[i] = &types.VoteEnvelope{}
	}
	return votes
}

func makeValidatorSet(size int) map[common.Address]*ValidatorInfo {
	validators := make(map[common.Address]*ValidatorInfo, size)
	for i := 0; i < size; i++ {
		addr := common.BigToAddress(big.NewInt(int64(i + 1)))
		validators[addr] = &ValidatorInfo{}
	}
	return validators
}

func TestGetFinalizedHeaderUsesParentSnapshotForFastFinalityQuorum(t *testing.T) {
	cfg := &params.ChainConfig{
		ChainID:    big.NewInt(56),
		PlatoBlock: big.NewInt(0),
		Parlia:     &params.ParliaConfig{},
	}

	genesis := &types.Header{
		Number: big.NewInt(0),
	}
	targetParent := &types.Header{
		Number:     big.NewInt(10),
		ParentHash: common.Hash{},
	}
	finalized := &types.Header{
		Number:     big.NewInt(9),
		ParentHash: genesis.Hash(),
	}
	targetParent.ParentHash = finalized.Hash()
	target := &types.Header{
		Number:     big.NewInt(11),
		ParentHash: targetParent.Hash(),
	}

	chain := &finalizedHeaderChain{
		cfg:     cfg,
		current: target,
		byHash: map[common.Hash]*types.Header{
			genesis.Hash():      genesis,
			targetParent.Hash(): targetParent,
			finalized.Hash():    finalized,
			target.Hash():       target,
		},
		byNumber: map[uint64]*types.Header{
			0:  genesis,
			9:  finalized,
			10: targetParent,
			11: target,
		},
	}

	parentSnap := &Snapshot{
		config:           cfg.Parlia,
		Number:           10,
		Hash:             targetParent.Hash(),
		Validators:       makeValidatorSet(4),
		Attestation:      &types.VoteData{SourceNumber: 9, SourceHash: finalized.Hash(), TargetNumber: 9, TargetHash: finalized.Hash()},
		Recents:          make(map[uint64]common.Address),
		RecentForkHashes: make(map[uint64]string),
	}
	headerSnap := &Snapshot{
		config:           cfg.Parlia,
		Number:           11,
		Hash:             target.Hash(),
		Validators:       makeValidatorSet(2),
		Attestation:      &types.VoteData{SourceNumber: 9, SourceHash: finalized.Hash(), TargetNumber: 10, TargetHash: targetParent.Hash()},
		Recents:          make(map[uint64]common.Address),
		RecentForkHashes: make(map[uint64]string),
	}

	engine := New(cfg, nil, nil, genesis.Hash())
	engine.recentSnaps.Add(targetParent.Hash(), parentSnap)
	engine.recentSnaps.Add(target.Hash(), headerSnap)
	engine.VotePool = &fixedVotePool{n: 2}

	finalizedHeader := engine.GetFinalizedHeader(chain, target)
	if finalizedHeader == nil {
		t.Fatal("expected finalized header, got nil")
	}
	if finalizedHeader.Number.Uint64() != finalized.Number.Uint64() {
		t.Fatalf("expected fallback finalized block %d, got %d", finalized.Number.Uint64(), finalizedHeader.Number.Uint64())
	}
}

var _ consensus.ChainHeaderReader = (*finalizedHeaderChain)(nil)

// TestJustifiedNumberOnLegacyBlocksSkipsTheSnapshot pins the guard that keeps a
// mainnet sync readable.
//
// Before it existed, every header insertion on a chain that had not forked yet
// asked Parlia for the justified block, which walked a snapshot over PBFT
// headers. Those carry a producer seal instead of the 32-byte vanity plus
// 65-byte secp256k1 seal Parlia expects, so the walk always failed -- with
// "invalid signature recovery id" on an ordinary block and "invalid validators
// bytes" on an epoch one -- and logged an ERROR per batch for the whole 800k
// block legacy history. The callers already treat an error as "nothing is
// justified", so the answer is the same either way; only the wasted walk and the
// noise are gone.
func TestJustifiedNumberOnLegacyBlocksSkipsTheSnapshot(t *testing.T) {
	genesis := &types.Header{Number: big.NewInt(0)}
	head := &types.Header{Number: big.NewInt(82798), Extra: make([]byte, 32+65)}
	chain := &finalizedHeaderChain{
		cfg:      params.PGPChainConfig, // ForkHeight is still the never-fork sentinel
		current:  head,
		byHash:   map[common.Hash]*types.Header{genesis.Hash(): genesis, head.Hash(): head},
		byNumber: map[uint64]*types.Header{0: genesis, 82798: head},
	}
	engine := New(params.PGPChainConfig, nil, nil, genesis.Hash())

	number, hash, err := engine.GetJustifiedNumberAndHash(chain, []*types.Header{head})
	if err != nil {
		t.Fatalf("a legacy head must not report an error: %v", err)
	}
	if number != 0 {
		t.Fatalf("justified number = %d, want 0", number)
	}
	if hash != genesis.Hash() {
		t.Fatalf("justified hash = %s, want the genesis hash %s", hash, genesis.Hash())
	}

	// The guard must stop at the fork: once the height is forked the snapshot is
	// consulted for real, and with a stub that cannot serve it that is an error.
	forked := *params.PGPChainConfig
	legacy := *params.PGPChainConfig.Legacy
	legacy.ForkHeight = 40
	// A configured fork height demands a validator set, otherwise New refuses to
	// build the engine at all (the same check that protects mainnet).
	legacy.ParliaGenesisValidators = []string{"0x0000000000000000000000000000000000000001"}
	legacy.ParliaGenesisVotingPowers = []uint64{100}
	legacy.ParliaGenesisCabinets = 1
	forked.Legacy = &legacy
	forkedChain := &finalizedHeaderChain{
		cfg:      &forked,
		current:  head,
		byHash:   map[common.Hash]*types.Header{genesis.Hash(): genesis, head.Hash(): head},
		byNumber: map[uint64]*types.Header{0: genesis, 82798: head},
	}
	forkedEngine := New(&forked, nil, nil, genesis.Hash())
	if _, _, err := forkedEngine.GetJustifiedNumberAndHash(forkedChain, []*types.Header{head}); err == nil {
		t.Fatal("a forked height must still consult the snapshot, so this stub cannot succeed")
	}
}
