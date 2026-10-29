//go:build race

package dpos

import (
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/dtime"

	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
)

// The recovery path is the second place that replaces the vote maps and the only
// place that writes workingHeight from outside Producers, so it needs its own
// cases. Both are race-detector only, and for the same reason as the view case:
// a plain-field or plain-map race has no runtime guard, so a green run without
// -race would mean nothing. The file is tagged instead of skipped so that cannot
// be misread.
//
// RecoverFromConsensusStatus has no caller in this tree -- HelpToRecoverAbnormal
// and RecoverAbnormal are the entry points and nothing calls them before the
// recovery messages are wired. That is exactly why the case drives the function
// directly rather than through a story: the day M3 connects it, this is the only
// thing standing between that wiring and a map rebuild racing a live vote.

// newRecoveryFixture builds a dispatcher whose recovery path will actually do
// work: three producers (so one vote cannot reach a confirm), a finished height
// above zero (so RecoverFromStatus takes its duty-index branch instead of skipping
// it) and a status with all four fields populated.
func newRecoveryFixture(t *testing.T) (*Dispatcher, *dmsg.ConsensusStatus, []*payload.DPOSProposal, []*payload.DPOSProposalVote) {
	t.Helper()

	accounts, producers := newProducerSetForRaceTest(t, 3)
	proposals, votes := buildSignedAcceptVotes(t, accounts[0], 4)

	d := NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)

	// Zero would make RecoverFromStatus skip the duty recomputation, and with it
	// most of the path this file is about.
	d.finishedMu.Lock()
	d.finishedHeight = 42
	d.finishedMu.Unlock()

	status := &dmsg.ConsensusStatus{
		ConsensusStatus: ConsensusRunning,
		ViewOffset:      1,
		ViewStartTime:   time.Unix(1762218000, 0),
		WorkingHeight:   7,
	}
	return d, status, proposals, votes
}

// TestRecoverFromConsensusStatusDoesNotRaceVotes covers the map half: the
// recovery replaces acceptVotes / rejectedVotes / pendingVotes while a vote path
// writes them. Before the fix the replacement ran outside d.mu, which is not a
// race report but `fatal error: concurrent map read and map write`.
//
// The failure to look for is a stack on one of the map lines in dispatcher.go. A
// stack on consensusview.go would mean the construction drifted onto the view
// fields instead, and the fix for that is a different construction, not a
// different assertion.
func TestRecoverFromConsensusStatusDoesNotRaceVotes(t *testing.T) {
	const (
		workers = 2
		rounds  = 32
	)

	d, status, proposals, votes := newRecoveryFixture(t)

	tr := newTracker(workers)
	drive(workers, rounds, tr.enter, tr.leave, func(id int) {
		switch id {
		case 0: // recovery chain: replaces the three vote maps
			_ = d.RecoverFromConsensusStatus(status)
		case 1: // vote path: writes pendingVotes, then acceptVotes through ProcessVote
			d.AddPendingVote(votes[0])
			d.setProcessingProposal(proposals[0])
			_, _, _ = d.ProcessVote(votes[0])
		}
	})
	tr.assert(t, workers)
}

// TestRecoverFromConsensusStatusDoesNotRaceWorkingHeight covers the field half.
// The recovery writes workingHeight through the view, and the only lock that can
// order it is Producers' own: viewMu protects none of Producers' fields, which is
// the very point of ticket item 8. The reader below takes p.mtx, so the two are
// ordered only after UpdateWorkingHeight exists.
func TestRecoverFromConsensusStatusDoesNotRaceWorkingHeight(t *testing.T) {
	const (
		workers = 2
		rounds  = 32
	)

	d, status, _, _ := newRecoveryFixture(t)

	tr := newTracker(workers)
	drive(workers, rounds, tr.enter, tr.leave, func(id int) {
		switch id {
		case 0: // recovery chain: writes workingHeight from inside RecoverFromStatus
			_ = d.RecoverFromConsensusStatus(status)
		case 1: // the reader that has to share a lock with it
			_ = d.consensusView.GetWorkingHeight()
		}
	})
	tr.assert(t, workers)
}
