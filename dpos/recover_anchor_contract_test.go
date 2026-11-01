package dpos

import (
	"fmt"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/dpos/dtime"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
)

// The anchor is the origin of the view grid, and it is only worth anything as an
// origin if it is the value the whole set agrees on. Recovery is the one path where
// it arrives from outside (the head header's timestamp, via recoveryAnchor), so it
// is the one path where a local clock can get into the origin.
//
// # The defect these cases were written for
//
// RecoverAbnormal rebuilt the anchor out of this node's own clock:
//
//	dispatcher.go:800  status.ViewStartTime = dtime.Int64ToTime(anchorNanos)    // A
//	dispatcher.go:802  offset, offsetTime := calculateOffsetTime(A, AdjustedTime())
//	dispatcher.go:804  status.ViewStartTime = AdjustedTime().Add(-offsetTime)   // A + 6*q
//
// where q = floor((now-A)/signTolerance) and now is this node's adjusted clock. The
// value carried forward is a WINDOW START, not an origin; RecoverFromStatus then
// stores it as the origin (consensusview.go:720), whose documented contract is
// "you give me the anchor, I work out which window now falls in"
// (consensusview.go:167-172). Two nodes recovering in different windows therefore
// store origins one grid point apart, permanently, because nothing re-derives the
// origin from the caller's value afterwards.
//
// # Why that is not cosmetic
//
// The stored anchor IS the grid origin, so a node one grid point behind reports a
// viewOffset one lower than the rest of the set for the whole cycle. ProcessProposal
// compares view offsets (dispatcher.go:124) and drops the proposal, so that node
// stops voting -- silently, since the others still make quorum exactly:
// int(6*2/3) = 4 of 6. On the rig that looked like six producers splitting 4/2 with
// anchors 16:24:32Z and 16:24:26Z while blocks kept coming.

// TestRecoveryStoresTheAnchorItWasGiven is the contract, at the boundary where it
// broke: whatever recoveryAnchor read out of the head header must be the origin that
// gets stored, unmodified, no matter which window the recovery happens to run in.
//
// # Why the table is parameterised by the anchor and not by the clock
//
// The only input that decides q is how long after the anchor the recovery runs, so
// sweeping the anchor across a window against a fixed real clock covers exactly the
// same space as sweeping two nodes' clocks across a boundary against a fixed anchor
// -- deterministically, with no clock seam and no sleep. TestTwoNodes... below then
// states the same property the way the rig showed it, using a shifted adjusted clock.
func TestRecoveryStoresTheAnchorItWasGiven(t *testing.T) {
	// Half a second inside the wall second, so that every case below sits half a
	// second away from a grid boundary instead of on one. `anchor` is truncated
	// to whole seconds, so with now at .500 the elapsed time for a case is
	// exactly |behind| + 0.5s: -1s -> 1.5s (window 0), -6s -> 6.5s (window 1),
	// -13s -> 13.5s (window 2). Without this, the -5s case would land a few
	// microseconds under a boundary and the view-offset assertion below could
	// flip on scheduling noise, which would be a flake and not a failure.
	now := time.Now().Truncate(time.Second).Add(500 * time.Millisecond)

	cases := []struct {
		name string
		// behind is how far before this node's clock the anchor sits, i.e. how long
		// after the anchor the recovery runs. It selects the window, which is the
		// whole variable under test.
		behind time.Duration
		// oldGrid is how many grid points the clock-rebuilding version would have
		// added, asserted below so that "red for the expected reason" is checkable
		// rather than assumed.
		oldGrid time.Duration
		// wantOffset is the view offset the recovery must still report afterwards.
		// It exists so that deleting the rewrite at dispatcher.go:804 cannot be
		// paid for by flattening the window too: zeroing the offset would make
		// GetAnchor agree with the anchor and pass every assertion above, while
		// destroying the clock the whole view grid is measured against.
		wantOffset uint32
	}{
		{"anchor one second back, inside this window", -1 * time.Second, 0, 0},
		{"anchor five seconds back, last second of the window", -5 * time.Second, 0, 0},
		{"anchor exactly one window back", -6 * time.Second, signToleranceForTest, 1},
		{"anchor one window and one second back", -7 * time.Second, signToleranceForTest, 1},
		{"anchor two windows back", -12 * time.Second, 2 * signToleranceForTest, 2},
		{"anchor two windows and one second back", -13 * time.Second, 2 * signToleranceForTest, 2},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := newRecoveryNilFixture(t)

			// Whole seconds: the wire carries the anchor as a whole second and so
			// does recoveryAnchor (int64(anchor) * int64(time.Second)).
			anchor := now.Add(tc.behind).Truncate(time.Second)

			d.RecoverAbnormal(&dmsg.ConsensusStatus{ConsensusStatus: ConsensusRunning}, anchor.UnixNano())

			got := d.GetConsensusView().GetAnchor()
			want := uint64(anchor.Unix())
			if got == want {
				// Origin is right. Now the other half of the contract: the window
				// still gets placed on top of it, from (anchor, now).
				if vo := d.GetConsensusView().GetViewOffset(); vo != tc.wantOffset {
					t.Fatalf("recovery stored the right origin (%d) but reports view offset "+
						"%d, want %d. The origin is shared and the WINDOW is this node's to "+
						"place (reanchorLocked, consensusview.go); an offset of 0 here means "+
						"the fix removed the window placement along with the origin rewrite",
						want, vo, tc.wantOffset)
				}
				return
			}
			if tc.oldGrid > 0 && got == uint64(anchor.Add(tc.oldGrid).Unix()) {
				t.Fatalf("recovery stored the origin %s, which is the anchor plus %v: "+
					"that is the local-clock rebuild at dispatcher.go:804, i.e. the window "+
					"start passed off as the origin. Every node recovering in a different "+
					"window stores a different origin, so two of them then report view "+
					"offsets one apart for good and the slower set stops voting",
					time.Unix(int64(got), 0).UTC().Format(time.RFC3339), tc.oldGrid)
			}
			t.Fatalf("recovery stored origin %d (%s), want %d (%s): the anchor the caller "+
				"read out of the head header must be stored unmodified",
				got, time.Unix(int64(got), 0).UTC().Format(time.RFC3339),
				want, anchor.UTC().Format(time.RFC3339))
		})
	}
}

