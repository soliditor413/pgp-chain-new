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
	"time"

	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"

	"github.com/ethereum/go-ethereum/log"
)

const (
	// viewTick is how often the view clock is polled. One second against a
	// signTolerance of six seconds gives five chances to notice a stalled view
	// before it is already late, which is why pgp-chain has always used 1s.
	viewTick = time.Second

	// maxViewOffset is the offset at which view rotation alone is declared
	// stuck and the node asks the set for a forced reset instead.
	//
	// With twelve producers and a six second window, 100 failed views is ten
	// minutes of a one-in-twelve proposer failing to produce. Below that the
	// rotation is still the cheaper remedy: it needs no message the others have
	// to agree on.
	maxViewOffset = 100
)

// ensureViewLoop starts the single view-change goroutine.
//
// One goroutine per engine, started on the first round and stopped by Close,
// deliberately not pgp-chain's start-and-stop-per-round. A loop whose lifetime is
// a round can be started twice by two racing Prepare calls, and the duplicate
// merely doubles the view-change rate, which is a silent way to produce blocks
// on a view the rest of the set has already left.
func (e *Engine) ensureViewLoop() {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	e.viewMu.Lock()
	if e.enableViewLoop {
		e.viewMu.Unlock()
		return
	}
	e.enableViewLoop = true
	e.viewMu.Unlock()

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

	// Rotation has failed maxViewOffset times in a row. Waiting for the offset to
	// wrap far enough to give this node's producer another turn is pointless when
	// the reason for the failure is shared, so ask the set for a forced reset.
	if e.dispatcher.GetConsensusView().GetViewOffset() >= maxViewOffset {
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
