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
	"sync"
	"time"

	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"

	"github.com/ethereum/go-ethereum/log"
)

const (
	// viewTick is how often the view clock is polled. One second against a
	// signTolerance of six seconds gives five chances to notice a stalled view
	// before it is already late, which is why pgp-chain has always used 1s.
	viewTick = time.Second

	// maxViewOffset is how far the view must advance, in whole views, before
	// rotation alone is declared stuck and the node asks the set for a forced
	// reset instead.
	//
	// Read that as a RATE, not as a threshold on the offset, and the difference is
	// the whole point: see shouldRequestResetView. With twelve producers and a six
	// second window, 100 failed views is ten minutes of a one-in-twelve proposer
	// failing to produce. Below that the rotation is still the cheaper remedy: it
	// needs no message the others have to agree on.
	maxViewOffset = 100
)

// resetViewGate is the state of the rate limiter described on
// shouldRequestResetView: the offset this node last asked at, and whether it has
// started counting yet.
//
// A dedicated mutex, and never held while taking another: both callers read the
// offset first and then ask, so this is a leaf.
type resetViewGate struct {
	mu   sync.Mutex
	last uint32
	on   bool
}

// shouldRequestResetView decides whether this tick asks the set for a forced view
// reset, and limits how often it may.
//
// # Why the old test could not survive the anchor repair
//
// It used to be `vo >= maxViewOffset`, with vo a counter that a reset zeroed. Two
// things changed. vo is now derived from the clock -- (now - anchor)/tolerance --
// and a reset does not zero it, because there is nothing to zero; that was the
// point of the derivation. So on a chain that has stopped producing, the anchor
// freezes at the last block's timestamp and vo grows without bound: measured on
// the six-producer chain, 249 within fourteen minutes. `vo >= 100` was therefore
// true on EVERY tick, on every node, forever, and each true answer broadcast a
// reset request plus recorded its own vote. The measured result was 8900 to 9500
// resetConsensus calls per node in fifteen minutes, about 10Hz, and a chain that
// could not finish a single round.
//
// The mistake was treating a large offset as a fault signal. vo is not a symptom
// of anything local any more: it is "how many view slots have passed since the
// last block", a number all six nodes compute identically, and a large value says
// only that the chain has been stuck for a while. A quantity every node derives
// from the clock cannot be the thing that makes them all shout.
//
// So the trigger is a RATE, which is what the constant was always describing: ask
// once this node has personally watched the offset advance maxViewOffset whole
// views without a block landing. Consequences, all intended:
//
//   - A node that joins an already-stuck chain with vo=250 does not ask at all
//     until it has seen 100 more views go by. It did not watch those 250 fail.
//   - A block landing re-anchors the grid, vo drops, and the gate re-bases on its
//     own: the counting is per generation of the anchor.
//   - At most one request per maxViewOffset views, which for six producers is the
//     ten minutes the constant's comment always claimed.
func (e *Engine) shouldRequestResetView(vo uint32) bool {
	e.resetGate.mu.Lock()
	defer e.resetGate.mu.Unlock()

	// First look at this grid, or the offset went backwards because a block
	// landed: start counting from here instead of asking. The "went backwards"
	// case is not a corner -- it is the normal end of a stall, and it is also why
	// this cannot be a plain subtraction on uint32 without it.
	if !e.resetGate.on || vo < e.resetGate.last {
		e.resetGate.on = true
		e.resetGate.last = vo
		return false
	}
	// Not enough views have gone by since the last request -- including the very
	// first one, which is why a node that starts at vo=300 says nothing.
	if vo-e.resetGate.last < maxViewOffset {
		return false
	}
	e.resetGate.last = vo
	return true
}

