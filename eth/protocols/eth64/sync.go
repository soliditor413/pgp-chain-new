// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	// maxHeaderFetch is the number of headers pulled per round trip. pgp-chain's
	// own downloader uses the same value.
	maxHeaderFetch = 192

	// maxBodyFetch is how many bodies are requested per round trip. It matches
	// pgp-chain's downloader.MaxBlockFetch (pgp-chain/eth/downloader/downloader.go:42),
	// which is the second cap on a BlockBodies answer: the server stops at
	// min(2 MiB, 128 bodies) (pgp-chain/eth/handler.go:549). Asking for exactly
	// that ceiling keeps the answer a single round trip per batch, and the 2 MiB
	// cut is handled by the prefix logic in fetchBlocks.
	maxBodyFetch = 128

	// maxSyncRounds bounds one sync attempt so a hostile peer cannot make the
	// syncer loop forever.
	maxSyncRounds = 4096

	// refusedRetryCooldown is how long a refusal parks the syncer before it
	// tries the same block again.
	//
	// The latch exists so a block the client genuinely cannot execute does not
	// re-fetch and re-fail every syncLoopInterval (10s, i.e. 360 log lines an
	// hour). It must not be permanent, because the most common reason a legacy
	// block is refused is that its ELA dependency has not caught up yet: block
	// 30115 of pgp mainnet needs recharge data from ELA height 2,076,538, and a
	// node syncing the ELA main chain from 0 spends hours getting there. A
	// permanent latch turns "wait for the dependency" into "restart the node by
	// hand", which is exactly the failure this cooldown removes. Five minutes
	// is short next to an ELA sync and long enough to keep the log readable.
	refusedRetryCooldown = 5 * time.Minute
)

// chainSyncer pulls the canonical chain from one eth/64 peer at a time.
//
// eth/64 has no request ids, so there is exactly one outstanding request at any
// moment: request, wait for the matching response, then issue the next one.
type chainSyncer struct {
	h         *handler
	triggerCh chan struct{}

	// blocked latches once a block was refused by the chain. A refusal that
	// makes no progress is a hard boundary (unsupported rule, unsupported
	// precompile, state root mismatch), so retrying every 10 seconds would only
	// spam the log with the same failure. RetryAt carries when it may be
	// retried anyway; see refusedRetryCooldown. Reset clears both.
	blocked atomic.Bool
	retryAt atomic.Int64 // unix nanoseconds; 0 means "no scheduled retry"

	// target is the block number of the head the remote peer announced, i.e.
	// the tip we are downloading towards. 0 means "never learned it".
	//
	// It exists only so eth_syncing can report a truthful highestBlock. The
	// built-in downloader cannot supply one: the pgp peers speak eth/64, which
	// that downloader never sees, so its own target stays 0 and SyncProgress.Done
	// answers true while blocks are still being imported. See Progress.
	target atomic.Uint64

	// start is the local head height at which the current catch-up began, so
	// eth_syncing can report a startingBlock and a caller can compute a real
	// percentage. 0 means "not catching up" (either caught up or never started),
	// which is also the only case where target is not ahead of the chain.
	start atomic.Uint64
}

// newChainSyncer creates a syncer for the given handler.
func newChainSyncer(h *handler) *chainSyncer {
	return &chainSyncer{h: h, triggerCh: make(chan struct{}, 1)}
}

// trigger asks the syncer to run a round as soon as possible.
func (s *chainSyncer) trigger() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

// loop runs sync rounds until the handler is closed.
func (s *chainSyncer) loop() {
	ticker := time.NewTicker(syncLoopInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.h.closeCh:
			return
		case <-s.triggerCh:
		case <-ticker.C:
		}
		peer := s.h.peers.BestPeer()
		if peer == nil {
			continue
		}
		if !s.due(time.Now()) {
			continue
		}
		if s.blocked.Load() {
			// The cooldown expired. The most likely reason the block was
			// refused was a dependency that has since caught up (see
			// refusedRetryCooldown), so clear the latch and try once more.
			peer.Log().Info("eth/64 download retrying after the refusal cooldown",
				"cooldown", refusedRetryCooldown)
			s.Reset()
		}
		s.syncToHead(peer)
	}
}

