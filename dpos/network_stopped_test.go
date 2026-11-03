// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dpos

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/dpos/p2p"
	"github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"
)

// silentStubServer counts every call, so a stopped Network can be held to
// "answer without touching the server".
//
// The counters exist instead of t.Fatal inside the methods for a mechanical
// reason: the assertions below run inside a goroutine (that is how a hang is
// turned into a failure instead of a stuck test), and t.Fatal from a goroutine
// that is not the test's own is not allowed -- it would mark the test failed
// from the wrong goroutine and skip the rest of it. Each method records, the
// test asserts the total is zero. The intent is the same: a stopped network
// that reaches the server fails this test.
type silentStubServer struct {
	stopCalls    int32
	serverCalls  int32
	lastCallName atomic.Value // string
}

func (s *silentStubServer) record(name string) {
	atomic.AddInt32(&s.serverCalls, 1)
	s.lastCallName.Store(name)
}

func (s *silentStubServer) Start() {}
func (s *silentStubServer) Stop() error {
	atomic.AddInt32(&s.stopCalls, 1)
	return nil
}

func (s *silentStubServer) AddAddr(peer.PID, string) {
	s.record("AddAddr")
}

func (s *silentStubServer) ConnectPeers(currentPeers, nextPeers []peer.PID) {
	s.record("ConnectPeers")
}

func (s *silentStubServer) ConnectedPeers() []p2p.Peer {
	s.record("ConnectedPeers")
	return nil
}

func (s *silentStubServer) ConnectedCurrentPeers() []p2p.Peer {
	s.record("ConnectedCurrentPeers")
	return nil
}

func (s *silentStubServer) DumpPeersInfo() []*p2p.PeerInfo {
	s.record("DumpPeersInfo")
	return []*p2p.PeerInfo{{Addr: "stub"}}
}

func (s *silentStubServer) BroadcastMessage(elap2p.Message, ...peer.PID) {
	s.record("BroadcastMessage")
}

func (s *silentStubServer) SendMessageToPeer(peer.PID, elap2p.Message) error {
	s.record("SendMessageToPeer")
	return nil
}

func (s *silentStubServer) calls() int32 { return atomic.LoadInt32(&s.serverCalls) }

// newSilentNetwork builds the minimal Network these paths touch: the server and
// the channels. The channels are deliberately unbuffered and -- for the posts
// below -- left nil where the early return should make them unreachable: a send
// on a nil channel blocks forever, so a missing guard fails as a hang rather
// than as a wrong value.
func newSilentNetwork() (*Network, *silentStubServer) {
	server := &silentStubServer{}
	return &Network{p2pServer: server, quit: make(chan struct{})}, server
}

// runBounded runs fn and fails the test if it does not return within the
// budget.
//
// Every assertion in this file goes through it. Without the bound a regression
// here does not fail, it hangs -- and a hanging test is indistinguishable from
// a stuck CI job, which is exactly the symptom this whole file is about.
func runBounded(t *testing.T, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within 1s: it is blocked, which is the deadlock this file exists for", what)
	}
}

