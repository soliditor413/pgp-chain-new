// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

import (
	"github.com/elastos/Elastos.ELA/events"

	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/dpos/elamsg"
	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
	"github.com/ethereum/go-ethereum/p2p"
)

// eth64Handler adapts the internal handler to the minimal interface the eth/64
// protocol needs. Beyond the chain it also bridges the three DPoS notifications,
// which is the whole reason this file exists rather than passing *handler
// straight through.
type eth64Handler handler

// Chain implements eth64.Backend. The concrete chain satisfies eth64.ChainReader,
// which is the narrowest surface eth/64 uses.
func (h *eth64Handler) Chain() eth64.ChainReader { return h.chain }

// eth64DPOSHooks translates eth/64 events into DPoS bus events.
//
// This is the only place the two worlds meet. The protocol layer must not import
// dpos (that is why ElaMsg lives in dpos/elamsg), but the DPoS event type numbers
// do live in dpos, and they must be the exact same constants dpos.Routes
// subscribes to. Elastos.ELA/events defines several same-named constants on a
// different numbering space, so a literal here would silently reach nobody.
//
// Every notify is either already asynchronous on the subscriber side (Routes
// spawns a goroutine per event) or dispatched to one below, because
// events.Notify holds the bus lock while calling every callback: a subscriber
// that blocks would wedge the whole bus, including the SPV listener.
type eth64DPOSHooks struct{}

var _ eth64.DPOSHooks = eth64DPOSHooks{}

// PeerUp tells the routing layer that a devp2p peer is now usable.
//
// dpos.Routes keys its per-peer request cache by this object, and its whole
// announce path iterates that cache -- so no PeerUp means no DAddr gossip, which
// is indistinguishable from a firewalled network.
func (eth64DPOSHooks) PeerUp(peer *eth64.Peer) {
	events.Notify(dpos.ETNewPeer, dpos.IPeer(peer))
}

// PeerDown drops the peer from the routing cache.
//
// Dispatched to its own goroutine because it runs on the connection's unwind
// path: doing the work inline would hold the read loop open for as long as
// Routes takes to drain its queue, and Routes' donequeue has capacity 1.
func (eth64DPOSHooks) PeerDown(peer *eth64.Peer) {
	p := dpos.IPeer(peer)
	go events.Notify(dpos.ETDonePeer, p)
}

// ElaMsg hands a decoded address-routing envelope to dpos.Routes.
//
// The peer travels with it because a DAddr reply goes back to the peer that asked
// and the envelope carries no sender field.
func (eth64DPOSHooks) ElaMsg(peer *eth64.Peer, msg *elamsg.ElaMsg) {
	events.Notify(dpos.ETElaMsg, &dpos.MsgEvent{ElaMsg: msg, Peer: peer})
}

// makeETH64Protocols returns the pgp-chain eth/64 protocol entry. It is only
// registered for chains that carry the pgp legacy history, never for
// BSC/Chapel/Rialto.
//
// The result is memoised because eth64.MakeProtocols is not a pure descriptor: it
// allocates a peer set and starts a sync goroutine. Protocols() is called twice
// per start - once to register (eth/backend.go:534) and once to name the protocols
// for a metrics gauge (cmd/geth/config.go:345) - so creating the entry twice would
// leave a second eth/64 syncer running with no peers for the life of the process,
// and would point progress reporting at whichever handler happened to be built
// last.
func (s *Ethereum) makeETH64Protocols() []p2p.Protocol {
	if s.blockchain == nil || s.blockchain.Config().Legacy == nil {
		return nil
	}
	s.eth64ProtosOnce.Do(func() {
		s.eth64Protos = eth64.MakeProtocols((*eth64Handler)(s.handler), s.networkID, eth64DPOSHooks{})
	})
	return s.eth64Protos
}
