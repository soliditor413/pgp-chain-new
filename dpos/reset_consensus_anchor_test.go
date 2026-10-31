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

package dpos

// The view grid is a pure function of one number: the anchor a force reset hands
// to ChangeView. setChangViewTimeLocked truncates that anchor to a whole second
// and writes viewStartTime = anchor, viewChangeTime = anchor + signTolerance, and
// viewOffset is then a counter of how many advances this node has made since its
// own reset. Two nodes agree on "which view are we in at height h" only if they
// were reset with the same anchor. Hand them different anchors and every proposal
// is stamped with a view number the others do not recognise, which dispatcher
// ProcessProposal drops on the "have different view offset" branch.
//
// These tests pin that mechanism rather than the particular call sites, because
// the call sites are what went wrong once already: the port reset the view on
// every imported block but anchored it to uint64(now.Unix()) of the local clock,
// so six producers sat one and two whole views apart, three against three, and
// livelocked at height 6 with no path back. See Dispatcher.ResetConsensus.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
)

const (
	// anchorTolerance is the engine's signTolerance, 2 * blockPeriod (3s). See
	// consensus/pbft/consts.go.
	anchorTolerance = 6 * time.Second

	// A node's one-second view ticker fires at the phase of its own process start
	// and is never re-aligned afterwards (consensus/pbft/view.go changeViewLoop
	// uses time.NewTicker), so the phase is a per-node constant, not something the
	// node can be talked out of. These are QA's measurements from the live rig.
	phaseA = 890 * time.Millisecond
	phaseB = 104 * time.Millisecond
)

// steppingTimeSource is a MedianTimeSource a test can move by hand. The real one
// wraps the wall clock, which would make "two nodes whose handlers fired at
// different instants" untestable without sleeping through the difference.
type steppingTimeSource struct {
	mu  sync.Mutex
	now time.Time
}

var _ dtime.MedianTimeSource = (*steppingTimeSource)(nil)

func newSteppingTimeSource(now time.Time) *steppingTimeSource {
	return &steppingTimeSource{now: now}
}

func (s *steppingTimeSource) AdjustedTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *steppingTimeSource) AddTimeSample(string, time.Time) {}

func (s *steppingTimeSource) Offset() time.Duration { return 0 }

