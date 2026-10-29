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

// CleanProposals replaces all four vote maps -- acceptVotes, rejectedVotes,
// pendingVotes, precociousProposals -- and it used to do so without a lock, while
// every other writer of those maps held mu. In Go a rebuild racing a vote is not
// a race-detector warning, it is
//
//	fatal error: concurrent map read and map write
//
// which kills the process and can leave no stack behind.
//
// This file guards two different failures, and they need two different kinds of
// test. Conflating them is what produced the previous version's judgement, which
// asserted how many votes survived and thereby asserted something that is legal
// at both extremes:
//
//   - "the rebuild lost its lock" is a concurrency defect. It is invisible to
//     counting -- a legal schedule has no lost vote at all -- and visible to
//     -race, which reports the moment two accesses are unordered, whatever the
//     schedule did. So the concurrent case below asserts only what it forces
//     (both sides entered, and they entered together) and lets -race decide.
//   - "the rebuild stopped happening" is a behaviour defect, not a concurrency
//     one: if CleanProposals returns early, nothing about the interleaving
//     matters. No counting of votes can see it either, because the votes are then
//     simply never cleared. It needs a sequential case that looks at the maps
//     across the call, and that is TestCleanProposalsEmptiesTheVoteMaps below.
//
// The vote counts that used to be assertions are now diagnostics. A count is a
// quantity that moves under a legal scheduler: measured on the real tree, the
// accepted count ranges over dozens of values with no defect present, and a
// mutant that made the clean-up do nothing produced the maximum, not the minimum
// -- so the old `counted == 0` guard could not fire on the very mutant it was
// meant to catch, while a `counted == all` guard would fire on healthy runs.
//
// The crossover is still the point. TestFinishedHeightRace drives
// FinishedProposal from several goroutines, but FinishedProposal's tail also
// writes ConsensusView -- so that shape goes red on the view rather than on the
// maps and cannot be used to grade this fix. Here the two goroutines are
// CleanProposals(false) and ProcessVote: the changeView=false branch never
// touches the view, so the only mutable state they share is the vote maps.
//
// This is exactly the pair the production paths produce once there is a network:
// a vote arrives on the p2p goroutine while the chain-head goroutine finishes a
// round. During M2 (one producer, no transport) every ProcessVote call is the
// proposer's own vote on the miner goroutine, so the two never overlap -- which is
// why the devnet's 107 blocks and 141 clean-ups show no fatal and are not evidence
// that this case is unnecessary.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
)

// TestCleanProposalsDoesNotRaceVotes runs CleanProposals(false) against ProcessVote
// and lets -race decide whether the two sides are ordered.
//
// No vote count is asserted, in either direction. Both extremes are reachable on
// healthy code -- zero when the clean-up happens to land outside the vote, the
// maximum when it never lands inside it -- so a count cannot say "the defect is
// absent" and cannot say "the defect is present" either. What is asserted here is
// the part the test forces: a rendezvous that makes both sides start together, and
// the fact that each side then ran. Those two cannot flake, and they are the only
// reason a silent -race run means anything.
func TestCleanProposalsDoesNotRaceVotes(t *testing.T) {
	const (
		// Enough interleavings to make a lost race certain under -race; the
		// ballots are signed up front so the loops themselves are tight.
		voteRounds  = 300
		cleanRounds = 300

		// Three producers, so the threshold for a confirm is two votes: one
		// accept vote can never reach it. That keeps createConfirm, onConfirm
		// and the view transitions out of this test entirely, which is what
		// isolates the vote maps.
		producerCount = 3
	)

	accounts, producers := newProducerSetForRaceTest(t, producerCount)

	// Ballots are built and signed before any goroutine starts: t.Fatalf is not
	// valid off the test goroutine, and signing inside the loop would make the
	// interleaving depend on how long a signature takes.
	proposals, votes := buildSignedAcceptVotes(t, accounts[0], voteRounds)

	d := NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)

	// One vote on its own first, so "the write path works at all" is established
	// before concurrency can be blamed for anything. Without this, a silent -race
	// run could mean the votes never reached the map at all.
	d.setProcessingProposal(proposals[0])
	if ok, _, err := d.ProcessVote(votes[0]); !ok {
		t.Fatalf("a single accept vote was not counted, so this test cannot say anything about the concurrent case: err=%v", err)
	}

	var (
		wg             sync.WaitGroup
		counted        int64
		entered        int64
		inFlight, peak int64
	)

	// The rendezvous is the whole construction: both sides announce themselves and
	// then wait for the same channel, so their first calls are concurrent by
	// arrangement rather than by luck. A barrier that only starts the goroutines
	// would leave the overlap to the scheduler.
	ready := make(chan struct{}, 2)
	start := make(chan struct{})

	enter := func() {
		atomic.AddInt64(&entered, 1)
		n := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
				return
			}
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ready <- struct{}{}
		<-start
		enter()
		for i := 1; i < voteRounds; i++ {
			d.setProcessingProposal(proposals[i])
			if ok, _, _ := d.ProcessVote(votes[i]); ok {
				atomic.AddInt64(&counted, 1)
			}
		}
		atomic.AddInt64(&inFlight, -1)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ready <- struct{}{}
		<-start
		enter()
		for i := 0; i < cleanRounds; i++ {
			d.CleanProposals(false)
		}
		atomic.AddInt64(&inFlight, -1)
	}()

	// Both sides must reach the rendezvous, otherwise one of them never ran and
	// the run proves nothing. The timeout keeps a broken rendezvous from looking
	// like a hung suite.
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 goroutines reached the rendezvous, so the two sides were never concurrent", i)
		}
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&entered); got != 2 {
		t.Fatalf("entered = %d, want 2: one side did not run, so this run proves nothing", got)
	}

	// Diagnostics, deliberately not assertions: see the file comment.
	t.Logf("votes counted = %d of %d, peak concurrent sides = %d", atomic.LoadInt64(&counted), voteRounds-1, atomic.LoadInt64(&peak))

	// The maps must still be usable afterwards, not left in a replaced-but-nil
	// state.
	d.setProcessingProposal(proposals[0])
	if ok, _, err := d.ProcessVote(votes[0]); !ok {
		t.Fatalf("the dispatcher stopped counting votes after %d clean-ups: err=%v", cleanRounds, err)
	}
}

