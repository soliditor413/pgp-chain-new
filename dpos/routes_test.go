// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dpos

import (
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/p2p/msg"
)

// fakeRoutesPeer is the IPeer slice that Routes actually uses: disconnecting and
// sending an ELAMSG. Both are no-ops; the announce path only needs the peer to
// exist as a map key in state.peerCache.
type fakeRoutesPeer struct {
	id int
}

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
