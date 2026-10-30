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

// dposTransport is the Transport implementation over the DPoS TCP network
// (dpos.Network). It is the outbound half of M4.
//
// The one design decision worth spelling out is what "broadcast" means here.
// dpos.Network has a BroadcastMessage that fans out to every CONNECTED peer, and
// using it would be wrong: the server connects to the current producers AND the
// next-epoch producers (dpos.Network.UpdatePeers passes both sets to
// ConnectPeers), so a network-level broadcast reaches peers that are not in the
// voting set. A consensus message delivered to a non-producer is at best wasted
// and at worst an eavesdropper's vote-collection feed.
//
// So every method below iterates the CURRENT producer set and unicasts, which is
// exactly what pgp-chain's Pbft.BroadMessage does. dpos.Network.BroadcastMessage
// is consequently unused by this package.

import (
	"bytes"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
	"github.com/ethereum/go-ethereum/log"
)

type dposTransport struct {
	network dpos.DPOSNetwork

	// producers is read on every send rather than captured, because the set
	// changes at an epoch boundary (ConsensusView.UpdateProducers). A captured
	// slice would keep addressing the previous epoch's producers, which looks
	// like a network partition for one epoch.
	producers func() [][]byte
}

func newDPOSTransport(network dpos.DPOSNetwork, producers func() [][]byte) *dposTransport {
	return &dposTransport{network: network, producers: producers}
}

// AttachNetwork wires the engine to a DPoS network in one call.
//
// Three hooks are installed together because they describe the same fact --
// "this node has peers now" -- and installing them separately is how a node ends
// up with a transport but a peer count of zero, i.e. recovery refuses to run
// (recoverAbnormalState requires activePeerCount >= majority) while the network
// is perfectly healthy. That failure looks exactly like a consensus stall.
func (e *Engine) AttachNetwork(network dpos.DPOSNetwork) {
	e.transportMu.Lock()
	e.network = network
	e.transportMu.Unlock()

	e.SetTransport(newDPOSTransport(network, e.currentProducers))
	e.SetAnnounceAddr(func() { e.AnnounceDAddr() })
	e.SetPeerCounter(func() int { return len(network.GetActivePeers()) })
}

// currentProducers is the live producer set, read through the dispatcher so that
// an epoch transition takes effect without re-wiring the transport.
func (e *Engine) currentProducers() [][]byte {
	if e.dispatcher == nil {
		return nil
	}
	return e.dispatcher.GetConsensusView().GetProducers()
}

// BroadcastBlock announces a freshly sealed block.
//
// Peers treat a block whose header.Extra is non-empty as already confirmed
// (network_listener.go OnBlock), and a sealed legacy header's Extra is exactly
// the serialized confirm plus nothing else -- Prepare sets Extra to nil (D6) and
// Seal fills it -- so the "confirmed" bit rides along for free.
func (t *dposTransport) BroadcastBlock(block *types.Block) {
	if block == nil {
		return
	}
	buf := new(bytes.Buffer)
	if err := block.EncodeRLP(buf); err != nil {
		log.Error("dposTransport: cannot encode block", "height", block.NumberU64(), "err", err)
		return
	}
	t.broadcast(dmsg.NewBlockMsg(buf.Bytes()), nil)
}

func (t *dposTransport) BroadcastProposal(proposal *payload.DPOSProposal) {
	if proposal == nil {
		return
	}
	t.broadcast(&emsg.Proposal{Proposal: *proposal}, nil)
}

func (t *dposTransport) BroadcastVote(vote *emsg.Vote) {
	if vote == nil {
		return
	}
	t.broadcast(vote, nil)
}

func (t *dposTransport) BroadcastConfirm(confirm *payload.Confirm, height uint64) {
	if confirm == nil {
		return
	}
	t.broadcast(dmsg.NewConfirmMsg(confirm, height), nil)
}

func (t *dposTransport) BroadcastResetView(msg *emsg.ResetView) {
	if msg == nil {
		return
	}
	t.broadcast(msg, nil)
}

func (t *dposTransport) BroadcastRaw(msg elap2p.Message) { t.broadcast(msg, nil) }

func (t *dposTransport) BroadcastRawExcept(msg elap2p.Message, except dpeer.PID) {
	t.broadcast(msg, &except)
}

func (t *dposTransport) SendToPeer(pid dpeer.PID, msg elap2p.Message) error {
	if msg == nil {
		return nil
	}
	return t.network.SendMessageToPeer(pid, msg)
}

// broadcast sends msg to every current producer, optionally skipping one.
//
// A send failure is logged at trace level and does not stop the loop: the
// producer set is a fixed list and the whole point of a broadcast is to reach the
// reachable subset. Consensus tolerates a missing peer -- that is what the
// majority threshold is for -- but it does not tolerate a broadcast that gives up
// after the first timeout.
func (t *dposTransport) broadcast(msg elap2p.Message, except *dpeer.PID) {
	if msg == nil || t.network == nil {
		return
	}
	for _, pubkey := range t.producers() {
		var pid dpeer.PID
		if len(pubkey) != len(pid) {
			// copy() would silently truncate or zero-pad and the message would
			// go to a peer that is not the one named. Refuse instead: a producer
			// key that is not a full 33 byte compressed P-256 point is a
			// configuration error, and sending consensus traffic to a
			// mismatched identity is worse than not sending it.
			log.Error("dposTransport: producer key has wrong length",
				"len", len(pubkey), "want", len(pid), "cmd", msg.CMD())
			continue
		}
		copy(pid[:], pubkey)
		if except != nil && pid == *except {
			continue
		}
		if err := t.network.SendMessageToPeer(pid, msg); err != nil {
			log.Trace("dposTransport: send failed", "cmd", msg.CMD(), "peer", pid.String(), "err", err)
		}
	}
}
