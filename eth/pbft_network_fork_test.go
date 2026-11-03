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

package eth

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/consensus/pbft"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// The restart gate of the retire mechanism: a node that boots with its chain
// head already past LegacyConfig.ForkHeight must not bring the DPoS network
// back up. The devnet hit exactly this -- nodes crossed the fork while running
// (the engine-side gate in pbft.OnBlockImported handled that), and every restart
// re-opened the direct links until this gate was added.
//
// The test discriminates the gate from the other legitimate "network not
// started" exits by their log lines, on purpose: every exit returns nil, so the
// return value alone cannot tell them apart. Past the fork the log says
// "past fork height"; below it the same node shape falls through to the
// no-producer-key exit, whose log says "no producer key". Same node, same call,
// only the fork height differs.
//
// The chain is built with a standard config and the LegacyConfig is attached
// afterwards: generating blocks under legacy rules needs the SPV scenery, which
// would make this test about the scenery instead of the gate. The gate reads
// chain.Config().Legacy.ForkHeight against the real chain head, so what the
// blocks were produced under is irrelevant to it.

// captureStartPBFTLogs redirects the root logger into a buffer for one test.
func captureStartPBFTLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := log.Root()
	log.SetDefault(log.NewLogger(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { log.SetDefault(previous) })
	return &buf
}

// newForkGateNode builds the minimal Ethereum the gate reads: a chain at height
// 3 and a producing-enabled PBFT engine with no keystore, i.e. a node that
// would otherwise stop at the no-producer-key exit.
func newForkGateNode(t *testing.T, forkHeight uint64) (*Ethereum, *pbft.Engine) {
	t.Helper()

	cfg := *params.TestChainConfig
	gspec := &core.Genesis{Config: &cfg, GasLimit: 30_000_000}
	chain := newTestBlockChain(t, 3, gspec, func(_ int, _ *core.BlockGen) {})
	t.Cleanup(chain.Stop)

	// Attached after the chain is built; see the file comment for why.
	chain.Config().Legacy = &params.LegacyConfig{ForkHeight: forkHeight}

	engine := pbft.New(&params.LegacyConfig{ForkHeight: forkHeight}, &pbft.Config{Enable: true})
	node := &Ethereum{
		blockchain: chain,
		config:     &ethconfig.Config{},
		pbftEngine: engine,
	}
	return node, engine
}

// TestStartPBFTNetworkPastForkDoesNotStart is the gate: head 3, fork 2, the
// node is already in the Parlia era and the DPoS stack must stay down.
func TestStartPBFTNetworkPastForkDoesNotStart(t *testing.T) {
	node, engine := newForkGateNode(t, 2)
	buf := captureStartPBFTLogs(t)

	if err := node.startPBFTNetwork(); err != nil {
		t.Fatalf("startPBFTNetwork past the fork returned %v, want nil (the fork is a normal state)", err)
	}
	if !strings.Contains(buf.String(), "past fork height") {
		t.Fatalf("the fork gate's log line is missing; without it the nil return could have come from "+
			"any of the other silent exits:\n%s", buf.String())
	}
	if engine.DPoSNetworkRunning() {
		t.Fatalf("a DPoS network is attached after startPBFTNetwork past the fork: the direct links the " +
			"fork was supposed to end came back")
	}
}

// TestStartPBFTNetworkBelowForkStillBuilds is the control: the same node with
// the fork above the head must fall through the gate to the next exit (no
// producer key), proving the gate is what changed the path above, not the
// engine shape.
func TestStartPBFTNetworkBelowForkStillBuilds(t *testing.T) {
	node, engine := newForkGateNode(t, 100)
	buf := captureStartPBFTLogs(t)

	if err := node.startPBFTNetwork(); err != nil {
		t.Fatalf("startPBFTNetwork below the fork returned %v, want nil", err)
	}
	if strings.Contains(buf.String(), "past fork height") {
		t.Fatalf("the fork gate fired below the fork; it must not")
	}
	if !strings.Contains(buf.String(), "no producer key") {
		t.Fatalf("the below-fork run did not reach the no-producer-key exit; the control path changed:\n%s",
			buf.String())
	}
	if engine.DPoSNetworkRunning() {
		t.Fatalf("a network was attached on the control path; the fixture is not the verifier shape it claims")
	}
}

// TestPbftLoopReconcilesARestartPastTheFork pins the restart half of the retire
// mechanism: startPBFTNetwork's gate leaves a restarted-past-the-fork node
// running with no network, which on its own is only half retired -- the engine
// flag stays false and every "is this engine in service" answer lies. pbftLoop's
// first act is to feed the existing chain head through OnBlockImported, the same
// door the events use, which takes the retire path and completes the shutdown.
//
// The test drives the real loop with a real subscription (not a hand-copied
// call to OnBlockImported) so that it pins the startup wiring in backend.go, not
// just the engine behaviour: if the reconciliation line is ever dropped, a node
// restarted past the fork is half retired again and this fails.
func TestPbftLoopReconcilesARestartPastTheFork(t *testing.T) {
	node, engine := newForkGateNode(t, 2) // head 3, fork 2: already in the Parlia era

	// The gate runs first, as in New: past the fork it leaves the network unstarted.
	if err := node.startPBFTNetwork(); err != nil {
		t.Fatalf("startPBFTNetwork past the fork returned %v, want nil", err)
	}

	// Then the loop starts, with the subscription the real wiring installs.
	node.pbftChainHeadCh = make(chan core.ChainHeadEvent, 16)
	node.pbftChainHeadSub = node.blockchain.SubscribeChainHeadEvent(node.pbftChainHeadCh)
	unsubscribed := false
	defer func() {
		if !unsubscribed {
			node.pbftChainHeadSub.Unsubscribe()
		}
	}()
	done := make(chan struct{})
	go func() { node.pbftLoop(); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for !engine.Retired() {
		if time.Now().After(deadline) {
			t.Fatalf("pbftLoop started with the head past the fork but the engine is not retired: " +
				"the startup reconciliation is gone or no longer reaches the retire path")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The loop must still be a working event loop afterwards (unsubscribe
	// makes Err fire and it returns); a loop that died on the reconciliation
	// call would leave a fork-crossing node without its finality plumbing.
	node.pbftChainHeadSub.Unsubscribe()
	unsubscribed = true
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("pbftLoop did not exit after unsubscribe; the reconciliation changed the loop's lifecycle")
	}
}
