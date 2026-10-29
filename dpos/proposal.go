// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/proposal.go.

package dpos

import (
	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/dpos/account"
)

// StartProposal signs a proposal for blockHash at the given view offset. It is
// the producing half of the round: only the on-duty arbiter calls it, from
// consensus/pbft's Seal.
func StartProposal(ac account.Account, blockHash elacom.Uint256, viewOffset uint32) (*payload.DPOSProposal, error) {
	Info("[StartProposal] start")
	defer Info("[StartProposal] end")
	proposal := &payload.DPOSProposal{Sponsor: ac.PublicKeyBytes(),
		BlockHash: blockHash, ViewOffset: viewOffset}
	sign, err := ac.SignProposal(proposal)
	if err != nil {
		Error("[StartProposal] start proposal failed:", err.Error())
		return nil, err
	}
	Info("[StartProposal] hash:", proposal.Hash().String())
	proposal.Sign = sign

	return proposal, nil
}

// CheckProposal verifies the sponsor's signature over a proposal.
//
// This is the per-object primitive the dispatcher needs while collecting votes.
// The whole-set check (membership + threshold) lives in consensus/pgp, which is
// what the header verifier also uses, so there is exactly one implementation of
// "is this confirm final".
func CheckProposal(proposal *payload.DPOSProposal) error {
	pk, err := crypto.DecodePoint(proposal.Sponsor)
	if err != nil {
		Error("[CheckProposal] decode signer "+"error, details: ", err)
		return err
	}

	if err := crypto.Verify(*pk, proposal.Data(), proposal.Sign); err != nil {
		Error("[CheckProposal] sign verify "+"error, details: ", err)
		return err
	}
	return nil
}
