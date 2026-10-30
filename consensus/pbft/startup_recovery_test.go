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
	"testing"
	"time"

	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"

	"github.com/ethereum/go-ethereum/params"
)

// sawConsensusRequest reports whether the engine asked the set for its status,
// which is the first thing recovery does.
func sawConsensusRequest(net *stubDPOSNetwork) bool {
	for _, s := range net.sends() {
		if s.cmd == emsg.CmdRequestConsensus {
			return true
		}
	}
	return false
}

// TestStartRecoveryWaitsForAMajority pins the gate that makes the cold start work
// at all: recovery must not run while the TCP layer has not connected a majority
// yet.
//
// This is not defensive coding, it is the observed failure. startPBFTNetwork used
// to announce the peer set and stop there; every producer then logged
// "waiting for consensus status recovery" every ten seconds forever, because
// recoverAbnormalState refuses to run below a majority and nothing retried once
// the connections came up.
func TestStartRecoveryWaitsForAMajority(t *testing.T) {
	accounts, keys := newProducerSet(t, 4)
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: keys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.SetIsCurrent(func() bool { return true })

	majority := engine.minAcceptVoteCount()
	if majority != 2 {
		t.Fatalf("majority of 4 producers = %d, want 2; the test's peer counts assume 2", majority)
	}
	// One peer is below the majority of two.
	net := &stubDPOSNetwork{peers: majority - 1}
	engine.AttachNetwork(net)

	engine.StartRecovery()
	time.Sleep(2500 * time.Millisecond)

	if sawConsensusRequest(net) {
		t.Fatalf("recovery ran with %d peers, majority is %d", majority-1, majority)
	}
}

// TestStartRecoveryRunsOnceEnoughPeersConnect is the half that matters, and it
// has to start below the majority and then cross it: the point of StartRecovery
// is not "call OnRecover", it is "keep waiting and call it when the set finally
// shows up".
//
// A version that calls OnRecover once and gives up would pass a test that starts
// with enough peers -- recoverAbnormalState would just do the work -- and would
// still be broken in production, where the TCP dials take a few seconds and the
// first attempt therefore always fails.
func TestStartRecoveryRunsOnceEnoughPeersConnect(t *testing.T) {
	accounts, keys := newProducerSet(t, 4)
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: keys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.SetIsCurrent(func() bool { return true })

	majority := engine.minAcceptVoteCount()
	// Start one peer short, exactly like a node whose dials have not landed yet.
	net := &stubDPOSNetwork{peers: majority - 1}
	engine.AttachNetwork(net)

	engine.StartRecovery()

	// It must still be waiting, not have given up.
	time.Sleep(1200 * time.Millisecond)
	if sawConsensusRequest(net) {
		t.Fatal("recovery ran before the peer majority was reached")
	}

	// The last dial lands.
	net.mu.Lock()
	net.peers = majority
	net.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sawConsensusRequest(net) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("recovery never asked the set for its consensus status after peers connected")
}
