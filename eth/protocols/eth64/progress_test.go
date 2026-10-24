// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

// newProgressTestSyncer builds a syncer over a fake chain at the given height
// without starting the sync loop, so a test can drive target/start by hand. The
// chain is returned too, so a test can move the head the way InsertChain would.
func newProgressTestSyncer(t *testing.T, head uint64) (*chainSyncer, *fakeChain) {
	t.Helper()

	chain := &fakeChain{head: &types.Header{Number: new(big.Int).SetUint64(head)}}
	h := newHandler(fakeBackend{chain: chain}, 860621)
	// newHandler leaves syncer nil (only start() sets it, and start() also
	// launches the loop). Progress treats a nil syncer as "not running yet", so
	// it has to be set explicitly for these tests to exercise the bookkeeping.
	s := newChainSyncer(h)
	h.syncer = s

	previous := currentHandler.Swap(h)
	t.Cleanup(func() { currentHandler.Store(previous) })
	return s, chain
}

// TestProgressWithoutAHandler covers the BSC/Chapel/Rialto case: no eth/64
// protocol set exists, and the caller must be told so rather than handed zeros
// that look like a finished sync at height 0.
func TestProgressWithoutAHandler(t *testing.T) {
	previous := currentHandler.Swap(nil)
	t.Cleanup(func() { currentHandler.Store(previous) })

	if p, ok := Progress(); ok {
		t.Fatalf("Progress reported a sync with no eth/64 protocol set: %+v", p)
	}
}

// TestProgressReportsTheGap is the property eth_syncing depends on: while the
// chain is behind the peer's announced head, Highest must exceed Current, or
// SyncProgress.Done answers "finished" and eth_syncing returns false mid-replay.
func TestProgressReportsTheGap(t *testing.T) {
	s, _ := newProgressTestSyncer(t, 1_147_004)

	s.target.Store(2_197_000)
	s.start.Store(69_474)

	p, ok := Progress()
	if !ok {
		t.Fatal("Progress reported no eth/64 sync while one is running")
	}
	want := SyncProgress{Current: 1_147_004, Highest: 2_197_000, Start: 69_474}
	if p != want {
		t.Fatalf("Progress = %+v, want %+v", p, want)
	}
	if p.Highest <= p.Current {
		t.Fatal("Highest did not exceed Current, so the caller's Done() would report the sync finished")
	}
}

// TestProgressNeverReportsBackwards pins the floor: a target left behind by a
// peer that turned out to be shorter, or by a reorg, must not make Highest drop
// below the head. Reporting a negative gap is worse than reporting none - a
// caller computing progress from it would see a negative percentage.
func TestProgressNeverReportsBackwards(t *testing.T) {
	s, _ := newProgressTestSyncer(t, 1_147_004)

	s.target.Store(500)
	s.start.Store(400)

	p, _ := Progress()
	if p.Highest != 1_147_004 {
		t.Fatalf("Highest = %d, want the head 1147004 when the target is stale", p.Highest)
	}
	if p.Start != 400 {
		t.Fatalf("Start = %d, want 400: a stale target is not a reason to forget the catch-up", p.Start)
	}
}

// TestProgressForgetsAStartAboveTheHead covers the reorg case: a start recorded
// before the chain reverted now sits above the head, and a caller computing
// (current-start)/(highest-start) from it would divide by a negative range.
func TestProgressForgetsAStartAboveTheHead(t *testing.T) {
	s, _ := newProgressTestSyncer(t, 1_000)

	s.target.Store(2_000)
	s.start.Store(5_000)

	if p, _ := Progress(); p.Start != 0 {
		t.Fatalf("Start = %d, want 0 when the recorded start is above the head", p.Start)
	}
}

// TestCatchUpFinishedStopsReportingAGap is what turns eth_syncing off without
// anyone resetting a flag: once the syncer decides it has caught up, Highest
// collapses onto the head and the caller's Done() comparison sees no gap.
func TestCatchUpFinishedStopsReportingAGap(t *testing.T) {
	s, chain := newProgressTestSyncer(t, 1_147_004)

	s.target.Store(2_197_000)
	s.start.Store(69_474)
	if p, _ := Progress(); p.Highest <= p.Current {
		t.Fatal("precondition: the syncer should report a gap before catching up")
	}

	// InsertChain has written the peer's whole chain by the time the syncer
	// decides it caught up, so the head and the last imported block are the same
	// height. Modelling that is the point: catchUpFinished is given the height it
	// reached, and Progress floors the target at the head.
	chain.head = &types.Header{Number: new(big.Int).SetUint64(2_197_000)}
	s.catchUpFinished(2_197_000)

	p, _ := Progress()
	if p.Highest != 2_197_000 || p.Current != 2_197_000 {
		t.Fatalf("Progress = %+v, want Highest == Current == 2197000 after catching up", p)
	}
	if p.Highest > p.Current {
		t.Fatal("a finished catch-up still reports a gap")
	}
	if p.Start != 0 {
		t.Fatalf("Start = %d, want 0 after catching up", p.Start)
	}
}
