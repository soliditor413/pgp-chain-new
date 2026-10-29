// Copyright (c) 2017-2020 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/chainbridge-core/dpos_msg/{darbiter,requirearbiter,
// reqarbiterssignature,feedbackArbitersSignature}.go.

package msg

import (
	"bytes"
	"io"
	"time"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/p2p"
)

// This file holds the four chainbridge (layer2) DPoS messages. The business
// logic behind them is NOT ported: no layer2 listener, no arbiter signature
// broker. They stay because dpos/network.go's createMessage must be able to
// decode every command a pgp-chain 1.9.7 peer can send; an unrecognised command
// there is a connection-fatal error.
//
// See docs/design/pbft-port-design.md D10.

const (
	// maxCipherLength indicates the max length of the address cipher.
	maxCipherLength = 256
)

// Ensure the four implement p2p.Message interface.
var (
	_ p2p.Message = (*DArbiter)(nil)
	_ p2p.Message = (*RequireArbiter)(nil)
	_ p2p.Message = (*RequireArbitersSignature)(nil)
	_ p2p.Message = (*FeedBackArbitersSignature)(nil)
)

// DArbiter announces one arbiter's encrypted network address.
type DArbiter struct {
	// The peer ID indicates who's address it is.
	PID [33]byte

	// Timestamp represents the time when this message created.
	Timestamp time.Time

	// Which peer ID is used to encode the address cipher.
	Encode [33]byte

	// The encrypted network address using the encode peer ID.
	Cipher []byte

	// Signature of the encode peer ID and cipher to proof the sender itself.
	Signature []byte

	// is current consensus producers
	IsCurrent bool
}

func (m *DArbiter) CMD() string {
	return CmdDArbiter
}

func (m *DArbiter) MaxLength() uint32 {
	return 388 // 33+33+256+65 + 1
}

func (m *DArbiter) Serialize(w io.Writer) error {
	var timestamp = m.Timestamp.Unix()
	err := common.WriteElements(w, m.PID, timestamp, m.Encode)
	if err != nil {
		return err
	}

	if err := common.WriteVarBytes(w, m.Cipher); err != nil {
		return err
	}

	err = common.WriteVarBytes(w, m.Signature)
	if err != nil {
		return err
	}
	if m.IsCurrent == true {
		err = common.WriteUint8(w, 1)
	} else {
		err = common.WriteUint8(w, 0)
	}
	return err
}

func (m *DArbiter) Deserialize(r io.Reader) error {
	var timestamp int64
	err := common.ReadElements(r, &m.PID, &timestamp, &m.Encode)
	if err != nil {
		return err
	}
	m.Timestamp = time.Unix(timestamp, 0)

	m.Cipher, err = common.ReadVarBytes(r, maxCipherLength, "DArbiter.Cipher")
	if err != nil {
		return err
	}

	m.Signature, err = common.ReadVarBytes(r, crypto.SignatureLength,
		"DArbiter.Signature")

	res, err := common.ReadUint8(r)
	if err != nil {
		return err
	}
	if res == 1 {
		m.IsCurrent = true
	} else {
		m.IsCurrent = false
	}
	return err
}

func (m *DArbiter) Data() []byte {
	b := new(bytes.Buffer)
	var timestamp = m.Timestamp.Unix()
	common.WriteElements(b, timestamp, m.Encode)
	common.WriteVarBytes(b, m.Cipher)
	if m.IsCurrent == true {
		common.WriteUint8(b, 1)
	} else {
		common.WriteUint8(b, 0)
	}
	return b.Bytes()
}

// RequireArbiter asks a peer to (re)announce its arbiter address.
type RequireArbiter struct {
	PID       [33]byte
	IsCurrent bool
}

func (msg *RequireArbiter) CMD() string {
	return CmdRequireArbiters
}

func (msg *RequireArbiter) MaxLength() uint32 {
	return 35
}

func (msg *RequireArbiter) Serialize(w io.Writer) error {
	_, err := w.Write(msg.PID[:])
	if err != nil {
		return err
	}
	if msg.IsCurrent == true {
		err = common.WriteUint8(w, 1)
	} else {
		err = common.WriteUint8(w, 0)
	}
	return err
}

func (msg *RequireArbiter) Deserialize(r io.Reader) error {
	pid := make([]byte, 33)
	_, err := r.Read(pid)
	if err != nil {
		return err
	}
	copy(msg.PID[:], pid)

	res, err := common.ReadUint8(r)
	if err != nil {
		return err
	}
	if res == 1 {
		msg.IsCurrent = true
	} else {
		msg.IsCurrent = false
	}
	return nil
}

// RequireArbitersSignature asks a peer for arbiter signatures.
type RequireArbitersSignature struct {
	PID          [33]byte
	ArbiterCount uint8
}

func (msg *RequireArbitersSignature) CMD() string {
	return CmdRequireArbitersSignature
}

func (msg *RequireArbitersSignature) MaxLength() uint32 {
	return 35
}

func (msg *RequireArbitersSignature) Serialize(w io.Writer) error {
	_, err := w.Write(msg.PID[:])
	if err != nil {
		return err
	}
	err = common.WriteUint8(w, msg.ArbiterCount)
	return err
}

func (msg *RequireArbitersSignature) Deserialize(r io.Reader) error {
	pid := make([]byte, 33)
	_, err := r.Read(pid)
	if err != nil {
		return err
	}
	copy(msg.PID[:], pid)

	msg.ArbiterCount, err = common.ReadUint8(r)
	return err
}

// FeedBackArbitersSignature answers RequireArbitersSignature.
type FeedBackArbitersSignature struct {
	Producer  []byte
	Signature []byte
}

func (m *FeedBackArbitersSignature) CMD() string {
	return CmdFeedbackArbiterSignature
}

func (m *FeedBackArbitersSignature) MaxLength() uint32 {
	return 33 + 65 + 2
}

func (m *FeedBackArbitersSignature) Serialize(w io.Writer) error {
	if err := common.WriteVarBytes(w, m.Producer); err != nil {
		return err
	}
	if err := common.WriteVarBytes(w, m.Signature); err != nil {
		return err
	}

	return nil
}

func (m *FeedBackArbitersSignature) Deserialize(r io.Reader) error {
	signer, err := common.ReadVarBytes(r, 33, "Signer")
	if err != nil {
		return err
	}
	m.Producer = signer

	sig, err := common.ReadVarBytes(r, 65, "Signature")
	if err != nil {
		return err
	}
	m.Signature = sig

	return nil
}
