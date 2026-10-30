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

import (
	"testing"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
)

// stashPrecocious puts a proposal in the stash the way ProcessProposal does when
// it is handed one from a view ahead of ours.
func stashPrecocious(d *Dispatcher, proposal *payload.DPOSProposal) {
	d.mu.Lock()
	d.precociousProposals[proposal.Hash()] = proposal
	d.mu.Unlock()
}

func stashLen(d *Dispatcher) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.precociousProposals)
}

// setView drives the view to a known (status, offset) pair. RecoverFromStatus is
// the only exported setter that writes both, and the pair is the whole input of
// UpdatePrecociousProposals, so the fixture cannot borrow a getter to set them.
func setView(t *testing.T, d *Dispatcher, status uint32, offset uint32) {
	t.Helper()
	d.GetConsensusView().RecoverFromStatus(status, offset, time.Unix(1_700_000_000, 0), 1, 0)
}

// TestUpdatePrecociousProposalsReleasesOnlyTheCurrentView is the contract that
// makes a drifting set converge.
//
// A proposal from a higher view offset is stashed by ProcessProposal instead of
// being rejected, because counting its votes against the current view is how a
// quorum gets forged. Something has to take it out again once the views match, and
// in this port nothing did: the devnet stashed every proposal on three nodes and
// timed out at height 1, with the proposer holding one vote of the two it needed.
func TestUpdatePrecociousProposalsReleasesOnlyTheCurrentView(t *testing.T) {
	d := newRecoveryNilFixture(t)
	proposal := &payload.DPOSProposal{
		Sponsor:    []byte{0x02, 0x01},
		BlockHash:  elacom.Uint256{1},
		ViewOffset: 2,
	}
	stashPrecocious(d, proposal)

	cases := []struct {
		name   string
		status uint32
		offset uint32
		want   bool
		why    string
	}{
		{
			name:   "still behind, not running",
			status: ConsensusReady,
			offset: 1,
			want:   false,
			why:    "a view that is not running owns no round to replay into",
		},
		{
			name:   "still behind, running",
			status: ConsensusRunning,
			offset: 0,
			want:   false,
			why:    "the offset is what the votes are counted against; one short is a different round",
		},
		{
			name:   "ahead of the proposal",
			status: ConsensusRunning,
			offset: 3,
			want:   false,
			why:    "the proposal belongs to a view this node has already left",
		},
		{
			name:   "caught up",
			status: ConsensusRunning,
			offset: 2,
			want:   true,
			why:    "the proposal is now current and its vote is the one the quorum is missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setView(t, d, tc.status, tc.offset)

			got := d.UpdatePrecociousProposals()
			if tc.want {
				if got != proposal {
					t.Fatalf("returned %v, want the stashed proposal: %s", got, tc.why)
				}
				// Handing it out twice would count the same votes twice, which is
				// exactly the quorum-forging this stash exists to prevent.
				if n := stashLen(d); n != 0 {
					t.Fatalf("the proposal is still stashed after being released (stash has %d)", n)
				}
				if again := d.UpdatePrecociousProposals(); again != nil {
					t.Fatalf("the same proposal was released twice")
				}
				return
			}
			if got != nil {
				t.Fatalf("returned a proposal, want none: %s", tc.why)
			}
			if n := stashLen(d); n != 1 {
				t.Fatalf("the stash holds %d proposals, want it kept at 1", n)
			}
		})
	}
}

// TestUpdatePrecociousProposalsWithoutAStashIsNil keeps the common case cheap and
// silent: every view change calls this, and almost none of them have anything to
// replay.
func TestUpdatePrecociousProposalsWithoutAStashIsNil(t *testing.T) {
	d := newRecoveryNilFixture(t)
	setView(t, d, ConsensusRunning, 0)
	if got := d.UpdatePrecociousProposals(); got != nil {
		t.Fatalf("returned %v from an empty stash", got)
	}
}