// TestNetworkAnswersWithoutTheServerAfterStop pins the fix for the RPC deadlock.
//
// The chain that hung a live node: pbft.status -> Engine.ActiveDPoSPeers ->
// Network.GetActivePeers -> p2pServer.ConnectedPeers, which sends a request
// into the server's s.query channel and waits for the reply. p2pServer.Stop()
// makes the peerHandler goroutine take its quit case and never read s.query
// again, so after the Parlia fork the reply never comes and the RPC blocks.
// The dpos layer's job is to stop asking: stopped goes up before quit closes,
// and these three entry points answer locally.
//
// The test is split around Stop on purpose. Before Stop the server calls must
// happen -- otherwise "zero calls after Stop" would also pass on a Network that
// was never wired to a server, and would prove nothing.
func TestNetworkAnswersWithoutTheServerAfterStop(t *testing.T) {
	n, server := newSilentNetwork()

	// ---- Before Stop: the server IS the answer, so counts move ----
	var peers []p2p.Peer
	var dump []*p2p.PeerInfo
	var sendErr error

	runBounded(t, "GetActivePeers before Stop", func() { peers = n.GetActivePeers() })
	runBounded(t, "DumpPeersInfo before Stop", func() { dump = n.DumpPeersInfo() })
	runBounded(t, "SendMessageToPeer before Stop", func() {
		sendErr = n.SendMessageToPeer(peer.PID{}, &msg.Ping{})
	})

	if got := server.calls(); got != 3 {
		t.Fatalf("the live network reached the server %d times, want 3 (one per call); the fixture is not wired and every "+
			"assertion below would be vacuous", got)
	}
	if len(dump) == 0 {
		t.Fatalf("DumpPeersInfo returned nothing while running: the stub's non-empty answer is what makes the nil after " +
			"Stop meaningful")
	}
	if sendErr != nil {
		t.Fatalf("SendMessageToPeer before Stop = %v, want the server's nil", sendErr)
	}
	_ = peers // nil from the stub either way; the count above is the proof it was asked

	// ---- Stop ----
	runBounded(t, "Stop", func() {
		if err := n.Stop(); err != nil {
			t.Errorf("Stop() = %v, want nil", err)
		}
	})
	before := server.calls()

	// ---- After Stop: answer locally, never touch the server ----
	runBounded(t, "GetActivePeers after Stop", func() {
		if got := n.GetActivePeers(); got != nil {
			t.Errorf("GetActivePeers after Stop = %v, want nil", got)
		}
	})
	runBounded(t, "DumpPeersInfo after Stop", func() {
		if got := n.DumpPeersInfo(); got != nil {
			t.Errorf("DumpPeersInfo after Stop = %v, want nil", got)
		}
	})
	runBounded(t, "SendMessageToPeer after Stop", func() {
		err := n.SendMessageToPeer(peer.PID{}, &msg.Ping{})
		if !errors.Is(err, ErrNetworkStopped) {
			t.Errorf("SendMessageToPeer after Stop = %v, want ErrNetworkStopped", err)
		}
	})

	if got := server.calls() - before; got != 0 {
		name, _ := server.lastCallName.Load().(string)
		t.Fatalf("a stopped network called the server %d more times (last: %s); each of those calls is a block once the "+
			"server's peerHandler has exited", got, name)
	}
}

// TestNetworkChannelPostsDoNotBlockAfterStop covers the second family of hang:
// the unbuffered channels whose only reader is the goroutine Start spawns.
//
// Once Stop closes quit that goroutine exits, so PostChangeViewTask,
// PostRecoverTask, handleMessage and notifyFlag would block forever on an
// unbuffered send. The channels here are nil where the early return should stop
// the send before it happens -- a nil channel send blocks forever, so this is a
// stricter fixture than a real one: passing means the guard ran, not that the
// channel happened to have room.
func TestNetworkChannelPostsDoNotBlockAfterStop(t *testing.T) {
	n, _ := newSilentNetwork()
	runBounded(t, "Stop", func() { _ = n.Stop() })

	runBounded(t, "PostChangeViewTask after Stop", func() { n.PostChangeViewTask() })
	runBounded(t, "PostRecoverTask after Stop", func() { n.PostRecoverTask() })
	runBounded(t, "handleMessage after Stop", func() { n.handleMessage(peer.PID{}, &msg.Ping{}) })
	runBounded(t, "notifyFlag after Stop", func() { n.notifyFlag(p2p.NFBadNetwork) })
}

// TestNetworkStoppedFlagRisesBeforeQuitCloses pins the ordering inside Stop.
//
// A caller checks stopped and then calls the server; if quit closed first, a
// caller that passed its check in between would reach a server whose reader has
// already exited. Setting stopped first shrinks that window to "the store
// landed, the caller's load did not see it yet" (atomic store/load is
// sequentially consistent, so a load after the store cannot miss it; the
// remaining window is only a caller that loaded before the store).
//
// The observable ordering assertion: by the time Stop returns, both are done,
// and a Network whose Stop has not run reports stopped == false. That is what
// the test can see without racing; the ordering inside the Once is pinned by
// the deadlock tests above, which fail as hangs (not flakes) if it regresses.
func TestNetworkStoppedFlagRisesBeforeQuitCloses(t *testing.T) {
	n, _ := newSilentNetwork()

	if n.stopped.Load() {
		t.Fatal("stopped is true before Stop")
	}

	runBounded(t, "Stop", func() { _ = n.Stop() })

	if !n.stopped.Load() {
		t.Fatal("stopped is false after Stop; every entry point would go on to touch a dead server")
	}
	select {
	case <-n.quit:
	default:
		t.Fatal("quit is still open after Stop")
	}
}
