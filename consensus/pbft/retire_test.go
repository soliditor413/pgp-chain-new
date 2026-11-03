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
	"bytes"
	"log/slog"
	"math/big"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	elp2p "github.com/elastos/Elastos.ELA/dpos/p2p"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// These tests pin the retire mechanism: at LegacyConfig.ForkHeight the engine
// takes its own machinery down -- once, whether or not anything repeats the
// event -- and afterwards the machinery this package runs locally is down: the
// view loop, the DPoS network, the announce trigger, the recovery spin, and
// every answer that asks whether the engine is still in service.
//
// The observability choice, and its limits:
//
//   - The normal path (below the fork) is asserted through the dispatcher's own
//     "[resetConsensus] start" log line rather than through dispatcher state.
//     Dispatcher internals are not exported for poking, and the log line is the
//     exact thing the devnet operator saw spammed after the fork -- pinning it
//     makes "the spam stopped" the assertion rather than a proxy for it.
//   - The view-loop stop is asserted through the enableViewLoop flag, not
//     through the goroutine's death: changeViewLoop has no done channel, and
//     adding one just for a test would widen the production surface. The flag
//     is the exact thing the loop re-reads every tick (view.go), so the test
//     pins the mechanism; the goroutine's exit is the loop reading that flag,
//     within one viewTick.
//
// Both limits are recorded here so nobody reads a green run as proving more
// than it does.

// retireCountingNetwork is the dpos.DPOSNetwork stub; it exists to count Stop
// calls, which is the one interaction retire has with the direct-link network.
type retireCountingNetwork struct {
	mu    sync.Mutex
	stops int
}

func (n *retireCountingNetwork) Start()      {}
func (n *retireCountingNetwork) Stop() error { n.mu.Lock(); defer n.mu.Unlock(); n.stops++; return nil }

func (n *retireCountingNetwork) SendMessageToPeer(dpeer.PID, elap2p.Message) error { return nil }
func (n *retireCountingNetwork) BroadcastMessage(elap2p.Message)                   {}
func (n *retireCountingNetwork) UpdatePeers(current, next []dpeer.PID)             {}
func (n *retireCountingNetwork) GetActivePeers() []elp2p.Peer                      { return nil }

func (n *retireCountingNetwork) stopCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stops
}

// captureRetireLogs redirects the root logger into a buffer for one test and
// restores it afterwards. The dpos helpers (including ResetConsensus's log
// line) funnel into this root logger, so a text handler sees all of it.
func captureRetireLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := log.Root()
	log.SetDefault(log.NewLogger(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { log.SetDefault(previous) })
	return &buf
}

// newRetireEngine builds a producing-shaped engine over a two-producer set and
// attaches the counting network, the shape a fork-crossing producer node has.
func newRetireEngine(t *testing.T, fork uint64) (*Engine, *retireCountingNetwork) {
	t.Helper()

	accounts, keys := newProducerSet(t, 2)
	legacy := &params.LegacyConfig{ForkHeight: fork, VerifySeal: false, Producers: keys}
	engine := New(legacy, &Config{Enable: true})
	t.Cleanup(func() { engine.Close() })
	engine.attachAccount(accounts[0])

	network := &retireCountingNetwork{}
	engine.AttachNetwork(network)
	return engine, network
}

// retireTestHeader carries only what OnBlockImported reads: the height. The
// normal path also touches header.Time, which stays zero here; ResetConsensus
// takes it as an opaque anchor.
func retireTestHeader(height uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(height)}
}

// countRetireLines counts how many times the one-shot retire log line has been
// emitted, which is the idempotence assertion: a second OnBlockImported past
// the fork must not produce a second "retired" event.
func countRetireLines(buf *bytes.Buffer) int {
	return strings.Count(buf.String(), "PBFT: Parlia fork reached")
}

