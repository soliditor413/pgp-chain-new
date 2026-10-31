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

// The inbound half of the DPoS channel: one method per decoded message command,
// called by dpos.Network on its own goroutine.
//
// Ported from pgp-chain/consensus/pbft/network.go. Four kinds of deviation, each
// marked at the method:
//
//  1. chain access. Upstream holds a *core.BlockChain and calls InsertChain
//     directly. This package cannot name core (core imports consensus), so the
//     write half is a narrow interface set by AttachBlockStore -- same answer as
//     D3 for consensus.Engine.
//  2. chainbridge traffic. OnSmallCroTxReceived and OnFailedWithdrawTxReceived
//     are no-ops here: the business is not ported (D10). The messages still
//     decode, they just do not do anything.
//  3. pgp-chain's producer-set machinery. changeNextTurnProduces,
//     blocksigner.SelfIsProducer and the BPoS validator cache have no counterpart
//     and are dropped; OnProducersMsg keeps only the part that drives this tree's
//     dispatcher. The rest is M5 work (#27).
//  4. producer participation statistics (producerStats) and the blacklist oracle
//     are not ported at all, so OnBlock has no stats branch.

// The two "common" packages must not both be imported under the same name:
// Elastos.ELA/common is used for Uint256, go-ethereum/common for Hash. Only the
// Elastos one is aliased, because every geth call site in this file says
// common.Hash and aliasing that one would make the file read backwards.
import (
	"bytes"
	"sort"
	"sync/atomic"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// maxRequestedBlocks bounds the pending-block request map, so a peer that
	// keeps announcing new hashes cannot grow it without limit.
	maxRequestedBlocks = 1024

	// maxOnBlockSleepDelay caps how long OnBlockReceived may wait for a block's
	// timestamp to arrive. The timestamp is peer supplied and not yet verified;
	// without the cap any peer could stall the whole DPoS message loop, which is
	// serial.
	maxOnBlockSleepDelay = 3 * time.Second

	// recoverWaitTimeout bounds how long a recovery exchange waits for peer
	// statuses before proceeding with whatever arrived.
	recoverWaitTimeout = 3 * time.Second

	// startupRecoverWait is how long StartRecovery waits for a producer majority
	// before it starts warning. Upstream uses two minutes for the same wait
	// (pgp-chain/consensus/pbft/pbft.go:1023); the value is not consensus
	// critical, it only decides how long the useful state is "starting up"
	// rather than "misconfigured".
	startupRecoverWait = 2 * time.Minute
)

// blockReader is the block-body half of the chain.
//
// consensus.ChainHeaderReader only reads headers, and AttachChain explains why
// that is the whole of what the production path needs. The sync path is
// different: a peer announces a block hash and this node has to answer with the
// full block, so it needs bodies.
type blockReader interface {
	GetBlock(hash common.Hash, number uint64) *types.Block
	GetBlockByHash(hash common.Hash) *types.Block

	// CurrentHeader, not CurrentBlock: *core.BlockChain.CurrentBlock returns a
	// *types.Header in this fork, and CurrentHeader is the name that says so.
	// Only the number is used, and a header is all the number lives in.
	CurrentHeader() *types.Header
}

// blockInserter is the write half of the chain.
type blockInserter interface {
	InsertChain(blocks types.Blocks) (int, error)
}

// AttachBlockStore wires the sync path to the chain.
//
// It is separate from AttachChain for the same reason AttachChain is separate
// from New: the engine is built before the chain exists. A nil store is legal and
// means "no sync": OnInv / OnGetBlock / OnBlockReceived then do nothing, which is
// the correct shape for a pure verifier and for the single-node acceptance run.
func (e *Engine) AttachBlockStore(blocks blockReader, inserter blockInserter) {
	e.blocks = blocks
	e.inserter = inserter
}

// SetPeerCounter installs the "how many DPoS peers are connected" probe.
//
// The recovery and block-sync paths use it to decide whether the set is large
// enough to make progress, so a node without a transport (nil) never thinks it
// has peers.
func (e *Engine) SetPeerCounter(fn func() int) {
	e.peerCount = fn
}

func (e *Engine) activePeerCount() int {
	if e.peerCount == nil {
		return 0
	}
	return e.peerCount()
}

// ---- status sync -----------------------------------------------------------------

// OnPing and OnPong carry the peer's height in their nonce.
//
// Upstream leaves them empty (network.go:323-329) and derives nothing from the
// value: the height already arrives in the Version handshake, and the nonce is
// only used to select the DPoS payload version, which this port pins
// unconditionally (wireversion.go). Kept as documented no-ops rather than removed,
// because the interface requires them and a future use should not need a
// signature change.
func (e *Engine) OnPing(id dpeer.PID, height uint32) {}
func (e *Engine) OnPong(id dpeer.PID, height uint32) {}

