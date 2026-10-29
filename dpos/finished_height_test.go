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

// The finality watermark is the first piece of DPoS state that two real
// goroutines touch: consensus/pbft advances it from its chain-head subscription
// (eth/backend.go pbftLoop -> OnBlockImported -> FinishedProposal) while the miner
// reads it on every Prepare and Seal (consensus/pbft/prepare.go, seal.go). Before
// finishedMu existed, that pair was a plain uint64 write racing a plain uint64
// read, and `go test -race` on the pbft seal tests reported it.
//
// These tests exist to keep it reported if the lock is ever dropped: they do
// nothing but hammer the accessors from several goroutines at once, so they are
// only meaningful under -race. They are not a substitute for the functional
// tests -- the assertions are deliberately weak (monotonicity), because the point
// is the race detector, not the value.

import (
	"sync"
	"testing"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
)

// newRaceTestDispatcher builds the smallest dispatcher the finality accessors
// need: no producers, no view listeners, and a real median time source because
// FinishedProposal asks it for "now".
func newRaceTestDispatcher() *Dispatcher {
	return NewDispatcher(nil, func(*payload.Confirm) error { return nil },
		func(*payload.Confirm) error { return nil }, signToleranceForTest, nil,
		dtime.NewMedianTime(), nil, 1)
}

// signToleranceForTest matches the engine's tolerance; the value only matters for
// the view clock, which these tests do not exercise.
const signToleranceForTest = 6 * time.Second

// TestFinishedHeightRace advances the watermark from one goroutine while several
// others read the three accessors.
func TestFinishedHeightRace(t *testing.T) {
	const (
		readers = 4
		rounds  = 200
	)

	d := newRaceTestDispatcher()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if h := d.GetFinishedHeight(); h > rounds+1 {
					t.Errorf("finished height %d is beyond anything that was ever proposed", h)
					return
				}
				d.GetFinishedBlockSealHash()
				d.GetFinishedProposal()
			}
		}()
	}

	for height := uint64(1); height <= rounds; height++ {
		d.FinishedProposal(height, elacom.Uint256{byte(height)}, uint64(time.Now().Unix()))
	}
	close(stop)
	wg.Wait()

	if got := d.GetFinishedHeight(); got != rounds {
		t.Fatalf("finished height is %d, want %d", got, rounds)
	}
	if got, want := d.GetFinishedBlockSealHash(), (elacom.Uint256{byte(rounds)}); got != want {
		t.Fatalf("finished block seal hash is %v, want %v", got, want)
	}
}

// TestFinishedProposalRejectsForkBlocks drives the watermark from out-of-order
// heights, which is what a side branch delivers, while a reader checks that it
// never moves backwards. A watermark that regressed would let core's reorg guard
// (M6) accept a block it has already finalised.
//
// There is exactly ONE writer on purpose. FinishedProposal is not reentrant: the
// work it does after releasing finishedMu includes the view transitions
// (SetReady/UpdateDutyIndex/ChangeView), and ConsensusView has no locks at all, so
// two simultaneous calls race there. That is a separate defect from the vote maps
// and is recorded on its own (R20 in docs/design/pbft-port-design.md); grading it
// here would mean this test goes red on the view, which says nothing about the
// finality watermark it is named for. Serialising FinishedProposal instead would
// mean holding a lock across the listener callback, which the M2 review explicitly
// ruled out.
//
// The vote-map half of that tail is fixed and graded elsewhere: CleanProposals now
// rebuilds the four maps under mu, and TestCleanProposalsDoesNotRaceVotes drives it
// against ProcessVote directly. Note which half is which -- this test's one-writer
// shape is about ConsensusView now, not about the maps, and the crossover case is
// what a future reader should extend if the view gets its lock.
func TestFinishedProposalRejectsForkBlocks(t *testing.T) {
	const rounds = 100

	d := newRaceTestDispatcher()

	var (
		done     = make(chan struct{})
		readerWG sync.WaitGroup
		writerWG sync.WaitGroup
	)

	// Readers checking monotonicity of the watermark as it is being written.
	for i := 0; i < 3; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			last := uint64(0)
			for {
				select {
				case <-done:
					return
				default:
				}
				height := d.GetFinishedHeight()
				if height < last {
					t.Errorf("finished height moved backwards: %d after %d", height, last)
					return
				}
				last = height
			}
		}()
	}

	// One writer, walking a permutation so that most heights offered are below the
	// current watermark and must be rejected as fork blocks.
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := 0; i < rounds*4; i++ {
			height := uint64((i*7)%rounds + 1)
			d.FinishedProposal(height, elacom.Uint256{byte(height)}, uint64(time.Now().Unix()))
		}
	}()

	writerWG.Wait()
	close(done)
	readerWG.Wait()

	// The watermark can only be the maximum that was ever offered, and every height
	// was offered at least once, so it ends at rounds.
	if got := d.GetFinishedHeight(); got != rounds {
		t.Fatalf("finished height is %d after every height was proposed, want %d", got, rounds)
	}
}
