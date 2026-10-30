// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/dpos/elamsg"
	"github.com/ethereum/go-ethereum/p2p"
)

// recordingHooks captures the three DPoS callbacks.
type recordingHooks struct {
	mu       sync.Mutex
	up       int
	down     int
	elaMsgs  []*elamsg.ElaMsg
	elaPeers []*Peer
}

func (r *recordingHooks) PeerUp(*Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.up++
}

func (r *recordingHooks) PeerDown(*Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down++
}

func (r *recordingHooks) ElaMsg(peer *Peer, msg *elamsg.ElaMsg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.elaMsgs = append(r.elaMsgs, msg)
	r.elaPeers = append(r.elaPeers, peer)
}

func (r *recordingHooks) snapshot() (int, int, []*elamsg.ElaMsg, []*Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.up, r.down, append([]*elamsg.ElaMsg(nil), r.elaMsgs...), append([]*Peer(nil), r.elaPeers...)
}

// TestELAMSGReachesTheHooks is the M3/M4 seam: 0x08 must decode into the
// address-routing envelope and be handed to the subscriber, not discarded.
//
// This is the inbound half of the only mechanism by which a producer learns the
// IP:port of the others -- dpos/p2p has no static seed list -- so a regression
// here shows up as four healthy-looking nodes that never connect on :20338.
func TestELAMSGReachesTheHooks(t *testing.T) {
	remote, local := p2p.MsgPipe()
	defer remote.Close()
	defer local.Close()

	hooks := &recordingHooks{}
	peer := NewPeer(ETH64, "test-peer", local)
	h := newHandler(nil, 860621, hooks)
	defer h.Close()

	done := make(chan error, 1)
	go func() { done <- h.dispatch(peer) }()

	want := &elamsg.ElaMsg{Type: elamsg.DAddr, Msg: []byte{1, 2, 3, 4}}
	if err := p2p.Send(remote, ELAMSG, want); err != nil {
		t.Fatalf("cannot send ELAMSG: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, msgs, _ := hooks.snapshot(); len(msgs) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	_, _, msgs, peers := hooks.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("hooks saw %d ELAMSG, want 1", len(msgs))
	}
	if msgs[0].Type != want.Type {
		t.Fatalf("message type = %d, want %d", msgs[0].Type, want.Type)
	}
	if string(msgs[0].Msg) != string(want.Msg) {
		t.Fatalf("message body = %x, want %x", msgs[0].Msg, want.Msg)
	}
	if peers[0] != peer {
		t.Fatal("the envelope arrived without its sender, so a DAddr reply cannot be routed")
	}

	remote.Close()
	<-done
}

// TestMalformedELAMSGDoesNotKillTheConnection keeps the property the old
// discard-only path had, now that 0x08 is really decoded.
//
// pgp-chain returns ErrDecode here (eth/handler.go:750) and drops the peer. This
// tree does not, because the same message code is also the "unknown code" guard:
// tearing down a connection over one unparsable address gossip turns a version
// skew into a peer-churn loop, and the routing layer loses nothing by ignoring
// it. What must NOT happen is the connection surviving while the loop spins on
// the same bad bytes, which is why the message is consumed either way.
func TestMalformedELAMSGDoesNotKillTheConnection(t *testing.T) {
	remote, local := p2p.MsgPipe()
	defer remote.Close()
	defer local.Close()

	hooks := &recordingHooks{}
	peer := NewPeer(ETH64, "test-peer", local)
	h := newHandler(nil, 860621, hooks)
	defer h.Close()

	done := make(chan error, 1)
	go func() { done <- h.dispatch(peer) }()

	// A dpos-shaped but not ElaMsg-shaped payload: two RLP items where ElaMsg is
	// a 2-tuple with a uint8 first field, so decoding fails.
	if err := p2p.Send(remote, ELAMSG, []interface{}{"dpos", []byte{0xde, 0xad}}); err != nil {
		t.Fatalf("cannot send malformed ELAMSG: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if _, _, msgs, _ := hooks.snapshot(); len(msgs) != 0 {
		t.Fatalf("an undecodable ELAMSG reached the hooks %d times", len(msgs))
	}
	select {
	case err := <-done:
		t.Fatalf("dispatch returned early on a malformed ELAMSG: %v", err)
	default:
	}

	// Still alive: a well formed message afterwards must still arrive.
	good := &elamsg.ElaMsg{Type: elamsg.Inv, Msg: []byte{9}}
	if err := p2p.Send(remote, ELAMSG, good); err != nil {
		t.Fatalf("cannot send ELAMSG after the malformed one: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, msgs, _ := hooks.snapshot(); len(msgs) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, _, msgs, _ := hooks.snapshot(); len(msgs) != 1 {
		t.Fatalf("the connection stopped delivering ELAMSG after one bad message (%d delivered)", len(msgs))
	}

	remote.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Logf("dispatch ended with %v (EOF-like is expected)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return after the pipe closed")
	}
}

// TestELAMSGIsNotDecodedWithoutHooks: a chain that does not run DPoS must not pay
// for decoding, and must not fail either. nil hooks is the BSC/Rialto shape.
func TestELAMSGIsNotDecodedWithoutHooks(t *testing.T) {
	remote, local := p2p.MsgPipe()
	defer remote.Close()
	defer local.Close()

	peer := NewPeer(ETH64, "test-peer", local)
	h := newHandler(nil, 860621, nil)
	defer h.Close()

	done := make(chan error, 1)
	go func() { done <- h.dispatch(peer) }()

	if err := p2p.Send(remote, ELAMSG, []interface{}{"dpos", []byte{1}}); err != nil {
		t.Fatalf("cannot send ELAMSG: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("dispatch returned on ELAMSG with no hooks installed: %v", err)
	default:
	}

	remote.Close()
	<-done
}
