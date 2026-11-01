// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestBestPeerSkipsTheElaMsgCapability is the guard for the cost of sharing one
// handler between the eth/64 and elamsg capabilities.
//
// The peer set is what BroadcastElaMsg iterates, so elamsg connections must
// register in it. It is also what chainSyncer.loop picks from, and an elamsg
// peer has no Status exchange: head zero, TD 0. If BestPeer returned one, the
// syncer would send GetBlockHeaders into the elamsg code window, where the far
// end discards it as an unknown code -- an unanswerable request retried every
// syncLoopInterval.
//
// The elamsg peer is given the HIGHER TD on purpose: with the version filter
// removed, TD ordering alone would pick it and this test would fail, so it
// proves the filter rather than the ordering.
func TestBestPeerSkipsTheElaMsgCapability(t *testing.T) {
	ps := newPeerSet()

	ela := NewPeer(ElaMsgProtocolVersion, "elamsg-peer", nil)
	ela.SetHead(common.Hash{0x01}, big.NewInt(100))
	eth := NewPeer(ETH64, "eth-peer", nil)
	eth.SetHead(common.Hash{0x02}, big.NewInt(50))

	if err := ps.Register(ela); err != nil {
		t.Fatalf("cannot register the elamsg peer: %v", err)
	}
	if err := ps.Register(eth); err != nil {
		t.Fatalf("cannot register the eth/64 peer: %v", err)
	}

	if best := ps.BestPeer(); best != eth {
		t.Fatalf("BestPeer returned %v, want the eth/64 peer: an elamsg peer carries no block traffic",
			best)
	}

	// With only an elamsg peer the syncer must see nil and skip the round
	// instead of issuing a request nothing can answer.
	if err := ps.Unregister(eth.ID()); err != nil {
		t.Fatalf("cannot unregister the eth/64 peer: %v", err)
	}
	if best := ps.BestPeer(); best != nil {
		t.Fatalf("BestPeer returned an elamsg peer when it is the only one in the set: %v", best)
	}
}
