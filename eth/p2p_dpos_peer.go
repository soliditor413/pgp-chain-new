// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

import (
	"github.com/elastos/Elastos.ELA/events"

	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/dpos/elamsg"
	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// p2pDPOSPeer is dpos.IPeer for a node the p2p server has a connection to.
//
// DPoS learns about peers from two layers, and they are complementary rather
// than redundant. The p2p server knows about every connection the moment it is
// established, whatever subprotocol was negotiated -- so a node that never runs
// eth/64 (a BSC chain has no eth/64 protocol set at all) still shows up as a
// peer. eth/64 then announces the subset that can actually carry an ELAMSG.
//
// Both notifications name the same node, and p2p only hands over an enode.ID,
// not a *p2p.Peer: the peer is not even in the peer set yet when the add event
// fires (p2p/server.go runPeer sends it before p.run) and it is already out by
// the time the drop event fires. So this type carries the identity and looks the
// live objects up on demand instead of holding them.
type p2pDPOSPeer struct {
	// id is enode.ID.String(): the exact string eth64.Peer.ID returns, which is
	// what lets dpos fold the two layers into one cache entry.
	id string
	// server resolves Disconnect() to the current socket. It is nil in tests
	// that only exercise ID.
	server *p2p.Server
	// eth64 resolves SendELAMessage() to the eth/64 session for the same node.
	// Injected as a function so this file does not depend on the eth64 handler's
	// internals; it is nil in tests that only exercise ID.
	eth64 func(id string) *eth64.Peer
}

var _ dpos.IPeer = (*p2pDPOSPeer)(nil)

func newP2PDPOSPeer(server *p2p.Server, eth64Peer func(string) *eth64.Peer, id enode.ID) *p2pDPOSPeer {
	return &p2pDPOSPeer{id: id.String(), server: server, eth64: eth64Peer}
}

// ID returns the remote node id.
//
// This must stay byte-for-byte identical to eth64.Peer.ID, because dpos keys its
// per-peer cache by this string: if the two layers spelled the id differently
// the same node would be cached twice, and the entry created here would never be
// removed. TestP2PDPOSPeerIDMatchesEth64 pins the equality.
func (p *p2pDPOSPeer) ID() string { return p.id }

// Disconnect drops the connection this peer was announced for.
//
// The lookup is by id because the event carried no handle. Not finding it means
// the socket is already gone, which is a normal race against teardown, so that
// case is silent.
func (p *p2pDPOSPeer) Disconnect() {
	if p.server == nil {
		return
	}
	for _, peer := range p.server.Peers() {
		if peer.ID().String() == p.id {
			peer.Disconnect(p2p.DiscQuitting)
			return
		}
	}
}

// SendELAMessage forwards the envelope to the eth/64 session for this node.
//
// It cannot be a no-op: the p2p layer carries no ELAMSG, so an implementation
// that dropped the message here would silently break every DAddr reply for a
// peer that was announced by p2p before eth/64 registered it. No eth/64 session
// means the envelope has nowhere to go yet (handshake in progress, or this chain
// never negotiates eth/64), hence the debug line rather than a warning.
func (p *p2pDPOSPeer) SendELAMessage(msg *elamsg.ElaMsg) {
	if p.eth64 == nil {
		return
	}
	peer := p.eth64(p.id)
	if peer == nil {
		log.Debug("DPoS: no eth/64 session for peer, dropping ELAMSG", "peer", p.id)
		return
	}
	peer.SendELAMessage(msg)
}

// watchP2PPeers subscribes to the devp2p peer feed and republishes adds and drops
// on the DPoS event bus. Only PBFT chains call it: they are the only ones that
// run dpos.Routes.
func (s *Ethereum) watchP2PPeers() {
	peerEvents := make(chan *p2p.PeerEvent, 128)
	sub := s.p2pServer.SubscribeEvents(peerEvents)
	go s.relayPeerEvents(peerEvents, sub)
}

// relayPeerEvents is watchP2PPeers' loop.
//
// Each notification is delivered synchronously here, not from a new goroutine
// per event, because the order matters: dpos deletes a cache entry on the first
// DonePeer it sees, so a drop overtaking its add would leave that entry behind
// until the next connect. The subscriber side (dpos.Routes) already dispatches
// internally, so this does not hold the p2p feed for long.
func (s *Ethereum) relayPeerEvents(peerEvents chan *p2p.PeerEvent, sub event.Subscription) {
	defer sub.Unsubscribe()

	lookup := func(id string) *eth64.Peer { return eth64.PeerByID(id) }

	for {
		select {
		case ev := <-peerEvents:
			peer := newP2PDPOSPeer(s.p2pServer, lookup, ev.Peer)
			switch ev.Type {
			case p2p.PeerEventTypeAdd:
				events.Notify(dpos.ETNewPeer, dpos.IPeer(peer))
			case p2p.PeerEventTypeDrop:
				events.Notify(dpos.ETDonePeer, dpos.IPeer(peer))
			}
		case err := <-sub.Err():
			if err != nil {
				log.Debug("DPoS: p2p peer event subscription ended", "err", err)
			}
			return
		case <-s.stopCh:
			return
		}
	}
}
