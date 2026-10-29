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

import "errors"

var (
	// ErrSignerNotOnduty means this node is a producer but it is not this
	// height's turn to propose. It is the normal state of a healthy node 11
	// times out of 12 and must never be logged as an error.
	ErrSignerNotOnduty = errors.New("pbft: signer is not on duty")

	// ErrUnauthorizedSigner means the configured key is not in the producer set
	// at all, i.e. the node is a follower. Also normal, and also not an error.
	ErrUnauthorizedSigner = errors.New("pbft: signer is not a producer")

	// ErrWaitRecoverStatus means the node has not yet finished resynchronising
	// its consensus state with its peers after a restart.
	ErrWaitRecoverStatus = errors.New("pbft: waiting for consensus status recovery")

	// ErrAlreadyConfirmedBlock means the height already has a confirm, so
	// proposing again would be a double proposal at a finalised height.
	ErrAlreadyConfirmedBlock = errors.New("pbft: height is already confirmed")

	// ErrConsensusIsRunning means a round is already in progress for this
	// height; the caller should wait for it rather than start another.
	ErrConsensusIsRunning = errors.New("pbft: consensus is already running")

	// ErrEpochTransitionPending means the producer set is being rotated and
	// mining must pause until the new set is installed.
	ErrEpochTransitionPending = errors.New("pbft: epoch transition pending")

	// ErrNotProducer is returned by Seal when there is no local signing key.
	ErrNotProducer = errors.New("pbft: no signer configured")

	// ErrRoundAborted is returned by Seal when the round ran but did not agree
	// on a block: the proposal was rejected, the view deadline passed, the miner
	// interrupted us, or a confirm arrived that belonged to a different block.
	//
	// It exists because nil is not a usable answer here. miner/worker.go stores
	// the in-flight task (and its state.StateDB) in pendingTasks before calling
	// Seal and only cleans the entry up on a non-nil error; a nil return would
	// leak one StateDB per aborted round until the block height has advanced by
	// 11.
	ErrRoundAborted = errors.New("pbft: round aborted without a confirm")
)
