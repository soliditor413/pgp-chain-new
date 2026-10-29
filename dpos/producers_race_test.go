//go:build race

package dpos

import (
	"sync"
	"testing"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
)

// The one bare read that used to sit on a live path: processVote's reject branch
// asks ConsensusView.IsMajorityRejected, which read len(p.producers) with no
// lock, while UpdateProducers replaces that slice under p.mtx.
//
// The asymmetry decides the priority: the read side runs every round (the
// proposer's own vote goes through ProcessVote, from Seal), and only the write
// side waits for the SPV producer-set update. So unlike the recovery chain, half
// of this pair is already executing.
//
// Three cases, and which one is the guard matters:
//
//   - TestIsMajorityRejectedDoesNotRaceProducerUpdates is the guard: the read
//     side touches no lock of Producers at all before the read.
//   - TestRejectedVotePathDoesNotRaceProducerUpdates is the same pair seen from
//     the production call site, through ProcessVote.
//   - TestRejectedVotePathReachesTheGuardedRead is the reachability witness: it
//     runs sequentially and asserts that a reject vote actually arrives at the
//     guarded read, which is what keeps the two cases above from guarding a path
//     nobody walks.
//
// Why hammer() and not drive(): measured on the unguarded mutant (producersCount
// without p.mtx), a per-round barrier releases both sides at every round
// boundary, and the access pair stops being reported -- 1 red in 5 runs with
// rounds=32, and 2 red in 5 runs with rounds=1, i.e. the extra rounds add no
// chances. The barrier orders each round against the previous one, so a run
// contains very few unordered pairs and whether -race sees one is a coin flip. A
// guard that is red two times in five is not a guard. hammer() synchronises once,
// before either side touches the word, and never again: 10 red in 10 runs.
//
// The iteration count is part of that measurement, not decoration: the same
// hammer() at 32 iterations per side was red in 3 runs of 5, and at 400 it was
// red in 10 of 10. More writes per side means more chances for a write's check to
// land while the reader's last access is still in the shadow slot.
//
// No assertion on vote counts anywhere: a count moves with the schedule for the
// same reasons the accepted-vote count did in clean_proposals_race_test.go.
// iterations is 400 for a measured reason, not for volume: at 32 per side the
// same construction was red in 3 runs of 5 on the unguarded mutant, at 400 it was
// red in 10 of 10. See the note above.
const iterations = 400

func TestIsMajorityRejectedDoesNotRaceProducerUpdates(t *testing.T) {
	_, producers := newProducerSetForRaceTest(t, 3)
	d := NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)

	hammer(t, 2, func(id int) {
		switch id {
		case 0: // the write side: the producer-set update replaces the slice
			for i := 0; i < iterations; i++ {
				d.consensusView.UpdateProducers(producers, len(producers), uint64(i))
			}
		case 1: // the read side: the guarded read on its own, no p.mtx in front
			for i := 0; i < iterations; i++ {
				d.consensusView.IsMajorityRejected(i)
			}
		}
	})
}

// TestRejectedVotePathDoesNotRaceProducerUpdates is the same pair from the
// production call site: the proposer's own reject vote going through ProcessVote.
//
// The interleaving is not as clean as the case above -- processVote calls
// ConsensusView.IsProducers (dispatcher.go) before the reject branch, and that
// takes p.mtx, so some pairs are ordered by the writer's own lock -- but the
// reader keeps returning to the bare read while the writer keeps replacing the
// slice, so pairs that are unordered remain. Measured on the unguarded mutant:
// 10 red in 10 runs, together with the case above (2 reports per run).
func TestRejectedVotePathDoesNotRaceProducerUpdates(t *testing.T) {
	accounts, producers := newProducerSetForRaceTest(t, 3)
	proposal := &payload.DPOSProposal{
		Sponsor:    accounts[0].PublicKeyBytes(),
		BlockHash:  elacom.Uint256{1, 2, 3, 4},
		ViewOffset: 1,
	}
	reject := buildSignedRejectVote(t, accounts[0], proposal)

	d := NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)
	d.setProcessingProposal(proposal)

	hammer(t, 2, func(id int) {
		switch id {
		case 0: // the write side
			for i := 0; i < iterations; i++ {
				d.consensusView.UpdateProducers(producers, len(producers), uint64(i))
			}
		case 1: // the read side: processVote's reject branch
			for i := 0; i < iterations; i++ {
				// Cleared every round so alreadyExistVote does not swallow the
				// vote before it reaches IsMajorityRejected.
				d.mu.Lock()
				d.rejectedVotes = make(map[elacom.Uint256]*payload.DPOSProposalVote)
				d.mu.Unlock()
				d.ProcessVote(reject)
			}
		}
	})
}

// TestRejectedVotePathReachesTheGuardedRead is the reachability half.
//
// succeed == true is only ever returned after processVote has written the vote
// into rejectedVotes and asked IsMajorityRejected about it: every early return
// above that line returns false, and an accept vote never gets here. So this
// asserts that the guarded read sits on a path a real vote walks, not just on a
// path a test can call.
func TestRejectedVotePathReachesTheGuardedRead(t *testing.T) {
	accounts, producers := newProducerSetForRaceTest(t, 3)
	proposal := &payload.DPOSProposal{
		Sponsor:    accounts[0].PublicKeyBytes(),
		BlockHash:  elacom.Uint256{5, 6, 7, 8},
		ViewOffset: 1,
	}
	reject := buildSignedRejectVote(t, accounts[0], proposal)

	d := NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)
	d.setProcessingProposal(proposal)

	succeed, _, err := d.ProcessVote(reject)
	if !succeed || err != nil {
		t.Fatalf("the reject vote never reached the reject branch: succeed=%v err=%v", succeed, err)
	}
	if _, ok := d.rejectedVotes[reject.Hash()]; !ok {
		t.Fatalf("the vote is not in rejectedVotes, so the guarded read did not run")
	}
}

// hammer runs f in workers goroutines with exactly one synchronisation point:
// a barrier both of them must reach before either calls f. That order is what
// makes "both sides were in flight at once" a property of the construction --
// both are inside the tracker when they reach the barrier -- and it is also why
// the two bodies below are left unordered with respect to each other, which is
// what the detector needs. See the note at the top of this file for the
// measurement that ruled out a per-round barrier.
func hammer(t *testing.T, workers int, f func(id int)) {
	t.Helper()

	tr := newTracker(workers)
	start := newBarrier(workers)
	var wg sync.WaitGroup
	for id := 0; id < workers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tr.enter(id)
			start.wait()
			f(id)
			tr.leave()
		}(id)
	}
	wg.Wait()
	tr.assert(t, int32(workers))
}

// buildSignedRejectVote signs a vote that rejects proposal, so processVote takes
// the rejectedVotes branch.
func buildSignedRejectVote(t *testing.T, account daccount.Account, proposal *payload.DPOSProposal) *payload.DPOSProposalVote {
	t.Helper()

	hash := proposal.Hash()
	vote, err := StartVote(&hash, false, account)
	if err != nil {
		t.Fatalf("cannot sign a reject vote: %v", err)
	}
	return vote
}
