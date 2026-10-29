// Copyright (c) 2017-2020 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/msg/responseconsensus.go.

package msg

import (
	"io"

	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
)

// DefaultResponseConsensusMessageDataSize is the cap the Elastos p2p layer
// applies to this message. It is deliberately huge (80 MB): a consensus state
// carries every pending proposal and vote.
const DefaultResponseConsensusMessageDataSize = 8000000 * 10

type ResponseConsensus struct {
	Consensus ConsensusStatus
}

func (msg *ResponseConsensus) CMD() string {
	return emsg.CmdResponseConsensus
}

func (msg *ResponseConsensus) MaxLength() uint32 {
	return DefaultResponseConsensusMessageDataSize
}

func (msg *ResponseConsensus) Serialize(w io.Writer) error {
	return msg.Consensus.Serialize(w)
}

func (msg *ResponseConsensus) Deserialize(r io.Reader) error {
	return msg.Consensus.Deserialize(r)
}
