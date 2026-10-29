// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/message.go.

// Package elamsg is the devp2p envelope that carries DPoS traffic.
//
// It lives in its own leaf package for one structural reason: two packages need
// the type but must not depend on each other.
//
//   - eth/protocols/eth64 encodes and decodes it on the wire (message code
//     ELAMSG = 0x08);
//   - dpos decodes the payload inside Msg and drives the address routing.
//
// If the type lived in dpos, eth64 would drag the whole Elastos DPoS p2p stack
// into the protocol layer. dpos re-exports it as a type alias, so
// *dpos.ElaMsg and *elamsg.ElaMsg are the same type and the IPeer interface can
// keep its pgp-chain signature.
package elamsg

import (
	"io"

	"github.com/elastos/Elastos.ELA/common"
)

// ElaMsgType discriminates the payload inside ElaMsg.
type ElaMsgType byte

const (
	// Inv announces that the sender knows an address record; GetData requests
	// the record; DAddr is the record itself.
	Inv ElaMsgType = iota
	GetData
	DAddr
)

// ElaMsg is the ELAMSG payload.
//
// IMPORTANT: this struct is RLP encoded on the devp2p wire by eth/protocols/eth64
// (pgp-chain's eth/handler.go decodes it with msg.Decode). Field names and order
// are part of the wire format. Serialize/Deserialize below are a *different*
// encoding, used only when an ElaMsg is relayed inside the Elastos DPoS p2p
// channel as an opaque byte string.
type ElaMsg struct {
	Type ElaMsgType
	Msg  []byte
}

func (em *ElaMsg) Serialize(w io.Writer) error {
	if err := common.WriteUint8(w, uint8(em.Type)); err != nil {
		return err
	}

	return common.WriteVarBytes(w, em.Msg)
}

func (em *ElaMsg) Deserialize(r io.Reader) error {
	msgType, err := common.ReadUint8(r)
	if err != nil {
		return err
	}
	em.Type = ElaMsgType(msgType)

	newMsg, err := common.ReadVarBytes(r, common.MaxVarStringLength, "ElaMsg")
	if err != nil {
		return err
	}
	em.Msg = newMsg

	return nil
}