// OnBlock is the "here is a block" push from a producer.
//
// Two shapes arrive on the same command and the length of Extra tells them apart.
// In pgp-chain the header is [32 byte vanity][confirm], so
// len(Extra) > extraVanity means "this block carries a confirm". This tree has no
// vanity at all -- Prepare sets header.Extra = nil unconditionally (D6) -- so any
// non-empty Extra means the same thing, and the comparison is against 0.
func (e *Engine) OnBlock(id dpeer.PID, block *dmsg.BlockMsg) {
	b := &types.Block{}
	if err := b.DecodeRLP(rlp.NewStream(bytes.NewBuffer(block.GetData()), 0)); err != nil {
		// Peer-controlled input: never panic, just drop it.
		log.Error("OnBlock: decode failed", "peer", id.String(), "err", err)
		return
	}
	if len(b.Extra()) > 0 {
		e.OnBlockReceived(id, block, true)
		return
	}
	if e.dispatcher == nil {
		return
	}
	head := e.headNumber()
	if b.NumberU64() <= head || b.NumberU64() <= e.dispatcher.GetFinishedHeight() {
		e.blockPool.AddBadBlock(newDBlock(b))
		log.Warn("OnBlock: stale height", "chain", head, "block", b.NumberU64(),
			"finished", e.dispatcher.GetFinishedHeight())
		return
	}

	// SealHash, not b.Hash(): the pool is keyed by what a proposal commits to.
	sealHash := pgp.SealHash(b.Header())
	if err := e.blockPool.AppendDposBlock(newDBlock(b)); err == consensus.ErrUnknownAncestor {
		log.Info("OnBlock: queued as future block", "height", b.NumberU64())
		e.blockPool.AppendFutureBlock(newDBlock(b))
	}

	if hash, err := elacom.Uint256FromBytes(sealHash.Bytes()); err == nil {
		if c, ok := e.blockPool.GetConfirm(*hash); ok {
			e.OnConfirmReceived(id, c, b.NumberU64())
		}
	}

	e.reqMu.Lock()
	delete(e.requestedBlocks, sealHash)
	e.reqMu.Unlock()
}

// OnInv is "I have this block". Answer with a GetBlock to the sender only.
func (e *Engine) OnInv(id dpeer.PID, blockHash elacom.Uint256) {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	if !e.dispatcher.IsProducer(e.account.PublicKeyBytes()) {
		return
	}
	if e.blockPool.HasBlock(blockHash) {
		return
	}
	hash := common.BytesToHash(blockHash.Bytes())
	if e.blocks != nil && e.blocks.GetBlockByHash(hash) != nil {
		return
	}

	e.reqMu.Lock()
	if _, ok := e.requestedBlocks[hash]; ok {
		e.reqMu.Unlock()
		return
	}
	if len(e.requestedBlocks)+1 > maxRequestedBlocks {
		for h := range e.requestedBlocks {
			delete(e.requestedBlocks, h)
			break
		}
	}
	e.requestedBlocks[hash] = struct{}{}
	e.reqMu.Unlock()

	log.Debug("[OnInv] requesting block", "hash", blockHash.String())
	go func() {
		if t := e.getTransport(); t != nil {
			if err := t.SendToPeer(id, emsg.NewGetBlock(blockHash)); err != nil {
				log.Debug("[OnInv] send getblock failed", "err", err)
			}
		}
	}()
}

// OnGetBlock answers a peer's GetBlock from the pool, then from the chain.
func (e *Engine) OnGetBlock(id dpeer.PID, blockHash elacom.Uint256) {
	block, ok := e.blockPool.GetBlock(blockHash)
	if !ok && e.blocks != nil {
		if b := e.blocks.GetBlockByHash(common.BytesToHash(blockHash.Bytes())); b != nil {
			block, ok = newDBlock(b), true
		}
	}
	if !ok || block == nil {
		return
	}
	// The pool stores dpos.DBlock, which in this tree is always *dBlock -- the
	// wrapper legacy.go builds, because that is the only type SealHash accepts.
	// The chain fallback above wraps for the same reason. Both paths therefore
	// unwrap the same way; a bare *types.Block here would mean something
	// bypassed newDBlock.
	blk, isEthBlock := block.(*dBlock)
	if !isEthBlock || blk == nil || blk.Block == nil {
		log.Error("[OnGetBlock] pooled block is not an ethereum block")
		return
	}
	buf := new(bytes.Buffer)
	if err := blk.Block.EncodeRLP(buf); err != nil {
		log.Error("[OnGetBlock] encode failed", "err", err)
		return
	}
	log.Debug("[OnGetBlock] sending block", "peer", id.String(), "height", blk.NumberU64())
	go func() {
		if t := e.getTransport(); t != nil {
			if err := t.SendToPeer(id, dmsg.NewBlockMsg(buf.Bytes())); err != nil {
				log.Debug("[OnGetBlock] send failed", "err", err)
			}
		}
	}()
}

// OnGetBlocks and OnResponseBlocks are empty upstream too
// (pgp-chain/consensus/pbft/network.go:579-585): the range-sync path was never
// finished, and only the single-block path above is used. Kept as no-ops so the
// commands decode and the connection stays up.
func (e *Engine) OnGetBlocks(id dpeer.PID, startBlockHeight, endBlockHeight uint32) {}
func (e *Engine) OnResponseBlocks(id dpeer.PID, blockConfirms []*dmsg.BlockMsg)     {}