// syncToHead downloads and imports everything the peer has that we do not.
func (s *chainSyncer) syncToHead(peer *Peer) {
	chain := s.h.backend.Chain()
	localHead := chain.CurrentBlock()
	if localHead == nil {
		return
	}
	peer.ResetLagging()

	peerHead, _ := peer.Head()
	if peerHead == localHead.Hash() {
		s.catchUpFinished(localHead.Number.Uint64())
		return
	}
	// Learn how far away the tip actually is before pulling anything. One header
	// by hash, once per sync attempt - not per batch - which is what makes an
	// honest eth_syncing affordable here: eth/64's Status carries a head hash and
	// a total difficulty but no height, so this request is the only way to get the
	// number.
	if height, ok := s.probeHeadHeight(peer, peerHead); ok {
		s.target.Store(height)
		if height > localHead.Number.Uint64() {
			// Keep the first height we were behind at, so the reported
			// startingBlock stays the start of this catch-up instead of drifting
			// forward with every attempt.
			s.start.CompareAndSwap(0, localHead.Number.Uint64())
		}
	}
	ancestor, err := s.findAncestor(peer, localHead.Number.Uint64())
	if err != nil {
		peer.Log().Debug("Failed to find a common ancestor", "err", err)
		peer.MarkLagging()
		return
	}
	peer.Log().Info("eth/64 sync starting", "ancestor", ancestor, "localHead", localHead.Number, "peerHead", peerHead)

	origin := ancestor + 1
	for round := 0; round < maxSyncRounds; round++ {
		select {
		case <-s.h.closeCh:
			return
		default:
		}
		if peer.Lagging() {
			return
		}
		headers, err := s.fetchHeaders(peer, origin)
		if err != nil {
			peer.Log().Debug("Header fetch failed", "origin", origin, "err", err)
			peer.MarkLagging()
			return
		}
		if len(headers) == 0 {
			s.catchUpFinished(s.localHeight())
			return // caught up
		}
		// The batch must continue our canonical chain, otherwise re-anchor.
		parent := chain.GetHeaderByNumber(origin - 1)
		if parent == nil || headers[0].ParentHash != parent.Hash() {
			peer.Log().Debug("Header batch does not continue our chain", "origin", origin)
			peer.MarkLagging()
			return
		}
		for i := 1; i < len(headers); i++ {
			if headers[i].Number.Uint64() != headers[i-1].Number.Uint64()+1 ||
				headers[i].ParentHash != headers[i-1].Hash() {
				peer.Log().Debug("Header batch is not contiguous", "index", i)
				peer.MarkLagging()
				return
			}
		}

		// A body response may be cut short at the peer's 2 MiB ceiling. Import the
		// served prefix and drain the rest of the batch here rather than going
		// back to the outer loop: returning would discard 143 fetched headers and
		// burn a round for every 49 blocks, which is what made the round budget
		// the real limit on sync progress.
		batch := headers
		var lastImported *types.Header
		aborted := false
		for len(batch) > 0 {
			blocks, complete, err := s.fetchBlocks(peer, batch)
			if err != nil {
				peer.Log().Debug("Body fetch failed", "count", len(batch), "err", err)
				peer.MarkLagging()
				return
			}
			if len(blocks) == 0 {
				peer.Log().Debug("Peer served no body at all", "requested", len(batch))
				peer.MarkLagging()
				return
			}
			if !complete {
				peer.Log().Debug("Body response truncated, importing the served prefix",
					"requested", len(batch), "served", len(blocks), "next", batch[len(blocks)].Number)
			}

			if !canExecuteBatch(chain.Config(), blocks) {
				// The legacy execution rules are not aligned, so the state root is
				// bound to mismatch. Stop here instead of letting InsertChain fail:
				// core records every block it fails to import in the on-disk bad
				// block list (core/blockchain.go reportBadBlock ->
				// rawdb.WriteBadBlock), and that list only keeps the ten most recent
				// entries, which would destroy the evidence of where it started.
				s.stopAtBoundary(peer, origin, blocks[0].Header(), errLegacyExecutionNotAligned)
				return
			}

			inserted, err := chain.InsertChain(blocks)
			if err != nil {
				if inserted == 0 {
					// Hard boundary: the chain refused the very first block of the
					// batch and nothing was imported. Retrying the same block every
					// round would only repeat the failure, so latch it.
					s.stopAtBoundary(peer, origin, blocks[0].Header(), err)
					return
				}
				// Blocks before the failure are in the chain, continue from there,
				// but re-anchor from the outer loop: nothing after the refused
				// block can be imported either.
				peer.Log().Debug("Block import failed", "imported", inserted, "err", err)
				importedMeter.Mark(int64(inserted))
				origin += uint64(inserted)
				lastImported = blocks[inserted-1].Header()
				peer.SetHead(lastImported.Hash(), peer.TD())
				aborted = true
				break
			}
			// InsertChain can also import nothing - or only a prefix - without
			// reporting an error. core returns (nil, 0, nil) when the chain is
			// already stopping on entry (core/blockchain.go:2282) and
			// (nil, it.index, nil) when it stops in the middle of a batch
			// (core/blockchain.go:2431 breaks the loop while err is still nil).
			//
			// Falling through to the accounting below would count the whole batch
			// as imported: origin would advance past blocks that were never written
			// and the log would report progress that did not happen, so the sync
			// either re-requests a batch it already "moved past" or stalls with no
			// error anywhere. Nothing refused a block here, so this must not latch.
			if inserted < len(blocks) {
				peer.Log().Debug("eth/64 sync stopped: the chain stopped inserting without reporting an error",
					"origin", origin, "requested", len(blocks), "inserted", inserted)
				if inserted > 0 {
					importedMeter.Mark(int64(inserted))
					lastImported = blocks[inserted-1].Header()
					peer.SetHead(lastImported.Hash(), peer.TD())
				}
				return
			}
			importedMeter.Mark(int64(len(blocks)))
			origin += uint64(len(blocks))
			lastImported = blocks[len(blocks)-1].Header()
			peer.SetHead(lastImported.Hash(), peer.TD())
			batch = batch[len(blocks):]
			if complete {
				break
			}
		}
		if aborted {
			continue
		}
		if len(headers) < maxHeaderFetch {
			peer.Log().Info("eth/64 sync caught up with peer", "head", lastImported.Number, "hash", lastImported.Hash())
			s.catchUpFinished(lastImported.Number.Uint64())
			return
		}
	}
	peer.Log().Warn("eth/64 sync round limit reached", "rounds", maxSyncRounds)
}

