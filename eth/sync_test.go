// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"
)

// Tests that snap sync is disabled after a successful sync cycle.
func TestSnapSyncDisabling68(t *testing.T) { testSnapSyncDisabling(t, eth.ETH68, snap.SNAP1) }

// Tests that snap sync gets disabled as soon as a real block is successfully
// imported into the blockchain.
func testSnapSyncDisabling(t *testing.T, ethVer uint, snapVer uint) {
	t.Parallel()

	// Create an empty handler and ensure it's in snap sync mode
	empty := newTestHandler(ethconfig.SnapSync)
	defer empty.close()

	// Create a full handler and ensure snap sync ends up disabled
	full := newTestHandlerWithBlocks(1024, ethconfig.SnapSync)
	defer full.close()

	// Sync up the two handlers via both `eth` and `snap`
	caps := []p2p.Cap{{Name: "eth", Version: ethVer}, {Name: "snap", Version: snapVer}}

	emptyPipeEth, fullPipeEth := p2p.MsgPipe()
	defer emptyPipeEth.Close()
	defer fullPipeEth.Close()

	emptyPeerEth := eth.NewPeer(ethVer, p2p.NewPeer(enode.ID{1}, "", caps), emptyPipeEth, empty.txpool, nil)
	fullPeerEth := eth.NewPeer(ethVer, p2p.NewPeer(enode.ID{2}, "", caps), fullPipeEth, full.txpool, nil)
	defer emptyPeerEth.Close()
	defer fullPeerEth.Close()

	go empty.handler.runEthPeer(emptyPeerEth, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(empty.handler), peer)
	})
	go full.handler.runEthPeer(fullPeerEth, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(full.handler), peer)
	})

	emptyPipeSnap, fullPipeSnap := p2p.MsgPipe()
	defer emptyPipeSnap.Close()
	defer fullPipeSnap.Close()

	emptyPeerSnap := snap.NewPeer(snapVer, p2p.NewPeer(enode.ID{1}, "", caps), emptyPipeSnap)
	fullPeerSnap := snap.NewPeer(snapVer, p2p.NewPeer(enode.ID{2}, "", caps), fullPipeSnap)

	go empty.handler.runSnapExtension(emptyPeerSnap, func(peer *snap.Peer) error {
		return snap.Handle((*snapHandler)(empty.handler), peer)
	})
	go full.handler.runSnapExtension(fullPeerSnap, func(peer *snap.Peer) error {
		return snap.Handle((*snapHandler)(full.handler), peer)
	})
	// Wait a bit for the above handlers to start
	time.Sleep(250 * time.Millisecond)

	// Check that snap sync was disabled
	op := peerToSyncOp(ethconfig.SnapSync, empty.handler.peers.peerWithHighestTD())
	if err := empty.handler.doSync(op); err != nil {
		t.Fatal("sync failed:", err)
	}
	// Snap sync and mode switching happen asynchronously, poll for completion.
	timeout := time.NewTimer(15 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer timeout.Stop()
	defer tick.Stop()

	for {
		select {
		case <-timeout.C:
			t.Fatalf("snap sync not disabled after successful synchronisation")
		case <-tick.C:
			if empty.handler.synced.Load() && empty.handler.downloader.ConfigSyncMode() == ethconfig.FullSync {
				return
			}
		}
	}
}

