// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

import (
	"testing"

	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// TestP2PDPOSPeerIDMatchesEth64 pins the assumption the whole two-layer design
// rests on: the p2p adapter and the eth/64 peer spell the same node the same
// way. dpos keys its per-peer cache by that string, so a difference -- a 0x
// prefix on one side, upper case on the other -- would silently produce two
// entries for one node, and the p2p one would never be removed.
//
// The eth/64 peer is built the way handler.run builds it, from
// p2pPeer.ID().String(), so this compares the adapter against the real
// construction path rather than against a hardcoded format.
func TestP2PDPOSPeerIDMatchesEth64(t *testing.T) {
	for i := 0; i < 4; i++ {
		var id enode.ID
		id[0] = byte(i + 1)
		id[len(id)-1] = 0xab

		ethPeer := eth64.NewPeer(eth64.ETH64, id.String(), nil)
		adapter := newP2PDPOSPeer(nil, nil, id)

		if got, want := adapter.ID(), ethPeer.ID(); got != want {
			t.Fatalf("adapter ID %q != eth/64 peer ID %q for node %s", got, want, id)
		}
		if got, want := adapter.ID(), id.String(); got != want {
			t.Fatalf("adapter ID %q != enode.ID.String() %q", got, want)
		}
	}
}
