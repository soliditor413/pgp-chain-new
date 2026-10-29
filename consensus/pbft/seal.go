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
	"errors"
	"sync/atomic"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/log"
)

// Seal implements consensus.Engine: one PBFT round.
//
// The shape is a two-phase round, not the textbook three-phase PBFT:
//
//	propose  -> broadcast the proposal, vote for it ourselves
//	collect  -> every accepting vote is counted; at 2/3 the dispatcher builds a
//	            confirm carrying all the signatures
//	attach   -> the confirm is serialized into header.Extra, which is what makes
//	            the block verifiable by every other node
//
// Seal blocks until either that confirm arrives, the votes are rejected, the view
// deadline passes, or the miner gives up. How long that can take, honestly:
//
//	pre-delay  the wait for header.Time, up to allowedFutureBlockTime + period
//	           (15s + 3s = 18s): header.Time is parent.Time + period (prepare.go)
//	           and a parent is accepted up to 15s ahead of our own clock.
//	deadline   measured *after* the pre-delay, so it is at most
//	           signTolerance - period = 3s (6s on the re-anchor path, where
//	           Prepare rewrites header.Time to now and restarts the view).
//	total      the worst case is therefore ~21s, not one window.
//
// The total is not just a latency figure: miner/worker.go calls Seal synchronously
// from its task loop and worker.close waits for that loop, so it is also the
// node's shutdown latency. A shutdown cannot interrupt an in-flight Seal today --
// the stop channel belongs to the task loop, which is busy inside this call. That
// is known debt, recorded as R12 in docs/design/pbft-port-design.md, and it is
// NOT fixed by shortening either wait below.
//
// Neither wait is clamped, deliberately, and the block is never released before
// header.Time. What that means, exactly: it is a statement about this function,
// not a claim that an early block would be lost. header.Time is fixed in Prepare,
// so sleeping less changes nothing about what any verifier accepts -- a verifier
// accepts or rejects against its own clock alone.
//
// A peer that is handed a header dated in its own future does not lose it, but
// what happens to it depends on how far ahead it is and on which binary the peer
// runs:
//
//   - a peer running this binary takes up to 15s of lead outright, no queue and
//     no wait (consensus/pgp/pgp.go:227, against allowedFutureBlockTime at :56);
//     past 15s it is queued and retried every 5s (core/blockchain.go:2404 ->
//     addFutureBlock:2224, updateFutureBlocks:3356-3357), and refused without
//     queueing once the gap passes maxTimeFutureBlocks, 30s
//     (core/blockchain.go:188, applied at :2225);
//   - a 1.9.7 peer is zero-tolerant in its verifier
//     (pgp-chain/consensus/pbft/pbft.go:395, :421) and defers through its own
//     queue instead: pgp-chain/core/blockchain.go:1664 -> addFutureBlock:1491,
//     retried every 5s at :2223, refused beyond 30s at :88.
//
// The real cost of releasing early is therefore the wait, not the loss. The
// first 15s of lead cost a peer running this binary nothing at all; past that
// the block sits in a queue until header.Time, and for that whole window no node
// can produce a block on top of that height. Losing it for good takes more than
// 30s of lead, which is the case R18 covers.
//
// Clamping the pre-delay to a budget B would buy no verification margin at all,
// for that same reason: the accepted window is set by header.Time, which this
// function cannot move. (The clamp that used to live in this function never
// clamped the pre-delay -- it limited the vote deadline -- so the argument that
// stood here had the wrong subject; it is deleted rather than rewritten, because
// correcting it would still leave a paragraph about a modification nobody was
// making.) The way to shrink the 21s is to refuse to start a round on a
// future-dated parent in Prepare, i.e. to gate it at the source; that is M3+ work
// (R12), not a change to the waits in this function.
func (e *Engine) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	if e.isEpochTransitionPending() {
		return ErrEpochTransitionPending
	}
	if e.account == nil {
		return ErrNotProducer
	}
	if atomic.LoadInt32(&e.recovered) == 0 {
		return ErrWaitRecoverStatus
	}

	parent := chain.GetHeader(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	if block.NumberU64() <= e.dispatcher.GetFinishedHeight() {
		return ErrAlreadyConfirmedBlock
	}
	// Vote lock. A confirm for this height that we already hold makes the height
	// final even if our own finishedHeight has not caught up yet (a partition, a
	// restart, a block imported while we were behind). Proposing on top of it
	// would be a double proposal at a finalised height.
	if _, hasConfirm := e.blockPool.GetConfirmByHeight(block.NumberU64()); hasConfirm {
		log.Warn("PBFT: height already has a confirm, refusing to propose", "height", block.NumberU64())
		return ErrAlreadyConfirmedBlock
	}
	if !e.IsProducer() {
		return ErrUnauthorizedSigner
	}
	if !e.IsOnduty() {
		return ErrSignerNotOnduty
	}

	// The block must be in the pool before the proposal goes out: a confirm that
	// arrives while we are still in this function is verified against the pool's
	// copy, so a missing entry turns a valid confirm into "not have DBlock".
	e.blockPool.AddToBlockMap(newDBlock(block))
	transport := e.getTransport()
	if transport != nil {
		transport.BroadcastBlock(block)
	}

	proposal, err := dpos.StartProposal(e.account, elacom.Uint256(pgp.SealHash(block.Header())),
		e.dispatcher.GetConsensusView().GetViewOffset())
	if err != nil {
		return err
	}
	// Register the proposal with the dispatcher before anything can vote on it.
	//
	// dpos.StartProposal only signs; it does not tell the dispatcher that this
	// proposal is the one being processed. Every vote is matched against
	// dispatcher.GetProcessingProposal() and rejected with "not proposal to
	// process vote" when that is nil, so skipping this call makes the round wait
	// for votes that can never be counted and time out on every height. It is
	// also what fixes the view offset the vote comparison uses.
	//
	// The peer id is our own public key: ProcessProposal treats "id matches the
	// sponsor" as the self-proposal case, which is exactly what this is.
	var self peer.PID
	copy(self[:], e.account.PublicKeyBytes())
	if err, _, _ := e.dispatcher.ProcessProposal(self, proposal); err != nil {
		return err
	}

	atomic.StoreInt32(&e.isSealing, 1)
	defer atomic.StoreInt32(&e.isSealing, 0)

	// One round, one resolution: drop the previous round's leftovers, and the
	// previous round's "already over" flag, before anything can produce a confirm
	// for this one.
	//
	// WHERE this sits is the whole point, and it is *before* the self-vote below,
	// not after it. legacyMajorityCount rounds down (consensus/pgp/confirm.go), so
	// a producer set of one needs zero votes and a set of two needs one: with one
	// or two producers the vote AcceptProposal casts *is* the majority, and
	// ProcessVote builds the confirm and calls onConfirm synchronously on this very
	// goroutine, before Seal ever reaches its select. Draining after the self-vote
	// therefore discarded the round's own confirm; the round then timed out, and
	// the height could not be retried either, because the confirm was already in
	// blockPool and every later Seal answered ErrAlreadyConfirmedBlock. That is a
	// permanent stall at height 1, which is exactly what the devnet showed.
	//
	// The drain also closes a gap the channels cannot close themselves: the
	// previous round ended somewhere between its finishRound and here, and
	// anything a vote goroutine delivered in that window belongs to it, not to
	// this round. Without the drain a stale confirm would be compared against this
	// block, fail the comparison below, and abort a round that was going fine.
	//
	// It sits after ProcessProposal and after isSealing=1 on purpose:
	//   - after ProcessProposal, so a late confirm for the *previous* proposal is
	//     rejected by onConfirm's "is this the proposal being sealed" comparison
	//     instead of being delivered into this round's select;
	//   - after isSealing=1, so a confirm that does belong to this round cannot be
	//     dropped for arriving while isSealing was still 0.
	//
	// The invariant to preserve when editing this function, stated once:
	//
	//	this drain must stay ABOVE every statement that can produce a confirm
	//	for this round (AcceptProposal and its ProcessVote), and
	//	BroadcastProposal must stay BELOW it.
	//
	// Moving the broadcast above the drain is what would break it: a peer can then
	// confirm inside the window between isSealing=1 and the drain, and the drain
	// eats that confirm -- exactly the bug this ordering exists to fix. Seal holds
	// one drain (this one) plus the one in finishRound, and no others.
	e.drainRoundChannels()

	// isSealOver is cleared in two places -- here and prepare.go:57 -- and this
	// one is redundant defence, not a load-bearing line: the miner prepares each
	// task (worker.go:1132) before it seals it (:626), so the flag has already
	// been cleared before control gets here. Measured rather than assumed:
	// deleting this line leaves the whole pbft suite green, including the
	// multi-height single-producer case.
	//
	// Do not delete both, though. With Prepare's reset gone as well, a stale
	// isSealOver==1 survives into the next round, onConfirm refuses that round's
	// own confirm (:416), and the round times out at a height it should have
	// sealed. QA measured that variant failing at height 2; it is not reproduced
	// here because reproducing it means mutating prepare.go, which this change
	// deliberately does not touch.
	//
	// Why keep a redundant store at all: the round's own bookkeeping belongs to
	// the function that owns the round. Leaving one caller's worth of ordering
	// (three files away, in the miner) as the only thing keeping this flag
	// consistent is the kind of hidden precondition this file already avoids
	// elsewhere. Two atomic stores per round is the cheaper side of that trade.
	atomic.StoreInt32(&e.isSealOver, 0)

	// Our own acceptance vote goes through the same dispatcher path a peer's vote
	// takes. With it the proposer counts itself, which is what makes an 8-of-12
	// threshold reachable; without it the proposer would need every other
	// producer to answer.
	if vote := e.dispatcher.AcceptProposal(proposal, e.account); vote != nil {
		if transport != nil {
			transport.BroadcastProposal(proposal)
			transport.BroadcastVote(vote)
		}
		if _, _, err := e.dispatcher.ProcessVote(&vote.Vote); err != nil {
			log.Debug("PBFT: own vote not accepted", "err", err)
		}
	}

	header := block.Header()

	// Wait until the block's own timestamp before asking for the votes. Not
	// because an early block would be thrown away -- a peer queues it until
	// header.Time -- but because until that moment no node can extend this
	// height, so releasing it early only moves the same stall somewhere this
	// process cannot see or bound. See the function comment for why this is not
	// clamped.
	delay := time.Unix(int64(header.Time), 0).Sub(e.dispatcher.GetNowTime())
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-stop:
			e.finishRound("stop")
			e.resetRound(block.NumberU64())
			return ErrRoundAborted
		}
	}

	// The view deadline, measured from here rather than from Seal's entry: the
	// pre-delay above already consumed whatever was left of the window, which is
	// why this value is at most signTolerance - period (3s) -- or 6s on the
	// re-anchor path, where Prepare restarted the view at now. A partition cannot
	// hold the miner past it.
	//
	// Do not add an upper clamp here. It would be dead code: with
	// viewChangeTime = parent.Time + signTolerance and now' >= header.Time =
	// parent.Time + period, deadline <= signTolerance - period < signTolerance
	// always, and ChangeView only ever moves viewChangeTime closer. See the
	// function comment for where the real 21s bound comes from.
	deadline := e.dispatcher.GetConsensusView().GetChangeViewTime().Sub(e.dispatcher.GetNowTime())
	if deadline < 0 {
		deadline = 0
	}

	// One round, one resolution. The drain above already cleared both round
	// channels and isSealOver; by the time control reaches this select the round's
	// own confirm may already be queued (it is produced synchronously for a
	// one-or-two-producer set), so it must be read, not drained.
	select {
	case confirm := <-e.confirmCh:
		// Second line of defence. Both channels are drained at the round
		// boundaries (once here at the start of the round, once in finishRound), so
		// a confirm still carrying another block's seal hash was completed after
		// this round's drain: a late vote from the previous view, or two views
		// proposing at once. Attaching it would put a signature set for a
		// different block into this header, which every verifier then rejects, so
		// the comparison removes the possibility entirely.
		if want := common.Hash(confirm.Proposal.BlockHash); want != pgp.SealHash(header) {
			log.Warn("PBFT: confirm belongs to a different block, ignoring",
				"height", block.NumberU64(), "confirm", want, "block", pgp.SealHash(header))
			e.finishRound("stale confirm")
			e.resetRound(block.NumberU64())
			return ErrRoundAborted
		}
		if err := e.addConfirmToBlock(header, confirm); err != nil {
			// Same permanent-stall hazard as the abort paths below: the confirm
			// exists but could not be attached, so no block is produced and the
			// view would otherwise stay Running forever. The error itself is
			// still returned, because a confirm that cannot be serialized is a
			// fault rather than a lost round.
			e.finishRound("attach failed")
			e.resetRound(block.NumberU64())
			return err
		}
		atomic.StoreInt32(&e.isSealOver, 1)
	case <-e.unConfirmCh:
		log.Warn("PBFT: proposal was rejected by the producer set", "height", block.NumberU64())
		e.finishRound("rejected")
		e.resetRound(block.NumberU64())
		return ErrRoundAborted
	case <-time.After(deadline):
		log.Warn("PBFT: seal timed out waiting for a confirm", "height", block.NumberU64())
		e.finishRound("timeout")
		e.resetRound(block.NumberU64())
		return ErrRoundAborted
	case <-stop:
		log.Debug("PBFT: seal interrupted by the miner", "height", block.NumberU64())
		e.finishRound("stop")
		e.resetRound(block.NumberU64())
		return ErrRoundAborted
	}

	finalBlock := block.WithSeal(header)
	select {
	case results <- finalBlock:
		if transport != nil {
			transport.BroadcastBlock(finalBlock)
			if confirm, ok := e.blockPool.GetConfirmByHeight(block.NumberU64()); ok {
				transport.BroadcastConfirm(confirm)
			}
		}
		e.blockPool.CleanFinalConfirmedBlock(block.NumberU64())
	default:
		// The miner replaced this task while we were waiting. Dropping the result
		// is correct: the next task carries a newer header.
		log.Warn("PBFT: sealing result is not read by the miner", "height", block.NumberU64())
	}
	return nil
}

