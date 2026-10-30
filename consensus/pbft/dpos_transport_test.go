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

import (
	"bytes"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/p2p"
	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/events"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	"github.com/ethereum/go-ethereum/dpos"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
)

// stubDPOSNetwork is the dpos.DPOSNetwork the transport talks to.
//
// BroadcastMessage PANICS. That is the assertion, not an accident: the transport
// must address the current producer set one PID at a time, because the DPoS
// server also connects to the NEXT epoch's producers (Network.UpdatePeers passes
// both sets to ConnectPeers). Using the network-level broadcast would deliver
// votes to peers that cannot count them. See dpos_transport.go.
type stubDPOSNetwork struct {
	mu    sync.Mutex
	sent  []recordedSend
	peers int
}

type recordedSend struct {
	pid dpeer.PID
	cmd string
}

func (s *stubDPOSNetwork) Start()      {}
func (s *stubDPOSNetwork) Stop() error { return nil }

func (s *stubDPOSNetwork) SendMessageToPeer(id dpeer.PID, msg elap2p.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, recordedSend{pid: id, cmd: msg.CMD()})
	return nil
}

func (s *stubDPOSNetwork) BroadcastMessage(elap2p.Message) {
	panic("dposTransport used the network broadcast; it must unicast to the producer set")
}

func (s *stubDPOSNetwork) UpdatePeers(currentPeers, nextPeers []dpeer.PID) {}

func (s *stubDPOSNetwork) GetActivePeers() []p2p.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return make([]p2p.Peer, s.peers)
}

func (s *stubDPOSNetwork) sends() []recordedSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedSend(nil), s.sent...)
}

func pidOf(t *testing.T, pubkey []byte) dpeer.PID {
	t.Helper()
	var pid dpeer.PID
	if len(pubkey) != len(pid) {
		t.Fatalf("producer key is %d bytes, PID is %d", len(pubkey), len(pid))
	}
	copy(pid[:], pubkey)
	return pid
}

// decodeKey turns one of newProducerSet's hex encoded public keys back into bytes.
func decodeKey(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("producer key %q is not hex: %v", s, err)
	}
	return b
}

// TestDPOSTransportBroadcastsToProducerSetOnly pins the difference between
// "connected" and "entitled to vote".
func TestDPOSTransportBroadcastsToProducerSetOnly(t *testing.T) {
	_, keys := newProducerSet(t, 4)
	producers := make([][]byte, len(keys))
	for i, k := range keys {
		producers[i] = decodeKey(t, k)
	}

	network := &stubDPOSNetwork{}
	transport := newDPOSTransport(network, func() [][]byte { return producers })
	transport.BroadcastRaw(&emsg.ResetView{Sponsor: []byte{1}})

	sends := network.sends()
	if len(sends) != len(producers) {
		t.Fatalf("broadcast reached %d peers, want the %d producers", len(sends), len(producers))
	}
	seen := make(map[dpeer.PID]bool, len(sends))
	for _, s := range sends {
		seen[s.pid] = true
	}
	for _, pk := range producers {
		if !seen[pidOf(t, pk)] {
			t.Fatalf("producer %x never received the broadcast", pk[:4])
		}
	}
}

// TestDPOSTransportSkipsExcludedPeer is the relay path: a block just received is
// not sent back to its sender.
func TestDPOSTransportSkipsExcludedPeer(t *testing.T) {
	_, keys := newProducerSet(t, 3)
	producers := make([][]byte, len(keys))
	for i, k := range keys {
		producers[i] = decodeKey(t, k)
	}
	excluded := pidOf(t, producers[1])

	network := &stubDPOSNetwork{}
	transport := newDPOSTransport(network, func() [][]byte { return producers })
	transport.BroadcastRawExcept(dmsg.NewConfirmMsg(&payload.Confirm{}, 1), excluded)

	sends := network.sends()
	if len(sends) != len(producers)-1 {
		t.Fatalf("broadcast except one reached %d peers, want %d", len(sends), len(producers)-1)
	}
	for _, s := range sends {
		if s.pid == excluded {
			t.Fatal("the excluded peer received the message anyway")
		}
	}
}

// TestDPOSTransportRejectsMalformedProducerKey covers the truncation trap.
//
// copy(pid[:], pubkey) silently truncates or zero-pads, so a 20 byte address (an
// account address, say, instead of a 33 byte compressed public key) would become
// a valid-looking PID belonging to somebody else. Sending votes to the wrong
// identity is worse than not sending them.
func TestDPOSTransportRejectsMalformedProducerKey(t *testing.T) {
	short := bytes.Repeat([]byte{0xaa}, 20)
	network := &stubDPOSNetwork{}
	transport := newDPOSTransport(network, func() [][]byte { return [][]byte{short} })

	transport.BroadcastRaw(&emsg.ResetView{})

	if n := len(network.sends()); n != 0 {
		t.Fatalf("a 20 byte producer key produced %d sends, want 0", n)
	}
}

// TestDPOSTransportConfirmCarriesHeight pins the height the wire needs and the
// confirm does not have.
func TestDPOSTransportConfirmCarriesHeight(t *testing.T) {
	_, keys := newProducerSet(t, 1)
	pk := decodeKey(t, keys[0])

	network := &stubDPOSNetwork{}
	transport := newDPOSTransport(network, func() [][]byte { return [][]byte{pk} })

	var captured elap2p.Message
	transport.network = &captureNetwork{onSend: func(m elap2p.Message) { captured = m }}

	transport.BroadcastConfirm(&payload.Confirm{Proposal: payload.DPOSProposal{}}, 42)

	msg, ok := captured.(*dmsg.ConfirmMsg)
	if !ok {
		t.Fatalf("expected *dmsg.ConfirmMsg, got %T", captured)
	}
	if msg.Height != 42 {
		t.Fatalf("confirm height = %d, want 42", msg.Height)
	}
}

