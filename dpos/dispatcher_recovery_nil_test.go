package dpos

import (
	"fmt"
	"runtime/debug"
	"testing"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
)

// No build tag on purpose. What these cases assert is deterministic -- a call
// either panics or it does not, an error is either non-nil or it is not -- so
// they have to run under a plain `go test ./dpos/` too. A case that exists only
// inside a //go:build race file is not guarded in the ordinary run, and this is
// the only guard these two functions have: devnet never reaches RecoverAbnormal,
// and -race reaches it only through a test.

// TestRecoverAbnormalWithNilStatusDoesNotPanic covers the inherited nil-order
// defect. The function's first statement read status.ConsensusStatus, with the
// nil check below it, so a nil status panicked there.
//
// Both medianTime values are covered. For the nil case the second row does not
// change the verdict -- every statement between the log line and the return
// dereferences status, so a check moved to any of those positions fails both rows
// (measured: the mutant that put the check back below the log line failed
// medianTime=0 and medianTime=1600000000 alike). The second row earns its place
// on the non-nil path in the next case, where medianTime != 0 is the branch that
// calls calculateOffsetTime; a table with only medianTime=0 would let that branch
// go uncovered while looking covered.
func TestRecoverAbnormalWithNilStatusDoesNotPanic(t *testing.T) {
	d := newRecoveryNilFixture(t)

	for _, medianTime := range []int64{0, 1600000000} {
		medianTime := medianTime
		t.Run(fmt.Sprintf("medianTime=%d", medianTime), func(t *testing.T) {
			mustNotPanic(t, "RecoverAbnormal(nil)", func() {
				d.RecoverAbnormal(nil, medianTime)
			})
		})
	}
}

// TestRecoverAbnormalAppliesStatusWithMedianTime drives the branch the nil cases
// above cannot reach: a real status with medianTime != 0, which recomputes the
// offset against the local clock and rewrites the start time before handing the
// status to RecoverFromConsensusStatus.
//
// The assertion is that the status actually arrived. "Did not panic" is not
// enough here: the function could return early and still not panic.
func TestRecoverAbnormalAppliesStatusWithMedianTime(t *testing.T) {
	d := newRecoveryNilFixture(t)

	status := &dmsg.ConsensusStatus{
		ConsensusStatus: ConsensusRunning,
		ViewOffset:      3,
	}
	mustNotPanic(t, "RecoverAbnormal(status, medianTime!=0)", func() {
		d.RecoverAbnormal(status, 1600000000)
	})

	if got := d.GetConsensusView().GetConsensusStatus(); got != ConsensusRunning {
		t.Fatalf("recovered status is %d, want %d; the status never reached the view", got, ConsensusRunning)
	}
	if status.ViewStartTime.IsZero() {
		t.Fatalf("ViewStartTime is still zero; the medianTime branch did not run")
	}
}

// TestRecoverFromConsensusStatusRejectsNil covers the same class one function
// further down. This one had no nil check at all, and it is exported, with
// upstream calling it directly from dposhandlerswitch.go rather than through
// RecoverAbnormal.
func TestRecoverFromConsensusStatusRejectsNil(t *testing.T) {
	d := newRecoveryNilFixture(t)

	var err error
	mustNotPanic(t, "RecoverFromConsensusStatus(nil)", func() {
		err = d.RecoverFromConsensusStatus(nil)
	})
	if err == nil {
		t.Fatalf("RecoverFromConsensusStatus(nil) returned nil; a caller cannot tell the status was dropped")
	}
}

// mustNotPanic turns a panic into a failure of this case, keeping the stack in
// the message. Without it a panic takes down the whole test binary and the report
// says nothing about which case did it; with it, the failure names the case and
// still shows where the dereference happened, which is what makes "red for the
// expected reason" something you can check instead of assume.
func mustNotPanic(t *testing.T, name string, f func()) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v\n%s", name, r, debug.Stack())
		}
	}()
	f()
}

// newRecoveryNilFixture builds a dispatcher with real identities. A bare
// Dispatcher would do for a nil-only path, but identities keep these cases honest
// if someone later moves the check into a branch that touches producers.
func newRecoveryNilFixture(t *testing.T) *Dispatcher {
	t.Helper()

	_, producers := newProducerSetForRaceTest(t, 3)
	return NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)
}
