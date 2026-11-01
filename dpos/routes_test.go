// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dpos

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/p2p/msg"

	glog "github.com/ethereum/go-ethereum/log"
)

// fakeRoutesPeer is the IPeer slice that Routes actually uses: an identity,
// disconnecting and sending an ELAMSG. Both of the latter are no-ops; the
// announce path only needs the peer to exist in state.peerCache, which is keyed
// by ID.
//
// id is an int so a test can name the same remote node from two different
// objects -- which is exactly what the p2p layer and eth/64 do.
type fakeRoutesPeer struct {
	id int
}

func (f *fakeRoutesPeer) ID() string             { return fmt.Sprintf("peer-%d", f.id) }
func (f *fakeRoutesPeer) Disconnect()            {}
func (f *fakeRoutesPeer) SendELAMessage(*ElaMsg) {}

// TestAnnounceWithFewPeers pins the behaviour the M4 rig depends on.
//
// pgp-chain gates the address announcement on having at least minPeersToAnnounce
// (5) connected peers, but commented out both statements that gave the gate any
// effect ("TODO Temporary cancellation"), leaving an empty if block. The gate is
// therefore dead and the announce always runs.
//
// That accidental no-op is load-bearing for M4: the acceptance rig is 4 nodes, so
// every node sees at most 3 peers, and 3 < 5 forever. If the gate were live,
// DAddr would never be announced, AddDirectLinkAddr would never be called, the
// independent TCP server would never dial a producer, and
// pbft_getArbiterPeersInfo would report zero 2WayConnection -- with no error
// anywhere to explain it.
//
// So this test asserts the announcement happens with 3 peers. It fails if someone
// "fixes" the empty block by adding `continue` or `return`.
func TestAnnounceWithFewPeers(t *testing.T) {
	// self + 2 other producers.
	const peersOnRig = 3
	pids := make([]peer.PID, peersOnRig)
	for i := 0; i < peersOnRig; i++ {
		_, pub, err := crypto.GenerateKeyPair()
		if err != nil {
			t.Fatalf("GenerateKeyPair: %v", err)
		}
		enc, err := pub.EncodePoint(true)
		if err != nil {
			t.Fatalf("EncodePoint: %v", err)
		}
		copy(pids[i][:], enc)
	}

	if peersOnRig >= minPeersToAnnounce {
		t.Fatalf("rig has %d peers, gate is %d: this test no longer exercises the gate",
			peersOnRig, minPeersToAnnounce)
	}

	relayed := make(chan struct{}, 8)
	r := New(&Config{
		PID:        pids[0][:],
		Addr:       "127.0.0.1:20338",
		TimeSource: dtime.NewMedianTime(),
		Sign: func(data []byte) []byte {
			// appendAddr stores the DAddr without verifying the signature, so
			// any placeholder is fine here; a real one would need the private key.
			return make([]byte, crypto.SignatureLength)
		},
		IsCurrent: func() bool { return true },
		RelayAddr: func(iv *msg.InvVect, data interface{}) {
			select {
			case relayed <- struct{}{}:
			default:
			}
		},
		OnCipherAddr: func(pid peer.PID, cipher []byte) {},
	})
	defer r.Stop()
	r.Start()

	// Fill peerCache with 3 connected peers, i.e. the state the gate looks at.
	for i := 0; i < peersOnRig; i++ {
		r.NewPeer(&fakeRoutesPeer{id: i})
	}

	// Becoming a producer triggers announceAddr -> announce <- struct{}{}.
	r.PeersChanged(pids)

	// Two relays: one DAddr per non-self producer. Nothing announces for self.
	timeout := time.After(10 * time.Second)
	for got := 0; got < peersOnRig-1; got++ {
		select {
		case <-relayed:
		case <-timeout:
			t.Fatalf("only %d of %d DAddr relays observed with %d peers; "+
				"the announce gate at routes.go must stay empty (m4 O7)",
				got, peersOnRig-1, peersOnRig)
		}
	}
}

