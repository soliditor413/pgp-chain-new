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

package pbft

// viewStartTime is the value that answers "which view are we in", so the whole
// set has to agree on it. That makes the set of inputs it may be derived from
// tiny: only values every node reads identically, which on a running chain means
// a block header's timestamp and nothing else. This node's own clock is not one
// of them, and the failure is not subtle -- two nodes anchoring on their own
// clocks sit on grids separated by the difference between the two instants.
//
// The bug this file pins was a one-line coupling. Prepare has always stamped
// header.Time with the local clock when the parent was more than one period old,
// which is correct and local: the timestamp travels with the block this node
// produces. The same branch also moved the view anchor, which is neither. On a
// chain that has stopped producing, every round takes that branch, so every node
// re-anchored on its own clock once per round and the set never had a shared grid
// at all. Measured on six producers stuck at height 4: viewStartTime split into
// 14:32:27Z x3 and 14:32:29Z x3, and the split existed from each node's very
// first round, not after some drift.

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// TestPrepareAnchorsTheViewOnParentTimeNotTheLocalClock drives Prepare on two
// nodes two seconds apart, against the same parent, and requires the same
// viewStartTime from both -- the parent's timestamp.
//
// Two seconds is not arbitrary: it is the gap the six-producer rig showed between
// its two groups. The two nodes are separate Engine instances with real clocks,
// so the sleep is what reproduces that gap; the assertion is equality of the
// anchor, and the anchor must be the parent's timestamp rather than either
// node's clock.
func TestPrepareAnchorsTheViewOnParentTimeNotTheLocalClock(t *testing.T) {
	accounts, producerKeys := newProducerSet(t, 4)

	// The parent is 100 seconds old, so header.Time = parent.Time + period is
	// already in the past and Prepare takes the late branch. That is every round
	// on a chain that has stopped producing, which is the state the rig was in.
	parentTime := uint64(time.Now().Add(-100 * time.Second).Unix())
	chain := &sealTestChain{genesis: &types.Header{
		Number:     big.NewInt(0),
		Time:       parentTime,
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}}

	// One fresh engine per call: separate dispatcher, separate view, separate
	// clock. Sharing an engine would make the second Prepare a no-op on the first
	// one's anchor, which is a different question.
	newEngine := func(t *testing.T) *Engine {
		t.Helper()
		engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, &Config{Enable: true})
		engine.attachAccount(accounts[0])
		engine.MarkRecovered()
		engine.AttachChain(chain)
		t.Cleanup(func() { _ = engine.Close() })
		return engine
	}

	// The duty gate sits after startView, so a node whose turn it is not still
	// gets its anchor set. The error is therefore irrelevant here and is dropped
	// on purpose; what this test reads is the view, not the verdict.
	//
	// It reads the ANCHOR and the derived offset rather than viewStartTime, which
	// is what this test read before the offset became a derived value. viewStartTime
	// is now the start of the window this node currently sits in -- anchor +
	// offset*signTolerance -- so with a 100s-old parent it is legitimately about
	// now rather than the parent, and comparing it across nodes would be comparing
	// two numbers that both encode "I am 16 views in". The shared value is the
	// anchor, so that is what has to be equal.
	type viewState struct {
		anchor uint64
		offset uint32
	}
	prepare := func(t *testing.T, e *Engine) viewState {
		t.Helper()
		header := newPrepareTestHeader()
		header.ParentHash = chain.genesis.Hash()
		_ = e.Prepare(chain, header)
		v := e.dispatcher.GetConsensusView()
		return viewState{anchor: v.GetAnchor(), offset: v.GetViewOffset()}
	}

	first := prepare(t, newEngine(t))

	time.Sleep(2 * time.Second)
	second := prepare(t, newEngine(t))

	want := parentTime
	if first.anchor != want {
		t.Fatalf("the first node anchored the view at %d, want the parent timestamp %d; "+
			"a late round must move header.Time without moving the anchor", first.anchor, want)
	}
	if second.anchor != want {
		t.Fatalf("the second node anchored the view at %d, want %d", second.anchor, want)
	}
	if first.anchor != second.anchor {
		t.Fatalf("two nodes sharing one parent anchored their views at %d and %d; "+
			"the local clock reached the anchor", first.anchor, second.anchor)
	}
	// And the anchors being equal is only worth anything if the derived view agrees
	// too. It can differ by at most one: the two Prepares are two real seconds
	// apart, and a derived offset is a function of the clock, so a sample that
	// straddles a tolerance boundary legitimately reports one view more. That
	// bounded window is the transient QA measured at 786ms between two tick phases;
	// what must never happen is a whole-view gap that persists, which is what the
	// counter produced and what the anchors above rule out.
	if diff := int(second.offset) - int(first.offset); diff < 0 || diff > 1 {
		t.Fatalf("one anchor gave views %d apart (%d and %d); at most the one-window skew is allowed",
			diff, first.offset, second.offset)
	}
	t.Logf("both nodes anchored at %d and both derived viewOffset %d; their local clocks were %v apart",
		first.anchor, first.offset, 2*time.Second)
}

// TestPrepareStillAdvancesTheBlockTimeWhenLate is the other half, and the reason
// the branch cannot simply be deleted: header.Time must still move to now, or the
// block is born already expired.
//
// Without this, "delete the late branch" would pass the test above -- the anchor
// would be parent.Time either way -- while quietly reintroducing stale blocks.
func TestPrepareStillAdvancesTheBlockTimeWhenLate(t *testing.T) {
	accounts, producerKeys := newProducerSet(t, 4)

	parentTime := uint64(time.Now().Add(-100 * time.Second).Unix())
	chain := &sealTestChain{genesis: &types.Header{
		Number:     big.NewInt(0),
		Time:       parentTime,
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}}

	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.MarkRecovered()
	engine.AttachChain(chain)
	defer engine.Close()

	before := uint64(time.Now().Unix())
	header := newPrepareTestHeader()
	header.ParentHash = chain.genesis.Hash()
	_ = engine.Prepare(chain, header)

	if header.Time < before {
		t.Fatalf("Prepare left header.Time at %d with the parent at %d: the block is born "+
			"already expired", header.Time, parentTime)
	}
	if got := engine.dispatcher.GetConsensusView().GetAnchor(); got != parentTime {
		t.Fatalf("the anchor followed header.Time to %d; it must stay on the parent %d",
			got, parentTime)
	}
	t.Logf("header.Time advanced to %d while the anchor stayed on parent %d", header.Time, parentTime)
}
