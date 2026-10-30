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
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
)

// Prepare implements consensus.Engine.
//
// It runs inside miner's work-generation path and is the only place that decides
// whether this node may propose at all. Every check below is a reason to produce
// nothing this time; none of them is an error the operator should be paged for,
// and the miner treats them as such (miner/worker.go's legacy branch logs them at
// debug level).
func (e *Engine) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	if header.Number == nil || header.Number.Sign() == 0 {
		// Genesis is constructed, not prepared.
		return consensus.ErrUnknownAncestor
	}
	// The two "this node cannot produce at all" gates come first, before anything
	// below mutates the header. A refused Prepare must leave the caller's header
	// untouched: "production is off" is a configuration answer, and answering it
	// by half-rewriting a header (Extra cleared, Difficulty forced to 2,
	// MixDigest zeroed) makes "disabled means no side effects" impossible to
	// state, and leaves anyone who reuses the header after the error holding a
	// mutated one. See docs/design/pbft-port-design.md D6.
	if !e.cfg.productionEnabled() {
		return pgp.ErrProductionNotSupported
	}
	// Configured to produce but no usable key: report the key, not the config,
	// because the config is already known to be willing.
	if e.account == nil {
		return ErrNotProducer
	}
	atomic.StoreInt32(&e.isSealOver, 0)

	nowTime := uint64(time.Now().Unix())
	if e.dispatcher != nil {
		nowTime = uint64(e.dispatcher.GetNowTime().Unix())
	}

	// Overwrite every field whose value the miner or a previous attempt may have
	// already set, BEFORE anything computes a hash from it. See
	// docs/design/pbft-port-design.md D6:
	//
	//   Extra      miner pre-fills it from --miner.extradata (worker.go:1063);
	//              a legacy block's Extra is the serialized confirm and nothing
	//              else, with no vanity prefix.
	//   MixDigest  miner writes genParams.random into it (worker.go:1073) on
	//              chains with a beacon root; a legacy header must be zero.
	//   Difficulty a legacy header must carry exactly 2.
	header.Difficulty = new(big.Int).Set(diffInTurn)
	header.Extra = nil
	header.MixDigest = common.Hash{}
	header.UncleHash = types.CalcUncleHash(nil)
	if e.dispatcher != nil {
		// header.Nonce anchors the ELA main chain height the confirm will be
		// verified against (see consensus/pgp verifyConfirm). Using the producer
		// set's own spv height keeps the sealer and the verifier reading the same
		// height even while the SPV listener is catching up.
		header.Nonce = types.EncodeNonce(e.dispatcher.GetConsensusView().GetSpvHeight())
	}

	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}

	// These four gates depend on engine state only -- recovery status, the finished
	// height, whether a round is already in flight for this height, and whether this
	// node is a producer. None of them depends on anything the rewrite above
	// changes (difficulty, extra, mix digest, uncle hash, nonce), nor on the view
	// this round would open; they sit here only because the parent lookup does. When
	// R12 adds its parent-time gate, the lookup and these gates move above the
	// rewrite together, so that a refused Prepare has not touched the header at all
	// -- the D6 invariant stated in this file's header, guarded by
	// TestPrepareIsSideEffectFreeWhenRefused.
	if atomic.LoadInt32(&e.recovered) == 0 {
		return ErrWaitRecoverStatus
	}
	if header.Number.Uint64() <= e.dispatcher.GetFinishedHeight() {
		return ErrAlreadyConfirmedBlock
	}
	// A round is already in flight for this height. Starting another would fork
	// the proposal, and letting Prepare rewrite the header mid-round would leave
	// the in-flight proposal committing to a different seal hash.
	if e.dispatcher.GetConsensusView().IsRunning() {
		return ErrConsensusIsRunning
	}
	if !e.IsProducer() {
		return ErrUnauthorizedSigner
	}

	head := chain.CurrentHeader()
	var headHeight uint64
	if head != nil && head.Number != nil {
		headHeight = head.Number.Uint64()
	}

	// Decide the round's clock anchor before opening the view, not after.
	//
	// The two orders differ by a window that is not theoretical. Opening the view
	// on parent.Time first and re-anchoring afterwards leaves the view pointing at
	// a parent that can be years old (a fresh chain's genesis, a node that was
	// offline) while the view loop is already running: its first tick sees
	// changeViewTime long past, calls ChangeView, and ChangeView counts the views
	// in between -- (now - parent.Time) / signTolerance, which on a cold start is
	// tens of millions of offsets and pushes duty onto a producer the rest of the
	// set disagrees with. Anchoring once, correctly, removes the window instead of
	// racing it.
	header.Time = parent.Time + e.period
	anchorTime := parent.Time
	if header.Time < nowTime {
		// We are late: the parent is older than one period ago. Stamp the block
		// with the current time so it is not born stale, and anchor the round on it
		// too so the deadline is measured from now.
		header.Time = nowTime
		anchorTime = nowTime
	}
	e.startView(anchorTime, headHeight)

	// Whose turn it is decides whether this header may be sealed at all, and only
	// now is the answer final: startView recomputes the duty index from the head
	// height for this very round, and StartRecovery may have left a recovered view
	// offset behind. Upstream has this gate at exactly this point, after Start and
	// after the timestamp is settled (pgp-chain/consensus/pbft/pbft.go:1105-1107).
	//
	// Without it a producer whose turn it is not still opens a round -- the view
	// is marked Running a few lines above -- and then Seal refuses with
	// ErrSignerNotOnduty, which returns before finishRound/resetRound. The view
	// then stays Running with no round behind it, and every later Prepare answers
	// ErrConsensusIsRunning: the height is lost until some block is imported, and
	// no block can be imported because nobody can produce. The devnet sat in
	// exactly that state at height 1, for 45 seconds, on all four producers.
	//
	// The error is benign by classification (IsBenignPrepareError), because a
	// peer's turn is not a fault: the arm that recovers is OnViewChanged, which
	// clears the Running mark once the view rotates to this node.
	if !e.IsOnduty() {
		return ErrSignerNotOnduty
	}
	return nil
}

// startView opens a round: align the view clock with the round's anchor time
// (the parent's timestamp, or now when the parent is more than one period old),
// recompute whose turn it is, and mark consensus running.
//
// The anchor is a parameter rather than read from the chain here because it has
// to be the same instant the header was stamped with. A view clock anchored to a
// parent the header does not use is a deadline for a round that never started --
// see the call site.
//
// The view loop that drives view changes is started once, on the first round, and
// no-ops until the view is running. That is deliberately different from
// pgp-chain, which starts and stops the loop around rounds: a loop that is
// created per round can be created twice by two racing Prepare calls, and two
// loops halve signTolerance without changing any configuration.
func (e *Engine) startView(headerTime uint64, headHeight uint64) {
	if e.account == nil {
		return
	}
	view := e.dispatcher.GetConsensusView()
	if view.IsRunning() {
		e.dispatcher.ResetView(headerTime)
	} else {
		view.SetChangViewTime(headerTime)
		view.UpdateDutyIndex(headHeight)
	}
	view.SetRunning()
	e.ensureViewLoop()
}

// isEpochTransitionPending reports whether the producer set is rotating.
//
// Not wired to anything yet: the producer rotation arrives with the SPV event
// path in M5 of the port. It exists so that Seal has the check in the right
// place when that lands, and so the answer is false rather than undefined today.
func (e *Engine) isEpochTransitionPending() bool { return false }
