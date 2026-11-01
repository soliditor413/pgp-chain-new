// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/message.go (MsgEvent, IPeer) and pgp-chain/dpos/routes.go (IPeer).

package dpos

import "github.com/ethereum/go-ethereum/dpos/elamsg"

// ElaMsg and ElaMsgType are aliases, not wrappers: eth/protocols/eth64 builds
// elamsg.ElaMsg values and hands them here, and IPeer.SendELAMessage takes
// *ElaMsg. With an alias the two are literally the same type, so no conversion
// (and no copy) appears anywhere on the path.
type (
	ElaMsg     = elamsg.ElaMsg
	ElaMsgType = elamsg.ElaMsgType
)

// Re-exported so callers can write dpos.DAddr the way pgp-chain does.
const (
	Inv     = elamsg.Inv
	GetData = elamsg.GetData
	DAddr   = elamsg.DAddr
)

// IPeer is the slice of a devp2p peer that the address routing needs. The eth
// protocol layer adapts its own peer to it; dpos never sees an eth peer.
//
// ID is part of the interface because two layers report the same remote node:
// the p2p server as soon as a connection exists (regardless of which subprotocol
// it negotiated) and eth/64 once that connection can actually carry an ELAMSG.
// Routes has to fold those two notifications into ONE cache entry, and object
// identity cannot do it -- the p2p adapter and the eth/64 peer are different
// objects wrapping the same node. Both implementations therefore return
// enode.ID.String(), and they must agree byte for byte: that string is the only
// thing tying the p2p-announced node to the socket an ELAMSG can go out on.
type IPeer interface {
	ID() string
	Disconnect()
	SendELAMessage(msg *ElaMsg)
}

// MsgEvent is what the eth protocol layer pushes onto the event bus when an
// ELAMSG arrives, so dpos can handle it asynchronously on its own goroutine
// instead of inside the peer read loop.
type MsgEvent struct {
	ElaMsg *ElaMsg
	Peer   IPeer
}
