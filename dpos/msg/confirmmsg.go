// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/msg/confirmmsg.go.

package msg

import (
	"bytes"
	"io"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
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

func (msg *ConfirmMsg) MaxLength() uint32 {
	buffer := new(bytes.Buffer)
	if err := msg.Serialize(buffer); err != nil {
		return 0
	}
	return uint32(buffer.Len())
}

func (msg *ConfirmMsg) Serialize(w io.Writer) error {
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