// catchUpFinished records that there is nothing left to download at head.
//
// Setting target to the local height (rather than leaving the peer's announced
// tip in place) is what makes eth_syncing turn itself off: Progress reports
// highest == current, the caller's Done() comparison sees no gap, and the node
// stops claiming to be syncing without anyone having to reset a flag.
func (s *chainSyncer) catchUpFinished(head uint64) {
	s.target.Store(head)
	s.start.Store(0)
}

// localHeight is our current chain head, or 0 if the chain cannot be read.
func (s *chainSyncer) localHeight() uint64 {
	if head := s.h.backend.Chain().CurrentBlock(); head != nil {
		return head.Number.Uint64()
	}
	return 0
}

// probeHeadHeight asks the peer for the header it announced as its head and
// returns that header's block number.
//
// WHY A REQUEST AND NOT A FIELD. eth/64's Status packet carries
// {ProtocolVersion, NetworkID, TD, Head, Genesis, ForkID} - the block number was
// dropped from the handshake back at eth/63 in favour of the total difficulty.
// The peer set therefore knows a head hash and a TD, and there is no arithmetic
// on TD that yields a trustworthy height (difficulty varies across the chain), so
// the number has to be asked for. Requesting the announced hash is the cheapest
// way: the peer either has that header or it does not, and the answer is one
// header.
//
// The reply must match the hash we asked for. servePeer in the tests answers any
// GetBlockHeaders by treating Origin.Number as the start, so a by-hash request
// comes back carrying genesis; without this check the syncer would conclude the
// peer is at height 0 and report a negative gap.
//
// A failure is not fatal: the caller keeps whatever target it had, and reporting
// falls back to what it can prove. It is also not retried within the attempt -
// syncToHead probes once per call, and one call walks up to maxSyncRounds
// batches, so a peer that does not answer this costs one round trip per attempt.
func (s *chainSyncer) probeHeadHeight(peer *Peer, head common.Hash) (uint64, bool) {
	headerReqMeter.Mark(1)
	if err := peer.RequestHeadersByHash(head, 1, 0, false); err != nil {
		return 0, false
	}
	headers, err := peer.WaitHeaders(responseTimeout)
	if err != nil || len(headers) == 0 {
		return 0, false
	}
	if headers[0].Hash() != head {
		return 0, false
	}
	return headers[0].Number.Uint64(), true
}

