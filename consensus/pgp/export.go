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

package pgp

import (
	"github.com/elastos/Elastos.ELA/core/types/payload"
)

// This file is the only door between the read-only replay engine in this
// package and the producing engine in consensus/pbft. It adds no behaviour: it
// re-exports the confirm plumbing that already exists here so the two engines
// cannot drift into two implementations of the same consensus bytes.
//
// The rule that makes this safe is the one on the other side of the door:
// consensus/pbft must never serialize a confirm by hand, it must call
// (*payload.Confirm).Serialize, exactly what the verifier here decodes. Because
// both sides then go through the Elastos codec, "the sealer accepts its own
// block" is true by construction rather than by test coverage.
//
// Everything exported below is documented where it is implemented; see
// confirm.go.

// DecodeConfirm parses the serialized payload.Confirm that occupies
// header.Extra.
func DecodeConfirm(extra []byte) (*payload.Confirm, error) {
	return decodeConfirm(extra)
}

// CheckConfirm verifies a confirm against a producer set: sponsor membership,
// proposal and vote signatures, vote deduplication and the threshold.
func CheckConfirm(confirm *payload.Confirm, producers [][]byte, minSignCount int) error {
	return checkConfirm(confirm, producers, minSignCount)
}

// VerifyConfirmAt checks a confirm against the producer set that applies to the
// given ELA height: the configured list when elaHeight is zero, the SPV arbiter
// set otherwise.
//
// This is the same function a header goes through in verifySeal. consensus/pbft
// calls it so that a sealer accepting its own block and a follower rejecting a
// forged one run identical code, including the SPV gating: a node whose ELA sync
// is behind rejects rather than guesses (ErrSealSPVBehind).
func (e *Engine) VerifyConfirmAt(confirm *payload.Confirm, elaHeight uint64) error {
	return e.verifyConfirm(confirm, elaHeight)
}

// MajorityCount is the DPoS threshold for a producer set of the given size,
// i.e. floor(totalSigner * 2 / 3), with a floor of one whenever the set is
// non-empty (F-14).
//
// It is exported because the producing side has to know how many votes make a
// confirm final *before* it has one, and computing that number differently on
// either side would produce confirms the verifier rejects. The floor of one is
// what keeps a single-producer devnet honest: without it the producer side needs
// no votes at all and the verifier checks none.
func MajorityCount(totalSigner int) int {
	return legacyMajorityCount(totalSigner)
}

// DecodeProducers turns the configured hex public keys into the byte slices the
// confirm check compares against. Empty entries are preserved as nil seats.
func DecodeProducers(hexKeys []string) ([][]byte, error) {
	return decodeProducers(hexKeys)
}

const (
	// LegacyBlockPeriod is the minimum distance in seconds between two legacy
	// block timestamps. The producing side must use this exact value: it is the
	// lower bound verifyHeader enforces.
	LegacyBlockPeriod = legacyBlockPeriod

	// ProposalWireLen and VoteWireLen are the serialized sizes of the two parts
	// of a confirm.
	ProposalWireLen = legacyProposalWireLen
	VoteWireLen     = legacyVoteWireLen

	// ConfirmMinLen is the shortest serializable confirm: a proposal plus the
	// vote count word, no votes.
	ConfirmMinLen = legacyConfirmMinLen
)

// ErrProductionNotSupported is returned by the block production entry points of
// the read-only engine. consensus/pbft overrides them; the sentinel is exported
// so the two engines report the same error for the same condition and a
// misconfigured node can be told apart from a bug.
var ErrProductionNotSupported = errProductionNotSupported