// TestOnBlockImportedRetiresAtTheFork walks one engine across the fork and
// requires the four retire effects to have happened exactly once.
func TestOnBlockImportedRetiresAtTheFork(t *testing.T) {
	const fork = uint64(10)
	engine, network := newRetireEngine(t, fork)
	buf := captureRetireLogs(t)

	// ---- Below the fork: the legacy path is fully alive ----
	engine.OnBlockImported(retireTestHeader(fork - 1))
	if !strings.Contains(buf.String(), "[resetConsensus] start") {
		t.Fatalf("OnBlockImported below the fork did not reach ResetConsensus; without this the " +
			"pre-fork half of the test proves nothing")
	}
	if engine.retired.Load() {
		t.Fatalf("retired was set below the fork (height %d, fork %d)", fork-1, fork)
	}
	if got := network.stopCount(); got != 0 {
		t.Fatalf("network.Stop was called %d times below the fork, want 0", got)
	}

	// A running view loop, so the retire stop is observed on a live one rather
	// than trivially. And the announce control: AnnounceDAddr must answer true
	// here for the false below to mean "retired" rather than "no account".
	engine.ensureViewLoop()
	if !engine.viewLoopEnabled() {
		t.Fatalf("fixture problem: the view loop did not start below the fork")
	}
	if !engine.AnnounceDAddr() {
		t.Fatalf("fixture problem: AnnounceDAddr answered false below the fork, so its answer after " +
			"the retire could be the no-account early return instead")
	}
	if !engine.DPoSNetworkRunning() {
		t.Fatalf("fixture problem: DPoSNetworkRunning answered false with a network attached, so its " +
			"answer after the retire could be the nil-network case instead")
	}
	buf.Reset()

	// ---- At the fork: retire, exactly the four effects ----
	engine.OnBlockImported(retireTestHeader(fork))
	if !engine.retired.Load() {
		t.Fatalf("retired was not set at the fork height %d", fork)
	}
	if engine.viewLoopEnabled() {
		t.Fatalf("the view loop flag is still up after retire; changeViewLoop will keep ticking")
	}
	if got := network.stopCount(); got != 1 {
		t.Fatalf("network.Stop was called %d times at the fork, want exactly 1", got)
	}
	if engine.AnnounceDAddr() {
		t.Fatalf("AnnounceDAddr answered true after retire: it re-triggers the DAddr address exchange")
	}
	if engine.DPoSNetworkRunning() {
		t.Fatalf("DPoSNetworkRunning answered true after retire: the server is stopped, the RPC status " +
			"would point an operator at the wrong knob")
	}
	if !engine.Retired() {
		t.Fatalf("Retired() answered false with the flag set: the observable half of retire is lying")
	}
	if strings.Contains(buf.String(), "[resetConsensus] start") {
		t.Fatalf("ResetConsensus ran at the fork height %d: the view re-anchor the fork was supposed "+
			"to stop is still happening", fork)
	}
	if got := countRetireLines(buf); got != 1 {
		t.Fatalf("the retire log line appeared %d times, want 1", got)
	}

	// A late Prepare (there is none after the fork, but this pins the guard)
	// must not resurrect the loop.
	engine.ensureViewLoop()
	if engine.viewLoopEnabled() {
		t.Fatalf("ensureViewLoop restarted the view loop on a retired engine")
	}

	// ---- Past the fork, again: idempotence ----
	engine.OnBlockImported(retireTestHeader(fork + 5))
	if got := network.stopCount(); got != 1 {
		t.Fatalf("network.Stop was called %d times on the second post-fork event, want it to stay at 1", got)
	}
	if got := countRetireLines(buf); got != 1 {
		t.Fatalf("the retire log line appeared %d times after two post-fork events, want 1", got)
	}
	if strings.Contains(buf.String(), "[resetConsensus] start") {
		t.Fatalf("ResetConsensus ran past the fork: the post-fork re-anchor the devnet saw is still happening")
	}

	// Close after retire must still work: retire must not have touched stopCh,
	// and this must not panic on a second close at cleanup time.
	if err := engine.Close(); err != nil {
		t.Fatalf("Close after retire failed: %v", err)
	}
}