// OnRequestConsensus answers a peer that is recovering and wants to know where
// the set is.
func (e *Engine) OnRequestConsensus(id dpeer.PID, height uint64) {
	if e.dispatcher == nil || !e.IsProducer() {
		return
	}
	if !e.dispatcher.GetConsensusView().IsProducers(id[:]) {
		log.Warn("[OnRequestConsensus] requester is not a producer", "id", id.String())
		return
	}
	status := e.dispatcher.HelpToRecoverAbnormal(id, height, e.headNumber())
	if status == nil {
		return
	}
	response := &dmsg.ResponseConsensus{Consensus: *status}
	go func() {
		if t := e.getTransport(); t != nil {
			if err := t.SendToPeer(id, response); err != nil {
				log.Debug("[OnRequestConsensus] send failed", "err", err)
			}
		}
	}()
}

// OnResponseConsensus collects one peer's status during a recovery exchange.
func (e *Engine) OnResponseConsensus(id dpeer.PID, status *dmsg.ConsensusStatus) {
	if e.dispatcher == nil || !e.IsProducer() {
		return
	}
	if !e.isRecoverStarted() {
		return
	}
	key := common.Bytes2Hex(id[:])
	e.statusMapMu.Lock()
	defer e.statusMapMu.Unlock()
	if e.statusMap[status.ViewOffset][key] != nil {
		return
	}
	if _, ok := e.statusMap[status.ViewOffset]; !ok {
		e.statusMap[status.ViewOffset] = make(map[string]*dmsg.ConsensusStatus)
	}
	e.statusMap[status.ViewOffset][key] = status
	log.Debug("[OnResponseConsensus]", "peer", id.String(), "viewOffset", status.ViewOffset)
}

// OnRequestProposal replays our in-flight proposal to a peer that missed it.
func (e *Engine) OnRequestProposal(id dpeer.PID, hash elacom.Uint256) {
	if e.dispatcher == nil {
		return
	}
	current := e.dispatcher.GetProcessingProposal()
	if current == nil {
		return
	}
	response := &emsg.Proposal{Proposal: *current}
	go func() {
		if t := e.getTransport(); t != nil {
			if err := t.SendToPeer(id, response); err != nil {
				log.Debug("[OnRequestProposal] send failed", "err", err)
			}
		}
	}()
}

// OnIllegalProposalReceived and OnIllegalVotesReceived feed the blacklist, which
// is M6 work (the slashing path has no consumer in this tree yet). The messages
// are logged and dropped.
func (e *Engine) OnIllegalProposalReceived(id dpeer.PID, proposals *payload.DPOSIllegalProposals) {
	log.Warn("[OnIllegalProposalReceived] not handled yet (blacklist is M6)", "peer", id.String())
}

func (e *Engine) OnIllegalVotesReceived(id dpeer.PID, votes *payload.DPOSIllegalVotes) {
	log.Warn("[OnIllegalVotesReceived] not handled yet (blacklist is M6)", "peer", id.String())
}

// ---- consensus round -------------------------------------------------------------

// OnProposalReceived is the main inbound path for a round.
//
// The order of the checks matters and is upstream's: an already-finalised height
// is rejected before anything else so a late proposal cannot move us backwards,
// and a proposal whose block we do not have is answered with a block request
// rather than a vote, because voting on a block we cannot verify would be a blind
// signature.
func (e *Engine) OnProposalReceived(id dpeer.PID, proposal *payload.DPOSProposal) {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	log.Debug("OnProposalReceived", "hash", proposal.Hash().String(), "from", id.String())

	e.reqMu.Lock()
	delete(e.requestedProposals, proposal.Hash())
	e.reqMu.Unlock()

	if e.dispatcher.GetProcessingProposal() != nil {
		log.Debug("OnProposalReceived: already processing a proposal")
		return
	}
	view := e.dispatcher.GetConsensusView()
	if !view.IsRunning() {
		log.Debug("OnProposalReceived: consensus not running")
		return
	}
	e.OnChangeView()

	if proposal.BlockHash.IsEqual(e.dispatcher.GetFinishedBlockSealHash()) {
		log.Debug("OnProposalReceived: block already finished")
		return
	}

	isBad := e.blockPool.IsBadBlockProposal(proposal)
	if _, ok := e.blockPool.GetBlock(proposal.BlockHash); !ok && !isBad {
		if e.blockPool.IsFutureBlock(proposal.BlockHash) {
			log.Debug("OnProposalReceived: future proposal, waiting for block sync")
			return
		}
		log.Debug("OnProposalReceived: block unknown, requesting it", "hash", proposal.BlockHash.String())
		e.OnInv(id, proposal.BlockHash)
		return
	}

	var voteMsg *emsg.Vote
	err, sendReject, handled := e.dispatcher.ProcessProposal(id, proposal)
	switch {
	case err != nil:
		log.Error("OnProposalReceived: ProcessProposal failed", "err", err)
		if sendReject {
			voteMsg = e.dispatcher.RejectProposal(proposal, e.account)
		} else if !handled {
			key := common.Bytes2Hex(id[:])
			e.reqMu.Lock()
			e.notHandledProposal[key] = struct{}{}
			count := len(e.notHandledProposal)
			e.reqMu.Unlock()
			log.Debug("[OnProposalReceived] not handled", "count", count)
			if view.GetViewOffset() != 0 && view.HasArbitersMinorityCount(count) {
				log.Info("[OnProposalReceived] minority could not handle the proposal, recovering")
				e.recoverAbnormalState()
			}
		}
	case isBad:
		log.Info("OnProposalReceived: bad proposal, rejecting")
		voteMsg = e.dispatcher.RejectProposal(proposal, e.account)
	case e.hasConfirmedCompetitor(proposal):
		// Vote lock: a different block at this height is already confirmed, so
		// accepting would double-sign a finalised height.
		log.Warn("OnProposalReceived: rejecting, height already finalised with another block",
			"proposal", proposal.Hash().String())
		voteMsg = e.dispatcher.RejectProposal(proposal, e.account)
	default:
		voteMsg = e.dispatcher.AcceptProposal(proposal, e.account)
	}

	if handled {
		e.reqMu.Lock()
		e.notHandledProposal = make(map[string]struct{})
		e.reqMu.Unlock()
	}
	if voteMsg != nil {
		if t := e.getTransport(); t != nil {
			t.BroadcastVote(voteMsg)
		}
	}
}