// errLegacyExecutionNotAligned is reported when consensus/pgp signals that the
// legacy execution rules are not aligned, so no block could be imported.
var errLegacyExecutionNotAligned = errors.New("eth64: legacy execution rules are not aligned")

// canExecuteBatch reports whether the batch may be handed to InsertChain.
//
// It is a property of the client, not of the batch: when
// consensus/pgp.LegacyExecutionReady reports that a legacy rule is not aligned,
// every legacy block would fail with a state root mismatch. Blocks at or after
// the legacy fork are executed by the Parlia engine and are unaffected, hence
// the per-height check.
func canExecuteBatch(cfg *params.ChainConfig, blocks []*types.Block) bool {
	if cfg == nil || cfg.Legacy == nil || pgp.LegacyExecutionReady() {
		return true
	}
	for _, block := range blocks {
		if cfg.IsPGPLegacy(block.Number()) {
			return false
		}
	}
	return true
}

// stopAtBoundary reports that the chain refused a block without importing
// anything, and latches the syncer so the same failure is not repeated every
// round.
//
// One case must NOT latch, because it is not a verdict on the block: the node is
// shutting down. InsertChain reports "blockchain is stopped" whenever a batch
// happens to be in flight when Close runs, and latching on that parks the syncer
// for the rest of the process life. The check is made on the error
// (core.IsChainStopped) and deliberately NOT on the handler's close channel: the
// channel says the handler was closed, which is a different (and, in production,
// never-observed) condition, while the error says a block was actually refused
// for a reason that is purely local and transient.
//
// It is logged at DEBUG for the same reason it does not latch: an operator
// watching a restart must not see a chain boundary that never happened, and
// boundary-stops must stay a real signal.
func (s *chainSyncer) stopAtBoundary(peer *Peer, origin uint64, refused *types.Header, err error) {
	if core.IsChainStopped(err) {
		peer.Log().Debug("eth/64 sync stopped: the chain is shutting down, not refusing the block",
			"origin", origin, "block", refused.Number, "err", err)
		return
	}
	boundaryStopMeter.Mark(1)
	peer.Log().Warn("eth/64 download stopped: the chain refused this block and nothing was imported",
		"origin", origin, "block", refused.Number, "hash", refused.Hash(), "err", err,
		"retryIn", refusedRetryCooldown)
	s.latch(time.Now())
}

// latch parks the syncer and records when it may try again. Split out of
// stopAtBoundary so the cooldown can be tested without a live peer.
func (s *chainSyncer) latch(now time.Time) {
	s.blocked.Store(true)
	s.retryAt.Store(now.Add(refusedRetryCooldown).UnixNano())
}

// Reset clears the hard-boundary latch and schedules a new sync round.
//
// It is the intended entry point for anything that changes the client's ability
// to execute legacy blocks at runtime (a new execution rule, a config change, a
// restart of the ELA dependency): without a reset the syncer stays parked on the
// first block it could not import until refusedRetryCooldown expires.
func (s *chainSyncer) Reset() {
	s.retryAt.Store(0)
	s.blocked.Store(false)
	s.trigger()
}

// due reports whether the syncer may run a round at the given time.
//
// Not latched always means yes. Latched means yes only once the cooldown set by
// stopAtBoundary has expired — and a latch with no retry time (only reachable if
// something set blocked without going through stopAtBoundary) never expires, so
// the safety property "a refusal cannot be retried every syncLoopInterval"
// survives any future call site.
func (s *chainSyncer) due(now time.Time) bool {
	if !s.blocked.Load() {
		return true
	}
	until := s.retryAt.Load()
	return until != 0 && now.UnixNano() >= until
}

// fetchHeaders requests amount headers starting at origin and waits for them.
func (s *chainSyncer) fetchHeaders(peer *Peer, origin uint64) ([]*types.Header, error) {
	headerReqMeter.Mark(1)
	if err := peer.RequestHeadersByNumber(origin, maxHeaderFetch, 0, false); err != nil {
		return nil, err
	}
	return peer.WaitHeaders(responseTimeout)
}

