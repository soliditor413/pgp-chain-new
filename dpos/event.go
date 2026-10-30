// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/event.go.

package dpos

import "github.com/elastos/Elastos.ELA/events"

// Event types published on the Elastos event bus.
//
// The bus is shared with the SPV service, which is the only reason these live
// on the Elastos events package rather than on a go-ethereum feed: the SPV
// listener is the producer of ETNextProducers and ETOnSPVHeight, and making it
// publish onto a go-ethereum feed instead would mean either importing dpos from
// spv (already the case, and one more edge in an already tangled graph) or
// rewriting that listener's notification path. Keeping the bus is the smaller
// and better-tested coupling; see docs/design/pbft-port-design.md R14.
//
// Numbers start at 1000 to stay clear of the Elastos internal event types.
const (
	ETNewPeer          events.EventType = 1000
	ETDonePeer         events.EventType = 1001
	ETStopRoutes       events.EventType = 1002
	ETElaMsg           events.EventType = 1003
	ETAnnounceAddr     events.EventType = 1004
	ETNextProducers    events.EventType = 1005
	ETOnSPVHeight      events.EventType = 1006
	ETSmallCroTx       events.EventType = 1007
	ETFailedWithdrawTx events.EventType = 1008
	ETUpdateProducers  events.EventType = 1009
	ETOnDutyEvent      events.EventType = 1010
	ETNextValidators   events.EventType = 1011

	// ETDirectPeersChanged carries *peer.PeersInfo{CurrentPeers, NextPeers} and
	// is the seam between the two DPoS channels:
	//
	//	pbft.AnnounceDAddr  --Notify-->  Routes.PeersChanged  --UpdatePeers--> Network
	//
	// i.e. it turns "who should I be peered with" into "dial these PIDs on the
	// independent TCP server".
	//
	// Elastos.ELA/events defines a constant of the same name (events.go:47,
	// value 6, an iota slot). This package deliberately does not use that one:
	// per the file comment above, dpos numbers its events from 1000 to stay out
	// of the Elastos internal range, and every producer and consumer in this
	// tree uses THIS constant, so the two numbering schemes never meet. Take the
	// Elastos value here and AnnounceDAddr's Notify would reach no subscriber.
	//
	// peer.PeersInfo has three fields; producers fill only the first two and
	// leave CRPeers nil. Keep that asymmetry -- CRPeers is the chainbridge
	// arbiter candidate set, which is not ported (D10).
	ETDirectPeersChanged events.EventType = 1012
)
