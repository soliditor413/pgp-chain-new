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

// Package eth64 implements the eth/64 wire protocol of pgp-chain, which is the
// only eth protocol version the pgp-chain mainnet speaks.
//
// This is a *separate* protocol implementation on purpose, not a reuse of
// eth/protocols/eth:
//
//   - eth/66 introduced request ids. pgp-chain's eth/64 messages are bare
//     structures (no RequestId field), so eth/protocols/eth's Peer, dispatcher
//     and Request/Response machinery cannot be reused: sending a request id to a
//     pgp peer is a decoding error on its side.
//   - the message code table differs. pgp-chain reserves 0x08 for ELAMSG (its
//     dpos message), while eth/68 uses 0x08 for NewPooledTransactionHashes, so an
//     unknown code coming from a pgp peer must be discarded, not treated as an
//     error (a single dpos broadcast would otherwise kill the connection).
//   - the pgp handshake is a single Status exchange without the eth/67+
//     UpgradeStatus handshake and without the snap/bsc extension barriers.
package eth64

import (
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// ETH64 is the eth protocol version spoken by pgp-chain mainnet.
const ETH64 = 64

// ProtocolName is the devp2p capability name.
const ProtocolName = "eth"

// protocolLengths is the number of message codes the protocol occupies.
//
// IMPORTANT: this must stay exactly 17. p2p/peer.go's matchProtocols assigns
// every subprotocol a message code window by accumulating `offset += Length`, so
// a wrong value here silently shifts every protocol registered after this one
// (snap, bsc, ...) and corrupts their message routing at runtime. Nothing in the
// code base validates this number.
var protocolLengths = map[uint]uint64{ETH64: 17}

// protocolMaxMsgSize is the maximum cap on the size of a protocol message,
// mirroring pgp-chain's 10 MiB limit.
const protocolMaxMsgSize = 10 * 1024 * 1024

// eth protocol message codes, taken verbatim from pgp-chain's eth/protocol.go.
// Note 0x08: pgp-chain uses it for ELAMSG, not for NewPooledTransactionHashes.
const (
	StatusMsg          = 0x00
	NewBlockHashesMsg  = 0x01
	TxMsg              = 0x02
	GetBlockHeadersMsg = 0x03
	BlockHeadersMsg    = 0x04
	GetBlockBodiesMsg  = 0x05
	BlockBodiesMsg     = 0x06
	NewBlockMsg        = 0x07
	ELAMSG             = 0x08
	GetNodeDataMsg     = 0x0d
	NodeDataMsg        = 0x0e
	GetReceiptsMsg     = 0x0f
	ReceiptsMsg        = 0x10
)

// StatusPacket is the eth/64 status message. It is a flat 6 field list, in this
// exact order: pgp-chain decodes it positionally.
type StatusPacket struct {
	ProtocolVersion uint32
	NetworkID       uint64
	TD              *big.Int
	Head            common.Hash
	Genesis         common.Hash
	ForkID          forkid.ID
}

// HashOrNumber is a union field selecting a block either by hash or by number.
// Exactly one of the two is encoded, see pgp-chain's hashOrNumber.
type HashOrNumber struct {
	Hash   common.Hash
	Number uint64
}

// EncodeRLP implements rlp.Encoder.
func (hn *HashOrNumber) EncodeRLP(w io.Writer) error {
	if hn.Hash == (common.Hash{}) {
		return rlp.Encode(w, hn.Number)
	}
	if hn.Number != 0 {
		return fmt.Errorf("both origin hash (%x) and number (%d) provided", hn.Hash, hn.Number)
	}
	return rlp.Encode(w, hn.Hash)
}

// DecodeRLP implements rlp.Decoder.
func (hn *HashOrNumber) DecodeRLP(s *rlp.Stream) error {
	_, size, err := s.Kind()
	if err != nil {
		return err
	}
	raw, err := s.Raw()
	if err != nil {
		return err
	}
	switch {
	case size == 32:
		return rlp.DecodeBytes(raw, &hn.Hash)
	case size <= 8:
		return rlp.DecodeBytes(raw, &hn.Number)
	default:
		return fmt.Errorf("invalid input size %d for origin", size)
	}
}

// GetBlockHeadersRequest is the eth/64 GetBlockHeaders request. Reverse is a
// bool here; it only became a uint in eth/62+ request wrappers of newer clients.
type GetBlockHeadersRequest struct {
	Origin  HashOrNumber
	Amount  uint64
	Skip    uint64
	Reverse bool
}

// NewBlockHashesPacket is a block announcement.
type NewBlockHashesPacket []struct {
	Hash   common.Hash
	Number uint64
}

// NewBlockPacket is a full block announcement.
type NewBlockPacket struct {
	Block *types.Block
	TD    *big.Int
}