// fetchBlocks resolves the bodies of the given headers and assembles full blocks.
//
// The response is NOT guaranteed to have one entry per requested hash: pgp-chain's
// answerGetBlockBodies stops at a 2 MiB soft limit and silently skips bodies it
// does not have (pgp-chain/eth/handler.go:549-562). Bodies therefore have to be
// matched back to headers by content: the transaction root and the uncle hash of
// the body identify its header unambiguously.
//
// A truncated answer is not an error. The returned blocks are the contiguous
// prefix the peer did serve and complete reports whether the whole request was
// satisfied. Callers import the prefix and re-request the remainder: treating
// truncation as a failure parks the sync on the same chunk forever, because the
// next round starts from the same ancestor and gets cut at the same byte.
func (s *chainSyncer) fetchBlocks(peer *Peer, headers []*types.Header) ([]*types.Block, bool, error) {
	blocks := make([]*types.Block, 0, len(headers))

	for start := 0; start < len(headers); start += maxBodyFetch {
		end := min(start+maxBodyFetch, len(headers))
		chunk := headers[start:end]

		hashes := make([]common.Hash, len(chunk))
		for i, header := range chunk {
			hashes[i] = header.Hash()
		}
		bodyReqMeter.Mark(1)
		if err := peer.RequestBodies(hashes); err != nil {
			return nil, false, err
		}
		bodies, err := peer.WaitBodies(responseTimeout)
		if err != nil {
			return nil, false, err
		}
		// Index the (possibly partial) answer by its content.
		index := make(map[bodyKey]*types.Body, len(bodies))
		for _, body := range bodies {
			if body == nil {
				continue
			}
			key := bodyKey{
				txRoot:    types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil)),
				uncleHash: types.CalcUncleHash(body.Uncles),
			}
			if _, exists := index[key]; !exists {
				index[key] = body
			}
		}
		for _, header := range chunk {
			body, ok := index[bodyKey{txRoot: header.TxHash, uncleHash: header.UncleHash}]
			if !ok {
				// Everything before this header is a valid chain segment: the
				// loop walks the chunk in order, so a gap here cannot be
				// followed by a served body without breaking contiguity.
				bodyTruncatedMeter.Mark(1)
				peer.Log().Debug("Body response truncated at peer soft limit",
					"block", header.Number, "served", len(blocks), "requested", len(headers))
				return blocks, false, nil
			}
			blocks = append(blocks, types.NewBlockWithHeader(header).WithBody(types.Body{
				Transactions: body.Transactions,
				Uncles:       body.Uncles,
			}))
		}
	}
	return blocks, true, nil
}

// bodyKey identifies a block body by the two commitments its header carries.
type bodyKey struct {
	txRoot    common.Hash
	uncleHash common.Hash
}

// findAncestor binary searches for the highest block number where our header and
// the peer's header are identical.
func (s *chainSyncer) findAncestor(peer *Peer, localHeight uint64) (uint64, error) {
	chain := s.h.backend.Chain()

	matches := func(number uint64) (bool, error) {
		local := chain.GetHeaderByNumber(number)
		if local == nil {
			return false, nil
		}
		headerReqMeter.Mark(1)
		if err := peer.RequestHeadersByNumber(number, 1, 0, false); err != nil {
			return false, err
		}
		headers, err := peer.WaitHeaders(responseTimeout)
		if err != nil {
			return false, err
		}
		if len(headers) == 0 {
			return false, nil
		}
		return headers[0].Hash() == local.Hash(), nil
	}

	// Fast path: the peer is often just barely ahead of us.
	if ok, err := matches(localHeight); err == nil && ok {
		ancestorHitMeter.Mark(1)
		return localHeight, nil
	} else if err != nil {
		return 0, err
	}
	ancestorMissMeter.Mark(1)

	// Genesis is always shared, otherwise the handshake would have rejected the
	// peer on the genesis hash.
	low, high := uint64(0), localHeight
	for low+1 < high {
		mid := low + (high-low)/2
		ok, err := matches(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			low = mid
		} else {
			high = mid
		}
	}
	return low, nil
}