// addConfirmToBlock serializes the confirm into header.Extra.
//
// This is the single write of the PBFT proof into a block, and it must go through
// (*payload.Confirm).Serialize -- the exact inverse of the decodeConfirm that
// consensus/pgp runs on arrival. Serializing by hand here is how a chain forks
// against its own verifier.
func (e *Engine) addConfirmToBlock(header *types.Header, confirm *payload.Confirm) error {
	sealBuf := new(bytes.Buffer)
	if err := confirm.Serialize(sealBuf); err != nil {
		log.Error("PBFT: cannot serialize confirm", "err", err)
		return err
	}
	header.Extra = make([]byte, sealBuf.Len())
	copy(header.Extra, sealBuf.Bytes())

	// The seal hash excludes Extra by construction, so attaching the confirm does
	// not change what the signatures commit to. Recomputing it here therefore
	// yields the hash the proposal carried, which is the invariant the wrapped
	// call below records as final.
	sealHash := pgp.SealHash(header)
	e.dispatcher.FinishedProposal(header.Number.Uint64(), elacom.Uint256(sealHash), header.Time)
	return nil
}

// finishRound marks the round resolved and empties what late votes left behind.
//
// It deliberately does not close or recreate the round channels: this is the only
// place that ever did, and doing it is unsafe. See drainRoundChannels.
func (e *Engine) finishRound(reason string) {
	atomic.StoreInt32(&e.isSealOver, 1)
	e.drainRoundChannels()
	log.Debug("PBFT: round finished", "reason", reason)
}

