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

	"github.com/elastos/Elastos.ELA/dpos/p2p"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"
)

// stubServer is a p2p.Server that counts Stop calls and does nothing else.
//
// Only Stop matters here: the behaviour under test is "how many times does the
// network forward a shutdown", not the server's own semantics.
type stubServer struct {
	stopCalls int
}

func (s *stubServer) Start()                                                     {}
func (s *stubServer) Stop() error                                                { s.stopCalls++; return nil }
func (s *stubServer) AddAddr(pid peer.PID, addr string)                          {}
func (s *stubServer) ConnectPeers(cur, next []peer.PID)                          {}
func (s *stubServer) ConnectedPeers() []p2p.Peer                                 { return nil }
func (s *stubServer) ConnectedCurrentPeers() []p2p.Peer                          { return nil }
func (s *stubServer) DumpPeersInfo() []*p2p.PeerInfo                             { return nil }
func (s *stubServer) BroadcastMessage(msg elap2p.Message, exclPeers ...peer.PID) {}
func (s *stubServer) SendMessageToPeer(pid peer.PID, msg elap2p.Message) error   { return nil }

// newStubNetwork builds the minimal Network that Stop touches: quit and
// p2pServer. Nothing else on the struct is read by Stop.
func newStubNetwork() (*Network, *stubServer) {
	server := &stubServer{}
	return &Network{quit: make(chan struct{}), p2pServer: server}, server
}

// TestNetworkStopIsIdempotent pins the fix for the upstream hang.
//
// pgp-chain/dpos/network.go:155-158 does `n.quit <- true` on an unbuffered
// channel. The first call is consumed by the event loop, but the loop then
// breaks out -- so a second Stop sends to a channel nobody reads and blocks
// forever. The same file's sibling implementation, routes.go:218-223, already
// does CAS-once + close(); this test makes sure network.go stays on that shape.
func TestNetworkStopIsIdempotent(t *testing.T) {
	n, server := newStubNetwork()

	if err := n.Stop(); err != nil {
		t.Fatalf("first Stop() = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() { done <- n.Stop() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Stop() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Stop() blocked; quit must be closed once, not sent on twice")
	}

	if server.stopCalls != 1 {
		t.Fatalf("p2pServer.Stop called %d times, want 1", server.stopCalls)
	}
}

// TestNetworkStopBeforeStartDoesNotBlock covers the startup-failure path: the
// assembly layer learns the config is bad after constructing the Network but
// before Start, and calls Stop on it. Upstream blocks there too, because no
// event loop exists to receive from quit.
func TestNetworkStopBeforeStartDoesNotBlock(t *testing.T) {
	n, server := newStubNetwork()

	done := make(chan error, 1)
	go func() { done <- n.Stop() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop() before Start = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() before Start() blocked")
	}

	if server.stopCalls != 1 {
		t.Fatalf("p2pServer.Stop called %d times, want 1", server.stopCalls)
	}
}

// TestNetworkStopClosesQuit makes sure the event loop is actually released:
// close(quit) is the only thing that ends the goroutine Start spawns.
func TestNetworkStopClosesQuit(t *testing.T) {
	n, _ := newStubNetwork()

	if err := n.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	select {
	case <-n.quit:
		// closed, as expected
	default:
		t.Fatal("quit is still open after Stop; the Start goroutine would never exit")
	}
}
