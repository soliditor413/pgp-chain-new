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
// which kills the process and can leave no stack behind. That is why this is a
// correctness test and not a race-detector nicety: the assertion is that votes
// keep being counted while rounds are torn down, and the -race entry point has to
// stay clean for it to mean anything.
//
// The crossover is the point. TestFinishedHeightRace drives FinishedProposal from
// several goroutines, but FinishedProposal's tail also writes ConsensusView, which
// has no locks at all (recorded separately as R20) -- so that shape goes red on
// the view rather than on the maps and cannot be used to grade this fix. Here the
// two goroutines are CleanProposals(false) and ProcessVote: the changeView=false
// branch never touches the view, so the only mutable state they share is the vote
// maps.
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

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
)

// TestCleanProposalsDoesNotRaceVotes runs CleanProposals(false) against ProcessVote
// and requires that votes are still accepted while that happens.
//
// The accepted count is deliberately asserted to be non-zero. Without it the case
// would pass just as well if both sides failed for unrelated reasons -- a vote that
// never reaches the map write cannot demonstrate anything about the map write. That
// is the one thing worse than a red test: a green one that exercises nothing.
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
	// before concurrency can be blamed for anything.
	d.setProcessingProposal(proposals[0])
	if ok, _, err := d.ProcessVote(votes[0]); !ok {
		t.Fatalf("a single accept vote was not counted, so this test cannot say anything about the concurrent case: err=%v", err)
	}

	var (
		wg      sync.WaitGroup
		counted int64
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i < voteRounds; i++ {
			d.setProcessingProposal(proposals[i])
			if ok, _, _ := d.ProcessVote(votes[i]); ok {
				atomic.AddInt64(&counted, 1)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < cleanRounds; i++ {
			d.CleanProposals(false)
		}
	}()

	wg.Wait()

	// A vote only fails here when the concurrently-running CleanProposals has
	// already cleared the processing proposal, so the two goroutines really did
	// interleave -- and some votes still landed while that was happening.
	if got := atomic.LoadInt64(&counted); got == 0 {
		t.Fatal("no vote was counted while CleanProposals was running; the two goroutines never overlapped, so this test proves nothing")
	}

	// The maps must still be usable afterwards, not left in a replaced-but-nil
	// state.
	d.setProcessingProposal(proposals[0])
	if ok, _, err := d.ProcessVote(votes[0]); !ok {
		t.Fatalf("the dispatcher stopped counting votes after %d clean-ups: err=%v", cleanRounds, err)
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