// drainRoundChannels discards everything currently queued on the round channels.
//
// Draining, rather than closing, is what keeps one round's result out of the
// next. The channels are read by Seal and written by whichever goroutine processed
// the last vote, so a writer may be holding the channel reference across a round
// boundary: trySendConfirm reads the field and only then sends. Closing at that
// moment turns the in-flight send into a panic on a closed channel, which is a
// crash on a node that is working exactly as intended (a late vote is normal on a
// network). Recreating the channels instead would leave that writer pointing at a
// channel nobody reads again.
//
// Nothing is lost by giving up close(): Seal is the only closer, and it is also
// the only reader, so closing never woke anybody up. Seal stops waiting on `stop`
// from the miner and on the view deadline, never on channel closure.
//
// It must drain to empty rather than once: a round can leave both an accept and a
// reject behind, and either of them reaching the next round would be attributed to
// the wrong block.
func (e *Engine) drainRoundChannels() {
	drainConfirms(e.confirmCh)
	drainConfirms(e.unConfirmCh)
}

// drainConfirms empties one round channel without blocking.
func drainConfirms(ch chan *payload.Confirm) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// resetRound releases the view after a round that produced no block.
//
// This is the ONLY path that lets a node keep producing after a failed round, so
// dropping it does not degrade anything gracefully -- it stops the node for good.
// The view is a two-state machine (dpos.ConsensusView): SetRunning is called by
// Prepare->startView, and the only two calls that put it back into
// ConsensusReady are Dispatcher.FinishedProposal (which runs when this node
// actually sealed a block) and Dispatcher.ResetConsensus. So a round that ends
// without a block leaves the view Running, and every later Prepare answers
// ErrConsensusIsRunning for the rest of the node's life.
//
// ResetConsensus also does the rest of the work a retry needs: it clears the
// processed proposal and the vote tallies, resets the view offset, refreshes the
// duty index, and calls ChangeView(force=true), whose listener notification
// reaches StartMine->miner.RequestNewWork. That wake-up is what actually makes
// the next attempt happen, instead of waiting for the next chain head.
func (e *Engine) resetRound(height uint64) {
	if e.dispatcher == nil {
		return
	}
	e.dispatcher.ResetConsensus(height)
}