// TestCleanProposalsEmptiesTheVoteMaps is the behavioural half: it asks whether
// the rebuild happened at all, which no amount of concurrency testing can answer.
// If CleanProposals returns early -- the mutant that made the count-based guard
// look useless, because a short-circuited rebuild leaves every vote in place and
// therefore reports the maximum count, not zero -- every map below keeps its
// entry, and this case fails deterministically.
//
// Sequential on purpose: no goroutines, no -race, no interleaving. Two different
// failures need two different detectors, and using the flaky one for the
// deterministic defect is how a real regression gets explained away as "that test
// is racy anyway".
func TestCleanProposalsEmptiesTheVoteMaps(t *testing.T) {
	const producerCount = 3

	accounts, producers := newProducerSetForRaceTest(t, producerCount)
	proposals, votes := buildSignedAcceptVotes(t, accounts[0], 2)

	d := NewDispatcher(producers,
		func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil },
		signToleranceForTest, nil, dtime.NewMedianTime(), nil, 1)

	d.setProcessingProposal(proposals[0])
	if ok, _, err := d.ProcessVote(votes[0]); !ok {
		t.Fatalf("preparing a counted vote failed, so the clean-up below would have nothing to clear: err=%v", err)
	}
	d.AddPendingVote(votes[1])

	if len(d.acceptVotes) == 0 || len(d.pendingVotes) == 0 {
		t.Fatalf("the maps were not populated before the clean-up (accept=%d pending=%d), so this case would pass on a no-op",
			len(d.acceptVotes), len(d.pendingVotes))
	}

	d.CleanProposals(false)

	if got := [4]int{len(d.acceptVotes), len(d.rejectedVotes), len(d.pendingVotes), len(d.precociousProposals)}; got != [4]int{} {
		t.Fatalf("CleanProposals left state behind (accept, rejected, pending, precocious) = %v; either the rebuild did not run or it skipped a map", got)
	}

	// The processing proposal is cleared too, and that half lives behind
	// proposalMu rather than mu.
	if p := d.GetProcessingProposal(); p != nil {
		t.Fatalf("processingProposal survived the clean-up: %x", p.Hash())
	}
}

// newProducerSetForRaceTest returns n arbiter accounts and their compressed public
// keys, without going through the engine: this test needs identities, not a
// running consensus round.
func newProducerSetForRaceTest(t *testing.T, n int) ([]daccount.Account, [][]byte) {
	t.Helper()

	accounts := make([]daccount.Account, n)
	producers := make([][]byte, n)
	for i := 0; i < n; i++ {
		account, _ := newTestAccount(t)
		accounts[i] = account
		producers[i] = account.PublicKeyBytes()
	}
	return accounts, producers
}

// buildSignedAcceptVotes returns n proposals with a signed accept vote for each.
// Every proposal carries a distinct block hash so the votes hash differently and
// the "already voted" guard cannot swallow them.
func buildSignedAcceptVotes(t *testing.T, account daccount.Account, n int) ([]*payload.DPOSProposal, []*payload.DPOSProposalVote) {
	t.Helper()

	proposals := make([]*payload.DPOSProposal, n)
	votes := make([]*payload.DPOSProposalVote, n)
	for i := 0; i < n; i++ {
		proposals[i] = &payload.DPOSProposal{
			Sponsor:    account.PublicKeyBytes(),
			BlockHash:  elacom.Uint256{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)},
			ViewOffset: 1,
		}
		hash := proposals[i].Hash()
		vote, err := StartVote(&hash, true, account)
		if err != nil {
			t.Fatalf("cannot sign vote %d: %v", i, err)
		}
		votes[i] = vote
	}
	return proposals, votes
}
