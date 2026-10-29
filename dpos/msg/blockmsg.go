// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/msg/blockmsg.go.

package msg

import (
	"io"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/elanet/pact"
	"github.com/elastos/Elastos.ELA/p2p"
)

// Ensure BlockMsg implement p2p.Message interface.
var _ p2p.Message = (*BlockMsg)(nil)

// BlockMsg is an opaque serialized block. The payload is not decoded here: the
// DPoS layer hands the bytes back to consensus/pbft, which deserializes them
// through core/types, keeping the cgo dependency out of dpos.
type BlockMsg struct {
	data []byte
}

func NewBlockMsg(data []byte) *BlockMsg {
	return &BlockMsg{data: data}
}

func (msg *BlockMsg) CMD() string {
	return p2p.CmdBlock
}

func (msg *BlockMsg) MaxLength() uint32 {
	return pact.MaxBlockContextSize
}

func (msg *BlockMsg) Serialize(w io.Writer) error {
	return common.WriteVarBytes(w, msg.data)
}

func (msg *BlockMsg) Deserialize(r io.Reader) error {
	data, err := common.ReadVarBytes(r, msg.MaxLength(), "")
	msg.data = data
	return err
}

func (msg *BlockMsg) GetData() []byte {
	return msg.data
}
