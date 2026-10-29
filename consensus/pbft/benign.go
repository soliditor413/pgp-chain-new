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

package pbft

import (
	"errors"

	"github.com/ethereum/go-ethereum/consensus/pgp"
)

// This file answers one question for the miner: is this error news?
//
// The miner calls Prepare and Seal once per attempt, i.e. constantly, and almost
// every attempt legitimately produces nothing. Logging those at error level is
// not merely noisy: it buries the one line an operator actually needs, and it
// teaches everyone to ignore the producer's log. The classification lives here
// rather than in miner so that the sentinels and the judgement about which of
// them are normal stay in the package that owns them. See
// docs/design/pbft-port-design.md M2.

// IsBenignPrepareError reports whether a Prepare error is a normal "not my turn /
// not ready" outcome rather than a fault. 11 of 12 producers hit one of these on
// every single height, so logging them at Error level floods the log at ~20
// lines/minute per node.
//
// consensus.ErrUnknownAncestor is deliberately NOT on the list, even though
// Prepare returns it from two places. One is a genesis header (header.Number==0),
// which a work item can never be. The other is "the parent is not in the chain",
// and that is a fault: a producer builds on chain.CurrentBlock(), so its parent is
// always present, and its absence means a storage or chain-integrity problem that
// must not be reported at Debug. IsBenignSealError does not classify it either --
// the two functions have to agree about what "benign" means or the same error gets
// two different levels depending on which call site saw it.
func IsBenignPrepareError(err error) bool {
	return anyOf(err,
		ErrSignerNotOnduty,            // a peer's turn, not ours
		ErrUnauthorizedSigner,         // this node is a follower, by design
		ErrWaitRecoverStatus,          // still waiting for the consensus-status exchange
		ErrAlreadyConfirmedBlock,      // the height is already final
		ErrConsensusIsRunning,         // a round for this height is already in flight
		ErrNotProducer,                // production enabled but no key loaded
		ErrEpochTransitionPending,     // the producer set is rotating
		pgp.ErrProductionNotSupported, // --pbft.enable was not given
	)
}

// IsBenignSealError reports whether a Seal error means "this round did not
// produce a block" rather than "something is broken".
//
// ErrRoundAborted is on the list because the round is expected to fail most of
// the time on a healthy set: only the on-duty producer's round can complete.
func IsBenignSealError(err error) bool {
	return anyOf(err,
		ErrRoundAborted,
		ErrSignerNotOnduty,
		ErrUnauthorizedSigner,
		ErrWaitRecoverStatus,
		ErrAlreadyConfirmedBlock,
		ErrNotProducer,
		ErrEpochTransitionPending,
		pgp.ErrProductionNotSupported,
	)
}

// anyOf reports whether err matches one of the sentinels. It is errors.Is based,
// so an engine that wraps a sentinel (router does not, but a future one might)
// still classifies correctly, and a nil err matches nothing.
func anyOf(err error, targets ...error) bool {
	for _, target := range targets {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