// TestAlreadyInSyncMarksTheNodeSynced verifies that nextSyncOp marks the node as
// synced when the local chain is already at (or above) the best peer's TD.
//
// A node restarting next to a peer with an equal TD takes that branch, which used
// to set acceptTxs alone and left handler.synced == false for the life of the
// process. synced is the gate of the block fetcher's inserter (handler.go), so
// every propagated block was discarded and the node could only advance through
// the PBFT direct-insert path.
func TestAlreadyInSyncMarksTheNodeSynced(t *testing.T) {
	t.Parallel()

	// Local node and peers share the same genesis-only chain, so the highest peer
	// TD equals our own and nextSyncOp takes the "already in sync" branch instead
	// of returning a sync operation.
	local := newTestHandler(ethconfig.FullSync)
	defer local.close()

	// nextSyncOp returns early unless the peer set is at least defaultMinSyncPeers.
	const peers = defaultMinSyncPeers

	sinks := make([]*testHandler, peers)
	for i := 0; i < peers; i++ {
		sinks[i] = newTestHandler(ethconfig.FullSync)
		defer sinks[i].close()

		localPipe, sinkPipe := p2p.MsgPipe()
		defer localPipe.Close()
		defer sinkPipe.Close()

		localPeer := eth.NewPeer(eth.ETH68, p2p.NewPeer(enode.ID{byte(i + 1)}, "", nil), localPipe, local.txpool, nil)
		sinkPeer := eth.NewPeer(eth.ETH68, p2p.NewPeer(enode.ID{0}, "", nil), sinkPipe, sinks[i].txpool, nil)
		defer localPeer.Close()
		defer sinkPeer.Close()

		go local.handler.runEthPeer(localPeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(local.handler), peer)
		})
		go sinks[i].handler.runEthPeer(sinkPeer, func(peer *eth.Peer) error {
			return eth.Handle((*ethHandler)(sinks[i].handler), peer)
		})
	}
	// Peers only enter the set once their handshake completed.
	for deadline := time.Now().Add(10 * time.Second); local.handler.peers.len() < peers; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("peers not connected: have %d, want %d", local.handler.peers.len(), peers)
		}
	}
	// Same TD on both sides: this is the restart case, not a real sync.
	peer := local.handler.peers.peerWithHighestTD()
	if peer == nil {
		t.Fatal("no peer with a TD in the peer set")
	}
	_, ourTD := (&chainSyncer{handler: local.handler}).modeAndLocalHead()
	_, peerTD := peer.Head()
	if peerTD.Cmp(ourTD) > 0 {
		t.Fatalf("peer td %v above local td %v, test setup would sync instead", peerTD, ourTD)
	}
	// Let the running sync loop consume the registration events: it will only
	// wake up again on the force timer (forceSyncCycle) after that.
	time.Sleep(300 * time.Millisecond)

	// Simulate the state right after a restart: nothing has been synced yet.
	local.handler.synced.Store(false)
	local.handler.acceptTxs.Store(false)
	if local.handler.synced.Load() {
		t.Fatal("precondition failed: handler already marked synced")
	}

	cs := &chainSyncer{handler: local.handler}
	if op := cs.nextSyncOp(); op != nil {
		t.Fatalf("sync op scheduled for already in sync node: peer td %v", op.td)
	}
	if !local.handler.synced.Load() {
		t.Error("handler not marked synced although already in sync by TD")
	}
	if !local.handler.acceptTxs.Load() {
		t.Error("transaction acceptance not enabled although already in sync by TD")
	}
}

func TestFullSyncWithBlobs(t *testing.T) {
	testChainSyncWithBlobs(t, ethconfig.FullSync, 128, 128)
}

func TestSnapSyncWithBlobs(t *testing.T) {
	testChainSyncWithBlobs(t, ethconfig.SnapSync, 128, 128)
}

