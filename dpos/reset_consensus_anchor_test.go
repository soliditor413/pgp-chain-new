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
// to ChangeView. reanchorLocked stores it as a whole second and every later
// viewOffset is DERIVED from it -- offset = (now - anchor) / signTolerance -- so
// two nodes agree on "which view are we in at height h" exactly when they were
// anchored with the same value. Hand them different anchors and every proposal is
// stamped with a view number the others do not recognise, which dispatcher
// ProcessProposal drops on the "have different view offset" branch.
//
// These tests pin that mechanism rather than the particular call sites, because
// the call sites are what went wrong once already: the port reset the view on
// every imported block but anchored it to uint64(now.Unix()) of the local clock,
// so six producers sat one and two whole views apart, three against three, and
// livelocked at height 6 with no path back. See Dispatcher.ResetConsensus.
//
// The second half of the same repair is that the offset is derived and not
// accumulated. As a counter it advanced only while the view was Running, so two
// nodes holding an identical anchor still reported different offsets, and the
// number each reported carried its own history: measured on the live rig, 96 on
// five producers against 288 on the one that had been on duty. A counter can only
// be repaired by resetting it, which is why ResetViewOffset existed and why every
// node that skipped a reset drifted. TestDerivedOffsetSurvivesUnequalResetRates
// and TestRecoverFromStatusIgnoresThePeerCounter below cover that half.

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
	// Not "zero": a derived offset has no counter to zero, and the two nodes are
	// not at offset 0 here -- they were reset onto a 31s-old anchor and are
	// therefore five whole views in. What the force reset buys is that the number
	// they report is a function of the shared anchor instead of their own history,
	// so the assertion is agreement, not zero.
	if fixedA.offset != fixedB.offset {
		t.Fatalf("one anchor must mean one view, got %v and %v", fixedA, fixedB)
	}
	// The window both nodes are sitting in, expressed from the shared anchor: this
	// is what makes the grid shared rather than merely the answer equal today.
	wantStart := headerTime.Add(time.Duration(fixedA.offset) * anchorTolerance)
	if !fixedA.start.Equal(wantStart) || !fixedB.start.Equal(wantStart) {
		t.Fatalf("both nodes must sit on %v (%v + %d tolerances), got %v and %v",
			wantStart, headerTime, fixedA.offset, fixedA.start, fixedB.start)
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

// TestDerivedOffsetSurvivesUnequalResetRates is the half the anchor fix could not
// reach, and the one the live rig reproduced after that fix was deployed.
//
// Both nodes are anchored to the same block header, so by the grid argument above
// they should never disagree. But the view offset used to be a counter that only
// advanced while the view was Running, and SetRunning is called from prepare.go's
// startView -- once per round -- while a reset zeroed the counter. A node that
// produced the block got a reset and started counting again from zero; a node that
// only watched stayed Ready, kept its number, or jumped. Measured on six
// producers with one shared anchor: 96 on five and 288 on the one that had been
// on duty. Nothing in either node could reconcile the two, because each number was
// that node's own history.
//
// This is the same test written against both implementations: one node runs for
// ten minutes without ever being reset, the other is reset every thirty seconds
// onto the identical anchor. Ten minutes is a hundred view windows.
func TestDerivedOffsetSurvivesUnequalResetRates(t *testing.T) {
	headerTime := time.Unix(1_700_000_000, 0)
	anchor := uint64(headerTime.Unix())

	// A produces and is re-anchored by every imported block; B has no block to
	// import for the next ten minutes and is only ever re-anchored when the driver
	// pokes it. Both start from the same anchor, which is the whole premise.
	producer := newAnchorTestNode("A (reset every 30s)", phaseA)
	watcher := newAnchorTestNode("B (never reset)", phaseB)

	for _, n := range []*anchorTestNode{producer, watcher} {
		n.clock.set(headerTime.Add(500 * time.Millisecond))
		n.resumeAndReset(6, anchor)
	}

	// Ten minutes of one-second ticks. The two tick at their own phases, which is
	// the skew the test above shows is harmless on a shared anchor.
	const seconds = 600
	for i := 1; i <= seconds; i++ {
		tick := headerTime.Add(time.Duration(i) * time.Second)

		watcher.tickAt(tick.Add(phaseB))

		if i%30 == 0 {
			// A block landed on A: ResetConsensus with the block's own header.Time,
			// which here is the anchor both nodes already share.
			producer.clock.set(tick.Add(phaseA))
			producer.resumeAndReset(uint64(6+i/30), anchor)
		} else {
			producer.tickAt(tick.Add(phaseA))
		}

		gotA, gotB := producer.snapshot(), watcher.snapshot()
		if gotA.offset != gotB.offset {
			t.Fatalf("second %d: one anchor must mean one view whatever the reset rate, got %v and %v",
				i, gotA, gotB)
		}
		if !gotA.start.Equal(gotB.start) {
			t.Fatalf("second %d: the two nodes must sit in the same window, got %v and %v", i, gotA, gotB)
		}
	}

	a, b := producer.snapshot(), watcher.snapshot()
	// 600s on a 6s tolerance. Stated as an absolute so the test fails loudly if a
	// future change rescales the grid instead of silently comparing two zeroes.
	if want := uint32(seconds / 6); a.offset != want {
		t.Fatalf("after %ds the shared view should be %d, got %v", seconds, want, a)
	}
	t.Logf("after %ds at unequal reset rates: %s is %v, %s is %v", seconds, producer.name, a, watcher.name, b)
}

// TestFrozenAnchorSurvivesAMixedResetRate is the shape of the second live
// incident, and it is the test that decides whether deriving the offset was enough.
//
// The chain stopped at height 5 and never produced another block. Every node kept
// re-anchoring -- on block import, on the recovered status exchange, on the reset
// storm -- and every one of those re-anchorings handed back the SAME timestamp,
// the frozen one in the parent block. So the grid never moved and the anchor was
// never the problem; the offset was. As a counter, a reset zeroed it and the next
// tick added back the whole elapsed distance, so one node reported 0 while its
// neighbour reported 249, two milliseconds apart, forever. Peers reject a proposal
// carrying another offset, so every round died on "have different view offset".
//
// Here the two nodes are reset at two rates an order of magnitude apart -- 10Hz and
// 1Hz, both with the frozen parent -- which is what the storm on the live chain was
// doing. If the offset is a position on the grid and not a private counter, the
// rate cannot be observed in the answer.
func TestFrozenAnchorSurvivesAMixedResetRate(t *testing.T) {
	// The parent block's timestamp: frozen, thirty minutes old, shared by both.
	frozen := time.Unix(1_700_000_000, 0)
	anchor := uint64(frozen.Unix())
	base := frozen.Add(30 * time.Minute)

	slammed := newAnchorTestNode("A (reset at 10Hz)", phaseA)
	occasional := newAnchorTestNode("B (reset at 1Hz)", phaseB)

	// Sixty seconds of wall clock in 100ms steps. Both nodes tick their view clock
	// once a second, as a real node does; the difference is how often they are
	// re-anchored -- one by a reset storm at 10Hz, one at 1Hz. Putting the tick in
	// is what makes the divergence visible while it happens rather than only at the
	// end: on the counter, the 10Hz node was zeroed ten times a second and the 1Hz
	// node kept its number, which is the 0-against-249 alternation QA measured two
	// milliseconds apart on the same box.
	for i := 0; i < 600; i++ {
		now := base.Add(time.Duration(i) * 100 * time.Millisecond)

		slammed.clock.set(now)
		slammed.resumeAndReset(6, anchor)

		occasional.clock.set(now)
		if i%10 == 0 {
			occasional.resumeAndReset(6, anchor)
			slammed.tickAt(now)
			occasional.tickAt(now)
		}

		a, b := slammed.snapshot(), occasional.snapshot()
		if a.offset != b.offset {
			t.Fatalf("step %d: the reset rate became observable in the view, got %v and %v",
				i, a, b)
		}
	}

	a, b := slammed.snapshot(), occasional.snapshot()
	// 30 minutes plus the sixty seconds the loop ran, on a 6s tolerance. Stated
	// absolutely so that two zeroes cannot pass for agreement -- 0 vs 0 is exactly
	// what the old code produced half the time.
	last := base.Add(599 * 100 * time.Millisecond)
	if want := uint32(last.Sub(frozen) / anchorTolerance); a.offset != want {
		t.Fatalf("the frozen anchor implies view %d at %v, got %v", want, last, a)
	}
	if !a.start.Equal(b.start) {
		t.Fatalf("both nodes must sit in the same window, got %v and %v", a, b)
	}
	t.Logf("after 60s at 10Hz and 1Hz resets on a frozen anchor: %s is %v, %s is %v",
		slammed.name, a, occasional.name, b)
}

// TestRecoverFromStatusIgnoresThePeerCounter is the same defect on the recovery
// path, and it is the one that would keep re-seeding the divergence after every
// other fix.
//
// RecoverAbnormal asks every peer for its view state and takes the median window
// start; pickRecoverStatus then picks one peer's status to apply. Under the
// counter, applying it meant `viewOffset += offset`, so the number this node
// carried afterwards was the peer's accident plus its own -- and the peer is
// chosen by map iteration order, so two nodes recovering from the same evidence
// landed on different views.
//
// The offset argument is still on the wire and still passed in. What this pins is
// that the local view is a function of the anchor and the clock only: any value in
// that argument, including a nonsense one, must leave the answer alone.
func TestRecoverFromStatusIgnoresThePeerCounter(t *testing.T) {
	headerTime := time.Unix(1_700_000_000, 0)
	node := newAnchorTestNode("A", phaseA)

	// Thirteen seconds past the shared anchor is two whole tolerances in.
	node.clock.set(headerTime.Add(13 * time.Second))

	// The peer's window start is a shared candidate; its counter is not. 999 is
	// what an accumulator on a long-running peer reports, and 0 is what a freshly
	// restarted one reports. Neither may move this node off the grid.
	for _, peerOffset := range []uint32{999, 0, 1, 2, 7} {
		node.d.GetConsensusView().RecoverFromStatus(ConsensusRunning, peerOffset, headerTime, 1, 0)

		view := node.d.GetConsensusView()
		if got := view.GetViewOffset(); got != 2 {
			t.Fatalf("peer reported viewOffset %d and the local view became %d, want the 2 the anchor implies",
				peerOffset, got)
		}
		if start := view.GetViewStartTime(); !start.Equal(headerTime.Add(12 * time.Second)) {
			t.Fatalf("peer reported viewOffset %d and the local window moved to %v, want %v",
				peerOffset, start, headerTime.Add(12*time.Second))
		}
	}
}