// TestOnBlockImportedWithoutLegacyConfigDoesNotRetire pins the compatibility
// path: e.legacy == nil (only a hand-built engine reaches it; New fills it in)
// means no fork is scheduled, so no height retires anything.
func TestOnBlockImportedWithoutLegacyConfigDoesNotRetire(t *testing.T) {
	engine, network := newRetireEngine(t, 10)
	engine.legacy = nil
	buf := captureRetireLogs(t)

	engine.OnBlockImported(retireTestHeader(100))

	if engine.retired.Load() {
		t.Fatalf("the engine retired without a LegacyConfig: the no-fork path must keep the old behaviour")
	}
	if got := network.stopCount(); got != 0 {
		t.Fatalf("network.Stop was called %d times without a LegacyConfig, want 0", got)
	}
	if !strings.Contains(buf.String(), "[resetConsensus] start") {
		t.Fatalf("OnBlockImported without a LegacyConfig did not reach ResetConsensus; the old " +
			"behaviour is not just 'do not retire', it is 'do everything as before'")
	}
	if !engine.AnnounceDAddr() {
		t.Fatalf("AnnounceDAddr answered false without a retire; the no-account early return is " +
			"leaking into the wrong path")
	}
}

// TestRetireWithoutAttachedNetworkDoesNotPanic is the verifier shape: no
// account, no network attached. The retire path must skip the nil network
// without a panic, and the engine still reports itself retired.
func TestRetireWithoutAttachedNetworkDoesNotPanic(t *testing.T) {
	legacy := &params.LegacyConfig{ForkHeight: 1}
	engine := New(legacy, nil) // verifier: no config at all, no account, no network
	t.Cleanup(func() { engine.Close() })

	engine.OnBlockImported(retireTestHeader(5))

	if !engine.retired.Load() {
		t.Fatalf("the verifier-shaped engine did not retire at height 5 past fork 1")
	}
	if engine.AnnounceDAddr() {
		t.Fatalf("AnnounceDAddr answered true on a retired verifier")
	}
}

// recoveryFrameCount counts live goroutines whose stack contains the
// StartRecovery spin, by name. It is how the test below observes that loop's
// life and death without a done channel.
//
// Counting goroutines by stack frame, not by runtime.NumGoroutine: the number
// moves when unrelated test goroutines start or stop, a frame match does not.
func recoveryFrameCount() int {
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		return -1
	}
	return strings.Count(buf.String(), "pbft.(*Engine).StartRecovery.func1")
}

// waitFor polls cond until it holds or the timeout does, and reports which.
func waitFor(t *testing.T, what string, cond func() bool, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartRecoveryStopsAtRetire pins the third spin-down gap.
//
// StartRecovery's wait goroutine only selects on e.stopCh, which retire
// deliberately does not close (Close owns that channel), so without a retired
// check of its own it would outlive the fork: the DPoS network is stopped, the
// peer count drops to zero, and the goroutine answers with the
// "still waiting for producers" warning every two minutes for as long as the
// node lives -- the devnet operator's exact complaint, one symptom removed.
//
// # Why the assertion is a goroutine frame, not the warning log
//
// The natural shape -- start the wait, retire, then wait two full
// startupRecoverWait (2 minutes each) and assert the warning never came -- is a
// four-minute test to prove a ≤1-second exit, and the log line is the weaker
// evidence anyway: silence could also mean "the deadline has not been reached".
// The frame disappearing from the goroutine profile is the exit itself. The
// bound is generous: the loop sleeps in one-second passes, so retire-to-exit is
// at most ~1s plus the retire's own work; the 10s timeout is there to absorb a
// loaded machine, not because the wait is expected to be long.
func TestStartRecoveryStopsAtRetire(t *testing.T) {
	const fork = uint64(10)
	engine, network := newRetireEngine(t, fork)

	// Fixture self-check: with the stub network reporting zero peers the
	// recovery wait cannot complete, so the goroutine must still be spinning
	// when retire arrives. If it has already exited the test proves nothing.
	if got := network.stopCount(); got != 0 {
		t.Fatalf("fixture problem: network.Stop was called %d times before the fork", got)
	}
	engine.StartRecovery()
	waitFor(t, "the StartRecovery goroutine to spin up", func() bool {
		return recoveryFrameCount() >= 1
	}, 10*time.Second)

	engine.OnBlockImported(retireTestHeader(fork))
	waitFor(t, "the StartRecovery goroutine to stop after retire", func() bool {
		return recoveryFrameCount() == 0
	}, 10*time.Second)
}