// OnVoteAccepted handles an accept vote (and, via OnVoteRejected, a reject one).
func (e *Engine) OnVoteAccepted(id dpeer.PID, vote *payload.DPOSProposalVote) {
	if e.dispatcher == nil || !e.IsProducer() {
		return
	}
	view := e.dispatcher.GetConsensusView()
	if !view.IsRunning() {
		return
	}
	if e.dispatcher.GetFinishedProposal().IsEqual(vote.ProposalHash) {
		return
	}
	if _, ok := e.blockPool.GetConfirm(vote.ProposalHash); ok {
		return
	}

	// A proposer that is not currently sealing has nothing to collect votes for.
	if atomic.LoadInt32(&e.isSealing) == 0 && e.IsOnduty() {
		return
	}

	// The vote names the PROPOSAL it belongs to; the pool is keyed by the block's
	// seal hash. Those are two different values -- a proposal hash covers the
	// sponsor and the view offset as well as the block -- so looking the vote's hash
	// up in the pool never matched, every vote took the "unknown" branch below and
	// was parked in pendingVotes, and the proposer held its own peers' votes while
	// its round waited for a quorum. That is the devnet's third stall: the proposer
	// timed out at height 1 with 0 of the 2 votes it needed while all three peers
	// had voted and the votes had arrived.
	//
	// Upstream does not consult the pool here at all. It compares the vote against
	// the proposal currently being processed
	// (pgp-chain/consensus/pbft/network.go:722, tryGetCurrentProposal) and asks the
	// sender for the proposal when it has none. The request half does not exist in
	// this port -- the transport carries no RequestProposal message -- so a vote for
	// a proposal we are not processing is parked, which is upstream's "not have
	// proposal, push vote into pending vote" branch.
	processing := e.dispatcher.GetProcessingProposal()
	if processing == nil || !processing.Hash().IsEqual(vote.ProposalHash) {
		log.Debug("OnVoteAccepted: proposal not being processed, holding the vote",
			"hash", vote.ProposalHash.String())
		e.dispatcher.AddPendingVote(vote)
		return
	}
	if _, ok := e.blockPool.GetConfirm(processing.BlockHash); ok {
		log.Warn("OnVoteAccepted: proposal already confirmed")
		return
	}
	if _, _, err := e.dispatcher.ProcessVote(vote); err != nil {
		log.Error("OnVoteAccepted: ProcessVote failed", "err", err)
	}
}

// OnVoteRejected routes a reject vote into the same bookkeeping: the dispatcher
// counts accept and reject separately.
func (e *Engine) OnVoteRejected(id dpeer.PID, vote *payload.DPOSProposalVote) {
	e.OnVoteAccepted(id, vote)
}

// OnChangeView advances the view and, past maxViewOffset, asks the set to reset.
func (e *Engine) OnChangeView() {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	e.dispatcher.OnChangeView()

	if e.dispatcher.GetConsensusView().GetViewOffset() >= maxViewOffset {
		m := &emsg.ResetView{Sponsor: e.account.PublicKeyBytes()}
		buf := new(bytes.Buffer)
		if err := m.SerializeUnsigned(buf); err != nil {
			log.Error("OnChangeView: serialize reset view failed", "err", err)
			return
		}
		m.Sign = e.account.Sign(buf.Bytes())
		if t := e.getTransport(); t != nil {
			t.BroadcastResetView(m)
		}
		// Record ourselves first: the broadcast above does not loop back.
		if !e.dispatcher.ResetViewRequestIsContain(m.Sponsor) {
			e.OnResponseResetViewReceived(m)
		}
	}
}

// OnBadNetwork is a notification, not a decision: the network layer has already
// re-announced our address (dpos/network.go notifyFlag).
func (e *Engine) OnBadNetwork() {
	log.Warn("DPoS network went bad")
}

// processPrecociousProposal replays the proposal that arrived while our view was
// behind the proposer's.
//
// ProcessProposal stashes such a proposal instead of rejecting it outright, and
// this is the other half of that arrangement: the moment our view catches up, the
// proposal belongs to the current round and has to be handled, or the set never
// collects the quorum that is waiting on our vote. See
// Dispatcher.UpdatePrecociousProposals for why nothing else pops the stash.
//
// The zero PID is upstream's (pgp-chain/consensus/pbft/pbft.go:1185). It is not a
// placeholder for an unknown peer: the proposal did not arrive from anyone just
// now, it was stored earlier, and ProcessProposal only uses the id to tell "this is
// our own proposal" from "this came from a peer". Zero means neither, which is
// correct -- we already handled it once.
func (e *Engine) processPrecociousProposal() {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	proposal := e.dispatcher.UpdatePrecociousProposals()
	if proposal == nil {
		return
	}
	log.Info("PBFT: replaying a proposal that arrived ahead of our view",
		"offset", proposal.ViewOffset, "block", proposal.BlockHash.String())
	e.OnProposalReceived(dpeer.PID{}, proposal)
}

