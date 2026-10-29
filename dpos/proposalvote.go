// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/proposalvote.go.

package dpos

import (
	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/dpos/account"
)

// StartVote signs an accept (or reject) vote for a proposal.
//
// The sponsor votes for its own proposal: StartVote identifies the actor with
// ac.PublicKeyBytes(), the same call StartProposal uses, so the proposer is
// always one of the votes that reach the threshold. consensus/pgp's
// checkConfirm counts it, which is what makes an 8-of-12 confirm reachable
// rather than needing 9.
func StartVote(ProposalHash *elacom.Uint256, isAcceipt bool, ac account.Account) (*payload.DPOSProposalVote, error) {
	Info("[StartVote] start", "proposal hash", ProposalHash.String())
	defer Info("[StartVote] end")

	vote := &payload.DPOSProposalVote{
		ProposalHash: *ProposalHash,
		Accept:       isAcceipt,
		Signer:       ac.PublicKeyBytes(),
	}

	sign, err := ac.SignVote(vote)
	if err != nil {
		return nil, err
	}
	vote.Sign = sign

	return vote, nil
}

// CheckVote verifies one vote's signature.
func CheckVote(vote *payload.DPOSProposalVote) error {
	pk, err := crypto.DecodePoint(vote.Signer)
	if err != nil {
		Error("[CheckVote] decode signer "+"error, details: ", err)
		return err
	}

	if err := crypto.Verify(*pk, vote.Data(), vote.Sign); err != nil {
		Error("[CheckVote] sign verify "+"error, details: ", err)
		return err
	}
	return nil
}