func testChainSyncWithBlobs(t *testing.T, mode downloader.SyncMode, preCancunBlks, postCancunBlks uint64) {
	t.Parallel()
	config := *params.ParliaTestChainConfig
	cancunTime := (preCancunBlks + 1) * 10
	config.CancunTime = &cancunTime

	// Create an empty handler
	empty := newTestParliaHandlerAfterCancun(t, &config, mode, 0, 0)
	defer empty.close()
	if ethconfig.SnapSync == mode && !empty.handler.snapSync.Load() {
		t.Fatalf("snap sync disabled on pristine blockchain")
	}

	// Create a full handler
	full := newTestParliaHandlerAfterCancun(t, &config, mode, preCancunBlks, postCancunBlks)
	defer full.close()
	if ethconfig.SnapSync == mode && full.handler.snapSync.Load() {
		t.Fatalf("snap sync not disabled on non-empty blockchain")
	}

	// check blocks and blobs
	checkChainWithBlobs(t, full.chain, preCancunBlks, postCancunBlks)

	// Sync up the two handlers via both `eth` and `snap`
	ethVer := uint(eth.ETH68)
	snapVer := uint(snap.SNAP1)

	// Sync up the two handlers via both `eth` and `snap`
	caps := []p2p.Cap{{Name: "eth", Version: ethVer}, {Name: "snap", Version: snapVer}}

	emptyPipeEth, fullPipeEth := p2p.MsgPipe(true)
	defer emptyPipeEth.Close()
	defer fullPipeEth.Close()

	emptyPeerEth := eth.NewPeer(ethVer, p2p.NewPeer(enode.ID{1}, "", caps), emptyPipeEth, empty.txpool, empty.chain.Config())
	fullPeerEth := eth.NewPeer(ethVer, p2p.NewPeer(enode.ID{2}, "", caps), fullPipeEth, full.txpool, full.chain.Config())
	defer emptyPeerEth.Close()
	defer fullPeerEth.Close()

	go empty.handler.runEthPeer(emptyPeerEth, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(empty.handler), peer)
	})
	go full.handler.runEthPeer(fullPeerEth, func(peer *eth.Peer) error {
		return eth.Handle((*ethHandler)(full.handler), peer)
	})

	emptyPipeSnap, fullPipeSnap := p2p.MsgPipe(true)
	defer emptyPipeSnap.Close()
	defer fullPipeSnap.Close()

	emptyPeerSnap := snap.NewPeer(snapVer, p2p.NewPeer(enode.ID{1}, "", caps), emptyPipeSnap)
	fullPeerSnap := snap.NewPeer(snapVer, p2p.NewPeer(enode.ID{2}, "", caps), fullPipeSnap)

	go empty.handler.runSnapExtension(emptyPeerSnap, func(peer *snap.Peer) error {
		return snap.Handle((*snapHandler)(empty.handler), peer)
	})
	go full.handler.runSnapExtension(fullPeerSnap, func(peer *snap.Peer) error {
		return snap.Handle((*snapHandler)(full.handler), peer)
	})

	for empty.handler.peers.snapLen() < 1 {
		// Wait a bit for the above handlers to start
		time.Sleep(100 * time.Millisecond)
	}

	op := peerToSyncOp(mode, empty.handler.peers.peerWithHighestTD())
	if err := empty.handler.doSync(op); err != nil {
		t.Fatal("sync failed:", err)
	}
	// Check that snap sync was disabled
	if !empty.handler.synced.Load() {
		t.Fatalf("full sync not done after successful synchronisation")
	}

	// check blocks and blobs
	checkChainWithBlobs(t, empty.chain, preCancunBlks, postCancunBlks)
}

func checkChainWithBlobs(t *testing.T, chain *core.BlockChain, preCancunBlks, postCancunBlks uint64) {
	block := chain.GetBlockByNumber(preCancunBlks)
	require.NotNil(t, block, preCancunBlks)
	require.Nil(t, chain.GetSidecarsByHash(block.Hash()), preCancunBlks)
	block = chain.GetBlockByNumber(preCancunBlks + 1)
	require.NotNil(t, block, preCancunBlks+1)
	require.NotNil(t, chain.GetSidecarsByHash(block.Hash()), preCancunBlks+1)
	block = chain.GetBlockByNumber(preCancunBlks + postCancunBlks)
	require.NotNil(t, block, preCancunBlks+postCancunBlks)
	require.NotNil(t, chain.GetSidecarsByHash(block.Hash()), preCancunBlks+postCancunBlks)
}
