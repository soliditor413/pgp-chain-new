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
	"fmt"

	"github.com/elastos/Elastos.ELA/dpos/dtime"
	dpp2p "github.com/elastos/Elastos.ELA/dpos/p2p"
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
// has no business telling the set who to peer with. A retired engine answers
// false for the same reason and one more: after the Parlia fork there is no set
// to tell, and the announce would restart an address exchange whose DPoS server
// retire has already stopped. Same early-return shape as the no-account case
// below, so a reader does not have to learn a second idiom.
//
// CRPeers is left nil deliberately. peer.PeersInfo has three fields and upstream
// producers fill only the first two; CRPeers is the chainbridge arbiter candidate
// set, which is not ported (D10). Subscribers read all three, so the asymmetry is
// load-bearing rather than sloppy -- keep it.
func (e *Engine) AnnounceDAddr() bool {
	if e.retired.Load() {
		return false
	}
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

// WatchPeerSet forwards the announced peer set to the DPoS TCP server.
//
// This is the second half of AnnounceDAddr, and without it the whole address
// machinery is inert. Upstream does the same forwarding from its own subscriber
// (pgp-chain/consensus/pbft/pbft.go:277-283): ETDirectPeersChanged carries PIDs,
// dpos.Routes consumes it to build DAddr records, and someone has to hand the
// same PIDs to Network.UpdatePeers so the TCP server knows whom to dial. In this
// tree that someone was missing -- UpdatePeers was implemented and never called
// -- so a four-node bench announced its peer set, logged
// "received ETDirectPeersChanged", and never opened a single DPoS connection.
//
// The forwarding runs on its own goroutine because events.Notify holds the bus
// lock while calling subscribers, and UpdatePeers blocks on the server's query
// channel: doing it inline would stall every other subscriber, including the
// ones that deliver blocks.
//
// It is a method rather than a constructor side effect so a test (or a
// single-node rig with no network) never installs a subscriber it cannot remove.
func (e *Engine) WatchPeerSet() {
	events.Subscribe(func(ev *events.Event) {
		if ev.Type != dpos.ETDirectPeersChanged {
			return
		}
		info, ok := ev.Data.(*dpeer.PeersInfo)
		if !ok {
			log.Warn("WatchPeerSet: unexpected payload", "type", fmt.Sprintf("%T", ev.Data))
			return
		}
		e.transportMu.RLock()
		n := e.network
		e.transportMu.RUnlock()
		if n == nil {
			return
		}
		go n.UpdatePeers(info.CurrentPeers, info.NextPeers)
	})
}

// GetTimeSource is the median time source the engine already shares with its
// dispatcher, exposed so dpos.Config can use the same clock.
//
// Sharing it is not cosmetic: the address records Routes signs carry a timestamp
// that is rejected outside maxTimeOffset (30s), and the DPoS server's peer
// handshake uses the same source. Two different median clocks on one node would
// make it reject its own relayed records on a skewed machine.
func (e *Engine) GetTimeSource() dtime.MedianTimeSource { return e.timeSource }

// DPoSNetworkRunning reports whether a DPoS TCP server is attached and in
// service.
//
// This is the difference between "no peers connected" and "no server to connect
// to", which the peer count alone cannot express: a verifier-only node and a
// single-producer rig both report zero peers and neither runs a server, while a
// misconfigured multi-node bench also reports zero peers but does. The RPC
// status needs the distinction to point an operator at the right knob.
//
// The retired term is what keeps it honest after the Parlia fork: retire stops
// the server but deliberately keeps the reference (transport and the block
// store wiring still read e.network; clearing it would need an audit of every
// reader), so "is the pointer non-nil" alone would answer true for a server
// that is down. The reference stays, the answer changes.
func (e *Engine) DPoSNetworkRunning() bool {
	e.transportMu.RLock()
	defer e.transportMu.RUnlock()
	return e.network != nil && !e.retired.Load()
}

// DumpDPoSPeersInfo snapshots the DPoS TCP server's connect list, including
// producers this node has never reached (State == CSNoneConnection, Addr ==
// "unknown").
//
// The connect list is the load-bearing half of the answer to "peers look
// connected in devp2p but nothing happens": devp2p peers and DPoS peers are two
// different sets over two different TCP servers, and only this table shows the
// second one. A producer missing here is a producer whose votes can never
// arrive, no matter what net_peerCount says.
//
// It returns nil, not an empty slice, when no server is attached, so a caller
// can tell "not running" from "running with nothing connected".
//
// A retired engine answers nil without touching the network. This is the second
// line of defence behind dpos.Network.stopped: the flag inside the network is
// set by Stop, so a caller that enters here a hair before Stop stores its flag
// would still reach the server. Asking a retired engine is a question whose
// answer is known -- there is no server to ask -- so it does not ask. See the
// deadlock note on ActiveDPoSPeers.
func (e *Engine) DumpDPoSPeersInfo() []*dpp2p.PeerInfo {
	if e.retired.Load() {
		return nil
	}
	e.transportMu.RLock()
	n := e.network
	e.transportMu.RUnlock()
	if n == nil {
		return nil
	}
	dumper, ok := n.(dpos.PeersDumper)
	if !ok {
		return nil
	}
	return dumper.DumpPeersInfo()
}

// ActiveDPoSPeers is the number of established DPoS TCP connections.
//
// It counts live connections, whereas DumpDPoSPeersInfo counts the connect list,
// which also holds peers that are unreachable. Recovery gates on this number
// (network_listener.go's StartRecovery compares it against the majority count),
// so it is the one an operator should compare against majoritycount.
//
// It counts CONNECTIONS, not distinct peers: a producer set whose members have
// all dialed each other reports twice as many as there are peers, because each
// pair holds one connection in each direction (measured: 6 for a 4-producer
// bench, 4 after one node was killed). Only DumpPeersInfo folds the pair into
// one row and calls it 2WayConnection. Comparing this against majoritycount is
// therefore conservative, never optimistic, which is the right direction for a
// gate -- but do not read it as a peer count.
//
// # Why a retired engine answers 0 without asking
//
// This is the call pbft.status() hung on in the field. The chain: this method
// -> Network.GetActivePeers -> p2pServer.ConnectedPeers, which posts a request
// into the server's s.query channel and waits for the reply on it. Once
// p2pServer.Stop() has run, the peerHandler goroutine takes its quit case and
// never reads s.query again, so the reply never comes and the RPC blocks
// forever -- not slow, blocked.
//
// The dpos layer now goes silent itself (Network.stopped), but the flag there
// is set inside Stop, leaving a narrow window between a caller's check and the
// store. A retired engine does not need to ask at all: retire stops the server
// before it returns, and the answer for every later caller is zero by
// construction. Both checks are kept on purpose -- neither one alone closes
// every path into a stopped server.
func (e *Engine) ActiveDPoSPeers() int {
	if e.retired.Load() {
		return 0
	}
	e.transportMu.RLock()
	n := e.network
	e.transportMu.RUnlock()
	if n == nil {
		return 0
	}
	return len(n.GetActivePeers())
}

// ActiveDPoSPeerInfos is ActiveDPoSPeers with the per-connection detail, for the
// RPC that shows who is actually talking right now.
//
// The connect-list dump already reports direction, but it can only report what
// the server knows about a configured peer. A connection that exists but whose
// PID is not in the connect list would appear in neither outboundPeers nor
// connectPeers, and that is exactly the shape of an address-book or magic
// mismatch. This path walks the live peers instead.
//
// A retired engine answers nil for the same reason ActiveDPoSPeers answers 0:
// the walk starts at Network.GetActivePeers, which is the call that blocks once
// the server is down.
func (e *Engine) ActiveDPoSPeerInfos() []*dpp2p.PeerInfo {
	if e.retired.Load() {
		return nil
	}
	e.transportMu.RLock()
	n := e.network
	e.transportMu.RUnlock()
	if n == nil {
		return nil
	}
	peers := n.GetActivePeers()
	out := make([]*dpp2p.PeerInfo, 0, len(peers))
	for _, p := range peers {
		real := p.ToPeer()
		if real == nil {
			continue
		}
		// NodeVersion is an exported field on the DPoS peer (it is NOT on its
		// StatsSnap, unlike the plain p2p peer in the same vendor tree), set
		// once during the Version handshake and never mutated afterwards.
		info := &dpp2p.PeerInfo{
			PID:         p.PID(),
			Addr:        real.Addr(),
			NodeVersion: real.NodeVersion,
		}
		if real.Inbound() {
			info.State = dpp2p.CSInboundOnly
		} else {
			info.State = dpp2p.CSOutboundOnly
		}
		out = append(out, info)
	}
	return out
}

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
