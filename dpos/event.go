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
)
