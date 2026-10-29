// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/msg/smallcrotxmsg.go.

package msg

import (
	"io"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/elanet/pact"
	"github.com/elastos/Elastos.ELA/p2p"
)

// Ensure SmallCroTx implement p2p.Message interface.
var _ p2p.Message = (*SmallCroTx)(nil)

// SmallCroTx relays a small cross chain transaction.
//
// The business side (smallcrosstx) is not ported; this codec exists so the
// command stays decodable and a peer carrying gateway traffic is answered with
// a no-op instead of a protocol error. See docs/design/pbft-port-design.md D10.
type SmallCroTx struct {
	signature string
	rawTx     string
}

func NewSmallCroTx(sig string, rawtx string) *SmallCroTx {
	return &SmallCroTx{signature: sig, rawTx: rawtx}
}

func (msg *SmallCroTx) CMD() string {
	return CmdSmallCroTx
}

func (msg *SmallCroTx) MaxLength() uint32 {
	return pact.MaxBlockContextSize
}

func (msg *SmallCroTx) Serialize(w io.Writer) error {
	err := common.WriteVarString(w, msg.signature)
	if err != nil {
		return err
	}
	err = common.WriteVarString(w, msg.rawTx)
	if err != nil {
		return err
	}
	return nil
}

func (msg *SmallCroTx) Deserialize(r io.Reader) error {
	data, err := common.ReadVarString(r)
	if err != nil {
		return err
	}
	msg.signature = data

	data, err = common.ReadVarString(r)
	if err != nil {
		return err
	}
	msg.rawTx = data
	return err
}

func (msg *SmallCroTx) GetSignature() string {
	return msg.signature
}

func (msg *SmallCroTx) GetRawTx() string {
	return msg.rawTx
}