func (s *steppingTimeSource) set(now time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// anchorTestProducers is six producers, the size the incident happened at. Their
// identity does not matter here; the rotation only needs distinct, sorted keys.
func anchorTestProducers() [][]byte {
	producers := make([][]byte, 6)
	for i := range producers {
		key := make([]byte, 21)
		key[0] = 0x03
		key[20] = byte(i + 1)
		producers[i] = key
	}
	return producers
}

// viewSnapshot is the triple that has to agree between two nodes at one instant.
type viewSnapshot struct {
	offset uint32
	start  time.Time
	change time.Time
}

func (s viewSnapshot) String() string {
	return fmt.Sprintf("viewOffset=%d viewStartTime=%s viewChangeTime=%s",
		s.offset, s.start, s.change)
}

// anchorTestNode is one producer's view: a clock the test can move, a ticker
// phase it cannot, and the dispatcher that owns the ConsensusView.
type anchorTestNode struct {
	name  string
	phase time.Duration
	clock *steppingTimeSource
	d     *Dispatcher
}

func newAnchorTestNode(name string, phase time.Duration) *anchorTestNode {
	clock := newSteppingTimeSource(time.Unix(0, 0))
	// dposStartHeight is only the base of the duty rotation; 100 keeps
	// UpdateDutyIndex away from the working-height edge. No listener: the view
	// callback reaches the miner, which this test does not have.
	d := NewDispatcher(anchorTestProducers(),
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		anchorTolerance, nil, clock, nil, 100)
	return &anchorTestNode{name: name, phase: phase, clock: clock, d: d}
}

// resumeAndReset models the producer-side sequence that precedes a reset: the
// view has to be running for ResetConsensus to do anything (its guard is
// IsRunning), and ResetConsensus itself leaves the view Ready. prepare.go's
// startView calls SetRunning again on the next round, which is what this mirrors.
func (n *anchorTestNode) resumeAndReset(height uint64, anchor uint64) {
	n.d.GetConsensusView().SetRunning()
	n.d.ResetConsensus(height, anchor)
}

// tickAt is one tick of the view loop: it makes the view live and then asks it
// to advance.
//
// The SetRunning is not decoration. TryChangeView only advances a view that is
// Running, and a reset leaves it Ready; on a node it is the next Prepare calling
// startView (prepare.go) that brings it back. Note what this deliberately does
// NOT do: prepare.go's other branch, SetChangViewTime(headerTime), re-anchors the
// times to the parent header and would paper over exactly the divergence under
// test.
func (n *anchorTestNode) tickAt(now time.Time) {
	n.d.GetConsensusView().SetRunning()
	n.clock.set(now)
	n.d.OnChangeView()
}

// runViewTicks drives the node's one-second view ticker for every tick in
// [from, to), through Dispatcher.OnChangeView so the call takes the same path the
// real loop takes (view.go changeViewLoop -> OnChangeView -> TryChangeView).
//
// Ticks that land before viewChangeTime are no-ops inside TryChangeView, so
// calling all of them rather than only the effective one is what makes this
// faithful: the test never has to decide which tick "was the real one".
func (n *anchorTestNode) runViewTicks(from, to time.Time) {
	tick := time.Unix(from.Unix(), int64(n.phase))
	if tick.Before(from) {
		tick = tick.Add(time.Second)
	}
	for ; tick.Before(to); tick = tick.Add(time.Second) {
		n.tickAt(tick)
	}
}

func (n *anchorTestNode) snapshot() viewSnapshot {
	v := n.d.GetConsensusView()
	return viewSnapshot{offset: v.GetViewOffset(), start: v.GetViewStartTime(), change: v.GetChangeViewTime()}
}

// TestResetConsensusAnchorDecidesTheViewGrid is the incident in one function: the
// same Dispatcher.ResetConsensus call, driven by the same two nodes over the same
// clock, goes from a permanent two-view fork to one shared grid -- and the only
// thing that changes between the two halves is where the anchor argument came
// from.
func TestResetConsensusAnchorDecidesTheViewGrid(t *testing.T) {
	// A whole second, and the header.Time of the block the reset is driven by:
	// the one value every producer reads identically off the wire.
	headerTime := time.Unix(1_700_000_000, 0)

	// Two producers whose block-import handlers fired twelve seconds apart -- two
	// whole tolerances. This is the worst case and the one the rig hit: its two
	// groups reported viewOffset 9 and 7 at the same height.
	handlerA := headerTime.Add(400 * time.Millisecond)
	handlerB := handlerA.Add(-12 * time.Second)

	nodeA := newAnchorTestNode("A", phaseA)
	nodeB := newAnchorTestNode("B", phaseB)

	// ---- the bug -----------------------------------------------------------------
	// What the port did: anchor = uint64(now.Unix()) of THIS node's clock, read at
	// the instant THIS node's handler ran.
	nodeA.clock.set(handlerA)
	nodeA.resumeAndReset(6, uint64(handlerA.Unix()))
	nodeB.clock.set(handlerB)
	nodeB.resumeAndReset(6, uint64(handlerB.Unix()))

	// Five view windows on a shared clock. Same tick period, same producers, same
	// height, same wall clock: nothing differs but the anchor the grids were built
	// from, and the tick phases, which are not the bug.
	nodeA.runViewTicks(headerTime.Add(time.Second), headerTime.Add(31*time.Second))
	nodeB.runViewTicks(headerTime.Add(time.Second), headerTime.Add(31*time.Second))

	forkA, forkB := nodeA.snapshot(), nodeB.snapshot()

	// Twelve seconds is a whole number of views, so the two grids coincide: both
	// start their current view at the same second. Phase alignment cannot reveal
	// the problem and cannot fix it either, because the disagreement is not in the
	// times -- it is in the counter, which is a count of advances since that node's
	// OWN reset.
	if !forkA.start.Equal(forkB.start) {
		t.Fatalf("test premise broken: the two grids should coincide for a whole-view anchor gap, got %v and %v",
			forkA.start, forkB.start)
	}
	if diff := int(forkB.offset) - int(forkA.offset); diff != 2 {
		t.Fatalf("expected the local-clock anchors to leave the two nodes two views apart, got %v vs %v (difference %d)",
			forkA, forkB, diff)
	}
	t.Logf("fork reproduced: %s is %v, %s is %v", nodeA.name, forkA, nodeB.name, forkB)

	// And it does not heal. This is the half that makes it a livelock rather than
	// a hiccup: five more windows of pure timeout advancing, and the gap is
	// exactly where it was. If this ever starts passing, the test has stopped
	// modelling the failure and the failure is still there.
	nodeA.runViewTicks(headerTime.Add(31*time.Second), headerTime.Add(61*time.Second))
	nodeB.runViewTicks(headerTime.Add(31*time.Second), headerTime.Add(61*time.Second))
	if diff := int(nodeB.snapshot().offset) - int(nodeA.snapshot().offset); diff != 2 {
		t.Fatalf("the fork changed on its own (difference %d, was 2): timeout advancing must not be able to move a node onto another node's grid",
			diff)
	}

	// ---- the fix -----------------------------------------------------------------
	// Same call, same code path, same two nodes, one different argument. The anchor
	// is now the imported block's header.Time, so it is the SAME value on both --
	// and note that the two nodes are still reset at different instants, which is
	// unavoidable and no longer matters. With the anchor supplied, the local clock
	// no longer reaches the grid.
	nodeA.clock.set(headerTime.Add(31 * time.Second))
	nodeA.resumeAndReset(7, uint64(headerTime.Unix()))
	nodeB.clock.set(headerTime.Add(33*time.Second + 700*time.Millisecond))
	nodeB.resumeAndReset(7, uint64(headerTime.Unix()))

	fixedA, fixedB := nodeA.snapshot(), nodeB.snapshot()
	if fixedA.offset != 0 || fixedB.offset != 0 {
		t.Fatalf("a force reset must zero the counter, got %v and %v", fixedA, fixedB)
	}
	if !fixedA.start.Equal(fixedB.start) || !fixedA.start.Equal(headerTime) {
		t.Fatalf("both nodes must anchor to the shared header.Time %v, got %v and %v",
			headerTime, fixedA.start, fixedB.start)
	}
	if !fixedA.change.Equal(fixedB.change) {
		t.Fatalf("the deadlines must be identical too, got %v and %v", fixedA.change, fixedB.change)
	}
	if fixedA.start.Nanosecond() != 0 || fixedA.change.Nanosecond() != 0 {
		t.Fatalf("the anchored grid must sit on whole seconds, got %v and %v", fixedA.start, fixedA.change)
	}
	t.Logf("converged: %s is %v, %s is %v", nodeA.name, fixedA, nodeB.name, fixedB)

	// Five more windows from the shared grid. The two nodes are advanced at their
	// own phases throughout, so this is the same clock skew that produced the fork
	// above; what changed is that both now walk the same grid.
	nodeA.runViewTicks(headerTime.Add(34*time.Second), headerTime.Add(64*time.Second))
	nodeB.runViewTicks(headerTime.Add(34*time.Second), headerTime.Add(64*time.Second))

	a, b := nodeA.snapshot(), nodeB.snapshot()
	// A jump of several views in one call is what a node that was stuck for 28s
	// does on its first tick, and it is the point: the result depends on the anchor
	// and the clock, not on how many ticks were missed. Both land on the same grid
	// point despite ticking at .890 and .104.
	if a.offset != b.offset {
		t.Fatalf("shared anchor must keep the counters equal, got %v and %v", a, b)
	}
	if !a.start.Equal(b.start) {
		t.Fatalf("shared anchor must keep the grids equal, got %v and %v", a, b)
	}
	if a.start.Nanosecond() != 0 {
		t.Fatalf("the grid drifted off the whole second: %v", a.start)
	}
	t.Logf("after five more windows: %s is %v, %s is %v", nodeA.name, a, nodeB.name, b)
}

// TestSharedAnchorIsStableUnderPhaseSkew separates the two disagreements the live
// rig showed, because only one of them is fatal and the repair must not be
// credited with fixing the other.
//
// Inside every view window there are 786ms -- the gap between the two tick phases
// -- where the earlier-ticking node has already advanced and the other has not, so
// for that long they report different view numbers. That is the window loss QA
// measured. It is bounded by one offset and it heals at the next tick, and it is
// present on a healthy chain. The fork in the test above is neither bounded nor
// self-healing. A repair that only aligned tickers would remove this and leave
// that.
func TestSharedAnchorIsStableUnderPhaseSkew(t *testing.T) {
	headerTime := time.Unix(1_700_000_000, 0)

	nodeA := newAnchorTestNode("A", phaseA)
	nodeB := newAnchorTestNode("B", phaseB)

	// Reset at different instants, with the same anchor: the whole point.
	nodeA.clock.set(headerTime.Add(500 * time.Millisecond))
	nodeA.resumeAndReset(6, uint64(headerTime.Unix()))
	nodeB.clock.set(headerTime.Add(2*time.Second + 300*time.Millisecond))
	nodeB.resumeAndReset(6, uint64(headerTime.Unix()))

	for round := 1; round <= 5; round++ {
		window := headerTime.Add(time.Duration(round) * anchorTolerance)

		// B's phase is earlier inside the second, so B ticks first.
		nodeB.tickAt(window.Add(phaseB))

		// The bounded transient: B is one view ahead until A's tick arrives.
		if diff := int(nodeB.snapshot().offset) - int(nodeA.snapshot().offset); diff != 1 {
			t.Fatalf("round %d: the in-window skew should be exactly one view while B has ticked and A has not, got %v and %v",
				round, nodeA.snapshot(), nodeB.snapshot())
		}

		nodeA.tickAt(window.Add(phaseA))
	}

	a, b := nodeA.snapshot(), nodeB.snapshot()
	if a.offset != b.offset {
		t.Fatalf("phase skew must not accumulate into a view difference, got %v and %v", a, b)
	}
	if !a.start.Equal(b.start) {
		t.Fatalf("phase skew must not move either node off the shared grid, got %v and %v", a, b)
	}
	if a.start.Nanosecond() != 0 {
		t.Fatalf("the grid drifted off the whole second: %v", a.start)
	}
	t.Logf("after five windows at .890 and .104: %s is %v, %s is %v", nodeA.name, a, nodeB.name, b)
}
