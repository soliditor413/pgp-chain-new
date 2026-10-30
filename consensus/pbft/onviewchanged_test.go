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

// OnViewChanged is the only path from "a round is running" back to "this node may
// prepare" that does not require an imported block. Prepare marks the view Running
// on every producer that asks, and refuses to open a second round while one is
// marked, so without this callback a round that ends by rotation, rejection or
// timeout takes the height with it: the devnet sat at height 1 with all four
// producers answering "consensus is already running" and no round in flight.

import (
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/params"
)

func newViewChangedEngine(t *testing.T, producers int) (*Engine, *int32) {
	t.Helper()

	accounts, producerKeys := newProducerSet(t, producers)
	chain := &sealTestChain{genesis: &types.Header{
		Number:     big.NewInt(0),
		Time:       uint64(time.Now().Add(-100 * time.Second).Unix()),
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}}

	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.MarkRecovered()
	engine.AttachChain(chain)
	t.Cleanup(func() { _ = engine.Close() })

	engine.SetIsCurrent(func() bool { return true })
	mined := new(int32)
	engine.StartMine = func() { atomic.AddInt32(mined, 1) }
	return engine, mined
}

// TestOnViewChangedReleasesTheProducerOnDuty walks the three outcomes the view
// loop can hand the engine, and pins that only one of them releases production.
func TestOnViewChangedReleasesTheProducerOnDuty(t *testing.T) {
	engine, mined := newViewChangedEngine(t, 4)
	view := engine.dispatcher.GetConsensusView()

	// Our turn: the Running mark has to go and the miner has to be poked, because
	// a poke is what rebuilds work for the view that just started. Missing either
	// half is the stall: without the mark the next Prepare is refused, without the
	// poke the miner waits for a block interval that will not come.
	view.SetRunning()
	engine.OnViewChanged(true, false)
	if view.IsRunning() {
		t.Fatal("a normal view change for the producer on duty left the view Running")
	}
	if got := atomic.LoadInt32(mined); got != 1 {
		t.Fatalf("the miner was poked %d times, want 1", got)
	}
}

// TestOnViewChangedIgnoresForcedResets keeps the release narrow. force is used by
// Dispatcher.FinishedProposal and Dispatcher.ResetConsensus, both of which have
// already called SetReady and re-anchored the view on a real block; releasing a
// second time there would let the miner prepare another round on top of a height
// that just became final.
func TestOnViewChangedIgnoresForcedResets(t *testing.T) {
	engine, mined := newViewChangedEngine(t, 4)
	view := engine.dispatcher.GetConsensusView()

	view.SetRunning()
	engine.OnViewChanged(true, true)
	if !view.IsRunning() {
		t.Fatal("a forced view change cleared the Running mark; FinishedProposal owns that")
	}
	if got := atomic.LoadInt32(mined); got != 0 {
		t.Fatalf("a forced view change poked the miner %d times, want 0", got)
	}
}

// TestOnViewChangedHoldsWhenTheTurnIsNotOurs covers the two ways "not yet": the
// view rotated to a peer, and the chain is still catching up. Both have to leave
// the Running mark alone -- rotation is what ends the round, and a node that is
// behind must not start one -- and neither may touch the miner.
func TestOnViewChangedHoldsWhenTheTurnIsNotOurs(t *testing.T) {
	engine, mined := newViewChangedEngine(t, 4)
	view := engine.dispatcher.GetConsensusView()

	view.SetRunning()
	engine.OnViewChanged(false, false)
	if !view.IsRunning() {
		t.Fatal("a view change to a peer's turn cleared our Running mark")
	}

	engine.SetIsCurrent(func() bool { return false })
	view.SetRunning()
	engine.OnViewChanged(true, false)
	if !view.IsRunning() {
		t.Fatal("a node that is still syncing released production on its own turn")
	}
	if got := atomic.LoadInt32(mined); got != 0 {
		t.Fatalf("the miner was poked %d times while the node was not current, want 0", got)
	}
}

// TestOnViewChangedWithoutADispatcherIsInert keeps the callback safe during
// shutdown: the view loop can deliver one last rotation after the dispatcher is
// gone, and OnViewChanged is reached from that goroutine.
func TestOnViewChangedWithoutADispatcherIsInert(t *testing.T) {
	engine, mined := newViewChangedEngine(t, 1)
	engine.dispatcher = nil

	engine.OnViewChanged(true, false) // no dispatcher: no-op, no panic
	if got := atomic.LoadInt32(mined); got != 0 {
		t.Fatalf("the miner was poked %d times without a dispatcher, want 0", got)
	}
}

// TestOnViewChangedReplaysTheProposalThatWasAheadOfUs covers the wiring, not the
// policy: Dispatcher.UpdatePrecociousProposals has its own test, but a correct
// function nothing calls is how this port got here in the first place -- the stash
// was filled by ProcessProposal and emptied by nobody.
//
// The proposal is stashed the way a real one is, through ProcessProposal's
// offset-mismatch branch, and the observable is the stash: after the view catches
// up, the proposal must be gone. It is deliberately given a zero block hash, which
// is the fresh dispatcher's finished seal hash, so the replay stops at
// OnProposalReceived's "block already finished" check instead of walking into the
// network paths a unit test has no peers for.
func TestOnViewChangedReplaysTheProposalThatWasAheadOfUs(t *testing.T) {
	engine, _ := newViewChangedEngine(t, 4)
	view := engine.dispatcher.GetConsensusView()
	now := time.Now()

	// Our view is at offset 0 and the proposal claims offset 1, so ProcessProposal
	// has to stash it rather than count its votes against the current round.
	view.RecoverFromStatus(dpos.ConsensusRunning, 0, now, 1, 0)
	proposal := &payload.DPOSProposal{ViewOffset: 1}
	if err, _, _ := engine.dispatcher.ProcessProposal(dpeer.PID{}, proposal); err == nil {
		t.Fatal("ProcessProposal accepted a proposal from a different view offset")
	}

	// Our view catches up to the proposal's offset, which is the moment it becomes
	// processable. Anything still in the stash afterwards is a vote the proposer
	// will never see.
	view.RecoverFromStatus(dpos.ConsensusRunning, 1, now, 1, 0)
	engine.OnViewChanged(false, false)

	if got := engine.dispatcher.UpdatePrecociousProposals(); got != nil {
		t.Fatal("OnViewChanged did not replay the proposal that our view caught up to")
	}
}
