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

	elaaccount "github.com/elastos/Elastos.ELA/account"
	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	elacrypto "github.com/elastos/Elastos.ELA/crypto"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
)

// newTestAccount returns a freshly generated arbiter account.
func newTestAccount(t *testing.T) (daccount.Account, *elaaccount.Account) {
	t.Helper()

	base, err := elaaccount.NewAccount()
	if err != nil {
		t.Fatalf("cannot generate a key: %v", err)
	}
	account, err := NewAccountFrom(base)
	if err != nil {
		t.Fatalf("cannot wrap the account: %v", err)
	}
	return account, base
}

// TestSignProposalVerifiesWithElastosCrypto is the regression test for the
// reason this package has its own signer.
//
// Upstream's elacrypto.Sign builds an ecdsa.PrivateKey with D set and the public
// key left nil, which crypto/ecdsa panics on from Go 1.24 onwards. Before this
// test existed, nothing in the tree ever signed anything: the replay path only
// verifies, and the golden vectors were recorded elsewhere. The first real round
// therefore took the process down with a nil-pointer dereference inside
// crypto/ecdsa. See signer.go.
//
// The assertion is not "the bytes look right" but "the same crypto.Verify the
// verifier runs accepts what this signer produced", because the signature has to
// be accepted by nodes that never link this package.
func TestSignProposalVerifiesWithElastosCrypto(t *testing.T) {
	account, _ := newTestAccount(t)

	proposal := &payload.DPOSProposal{
		Sponsor:    account.PublicKeyBytes(),
		BlockHash:  elacom.Uint256{0xde, 0xad, 0xbe, 0xef},
		ViewOffset: 3,
	}
	signature, err := account.SignProposal(proposal)
	if err != nil {
		t.Fatalf("SignProposal failed: %v", err)
	}
	if len(signature) != elacrypto.SignatureLength {
		t.Fatalf("signature is %d bytes, want %d", len(signature), elacrypto.SignatureLength)
	}

	publicKey, err := elacrypto.DecodePoint(account.PublicKeyBytes())
	if err != nil {
		t.Fatalf("the account's own public key does not decode: %v", err)
	}
	if err := elacrypto.Verify(*publicKey, proposal.Data(), signature); err != nil {
		t.Fatalf("crypto.Verify rejected a signature this signer produced: %v", err)
	}

	// Negative control: a signature that verifies no matter what it covers would
	// make the test above meaningless.
	tampered := append([]byte(nil), signature...)
	tampered[len(tampered)-1] ^= 0x01
	if err := elacrypto.Verify(*publicKey, proposal.Data(), tampered); err == nil {
		t.Fatal("crypto.Verify accepted a corrupted signature")
	}
}

// TestSignVoteVerifiesWithElastosCrypto covers the other signing entry point: a
// vote commits to a different structure than a proposal, and the two are signed
// through separate methods.
func TestSignVoteVerifiesWithElastosCrypto(t *testing.T) {
	account, _ := newTestAccount(t)

	vote := &payload.DPOSProposalVote{
		ProposalHash: elacom.Uint256{0x01, 0x02},
		Accept:       true,
		Signer:       account.PublicKeyBytes(),
	}
	signature, err := account.SignVote(vote)
	if err != nil {
		t.Fatalf("SignVote failed: %v", err)
	}

	publicKey, err := elacrypto.DecodePoint(account.PublicKeyBytes())
	if err != nil {
		t.Fatalf("the account's own public key does not decode: %v", err)
	}
	if err := elacrypto.Verify(*publicKey, vote.Data(), signature); err != nil {
		t.Fatalf("crypto.Verify rejected a vote signature: %v", err)
	}
}

// TestCompressedPublicKeyMatchesTheSigningKey guards the identity that ties a
// vote to its signer.
//
// The producer set compares 33 byte compressed keys, the confirm check compares
// the same 33 bytes, and the signature is verified against whatever those bytes
// decode to. If PublicKeyBytes and the signing key ever referred to different
// keys, every signature would verify locally and be rejected by every peer.
func TestCompressedPublicKeyMatchesTheSigningKey(t *testing.T) {
	account, base := newTestAccount(t)

	encoded, err := base.PublicKey.EncodePoint(true)
	if err != nil {
		t.Fatalf("cannot compress the public key: %v", err)
	}
	if string(account.PublicKeyBytes()) != string(encoded) {
		t.Fatal("PublicKeyBytes does not match the account's compressed public key")
	}

	signature := account.Sign([]byte("pbft"))
	if signature == nil {
		t.Fatal("Sign returned nil")
	}
	publicKey, err := elacrypto.DecodePoint(account.PublicKeyBytes())
	if err != nil {
		t.Fatalf("cannot decode the compressed key: %v", err)
	}
	if err := elacrypto.Verify(*publicKey, []byte("pbft"), signature); err != nil {
		t.Fatalf("the compressed key does not verify this account's signature: %v", err)
	}
}