// TestAnnounceIsSuppressedWhenNotCurrent covers the gate that IS live: an
// unsynced chain must not announce, because the address would point at a node
// that cannot serve blocks yet.
func TestAnnounceIsSuppressedWhenNotCurrent(t *testing.T) {
	_, pub, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	enc, err := pub.EncodePoint(true)
	if err != nil {
		t.Fatalf("EncodePoint: %v", err)
	}
	var self peer.PID
	copy(self[:], enc)

	relayed := make(chan struct{}, 8)
	r := New(&Config{
		PID:        self[:],
		Addr:       "127.0.0.1:20338",
		TimeSource: dtime.NewMedianTime(),
		Sign:       func(data []byte) []byte { return make([]byte, crypto.SignatureLength) },
		IsCurrent:  func() bool { return false }, // not synced
		RelayAddr: func(iv *msg.InvVect, data interface{}) {
			select {
			case relayed <- struct{}{}:
			default:
			}
		},
		OnCipherAddr: func(pid peer.PID, cipher []byte) {},
	})
	defer r.Stop()
	r.Start()

	r.PeersChanged([]peer.PID{self})

	select {
	case <-relayed:
		t.Fatal("relayed an address while IsCurrent() is false; announceAddr must bail out")
	case <-time.After(500 * time.Millisecond):
		// expected: nothing announced
	}
}

// warnRecorder keeps every record the root logger hands out, so a test can
// assert on LEVEL, not just on wording. recordingHandler in log_test.go only
// keeps the message.
type warnRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *warnRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Record.Clone because the caller may reuse the record once Handle returns.
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnRecorder) WithGroup(string) slog.Handler      { return h }

func (h *warnRecorder) warnings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []string
	for _, r := range h.records {
		if r.Level >= slog.LevelWarn {
			out = append(out, r.Message)
		}
	}
	return out
}

// newTestState builds the slice of state the two peer-cache handlers touch.
func newTestState() *state {
	return &state{
		peers:     make(map[peer.PID]struct{}),
		requested: make(map[common.Uint256]struct{}),
		peerCache: make(map[string]*cache),
	}
}

// TestPeerCacheFoldsBothLayersForOneNode pins the reason peerCache is keyed by
// ID. The p2p server announces every connection and eth/64 announces the ones
// that can carry an ELAMSG, so the same remote node arrives twice, from two
// different objects. Object keying would leave two entries: DonePeer only
// deletes the one it is handed, and the survivor would never be removed.
func TestPeerCacheFoldsBothLayersForOneNode(t *testing.T) {
	const node = 7
	id := fmt.Sprintf("peer-%d", node)

	r := &Routes{}
	s := newTestState()

	// p2p first: it sees the connection before any subprotocol is negotiated.
	r.handleNewPeer(s, &fakeRoutesPeer{id: node})
	// An in-flight request must survive the second notification.
	hash := common.Uint256{1}
	s.peerCache[id].requested[hash] = struct{}{}

	// eth/64 second: a different object for the same node.
	r.handleNewPeer(s, &fakeRoutesPeer{id: node})

	if len(s.peerCache) != 1 {
		t.Fatalf("peerCache holds %d entries for one node, want 1: %v", len(s.peerCache), s.peerCache)
	}
	if _, ok := s.peerCache[id]; !ok {
		t.Fatalf("peerCache is not keyed by peer ID: %v", s.peerCache)
	}
	if _, ok := s.peerCache[id].requested[hash]; !ok {
		t.Fatal("the second announcement replaced the cache entry and dropped an in-flight request; " +
			"its DAddr reply would then look unsolicited and disconnect a healthy peer")
	}
}

// TestDonePeerIsIdempotent covers the other half of the double notification:
// both layers report the drop, so the second DonePeer always finds nothing.
// That must be silent and harmless -- a warning would fire on every disconnect,
// twice per reconnect.
func TestDonePeerIsIdempotent(t *testing.T) {
	previous := glog.Root()
	recorder := &warnRecorder{}
	glog.SetDefault(glog.NewLogger(recorder))
	defer glog.SetDefault(previous)

	const node = 7

	r := &Routes{}
	s := newTestState()
	r.handleNewPeer(s, &fakeRoutesPeer{id: node})

	r.handleDonePeer(s, &fakeRoutesPeer{id: node}) // p2p
	r.handleDonePeer(s, &fakeRoutesPeer{id: node}) // eth/64, entry already gone

	if len(s.peerCache) != 0 {
		t.Fatalf("peerCache still holds %d entries after both DonePeer notifications: %v",
			len(s.peerCache), s.peerCache)
	}
	if w := recorder.warnings(); len(w) != 0 {
		t.Fatalf("the duplicate DonePeer logged %d warning(s): %q", len(w), w)
	}
}
