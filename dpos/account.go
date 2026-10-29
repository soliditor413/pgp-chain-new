// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/account.go.

package dpos

import (
	"github.com/elastos/Elastos.ELA/account"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
)

// GetDposAccount opens the arbiter keystore and wraps it in the Elastos DPoS
// account interface, i.e. the thing that signs proposals and votes.
//
// The wrapper is bsc's own (see signer.go), not daccount.New, because the
// upstream signing implementation panics on the Go version bsc builds with.
//
// pgp-chain's account.go also carries GetBridgeAccount, which derives an EVM
// secp256k1 keypair from the same keystore for the chainbridge layer2 signer.
// That has no caller here: layer2 is not ported (see
// docs/design/pbft-port-design.md, settled decisions). Do not reintroduce it
// without also porting chainbridge-core, or the key material would be unlocked
// for nothing.
func GetDposAccount(keystorePath string, password []byte) (daccount.Account, error) {
	client, err := account.Open(keystorePath, password)
	if err != nil {
		return nil, err
	}
	return newP256Account(client.GetMainAccount())
}

// NewAccountFrom wraps an already-open Elastos account in the DPoS account
// interface.
//
// It exists for callers that obtain the key without a keystore file -- tests, and
// a future signer service -- so that those callers cannot accidentally pick
// daccount.New and inherit the panicking signer.
func NewAccountFrom(account *account.Account) (daccount.Account, error) {
	return newP256Account(account)
}
