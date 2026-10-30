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
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/dpos/p2p"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/events"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/params"
)

// updateRecorder is a DPOSNetwork that records UpdatePeers.
//
// UpdatePeers must not block: it is called from a goroutine the engine spawns,
// but the event bus holds its lock while delivering to subscribers, so a
// blocking recorder would wedge every other subscriber in the package.
type updateRecorder struct {
	mu      sync.Mutex
	current []dpeer.PID
	next    []dpeer.PID
	calls   int
	got     chan struct{}
}

func (u *updateRecorder) Start()      {}
func (u *updateRecorder) Stop() error { return nil }

func (u *updateRecorder) SendMessageToPeer(dpeer.PID, elap2p.Message) error { return nil }
func (u *updateRecorder) BroadcastMessage(elap2p.Message)                   {}
func (u *updateRecorder) GetActivePeers() []p2p.Peer                        { return nil }

func (u *updateRecorder) UpdatePeers(current, next []dpeer.PID) {
	u.mu.Lock()
	u.current = append([]dpeer.PID(nil), current...)
	u.next = append([]dpeer.PID(nil), next...)
	u.calls++
	u.mu.Unlock()
	select {
	case u.got <- struct{}{}:
	default:
	}
}

func (u *updateRecorder) snapshot() ([]dpeer.PID, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]dpeer.PID(nil), u.current...), u.calls
}

// TestWatchPeerSetForwardsToTheNetwork is the guard for a seam that was missing
// entirely: AnnounceDAddr published ETDirectPeersChanged, dpos.Routes consumed it
// to build DAddr records, UpdatePeers existed on the network and was never
// called. The symptom was a four-node bench with four healthy "DPoS network
// started" lines, a correct announce, and not one DPoS TCP connection.
func TestWatchPeerSetForwardsToTheNetwork(t *testing.T) {
	accounts, keys := newProducerSet(t, 3)
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: keys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])

	rec := &updateRecorder{got: make(chan struct{}, 8)}
	engine.AttachNetwork(rec)
	engine.WatchPeerSet()

	if !engine.AnnounceDAddr() {
		t.Fatal("AnnounceDAddr returned false for a node with a producer key")
	}

	select {
	case <-rec.got:
	case <-time.After(2 * time.Second):
		t.Fatal("UpdatePeers was never called: the announced peer set did not reach the network")
	}

	current, calls := rec.snapshot()
	if calls != 1 {
		t.Errorf("UpdatePeers called %d times, want 1", calls)
	}
	if len(current) != len(keys) {
		t.Fatalf("UpdatePeers got %d peers, want %d", len(current), len(keys))
	}
	want := make(map[dpeer.PID]bool, len(keys))
	for _, k := range keys {
		blob, err := hex.DecodeString(k)
		if err != nil {
			t.Fatalf("producer key %q is not hex: %v", k, err)
		}
		var pid dpeer.PID
		copy(pid[:], blob)
		want[pid] = true
	}
	for _, pid := range current {
		if !want[pid] {
			t.Errorf("UpdatePeers got an unexpected peer %x", pid[:])
		}
	}
}

// TestWatchPeerSetWithoutNetworkIsInert covers a verifier or a single-producer
// node: no network was attached, so a notification must be dropped rather than
// dereference a nil interface.
func TestWatchPeerSetWithoutNetworkIsInert(t *testing.T) {
	engine := New(&params.LegacyConfig{}, &Config{})
	engine.WatchPeerSet()

	done := make(chan struct{})
	go func() {
		events.Notify(dpos.ETDirectPeersChanged, &dpeer.PeersInfo{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifying without an attached network blocked")
	}
}
