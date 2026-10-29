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
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"math/big"

	elaaccount "github.com/elastos/Elastos.ELA/account"
	"github.com/elastos/Elastos.ELA/core/types/interfaces"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	elacrypto "github.com/elastos/Elastos.ELA/crypto"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
)

// This file replaces Elastos' dpos/account.dAccount signing methods.
//
// The upstream implementation builds its ecdsa.PrivateKey like this
// (Elastos.ELA/crypto/crypto.go:113):
//
//	privateKey := new(ecdsa.PrivateKey)
//	privateKey.Curve = DefaultCurve
//	privateKey.D = big.NewInt(0)
//	privateKey.D.SetBytes(priKey)
//	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
//
// Note what is missing: PublicKey.X and PublicKey.Y are never set. In Go 1.20,
// which is what pgp-chain compiles with, crypto/ecdsa.Sign only ever consulted
// D and the curve, so the omission was harmless. Since Go 1.24, crypto/ecdsa
// routes P-256 through the FIPS 186-5 module, and privateKeyToFIPS dereferences
// PublicKey.X on the way in. With X == nil that is a nil-pointer panic:
//
//	crypto/ecdsa.privateKeyToFIPS(...)
//	  crypto/ecdsa.pointFromAffine({...}, 0x0, 0x0)   <- X is nil
//	math/big.(*Int).Sign(...)
//	[signal SIGSEGV: segmentation violation ...]
//
// bsc requires Go 1.25 (geth 1.16), so the upstream path cannot be used for
// signing at all: the first proposal this node tries to sign takes the process
// down. The read-only replay path never signs, which is why this went unnoticed
// until a round was actually driven.
//
// The fix is local and small: populate the public key from the private key, then
// sign with the standard library exactly as upstream did. The output is the same
// 64 byte r||s encoding in the same P-256 curve, so a signature produced here is
// accepted by crypto.Verify on any node, including the ones still running
// pgp-chain on Go 1.20. The bytes are ECDSA, not an Elastos invention; only the
// encoding convention (two 32 byte big-endian halves) has to be matched, and it
// is matched below.
//
// Do not "simplify" this by calling elacrypto.Sign: it is the panicking function.

// p256Account is daccount.Account with a Go 1.25 safe signing implementation.
//
// Everything except signing is delegated to the embedded Elastos account, so the
// public key encoding, the program hash and the address stay exactly what the
// rest of Elastos expects.
type p256Account struct {
	*elaaccount.Account
	publicKey []byte
}

// newP256Account wraps an Elastos account.
//
// The compressed public key is computed here rather than lazily because it is
// read on every round: it identifies the proposer in a proposal, in a vote, and
// in the producer set comparison that decides whether this node is on duty.
func newP256Account(account *elaaccount.Account) (daccount.Account, error) {
	publicKey, err := account.PublicKey.EncodePoint(true)
	if err != nil {
		return nil, err
	}
	return &p256Account{Account: account, publicKey: publicKey}, nil
}

func (a *p256Account) PublicKey() *elacrypto.PublicKey { return a.Account.PublicKey }

func (a *p256Account) PublicKeyBytes() []byte { return a.publicKey }

func (a *p256Account) SignProposal(proposal *payload.DPOSProposal) ([]byte, error) {
	return signP256(a.PrivateKey, proposal.Data())
}

func (a *p256Account) SignVote(vote *payload.DPOSProposalVote) ([]byte, error) {
	return signP256(a.PrivateKey, vote.Data())
}

// Sign returns nil on failure, which is the upstream contract: the only caller
// treats a nil signature as a reason to drop the message.
func (a *p256Account) Sign(data []byte) []byte {
	signature, err := signP256(a.PrivateKey, data)
	if err != nil {
		Error("cannot sign", "err", err)
		return nil
	}
	return signature
}

func (a *p256Account) SignTx(tx interfaces.Transaction) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := tx.SerializeUnsigned(buf); err != nil {
		return nil, err
	}
	return signP256(a.PrivateKey, buf.Bytes())
}

func (a *p256Account) DecryptAddr(cipher []byte) (string, error) {
	data, err := elacrypto.Decrypt(a.PrivateKey, cipher)
	return string(data), err
}

// signP256 produces Elastos' 64 byte r||s signature over sha256(data).
//
// The digest is hashed here rather than by the caller because that is what
// elacrypto.Sign does and what elacrypto.Verify expects: every proposal and vote
// signature on the chain is sha256 over the object's own serialization.
func signP256(priKey, data []byte) ([]byte, error) {
	digest := sha256.Sum256(data)

	privateKey := &ecdsa.PrivateKey{D: new(big.Int).SetBytes(priKey)}
	privateKey.PublicKey.Curve = elacrypto.DefaultCurve
	privateKey.PublicKey.X, privateKey.PublicKey.Y =
		elacrypto.DefaultCurve.ScalarBaseMult(priKey)

	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
	if err != nil {
		return nil, err
	}

	// Right aligned, big endian, r then s. Written this way rather than with
	// r.FillBytes so the encoding stays visibly the same as upstream's, whose
	// slices are indexed by SignerLength/SignatureLength.
	signature := make([]byte, elacrypto.SignatureLength)
	copy(signature[elacrypto.SignerLength-len(r.Bytes()):], r.Bytes())
	copy(signature[elacrypto.SignatureLength-len(s.Bytes()):], s.Bytes())
	return signature, nil
}