// onConfirm is the dispatcher callback that fires when 2/3 of the producers have
// accepted a proposal.
func (e *Engine) onConfirm(confirm *payload.Confirm) error {
	if confirm == nil {
		return errors.New("pbft: nil confirm")
	}
	if err := e.blockPool.AppendConfirm(confirm); err != nil {
		log.Debug("PBFT: received unusable confirm", "proposal", confirm.Proposal.Hash(), "err", err)
		return err
	}
	if !e.IsOnduty() {
		// A follower: recording the confirm in the pool is enough, the block will
		// arrive over the normal block propagation path.
		return nil
	}
	if atomic.LoadInt32(&e.isSealOver) == 1 {
		return errors.New("pbft: seal already finished, cannot confirm")
	}
	current := e.dispatcher.GetProcessingProposal()
	if current == nil || !current.BlockHash.IsEqual(confirm.Proposal.BlockHash) {
		return errors.New("pbft: confirm does not belong to the proposal being sealed")
	}
	if atomic.LoadInt32(&e.isSealing) == 1 {
		e.trySendConfirm(confirm)
	}
	return nil
}

// onUnConfirm is the dispatcher callback for a rejected proposal.
func (e *Engine) onUnConfirm(unconfirm *payload.Confirm) error {
	if unconfirm == nil {
		return nil
	}
	if atomic.LoadInt32(&e.isSealing) == 1 {
		e.trySendUnconfirm(unconfirm)
	}
	return nil
}

// trySendConfirm delivers the confirm to the Seal waiting on it.
//
// Never blocks, and never closes: the channel is buffered with one slot and a
// dropped confirm is recovered by the view change -- finishRound resets the round,
// the next view retries the same height -- whereas blocking here would stall the
// vote-processing goroutine the whole producer set depends on. Stalling that
// goroutine is the worse failure by far: it stops votes being counted for every
// height, not just this one.
//
// The channel field is read without a lock because it is immutable after New; see
// the Engine field comment.
func (e *Engine) trySendConfirm(confirm *payload.Confirm) {
	select {
	case e.confirmCh <- confirm:
	default:
		log.Warn("PBFT: confirm channel busy, dropping confirm")
	}
}

// trySendUnconfirm delivers a rejection to the Seal waiting on it, with the same
// non-blocking and non-closing contract as trySendConfirm.
func (e *Engine) trySendUnconfirm(unconfirm *payload.Confirm) {
	select {
	case e.unConfirmCh <- unconfirm:
	default:
		log.Warn("PBFT: unconfirm channel busy, dropping reject")
	}
}