// TestTwoNodesRecoveringFromOneHeadShareOneAnchor is the same property stated the way
// the rig showed it: two nodes, one head, one anchor, different moments.
//
// The two nodes are given the same anchor and are made to disagree only in their
// adjusted clocks -- five seconds apart, which puts them in different windows of that
// one anchor. That is the whole of the difference between them in reality: the anchor
// comes from a block header both of them hold.
func TestTwoNodesRecoveringFromOneHeadShareOneAnchor(t *testing.T) {
	// One head, eight seconds old as far as the real clock is concerned: the node on
	// the unmodified clock falls in window 1 of this anchor (8/6 = 1), the node three
	// seconds behind it falls in window 0 (5/6 = 0). Both sit at least a second inside
	// their window, so scheduling jitter of a few hundred microseconds cannot move
	// either one across a boundary and make the case pass for the wrong reason.
	anchor := time.Now().Add(-8 * time.Second).Truncate(time.Second)

	ahead := newRecoveryNilFixture(t)
	behind := newRecoveryNilFixture(t)
	shiftAdjustedClock(t, behind, -3*time.Second)

	for _, d := range []*Dispatcher{ahead, behind} {
		d.RecoverAbnormal(&dmsg.ConsensusStatus{ConsensusStatus: ConsensusRunning}, anchor.UnixNano())
	}

	ga := ahead.GetConsensusView().GetAnchor()
	gb := behind.GetConsensusView().GetAnchor()

	if ga != gb {
		t.Fatalf("two nodes recovering from the same head stored different origins: %s and "+
			"%s (%v apart). Neither will agree with the other about the current view, so "+
			"one of them rejects every proposal at dispatcher.go:124 and stops voting "+
			"while the rest of the set makes quorum and keeps producing -- a silent split",
			time.Unix(int64(ga), 0).UTC().Format(time.RFC3339),
			time.Unix(int64(gb), 0).UTC().Format(time.RFC3339),
			time.Duration(int64(ga)-int64(gb))*time.Second)
	}
	if want := uint64(anchor.Unix()); ga != want {
		t.Fatalf("both nodes agree, but on %d (%s) rather than the anchor they were given, "+
			"%d (%s): a consistent grid that is not the chain's grid is still a fork",
			ga, time.Unix(int64(ga), 0).UTC().Format(time.RFC3339),
			want, anchor.UTC().Format(time.RFC3339))
	}
}

// shiftAdjustedClock moves d's adjusted clock by roughly by, using the same mechanism
// production uses to absorb peer clock skew: medianTime only adopts an offset once it
// holds five samples AND the count is odd, so five distinct IDs are the minimum that
// actually moves the clock, and a sample set with an even count would silently leave
// the offset at zero and make this test pass for the wrong reason.
//
// by is capped at medianTime's own maxAllowedOffset of 4.2s: a median beyond that is
// rejected outright and the offset stays at zero (mediantime.go:151), which is the
// failure this helper reports instead of letting the caller believe in a shift that
// never happened.
func shiftAdjustedClock(t *testing.T, d *Dispatcher, by time.Duration) {
	t.Helper()

	if by > 4*time.Second || by < -4*time.Second {
		t.Fatalf("requested shift %v exceeds medianTime's maxAllowedOffset (4.2s); "+
			"samples beyond it are dropped and the clock does not move", by)
	}

	m := dtime.NewMedianTime()
	for i := 0; i < 5; i++ {
		m.AddTimeSample(fmt.Sprintf("shift-sample-%d", i), time.Now().Add(by))
	}
	if got := m.Offset(); got < by-time.Second || got > by+time.Second {
		t.Fatalf("the clock shift did not take: offset is %v, want about %v; "+
			"every case in this file that depends on which grid window a node falls "+
			"into would be meaningless", got, by)
	}
	d.timeSource = m
}