// OnRecover asks the set where it is, then adopts the majority answer.
func (e *Engine) OnRecover() {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	if !e.dispatcher.IsProducer(e.account.PublicKeyBytes()) {
		return
	}
	e.recoverAbnormalState()
}

// StartRecovery is the cold-start kick: wait until enough producers are actually
// connected, then run the recovery exchange.
//
// Without it a freshly started producer never leaves the "waiting for consensus
// status recovery" state and never produces a block. Nothing else calls
// OnRecover at startup: its two callers are Network.recover, which fires when the
// network goes bad after having been good, and OnConfirmReceived, which needs a
// confirm from a peer -- and a set that has never finalised a height has none to
// send. The result is a deadlock between "produce" (gated on recovered) and
// "recover" (gated on a peer event that only happens after someone produces).
//
// Upstream has the same kick in finishInitCurrentProducers -> p.Recover()
// (pgp-chain/consensus/pbft/pbft.go:263-271 and :1010-1045), including the part
// that matters here: it waits for a peer majority before recovering, because at
// process start the TCP layer has just been handed its dial list and has not
// connected anyone yet. recoverAbnormalState refuses to run with fewer peers than
// a majority (and only logs at Error), so an immediate call would be a no-op.
//
// One deliberate difference from upstream: on timeout upstream marks the engine
// recovered and lets it produce alone (pbft.go:1035-1041). This port keeps
// waiting and only warns. A producer set of N>1 cannot finalise without a
// quorum -- legacyMajorityCount of 4 is 2 votes -- so "produce alone" does not
// make progress, it makes N competing chains, and the bench would then have to
// explain a fork instead of a stall. A single-producer set never reaches this
// code: startPBFTNetwork leaves its transport nil and StartMining releases the
// gate itself.
func (e *Engine) StartRecovery() {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	go func() {
		deadline := time.Now().Add(startupRecoverWait)
		for {
			select {
			case <-e.stopCh:
				return
			default:
			}
			need := e.minAcceptVoteCount()
			have := e.activePeerCount()
			want := need
			if want < 1 {
				want = 1
			}
			if have >= want && want > 0 && e.IsCurrent() {
				log.Info("PBFT: producers connected, starting recovery",
					"peers", have, "need", need)
				e.OnRecover()
				return
			}
			if time.Now().After(deadline) {
				// Keep waiting, loudly. See the note above on why this does not
				// adopt upstream's produce-anyway timeout.
				log.Warn("PBFT: still waiting for producers before recovery",
					"peers", have, "need", want,
					"hint", "check --pbft.net.port reachability and --pbft.net.magic on every producer")
				deadline = time.Now().Add(startupRecoverWait)
			}
			time.Sleep(time.Second)
		}
	}()
}

// OnResponseResetViewReceived tallies reset-view requests and resets once enough
// producers have asked.
func (e *Engine) OnResponseResetViewReceived(m *emsg.ResetView) {
	if e.dispatcher == nil || !e.IsProducer() {
		return
	}
	if !e.dispatcher.GetConsensusView().IsProducers(m.Sponsor) {
		log.Warn("[OnResponseResetViewReceived] sponsor is not a producer",
			"sponsor", common.Bytes2Hex(m.Sponsor))
		return
	}
	if err := e.dispatcher.OnResponseResetViewReceived(m); err != nil {
		return
	}
	if e.dispatcher.GetResetViewReqCount() >= e.minAcceptVoteCount() {
		height, anchor, ok := e.headAnchor()
		if !ok {
			// Degraded anchor, and the only place in this tree where the local
			// clock is allowed to be one. See headAnchor: it is tolerable here
			// because this path runs at startup recovery and the first imported
			// block re-anchors every producer through
			// OnBlockImported(header.Time), but it is still a fork risk for the
			// instant before that block arrives, so it is logged rather than
			// silently taken.
			anchor = uint64(e.dispatcher.GetNowTime().Unix())
			log.Warn("[OnResponseResetViewReceived] no head header, anchoring the view "+
				"to the local clock until the next block import", "anchor", anchor)
		}
		e.dispatcher.ResetConsensus(height, anchor)
		log.Info("[OnResponseResetViewReceived] consensus reset", "height", height, "anchor", anchor)
		if e.StartMine != nil {
			e.StartMine()
		}
	}
}

