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
type IPeer interface {
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