// captureNetwork hands every sent message to a callback.
type captureNetwork struct {
	onSend func(elap2p.Message)
}

func (c *captureNetwork) Start()      {}
func (c *captureNetwork) Stop() error { return nil }
func (c *captureNetwork) SendMessageToPeer(_ dpeer.PID, msg elap2p.Message) error {
	c.onSend(msg)
	return nil
}
func (c *captureNetwork) BroadcastMessage(elap2p.Message) {
	panic("dposTransport used the network broadcast")
}
func (c *captureNetwork) UpdatePeers(currentPeers, nextPeers []dpeer.PID) {}
func (c *captureNetwork) GetActivePeers() []p2p.Peer                      { return nil }

// announceEvents collects ETDirectPeersChanged notifications.
//
// The Elastos event bus has no unsubscribe, so this subscription outlives the
// test. A non-blocking send keeps it from stalling Notify, which holds the bus
// lock while calling every subscriber -- a blocking callback here would wedge
// every later test in the package.
var announceEvents = make(chan *dpeer.PeersInfo, 16)

var subscribeAnnounceOnce sync.Once

func subscribeAnnounce() {
	subscribeAnnounceOnce.Do(func() {
		events.Subscribe(func(e *events.Event) {
			if e.Type != dpos.ETDirectPeersChanged {
				return
			}
			info, ok := e.Data.(*dpeer.PeersInfo)
			if !ok {
				return
			}
			select {
			case announceEvents <- info:
			default:
			}
		})
	})
}

// TestAnnounceDAddrReachesSubscribers is the contract between the two DPoS
// channels, and it is worth a test because the failure is silent: if the event
// type did not match what dpos.Routes subscribes to, Notify would succeed, reach
// nobody, and the only symptom would be producers that never dial each other.
//
// dpos/event.go documents the trap -- Elastos.ELA/events has its own
// ETDirectPeersChanged (value 6) and using it here would deliver to no one,
// because dpos numbers from 1000.
func TestAnnounceDAddrReachesSubscribers(t *testing.T) {
	subscribeAnnounce()
	// Drain anything a previous test left behind.
	for len(announceEvents) > 0 {
		<-announceEvents
	}

	engine, _ := newListenerEngine(t)
	if !engine.AnnounceDAddr() {
		t.Fatal("AnnounceDAddr refused on an engine with a producer key")
	}

	select {
	case info := <-announceEvents:
		if info.CurrentPeers == nil && info.NextPeers == nil {
			t.Fatal("the announcement carried no PIDs at all")
		}
		// This engine is the only producer, and it is on duty, so its own PID
		// must be in one of the sets it is telling the network to connect to.
		self := pidOf(t, engine.account.PublicKeyBytes())
		found := false
		for _, p := range append(append([]dpeer.PID{}, info.CurrentPeers...), info.NextPeers...) {
			if p == self {
				found = true
			}
		}
		if !found {
			t.Fatalf("our own PID %s is in neither the current nor the next set", self.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ETDirectPeersChanged never reached a subscriber")
	}
}

// TestAnnounceDAddrRefusesWithoutKey: a verifier must not tell the set who to
// peer with.
func TestAnnounceDAddrRefusesWithoutKey(t *testing.T) {
	engine := New(nil, &Config{})
	defer engine.Close()

	if engine.AnnounceDAddr() {
		t.Fatal("a node with no producer key announced a peer set")
	}
}

// TestIsCurrentDefaultsToFalse pins the safe default: with no probe installed the
// node does not announce its address yet.
func TestIsCurrentDefaultsToFalse(t *testing.T) {
	engine := New(nil, &Config{})
	defer engine.Close()

	if engine.IsCurrent() {
		t.Fatal("IsCurrent is true without a probe")
	}

	engine.SetIsCurrent(func() bool { return true })
	if !engine.IsCurrent() {
		t.Fatal("IsCurrent ignored the installed probe")
	}
}

// TestAttachNetworkInstallsAllThreeHooks pins the reason AttachNetwork exists.
//
// Three independent setters would let an assembly layer install a transport and
// forget the peer counter. The node then broadcasts normally, looks connected,
// and never recovers -- because recoverAbnormalState refuses to start when
// activePeerCount is 0 -- so it never seals. That failure is a silent stall, not
// an error, which is exactly the kind of wiring mistake worth a test.
func TestAttachNetworkInstallsAllThreeHooks(t *testing.T) {
	subscribeAnnounce()
	for len(announceEvents) > 0 {
		<-announceEvents
	}

	engine, _ := newListenerEngine(t)
	network := &stubDPOSNetwork{peers: 3}
	engine.AttachNetwork(network)

	if !engine.HasTransport() {
		t.Fatal("AttachNetwork did not install a transport")
	}
	if got := engine.activePeerCount(); got != 3 {
		t.Fatalf("peer counter reports %d, want 3 (it is not wired to the network)", got)
	}
	if engine.announce == nil {
		t.Fatal("AttachNetwork did not install the announce hook")
	}

	// The hook must reach dpos.Routes, i.e. fire ETDirectPeersChanged.
	engine.announceAddr()
	select {
	case <-announceEvents:
	case <-time.After(2 * time.Second):
		t.Fatal("the announce hook installed by AttachNetwork reached no subscriber")
	}

	// And the transport must address the producer set rather than the network:
	// one send per current producer, none via the network broadcast.
	engine.getTransport().BroadcastRaw(&emsg.ResetView{})
	if n, want := len(network.sends()), 1; n != want {
		t.Fatalf("AttachNetwork's transport made %d sends, want %d (one per producer)", n, want)
	}
}
