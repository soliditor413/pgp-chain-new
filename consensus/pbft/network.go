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

package pbft

// Address announcement: the seam that turns "these are the producers I must be
// peered with" into "dial these PIDs on the independent DPoS TCP server".
//
// The two DPoS channels meet here:
//
//	pbft.AnnounceDAddr --Notify(ETDirectPeersChanged)--> dpos.Routes
//	                   --RelayAddr(ELAMSG Inv)--------> devp2p peers
//	                   --DAddr reply------------------> OnCipherAddr
//	                   --decrypt--> Network.AddDirectLinkAddr --> TCP dial
//
// The engine owns the first hop only. It knows the producer set and therefore
// which PIDs it must connect to; it does not know any of their IP addresses, and
// that is what the relay is for. See docs/design/m4-network-layer-breakdown.md
// T7.

import (
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/events"

	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/log"
)

// AnnounceDAddr publishes the PIDs this node must connect to.
//
// Subscribers: dpos.Routes (ETDirectPeersChanged) rewrites its peer set and asks
// the network to re-dial. The event carries PIDs only -- peer.PeersInfo has no
// addresses -- so this call on its own cannot connect anything; it is the trigger
// for the DAddr exchange that can.
//
// It returns false when there is nothing to announce, which is the shape of a
// verifier-only node: no account means it is not a producer, and a non-producer
// has no business telling the set who to peer with.
//
// CRPeers is left nil deliberately. peer.PeersInfo has three fields and upstream
// producers fill only the first two; CRPeers is the chainbridge arbiter candidate
// set, which is not ported (D10). Subscribers read all three, so the asymmetry is
// load-bearing rather than sloppy -- keep it.
func (e *Engine) AnnounceDAddr() bool {
	if e.account == nil || e.dispatcher == nil {
		log.Debug("AnnounceDAddr: node has no producer key, nothing to announce")
		return false
	}
	current := e.dispatcher.GetCurrentNeedConnectArbiters()
	next := e.dispatcher.GetNextNeedConnectArbiters()
	log.Debug("AnnounceDAddr", "current", len(current), "next", len(next))
	events.Notify(dpos.ETDirectPeersChanged, &dpeer.PeersInfo{
		CurrentPeers: current,
		NextPeers:    next,
	})
	return true
}

// GetTimeSource is the median time source the engine already shares with its
// dispatcher, exposed so dpos.Config can use the same clock.
//
// Sharing it is not cosmetic: the address records Routes signs carry a timestamp
// that is rejected outside maxTimeOffset (30s), and the DPoS server's peer
// handshake uses the same source. Two different median clocks on one node would
// make it reject its own relayed records on a skewed machine.
func (e *Engine) GetTimeSource() dtime.MedianTimeSource { return e.timeSource }

// SetIsCurrent installs the "is the chain caught up" probe.
//
// dpos.Routes uses it as the gate on announcing our address (routes.go:408):
// announcing before the chain has synced publishes a stale address record, and
// the set then dials a node that cannot answer. It is a setter rather than a
// constructor argument because eth/backend builds the engine before the
// downloader exists.
//
// Per O2 this is bound to downloader progress -- progress.CurrentBlock >=
// progress.HighestBlock -- and NOT to a bad-block fallback. Upstream also
// returns true when the highest known block is a bad block one height above the
// head, which recovers a node whose only peer offers an unusable tip; that is a
// recovery concern and moves to M6.
func (e *Engine) SetIsCurrent(fn func() bool) {
	e.chainMu.Lock()
	e.isCurrent = fn
	e.chainMu.Unlock()
}

// IsCurrent reports whether the chain is caught up. A node with no probe
// installed answers false, i.e. "do not announce yet", which is the safe default:
// announcing nothing costs a retry after retryAnnounceDuration (3s), while
// announcing a bad address costs every peer a failed dial.
func (e *Engine) IsCurrent() bool {
	e.chainMu.RLock()
	fn := e.isCurrent
	e.chainMu.RUnlock()
	if fn == nil {
		return false
	}
	return fn()
}