// OnBlockReceived inserts a block that arrived with a confirm.
//
// The sleep is upstream's: a block whose timestamp is in the future would be
// rejected if inserted now, so the producer waits out the remainder of the slot.
// The delay is clamped because the timestamp is peer controlled and this handler
// runs on the single DPoS message goroutine -- an unclamped sleep would let any
// peer stop consensus for every other producer.
func (e *Engine) OnBlockReceived(id dpeer.PID, b *dmsg.BlockMsg, confirmed bool) {
	if !confirmed {
		return
	}
	if e.dispatcher == nil || e.inserter == nil || e.blocks == nil {
		return
	}
	block := &types.Block{}
	if err := block.DecodeRLP(rlp.NewStream(bytes.NewBuffer(b.GetData()), 0)); err != nil {
		log.Error("OnBlockReceived: decode failed", "peer", id.String(), "err", err)
		return
	}

	delay := time.Unix(int64(block.Time()), 0).Sub(e.dispatcher.GetNowTime())
	if delay < 0 {
		delay = 0
	}
	if delay > maxOnBlockSleepDelay {
		log.Warn("OnBlockReceived: block time too far ahead, clamping wait",
			"delay", delay, "height", block.NumberU64())
		delay = maxOnBlockSleepDelay
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	head := e.blocks.CurrentHeader()
	if head == nil || head.Number == nil {
		return
	}
	headNumber := head.Number.Uint64()
	if block.NumberU64() > 0 && e.blocks.GetBlock(block.ParentHash(), block.NumberU64()-1) == nil {
		log.Warn("OnBlockReceived: unknown ancestor", "height", block.NumberU64(),
			"peers", e.activePeerCount())
		if !e.dispatcher.GetConsensusView().HasProducerMajorityCount(e.activePeerCount()) {
			go e.announceAddr()
		}
		return
	}
	if block.NumberU64() > headNumber+1 {
		log.Warn("OnBlockReceived: gap too large", "block", block.NumberU64(), "head", headNumber)
		return
	}
	if e.blocks.GetBlockByHash(block.Hash()) != nil {
		log.Debug("OnBlockReceived: already known", "hash", block.Hash().String())
		return
	}

	if _, err := e.inserter.InsertChain(types.Blocks{block}); err != nil {
		log.Warn("OnBlockReceived: insert failed", "height", block.NumberU64(), "err", err)
		return
	}

	// Tell the rest of the set, but not the peer we got it from.
	if t := e.getTransport(); t != nil {
		if parent, err := elacom.Uint256FromBytes(block.ParentHash().Bytes()); err == nil {
			t.BroadcastRawExcept(emsg.NewInventory(*parent), id)
		}
		if hash, err := elacom.Uint256FromBytes(block.Hash().Bytes()); err == nil {
			t.BroadcastRawExcept(emsg.NewInventory(*hash), id)
		}
	}
}

// OnConfirmReceived handles a confirm for a height this node may not have.
//
// If we are the on-duty producer we are mid-round and a confirm for the height we
// are sealing means someone else won: abandon the round and resynchronise. That is
// the "kill -9 a producer and the set recovers within two views" path in M4.
func (e *Engine) OnConfirmReceived(pid dpeer.PID, c *payload.Confirm, height uint64) {
	if e.dispatcher == nil {
		return
	}
	log.Debug("OnConfirmReceived", "confirm", c.Proposal.Hash().String(), "height", height)

	if e.IsOnduty() {
		atomic.StoreInt32(&e.isSealOver, 1)
		go e.OnRecover()
		return
	}
	if height > e.headNumber()+1 {
		log.Debug("OnConfirmReceived: future confirm", "height", height)
		return
	}
	if height <= e.dispatcher.GetFinishedHeight() {
		log.Debug("OnConfirmReceived: height already finished", "height", height)
		return
	}
	if _, ok := e.blockPool.GetConfirmByHeight(height); ok {
		log.Debug("OnConfirmReceived: already have a confirm", "height", height)
		return
	}
	if _, ok := e.blockPool.GetBlock(c.Proposal.BlockHash); !ok {
		log.Debug("OnConfirmReceived: block unknown, requesting it")
		e.OnInv(pid, c.Proposal.BlockHash)
		return
	}
	if _, ok := e.blockPool.GetConfirm(c.Proposal.BlockHash); !ok {
		for i := range c.Votes {
			vote := c.Votes[i]
			if _, _, err := e.dispatcher.ProcessVote(&vote); err != nil {
				log.Debug("OnConfirmReceived: ProcessVote failed", "err", err)
			}
		}
	}
}

// OnSmallCroTxReceived and OnFailedWithdrawTxReceived are the ELA gateway traffic.
//
// NOT PORTED, deliberately (docs/design/pbft-port-design.md D10): the small-cross-
// transaction and failed-withdrawal machines live in pgp-chain's smallcrosstx and
// withdrawfailedtx packages, which are chainbridge business. The messages decode
// (dpos/msg/smallcrotxmsg.go, failedwithdrawmsg.go) and are then dropped here.
func (e *Engine) OnSmallCroTxReceived(id dpeer.PID, m *dmsg.SmallCroTx) {
	log.Debug("[OnSmallCroTxReceived] dropped: chainbridge is not ported", "peer", id.String())
}

func (e *Engine) OnFailedWithdrawTxReceived(id dpeer.PID, m *dmsg.FailedWithdrawTx) {
	log.Debug("[OnFailedWithdrawTxReceived] dropped: chainbridge is not ported", "peer", id.String())
}

// OnProducersMsg applies a producer-set change pushed by a peer.
//
// Only the part this tree can act on is kept: the next producer set goes into the
// consensus view and the SPV height is pushed to the listener. pgp-chain also calls
// changeNextTurnProduces and sets blocksigner.SelfIsProducer -- neither has a
// counterpart here, and both belong to the M5 producer-rotation work (#27).
func (e *Engine) OnProducersMsg(m *dmsg.ProducersMsg) {
	if e.dispatcher == nil || e.account == nil {
		return
	}
	if !e.IsProducer() {
		return
	}
	if m.SpvHeight > e.dispatcher.GetConsensusView().GetSpvHeight() {
		log.Warn("OnProducersMsg: message is ahead of our SPV height",
			"msg", m.SpvHeight, "local", e.dispatcher.GetConsensusView().GetSpvHeight())
		return
	}
	if e.dispatcher.GetConsensusView().IsSameProducers(m.Producers) {
		log.Debug("OnProducersMsg: set unchanged")
		return
	}

	next := make([]dpeer.PID, len(m.Producers))
	for i, p := range m.Producers {
		copy(next[i][:], p)
	}
	e.dispatcher.GetConsensusView().UpdateNextProducers(next, len(next))
	log.Info("OnProducersMsg: next producer set recorded", "count", len(next),
		"changeHeight", m.ChangeHeight)
	// TODO(M5, #27): changeNextTurnProduces + ResetConsensusForEpochTransition.
	// Deliberately absent: the rotation state machine it drives
	// (needChangeNextTurnProducers) does not exist in this tree, so running the
	// reset without it would desynchronise the epoch boundary instead of moving
	// it. M4's rig never changes the set.
}

// ---- helpers ---------------------------------------------------------------------

// headAnchor returns the height and timestamp of the current head, which is what
// a recovery-path reset anchors its view grid to.
//
// The timestamp rather than the local clock is the anchor because the grid has to
// be a value every node computes identically (see Dispatcher.ResetConsensus). A
// header is such a value: every producer holding the same head reads the same
// Time out of it, regardless of when that producer's own handler happens to run.
//
// ok is false without a chain reader, or before the first head header is
// readable. The caller falls back to the local clock then; headAnchor does not do
// it, because the fallback needs a comment wherever it is taken and a silent one
// here would be indistinguishable from the bug this signature exists to fix.
func (e *Engine) headAnchor() (height uint64, anchor uint64, ok bool) {
	chain := e.chainReader()
	if chain == nil {
		return 0, 0, false
	}
	h := chain.CurrentHeader()
	if h == nil || h.Number == nil {
		return 0, 0, false
	}
	return h.Number.Uint64(), h.Time, true
}

func (e *Engine) headNumber() uint64 {
	height, _, ok := e.headAnchor()
	if !ok {
		return 0
	}
	return height
}

// SetAnnounceAddr installs the re-announce hook.
//
// The network layer owns address announcement (dpos.NetworkConfig.AnnounceAddr);
// the engine only decides WHEN to ask for one, which is when a peer sent a block
// whose parent we do not have and the set is too small to have the block
// replicated. Without the hook the engine still works, it just cannot pull itself
// out of a partition.
func (e *Engine) SetAnnounceAddr(fn func()) { e.announce = fn }

// announceAddr asks the network to re-announce our DPoS address. It is nil unless
// the assembly layer installed it, so a transport-less node is a no-op.
func (e *Engine) announceAddr() {
	if e.announce != nil {
		e.announce()
	}
}

// hasConfirmedCompetitor reports whether a different block at this height is
// already final.
func (e *Engine) hasConfirmedCompetitor(proposal *payload.DPOSProposal) bool {
	block, ok := e.blockPool.GetBlock(proposal.BlockHash)
	if !ok || block == nil {
		return false
	}
	confirmed, has := e.blockPool.GetConfirmByHeight(block.GetHeight())
	if !has {
		return false
	}
	return !confirmed.Proposal.BlockHash.IsEqual(proposal.BlockHash)
}

func (e *Engine) minAcceptVoteCount() int {
	if e.dispatcher == nil {
		return 0
	}
	return e.dispatcher.GetConsensusView().GetMajorityCount()
}

func (e *Engine) isRecoverStarted() bool {
	e.recoverMu.Lock()
	defer e.recoverMu.Unlock()
	return e.recoverStarted
}

// beginRecover claims the single recovery exchange slot.
func (e *Engine) beginRecover() bool {
	e.recoverMu.Lock()
	defer e.recoverMu.Unlock()
	if e.recoverStarted {
		return false
	}
	e.recoverStarted = true
	return true
}

func (e *Engine) finishRecover() bool {
	e.recoverMu.Lock()
	defer e.recoverMu.Unlock()
	if !e.recoverStarted {
		return false
	}
	e.recoverStarted = false
	return true
}

// recoverAbnormalState asks the set for its consensus status and adopts the
// majority answer.
//
// Ported from pgp-chain/consensus/pbft/network.go:833-872. The difference is that
// this version cannot fall back to `p.Recover()` when there are too few peers --
// recovery IS this path -- so it just gives up, which leaves the node waiting for
// the next OnRecover from the view loop.
func (e *Engine) recoverAbnormalState() bool {
	minCount := e.minAcceptVoteCount()
	if e.dispatcher == nil || minCount == 0 {
		return false
	}
	if len(e.dispatcher.GetConsensusView().GetProducers()) == 0 {
		return false
	}
	if peers := e.activePeerCount(); peers < minCount {
		log.Error("[recoverAbnormalState] not enough active peers", "need", minCount, "have", peers)
		return false
	}
	if !e.beginRecover() {
		return false
	}

	// Until the exchange completes, production must not race a peer that already
	// has a confirm for the height (see the recovered field's comment).
	e.recovered = 0
	e.sendRequestConsensus()

	start := time.Now()
	go func() {
		for {
			e.statusMapMu.Lock()
			count := countStatuses(e.statusMap)
			e.statusMapMu.Unlock()
			if count >= minCount || time.Since(start) > recoverWaitTimeout {
				e.finishRecoverAndApply()
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return true
}

// sendRequestConsensus broadcasts where we think we are.
func (e *Engine) sendRequestConsensus() {
	t := e.getTransport()
	if t == nil || e.dispatcher == nil {
		return
	}
	req := &dmsg.RequestConsensus{Height: e.dispatcher.GetFinishedHeight()}
	go t.BroadcastRaw(req)
}

// finishRecoverAndApply adopts the highest view offset any peer reported, using
// the median view start time so the set lands on a common clock.
func (e *Engine) finishRecoverAndApply() {
	if !e.finishRecover() {
		return
	}
	e.statusMapMu.Lock()
	count := countStatuses(e.statusMap)
	var status *dmsg.ConsensusStatus
	var medianTime int64
	if count > 0 {
		status, medianTime = pickRecoverStatus(e.statusMap, e.dispatcher.GetNowTime().UnixNano())
	}
	e.statusMap = make(map[uint32]map[string]*dmsg.ConsensusStatus)
	e.statusMapMu.Unlock()
	if count > 0 && status != nil {
		e.dispatcher.RecoverAbnormal(status, medianTime)
	}
	e.reqMu.Lock()
	e.notHandledProposal = make(map[string]struct{})
	e.reqMu.Unlock()
	e.MarkRecovered()
	if e.StartMine != nil {
		e.StartMine()
	}
}

func countStatuses(m map[uint32]map[string]*dmsg.ConsensusStatus) int {
	n := 0
	for _, bucket := range m {
		n += len(bucket)
	}
	return n
}

// recoverClockSlack is how far ahead of our own clock a peer's view start time
// may be and still be read as a clock rather than as a placeholder.
const recoverClockSlack = time.Minute

// usableViewStart reports whether t is a real view clock.
//
// A producer that has never opened a view answers a recovery request anyway, and
// what it answers with is the zero view state: an unset view start time, which
// serialises to the zero time.Time and decodes on our side as the unix epoch (or
// as whatever a sub-second median of the two lands on). That value is not a
// clock coordinate, and treating it as one is not a rounding error: recovery
// derives viewOffset from (now - start) / signTolerance, so "1970" against 2026
// is an offset in the hundreds of millions, and each node computes it against its
// own `now`. Two nodes then differ by one offset and disagree about whose turn it
// is, which is a set that cannot form a quorum.
//
// So the rule is: a start time at or before the epoch, or in the future, is not a
// view clock and is excluded from the median. Upstream has no such check and does
// not need one on a chain that has been producing: its members always carry a
// real view start time. A freshly generated devnet is the case it never sees.
func usableViewStart(t time.Time, anchor int64) bool {
	if t.Unix() <= 0 {
		return false
	}
	return t.UnixNano() <= anchor+recoverClockSlack.Nanoseconds()
}

// pickRecoverStatus returns the status from the highest view offset present and
// the median start time across that bucket, in unix nanoseconds.
//
// When no peer in that bucket reports a usable view clock, the anchor is returned
// instead of a median. That is the right answer rather than a fallback: if nobody
// in the set has an open view, there is no view clock to align to, so the set has
// to start one, and starting it now with a zero offset is what makes every member
// compute the same duty. Dispatcher.RecoverAbnormal turns an anchor-based
// medianTime into exactly that -- offset 0, view start anchored at now.
func pickRecoverStatus(m map[uint32]map[string]*dmsg.ConsensusStatus, anchor int64) (*dmsg.ConsensusStatus, int64) {
	var maxOffset uint32
	var seen bool
	for offset := range m {
		if !seen || offset > maxOffset {
			maxOffset, seen = offset, true
		}
	}
	if !seen {
		return nil, 0
	}
	var status *dmsg.ConsensusStatus
	startTimes := make([]int64, 0, len(m[maxOffset]))
	for _, v := range m[maxOffset] {
		if status == nil {
			status = v
		}
		if !usableViewStart(v.ViewStartTime, anchor) {
			continue
		}
		startTimes = append(startTimes, v.ViewStartTime.UnixNano())
	}
	if len(startTimes) == 0 {
		// The bucket has answers but none of them is a view clock, so there is
		// nothing to align to. Anchor at now: see this function's comment.
		return status, anchor
	}
	sort.Slice(startTimes, func(i, j int) bool { return startTimes[i] < startTimes[j] })
	return status, medianInt64(startTimes)
}

func medianInt64(nums []int64) int64 {
	l := len(nums)
	if l == 0 {
		return 0
	}
	if l%2 == 0 {
		return (nums[l/2] + nums[l/2-1]) / 2
	}
	return nums[l/2]
}

// The engine is the DPoS listener. Checked at compile time because a drift in
// either direction is otherwise silent: dpos.Network takes the interface, so a
// method that no longer matches would simply stop being called, and the node
// would keep sealing local blocks while ignoring every peer.
var _ dpos.NetworkEventListener = (*Engine)(nil)
