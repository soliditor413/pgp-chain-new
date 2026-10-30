// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/msg/confirmmsg.go.

package msg

import (
	"errors"
	"io"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/elanet/pact"
	"github.com/elastos/Elastos.ELA/p2p"
)

// Ensure ConfirmMsg implement p2p.Message interface.
var _ p2p.Message = (*ConfirmMsg)(nil)

// ConfirmMsg carries a finalised confirm plus the pgp height it belongs to.
//
// Height is what lets a receiver match the confirm against a block it may not
// have imported yet; the confirm itself only commits to the proposal.
type ConfirmMsg struct {
	Confirm *payload.Confirm
	Height  uint64
}

func NewConfirmMsg(confirm *payload.Confirm, height uint64) *ConfirmMsg {
	return &ConfirmMsg{Confirm: confirm, Height: height}
}

func (msg *ConfirmMsg) CMD() string {
	return CmdConfirm
}

// maxConfirmMessageDataSize bounds a confirm on the wire.
//
// It is a constant, like every other message in this package, because MaxLength is
// asked on an EMPTY message: the p2p layer creates the message from the command,
// asks it how large it may be, and only then reads and deserialises the payload
// (Elastos.ELA/p2p/peer/peer.go:583, before Deserialize). Answering from
// len(Serialize(self)) therefore serialises a nil *payload.Confirm, and that is a
// nil dereference on the goroutine that reads from a peer -- the process dies on
// the first confirm anybody sends it. Measured, not feared: on the devnet the
// producer sealed height 1 and the other three nodes panicked inside
// payload.(*Confirm).Serialize with Confirm == nil.
//
// Upstream has the identical defect (pgp-chain/dpos/msg/confirmmsg.go) and gets away
// with it because its readers are peers that have already exchanged the message once
// on a live chain; a freshly started set sends the first confirm while everybody's
// factory product is still empty.
//
// The value is the same bound the other block-carrying messages use
// (BlockMsg.MaxLength, and the reader limit ELA puts on its own payloads), so a
// confirm can never be refused for being larger than a block context.
var maxConfirmMessageDataSize = pact.MaxBlockContextSize

func (msg *ConfirmMsg) MaxLength() uint32 {
	return maxConfirmMessageDataSize
}

// Serialize refuses a nil confirm instead of dereferencing it. MaxLength no longer
// calls this on an empty message, but Serialize is on two other paths -- the send
// side and any future caller -- and a nil payload there is a bug in the caller, not
// a reason to take the process down.
func (msg *ConfirmMsg) Serialize(w io.Writer) error {
	if msg.Confirm == nil {
		return errors.New("confirm message carries no confirm")
	}
	err := msg.Confirm.Serialize(w)
	if err != nil {
		return err
	}
	if err := common.WriteUint64(w, msg.Height); err != nil {
		return err
	}

	return nil
}

func (msg *ConfirmMsg) Deserialize(r io.Reader) error {
	if msg.Confirm == nil {
		msg.Confirm = &payload.Confirm{}
	}
	err := msg.Confirm.Deserialize(r)
	if err != nil {
		return err
	}
	if msg.Height, err = common.ReadUint64(r); err != nil {
		return err
	}
	return nil
}
