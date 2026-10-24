// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

import (
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
)

// TestMergeEth64ProgressBreaksTheFalseDone is the regression this whole merge
// exists for.
//
// The built-in downloader reports HighestBlock 0 on a pgp network, because the
// peers speak eth/64 and it only ever registers peers for eth/68 and eth/70. With
// CurrentBlock at the real head and HighestBlock at 0, SyncProgress.Done compares
// 0 against the head, answers true, and eth_syncing returns false while millions
// of blocks are still being imported (internal/ethapi/api.go:166-172). A caller
// that uses eth_syncing to decide "sync finished" is misled for the whole replay.
func TestMergeEth64ProgressBreaksTheFalseDone(t *testing.T) {
	before := ethereum.SyncProgress{CurrentBlock: 1_147_004, HighestBlock: 0}
	if !before.Done() {
		t.Fatal("precondition: the downloader's own report must claim the sync is finished")
	}

	after := mergeEth64Progress(before, eth64.SyncProgress{
		Current: 1_147_004,
		Highest: 2_197_000,
		Start:   69_474,
	}, true)

	if after.Done() {
		t.Fatalf("Done() is still true after the merge: %+v", after)
	}
	if after.HighestBlock != 2_197_000 {
		t.Fatalf("HighestBlock = %d, want the eth/64 target", after.HighestBlock)
	}
	if after.StartingBlock != 69_474 {
		t.Fatalf("StartingBlock = %d, want the height the catch-up began at", after.StartingBlock)
	}
	if after.CurrentBlock != 1_147_004 {
		t.Fatalf("CurrentBlock = %d, the merge must not move the head", after.CurrentBlock)
	}
}

// TestMergeEth64ProgressStopsWhenCaughtUp is the other half: the report has to
// turn itself off. A merge that only ever raised HighestBlock would leave
// eth_syncing true forever, which is a worse lie than always false.
func TestMergeEth64ProgressStopsWhenCaughtUp(t *testing.T) {
	after := mergeEth64Progress(
		ethereum.SyncProgress{CurrentBlock: 2_197_000, HighestBlock: 0},
		eth64.SyncProgress{Current: 2_197_000, Highest: 2_197_000},
		true,
	)
	if !after.Done() {
		t.Fatalf("Done() = false after catching up: %+v", after)
	}
	if after.HighestBlock != 2_197_000 {
		t.Fatalf("HighestBlock = %d, want the head once caught up", after.HighestBlock)
	}
}

// TestMergeEth64ProgressLeavesOtherChainsAlone pins the scope: neither a BSC node
// (no eth/64 protocol set at all) nor a chain whose peers are ahead over the
// regular protocols may have its report rewritten.
func TestMergeEth64ProgressLeavesOtherChainsAlone(t *testing.T) {
	bsc := ethereum.SyncProgress{CurrentBlock: 40_000_000, HighestBlock: 0}
	if got := mergeEth64Progress(bsc, eth64.SyncProgress{}, false); got != bsc {
		t.Fatalf("a node without an eth/64 protocol set was modified: %+v", got)
	}

	// The downloader is further ahead than eth/64 claims: whichever peer set is
	// ahead is the one worth reporting, and lowering it would hide real work.
	ahead := ethereum.SyncProgress{StartingBlock: 1_000, CurrentBlock: 2_000_000, HighestBlock: 3_000_000}
	if got := mergeEth64Progress(ahead, eth64.SyncProgress{Current: 1_147_004, Highest: 2_197_000}, true); got != ahead {
		t.Fatalf("a higher downloader target was lowered: %+v", got)
	}
}

// TestMergeEth64ProgressStartingBlockFallbacks covers the two ways Start can be
// unusable, so the reported triple stays ordered and a caller computing
// (current-start)/(highest-start) cannot divide by a negative range.
func TestMergeEth64ProgressStartingBlockFallbacks(t *testing.T) {
	// Start unknown: report the current head rather than 0, which would claim the
	// replay began at genesis.
	got := mergeEth64Progress(
		ethereum.SyncProgress{CurrentBlock: 1_147_004, HighestBlock: 0},
		eth64.SyncProgress{Current: 1_147_004, Highest: 2_197_000, Start: 0},
		true,
	)
	if got.StartingBlock != 1_147_004 {
		t.Fatalf("StartingBlock = %d, want the current head when Start is unknown", got.StartingBlock)
	}

	// A start the downloader already knows, and which is below the eth/64 head, is
	// the better answer and must survive the merge.
	got = mergeEth64Progress(
		ethereum.SyncProgress{StartingBlock: 500, CurrentBlock: 1_147_004, HighestBlock: 0},
		eth64.SyncProgress{Current: 1_147_004, Highest: 2_197_000, Start: 69_474},
		true,
	)
	if got.StartingBlock != 500 {
		t.Fatalf("StartingBlock = %d, want the downloader's 500 to survive", got.StartingBlock)
	}

	// A start above the eth/64 head is nonsense (it can only come from an older,
	// taller chain) and is replaced.
	got = mergeEth64Progress(
		ethereum.SyncProgress{StartingBlock: 9_000_000, CurrentBlock: 1_147_004, HighestBlock: 0},
		eth64.SyncProgress{Current: 1_147_004, Highest: 2_197_000, Start: 69_474},
		true,
	)
	if got.StartingBlock != 69_474 {
		t.Fatalf("StartingBlock = %d, want 69474 when the downloader's start is above the head", got.StartingBlock)
	}
}