// ensureViewLoop starts the single view-change goroutine.
//
// One goroutine per engine, started on the first round and stopped by Close,
// deliberately not pgp-chain's start-and-stop-per-round. A loop whose lifetime is
// a round can be started twice by two racing Prepare calls, and the duplicate
// merely doubles the view-change rate, which is a silent way to produce blocks
// on a view the rest of the set has already left.
//
// The retired check is under viewMu, not before it: retire clears
// enableViewLoop under the same lock, so checking before it would let a racing
// pair through -- ensure past the early return, retire clearing the flag,
// ensure setting it again -- and start a view loop that nothing ever stops. A
// retired engine starts no rounds, so this is the only guard it needs.
func (e *Engine) ensureViewLoop() {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	e.viewMu.Lock()
	defer e.viewMu.Unlock()
	if e.retired.Load() || e.enableViewLoop {
		return
	}
	e.enableViewLoop = true

	go e.changeViewLoop()
}

func (e *Engine) viewLoopEnabled() bool {
	e.viewMu.RLock()
	defer e.viewMu.RUnlock()
	return e.enableViewLoop
}

// changeViewLoop drives the view clock.
//
// It is the only timer in the engine. Everything that can stall a round — a
// proposer that crashed, a vote that never arrived, a partition — is detected
// here and nowhere else, which is why it must not be shared with, say, the seal
// wait: a goroutine that both rotates the view and waits for a confirm would
// rotate the view out from under its own round.
func (e *Engine) changeViewLoop() {
	ticker := time.NewTicker(viewTick)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			if !e.viewLoopEnabled() {
				return
			}
			// Before the view work, and outside its Running guard: a chain stuck
			// on a confirm whose block never arrived is often Ready, and this is
			// the only timer in the engine that keeps ticking either way. See
			// reclaimOrphanedConfirm.
			e.reclaimOrphanedConfirm(time.Now())
			e.onChangeView()
		}
	}
}

// onChangeView advances the view if its deadline has passed.
//
// TryChangeView is a no-op unless the view is running and past its change time,
// so this can be called unconditionally every tick: between rounds the view sits
// in ConsensusReady and the tick costs one comparison.
func (e *Engine) onChangeView() {
	if e.dispatcher == nil {
		return
	}
	if !e.dispatcher.GetConsensusView().IsRunning() {
		return
	}
	e.dispatcher.OnChangeView()

	// Ask for a forced reset only after watching rotation fail for
	// maxViewOffset whole views. Not "if the offset is large": that condition is
	// true on every tick of a stuck chain, and asking ten times a second is what
	// kept it stuck. See shouldRequestResetView.
	if e.shouldRequestResetView(e.dispatcher.GetConsensusView().GetViewOffset()) {
		e.broadcastResetView()
	}
}

// broadcastResetView asks the producer set to abandon the current view.
//
// Without a transport there is nobody to ask, and the request would also be
// unsatisfiable: a forced reset needs a quorum of requests, which is exactly
// GetResetViewReqCount() >= majority. Recording our own request is therefore
// conditional on the broadcast actually happening.
func (e *Engine) broadcastResetView() {
	transport := e.getTransport()
	if transport == nil || e.account == nil {
		return
	}
	if !e.dispatcher.IsProducer(e.account.PublicKeyBytes()) {
		return
	}
	request := &emsg.ResetView{Sponsor: e.account.PublicKeyBytes()}
	buf := new(bytes.Buffer)
	if err := request.SerializeUnsigned(buf); err != nil {
		log.Error("PBFT: cannot serialize reset view request", "err", err)
		return
	}
	request.Sign = e.account.Sign(buf.Bytes())

	log.Debug("PBFT: broadcasting reset view request",
		"viewOffset", e.dispatcher.GetConsensusView().GetViewOffset())
	transport.BroadcastResetView(request)

	// Count our own request, as pgp-chain does: with a set of exactly one the
	// quorum is one, and a reset request from the only producer must not be
	// unable to reach its own threshold.
	if !e.dispatcher.ResetViewRequestIsContain(request.Sponsor) {
		if err := e.dispatcher.OnResponseResetViewReceived(request); err != nil {
			log.Debug("PBFT: own reset view request rejected", "err", err)
		}
	}
}
