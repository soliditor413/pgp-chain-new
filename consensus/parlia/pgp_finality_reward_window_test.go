// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The pgp-chain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the pgp-chain library. If not, see <http://www.gnu.org/licenses/>.

package parlia

// The finality-reward walk crosses the fork on the first epoch block.
//
// distributeFinalityReward() runs only when number % finalityRewardInterval == 0
// (200), and then walks that many heights back. On a chain that switched to
// Parlia at ForkHeight, the very first run at block 200 therefore reaches height
// 1. For each height it asks p.epochLength(), which asks for a snapshot of
// height-1. Below the snapshot anchor at ForkHeight-1 that lookup falls through
// to the genesis header, whose extra field is in the legacy consensus layout, so
// parseValidators finds no address records and returns "invalid validators
// bytes".
//
// The consequence is not a wrong reward: block 200 simply cannot be assembled,
// every validator retries every 3 seconds, and the chain stops at 199 with the
// only clue being "commitWork failed to assemble a block". The fix stops the walk
// at the fork.
//
// These tests pin the arithmetic that makes it happen, since it is invisible for
// any ForkHeight >= finalityRewardInterval and only fires on epoch blocks.

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// finalityWalkHeights reproduces the loop bounds of distributeFinalityReward
// without the guard: heights currentHeight-1 down to 1 inclusive, as long as
// height+finalityRewardInterval >= currentHeight.
func finalityWalkHeights(currentHeight uint64) []uint64 {
	var heights []uint64
	for height := currentHeight - 1; height+finalityRewardInterval >= currentHeight && height >= 1; height-- {
		heights = append(heights, height)
	}
	return heights
}

func TestFinalityRewardIntervalIsTwoHundred(t *testing.T) {
	// The whole test below depends on this number: it is what makes the walk
	// reach the fork at ForkHeight 20 but never at ForkHeight 200+.
	if finalityRewardInterval != 200 {
		t.Fatalf("finalityRewardInterval = %d, want 200", finalityRewardInterval)
	}
}

// TestFirstEpochBlockWalkCrossesTheFork is the regression condition: with the
// default epoch length (200) and any ForkHeight below it, the unguarded walk
// from the first epoch block reaches legacy heights.
func TestFirstEpochBlockWalkCrossesTheFork(t *testing.T) {
	cfg := &params.ChainConfig{Legacy: &params.LegacyConfig{ForkHeight: 20}}
	heights := finalityWalkHeights(defaultEpochLength) // block 200

	lowest := heights[len(heights)-1]
	if lowest != 1 {
		t.Fatalf("walk from block %d ends at %d, want 1", defaultEpochLength, lowest)
	}
	if !cfg.IsPGPLegacy(new(big.Int).SetUint64(lowest)) {
		t.Fatal("the walk must reach a legacy height; without that this fix is dead code")
	}

	// Where the guard breaks: heights are visited in descending order, so the
	// first legacy height encountered is the largest one, ForkHeight-1. That
	// leaves the window [ForkHeight, epoch-1].
	var breakAt uint64
	for _, h := range heights {
		if cfg.IsPGPLegacy(new(big.Int).SetUint64(h)) {
			breakAt = h
			break
		}
	}
	if breakAt != 19 {
		t.Fatalf("guard breaks at %d, want 19 (ForkHeight-1)", breakAt)
	}
	kept := 0
	for _, h := range heights {
		if h >= cfg.Legacy.ForkHeight {
			kept++
		}
	}
	if kept != int(defaultEpochLength-uint64(cfg.Legacy.ForkHeight)) {
		t.Fatalf("guarded window keeps %d heights, want %d", kept, defaultEpochLength-cfg.Legacy.ForkHeight)
	}
}

// TestLaterEpochBlocksStopAboveTheFork records where the problem goes away on
// its own: once the chain has a full window of Parlia blocks behind it, the walk
// no longer reaches a legacy height, so the guard is inert. This is why only the
// FIRST epoch block after a low fork is affected, and why a ForkHeight above the
// epoch length would have hidden the bug entirely.
func TestLaterEpochBlocksStopAboveTheFork(t *testing.T) {
	cfg := &params.ChainConfig{Legacy: &params.LegacyConfig{ForkHeight: 20}}
	heights := finalityWalkHeights(2 * defaultEpochLength) // block 400

	lowest := heights[len(heights)-1]
	if lowest != defaultEpochLength {
		t.Fatalf("walk from block %d ends at %d, want %d", 2*defaultEpochLength, lowest, defaultEpochLength)
	}
	for _, h := range heights {
		if cfg.IsPGPLegacy(new(big.Int).SetUint64(h)) {
			t.Fatalf("height %d should be Parlia's; the walk must stay above the fork here", h)
		}
	}
}
