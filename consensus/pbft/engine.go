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

// Package pbft is the producing half of the pgp-chain PBFT/DPoS consensus.
//
// It is the counterpart of consensus/pgp, which validates the same blocks but
// refuses to make them. The two are not alternatives: pbft EMBEDS pgp.Engine, so
// there is exactly one implementation of header verification, of the PBFT
// confirm bytes in header.Extra, and of SealHash. Only block production,
// signing, and the round state machine are new here.
//
// That embedding is the whole safety argument for the port. A sealed block has
// to be acceptable to consensus/pgp's verifier -- including on nodes that never
// upgrade their producer key -- so the sealer must go through the same code, not
// a reimplementation of it. See docs/design/pbft-port-design.md D1/R1.
//
// What is inherited from consensus/pgp and must not be redefined here:
//
//	Author, VerifyHeader, VerifyHeaders, VerifyUncles, VerifyRequests, SealHash
//
// What is overridden:
//
//	Prepare, Finalize, CalcDifficulty, Delay, NextInTurnValidator, Close
//
// What is added:
//
//	Seal (the round), the DPoS dispatcher, the SPV view, the irreversible height
package pbft

import (
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/dpos"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// legacyDposStartHeight is the height the producer rotation counts from.
//
// It must stay equal to pgp-chain's params.PbftConfig.DPoSV2StartHeight /
// ChainConfig.PBFTBlock, which is 1 on mainnet, because the duty index is
//
//	(index) = (height + 1 - workingHeight) % len(producers)
//
// and that determines whose turn it is at every height. A different value does
// not produce an invalid block that verification would catch; it produces a
// block proposed by the wrong arbiter, which the others reject as not-on-duty.
// See docs/design/pbft-port-design.md R3.
const legacyDposStartHeight = 1

// Engine is the PBFT/DPoS consensus engine of the pgp-chain legacy era.
//
// Zero value is not usable; call New.
type Engine struct {
	*pgp.Engine

	cfg        Config
	legacy     *params.LegacyConfig
	period     uint64
	timeSource dtime.MedianTimeSource

	account    daccount.Account
	producers  [][]byte
	dispatcher *dpos.Dispatcher
	blockPool  *dpos.BlockPool

	chainMu sync.RWMutex
	chain   consensus.ChainHeaderReader

	// isCurrent is the "chain caught up" probe dpos.Routes gates announcing on.
	// Guarded by chainMu because it is installed by the assembly layer after the
	// downloader exists, which is after the engine is already serving.
	isCurrent func() bool

	// confirmCh and unConfirmCh carry the round result from the dispatcher's
	// callback (called from the vote path, possibly a network goroutine) to
	// Seal, which blocks waiting for it.
	//
	// Immutable after New: never closed, never replaced. A late vote can still be
	// holding the reference across a round boundary -- trySendConfirm reads the
	// field and then sends -- so closing one would turn that in-flight send into
	// a panic on a closed channel, and recreating one leaves the stale pointer
	// pointing at a channel nobody drains. Draining replaces closing: see
	// (*Engine).drainRoundChannels. No lock is needed for the same reason the
	// field is immutable.
	confirmCh   chan *payload.Confirm
	unConfirmCh chan *payload.Confirm

	// isSealing is read by onConfirm to decide whether a confirm arriving now
	// has a Seal waiting for it. Atomic because onConfirm runs on whichever
	// goroutine delivered the last vote.
	isSealing int32
	// isSealOver is the same handshake in the other direction: it tells a late
	// confirm that the round it belongs to has already been resolved. Atomic for
	// the same reason as isSealing, and it must be, because the writer is Seal and
	// the reader is the vote goroutine.
	isSealOver int32

	// enableViewLoop guards the single view-change goroutine. Access under
	// viewMu; the loop itself re-reads it every tick.
	viewMu         sync.RWMutex
	enableViewLoop bool
	stopCh         chan struct{}
	stopOnce       sync.Once

	// retired and retireOnce implement the fork-time shutdown. See (*Engine).retire
	// for why the flag is atomic and the four effects run behind a Once.
	retired    atomic.Bool
	retireOnce sync.Once

	// transport is the DPoS network, nil on a node that only verifies and on
	// the single-node acceptance run. See transport.go.
	transportMu sync.RWMutex
	transport   Transport
	// network is the same object as a dpos.DPOSNetwork, kept alongside the
	// transport because ConnectPeers is not part of the Transport interface:
	// Transport carries DPoS messages, while UpdatePeers tells the TCP server
	// whom to dial. See WatchPeerSet.
	network dpos.DPOSNetwork

	// recovered is false between Start and the completion of the consensus
	// status exchange, during which production would race a peer that already
	// has a confirm for the height. It is only pre-opened when production is
	// disabled altogether (see New): a node that wants to produce without a
	// transport must be released by the assembly layer via MarkRecovered.
	recovered int32

	// resetGate rate-limits the "rotation alone is not fixing this" reset-view
	// request. See shouldRequestResetView for why an absolute offset threshold
	// cannot be the trigger any more.
	resetGate resetViewGate

	// orphanMu guards the confirm-without-block watchdog: which height it is
	// watching, and since when. See reclaimOrphanedConfirm.
	orphanMu     sync.Mutex
	orphanHeight uint64
	orphanSince  time.Time

	// StartMine is injected by the node wiring. It asks the miner to build a new
	// work item, which is how a view change hands the turn to the next producer.
	StartMine func()

	// ---- DPoS message state (see network_listener.go) ----
	//
	// These are the bookkeeping maps the inbound handlers need. They are all
	// guarded by reqMu because DPoS messages are delivered from the network
	// goroutine while the miner reads the same state.
	reqMu              sync.Mutex
	requestedBlocks    map[common.Hash]struct{}
	requestedProposals map[elacom.Uint256]struct{}
	notHandledProposal map[string]struct{}

	// statusMap collects the consensus statuses peers answer a recovery request
	// with, bucketed by view offset: recovery picks the highest offset that has
	// any answer, because that is the furthest the set has moved.
	statusMapMu sync.Mutex
	statusMap   map[uint32]map[string]*dmsg.ConsensusStatus

	// recoverStarted guards the one in-flight recovery exchange.
	recoverMu      sync.Mutex
	recoverStarted bool

	// ---- chain write seam (see network_listener.go) ----
	//
	// consensus.ChainHeaderReader is read-only on purpose (AttachChain explains
	// why), but the DPoS sync path has to read block bodies and insert a block a
	// peer sent. core imports consensus, so the engine cannot name
	// *core.BlockChain without a cycle; these two narrow interfaces are the same
	// answer as D3.
	blocks    blockReader
	inserter  blockInserter
	peerCount func() int

	// announce asks the DPoS network to re-announce our address.
	//
	// It is the engine's copy of dpos.NetworkConfig.AnnounceAddr, installed by
	// the assembly layer rather than read out of the network object, so that a
	// transport-less node has a nil here and re-announcing becomes a no-op. See
	// network_listener.go's announceAddr.
	announce func()
}

// New creates the engine.
//
// It never fails: a node whose keystore cannot be opened is still a perfectly
// good verifier, and refusing to start would take a follower offline for a
// reason that only affects block production. Production-related failures are
// reported by Prepare/Seal instead, where the miner can log them once.
func New(legacy *params.LegacyConfig, cfg *Config) *Engine {
	if legacy == nil {
		legacy = &params.LegacyConfig{}
	}
	e := &Engine{
		Engine:      pgp.New(legacy),
		legacy:      legacy,
		period:      blockPeriod,
		timeSource:  dtime.NewMedianTime(),
		confirmCh:   make(chan *payload.Confirm, 1),
		unConfirmCh: make(chan *payload.Confirm, 1),
		stopCh:      make(chan struct{}),

		requestedBlocks:    make(map[common.Hash]struct{}),
		requestedProposals: make(map[elacom.Uint256]struct{}),
		notHandledProposal: make(map[string]struct{}),
		statusMap:          make(map[uint32]map[string]*dmsg.ConsensusStatus),
	}
	if cfg != nil {
		e.cfg = *cfg
	}
	// A node that does not produce has no consensus state to recover: there is
	// nothing it could race. Only a producer waits for the status exchange.
	if !e.cfg.productionEnabled() {
		atomic.StoreInt32(&e.recovered, 1)
	}

	e.blockPool = dpos.NewBlockPool(e.verifyConfirm, e.verifyBlock, sealHashOf)

	producers, err := pgp.DecodeProducers(legacy.Producers)
	if err != nil {
		// pgp.verifyConfirm reports the same condition on the verification path
		// with the same sentinel, so this cannot be silent there; log it here so
		// an operator sees it at startup rather than on the first header.
		log.Error("PBFT: configured producer set is unusable", "err", err)
	}
	e.producers = producers
	e.dispatcher = dpos.NewDispatcher(producers, e.onConfirm, e.onUnConfirm,
		signTolerance, nil, e.timeSource, e, legacyDposStartHeight)

	if e.cfg.productionEnabled() && e.cfg.hasKeyStore() {
		account, err := dpos.GetDposAccount(e.cfg.KeyStore, []byte(e.cfg.KeyStorePassword))
		if err != nil {
			// A node whose keystore cannot be opened is still a perfectly good
			// verifier: refusing to start would take a follower offline for a
			// reason that only affects block production. Prepare and Seal report
			// the missing key where the miner can log it once.
			log.Error("PBFT: cannot open arbiter keystore, node will only verify blocks",
				"keystore", e.cfg.KeyStore, "err", err)
		} else {
			e.attachAccount(account)
		}
	} else if e.cfg.productionEnabled() {
		log.Warn("PBFT: production enabled without a keystore, node will only verify blocks")
	}

	return e
}

// attachAccount installs the signing account and rebuilds the dispatcher around
// it.
//
// Rebuilds is the operative word: NewDispatcher calls NewConsensusView, so this
// replaces the view as well as the dispatcher. The old ConsensusView is dropped,
// not reset -- viewOffset, viewStartTime and isDposOnDuty come back at their
// construction values, and any epoch-transition state the previous view carried
// is gone. That is the opposite of dpos.Dispatcher.ResetConsensusForEpochTransition,
// which keeps the view object and rewrites its fields in place. The two are not
// interchangeable in either direction, and mixing them is how a node ends up with
// the dispatcher and the chain head each holding a different view: a caller that
// fetched the dispatcher before this ran, and then resets it, is resetting a view
// nothing else reads.
//
// The two cannot be set independently: the dispatcher captures the public key at
// construction because it decides locally whether this node is on duty, and a
// dispatcher built with a nil key would answer "never on duty" no matter which
// account the sealer then used.
//
// It is unexported so that the only ways to obtain an account stay
// dpos.GetDposAccount (a keystore) and the package's own tests. A node that wants
// to load its key some other way should grow a GetDposAccount variant rather than
// a public setter, because the keystore password handling is the part worth
// keeping in one place.
func (e *Engine) attachAccount(account daccount.Account) {
	e.account = account
	e.dispatcher = dpos.NewDispatcher(e.producers, e.onConfirm, e.onUnConfirm,
		signTolerance, account.PublicKeyBytes(), e.timeSource, e, legacyDposStartHeight)
}

// AttachChain gives the engine the chain it reads parent headers from.
//
// It is separate from New because eth/ethconfig builds the engine before
// core.NewBlockChain exists, and the engine only needs the chain on the
// production path.
//
// The parameter is a ChainHeaderReader rather than a full ChainReader because
// that is the whole of what this engine reads: SpvHeightAt needs
// GetHeaderByNumber and chainConfig needs Config. A producer never needs block
// bodies or receipts -- it is handed the transactions to include -- so requiring
// the wider interface would only make the engine harder to test.
func (e *Engine) AttachChain(chain consensus.ChainHeaderReader) {
	e.chainMu.Lock()
	e.chain = chain
	e.chainMu.Unlock()
}

func (e *Engine) chainReader() consensus.ChainHeaderReader {
	e.chainMu.RLock()
	defer e.chainMu.RUnlock()
	return e.chain
}

func (e *Engine) chainConfig() *params.ChainConfig {
	if chain := e.chainReader(); chain != nil {
		return chain.Config()
	}
	return params.PGPChainConfig
}

// Author implements consensus.Engine, inherited from consensus/pgp.
// VerifyHeader, VerifyHeaders, VerifyUncles, VerifyRequests and SealHash are
// inherited as well: they are the verification half and must stay byte identical
// to what a non-producing node runs.

// NextInTurnValidator implements consensus.Engine.
//
// It returns the zero address together with a nil error, deliberately. PBFT
// identifies the in-turn producer by its 33 byte compressed P-256 public key,
// which has no lossless mapping to a 20 byte account address; inventing one
// would make miner's in-turn comparison wrong in a way that silently disables
// the "am I allowed to seal this" shortcut. The authoritative on-duty check
// happens inside Prepare, which is the only place that matters for correctness.
//
// Implementing this instead of inheriting consensus/pgp's error return also
// keeps miner on its generic path: an error here would abort work generation for
// every producer.
func (e *Engine) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	return common.Address{}, nil
}

// CalcDifficulty implements consensus.Engine.
func (e *Engine) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return new(big.Int).Set(diffInTurn)
}

// Finalize implements consensus.Engine by forwarding to the embedded pgp engine.
//
// The forwarding is not cosmetic. *pgp.Engine implements FinalizeAndAssemble and
// *pbft.Engine inherits it by embedding, so a block this node assembles reaches
// pgp.Finalize through that method, while an imported block reaches this one
// through core/state_processor.go:257, which calls Engine().Finalize for EVERY
// imported block, not only for blocks this node assembles. If this method kept a
// body of its own, the two paths would compute one consensus rule in two
// different functions, and only one of them would get changed. Do not give it a
// body again.
//
// It is a pure function of the state: recompute the root, clear the uncle hash.
//
// pgp-chain advances its irreversible height inside Finalize
// (dispatcher.FinishedProposal). That must NOT be reproduced here: a side branch
// or a block that is later replaced would move the finality watermark. The
// advance happens in (*Engine).OnBlockImported instead, driven by the node's
// chain-head subscription. See docs/design/pbft-port-design.md D2/R8.
func (e *Engine) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB,
	txs *[]*types.Transaction, uncles []*types.Header, withdrawals []*types.Withdrawal,
	receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) error {
	return e.Engine.Finalize(chain, header, state, txs, uncles, withdrawals, receipts, systemTxs, usedGas, tracer)
}

// Delay implements consensus.Engine.
//
// It must never return nil: miner treats a nil delay as "something is wrong",
// logs a warning and stops collecting transactions until the next block
// (miner/worker.go:1418). Returning zero means "seal now".
func (e *Engine) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	delay := time.Until(time.Unix(int64(header.Time), 0))
	if leftOver != nil {
		delay -= *leftOver
	}
	if delay < 0 {
		delay = 0
	}
	return &delay
}

// Close implements consensus.Engine.
func (e *Engine) Close() error {
	e.stopOnce.Do(func() {
		e.viewMu.Lock()
		e.enableViewLoop = false
		e.viewMu.Unlock()
		close(e.stopCh)
	})
	return e.Engine.Close()
}

// GetIrreversibleHeight returns the highest height a confirm has finalised.
//
// It is not part of consensus.Engine, and nothing in core reads it today: the
// reorg guard that would consume it is M6 work (the design calls for a
// core/legacy_finality.go, which does not exist yet). Until then this is the
// value an operator or an RPC can use to see how far finality has advanced; it
// does not protect the chain from a reorg on its own. See D3/R7.
func (e *Engine) GetIrreversibleHeight() uint64 {
	if e.dispatcher == nil {
		return 0
	}
	return e.dispatcher.GetFinishedHeight()
}

// retire takes the PBFT/DPoS machinery out of service, once, at the Parlia fork.
//
// It is called from OnBlockImported -- i.e. from the chain-head subscription --
// and deliberately NOT from eth.pbftLoop: pbftLoop is one caller among possible
// future ones (any path that reports imported blocks would re-anchor the legacy
// view grid after the fork if the gate lived there), and with the gate in the
// engine, pbftLoop can keep forwarding every event and the engine decides what
// an event means. That is also why pbftLoop itself is left unchanged.
//
// Exactly once. Chain-head events can repeat a height (a restart that replays
// the tail, a reorg), so the four effects below run behind a Once: the network's
// Stop is idempotent, but the view flag, the log line and the retired flag are
// ours, and a second pass would log "retired" twice at two different heights
// while claiming to be one event.
//
// # Stopping the view loop
//
// changeViewLoop (view.go) selects on e.stopCh and, on every viewTick (1s),
// re-reads enableViewLoop through viewLoopEnabled(). Close() closes stopCh
// under stopOnce and is still going to run at node shutdown, so retire must NOT
// close stopCh -- a second close panics. retire instead clears enableViewLoop
// under viewMu, which is the same lock the loop's per-tick check reads: the
// goroutine sees the flag on its next tick and returns within one second. The
// channel stays open for Close(). ensureViewLoop checks the retired flag under
// the same lock, so a late Prepare cannot start a replacement goroutine.
//
// # What is deliberately not done
//
// The inbound On* handlers in network_listener.go get no retired early return:
// stopping the DPoS TCP server removes the transport that delivers those
// messages, so they cannot fire, and gating a dozen handlers one by one would
// be a shotgun with twelve chances to miss one. GetIrreversibleHeight is left
// alone: its value freezes at the last legacy height, which is the correct
// answer for the RPC that reads it, and Parlia finality takes over from here.
func (e *Engine) retire(height uint64) {
	e.retireOnce.Do(func() {
		e.retired.Store(true)

		// View loop: see "Stopping the view loop" above. Under viewMu so that a
		// concurrent ensureViewLoop is fully ordered against this write.
		e.viewMu.Lock()
		e.enableViewLoop = false
		e.viewMu.Unlock()

		// Direct links down. The nil case is a verifier node, which never had a
		// network attached; Stop's own stopOnce makes a second call a no-op, so
		// a Close()-time double stop is not a concern here either.
		e.transportMu.RLock()
		network := e.network
		e.transportMu.RUnlock()
		if network != nil {
			if err := network.Stop(); err != nil {
				log.Warn("PBFT: error stopping the DPoS network at the fork", "err", err)
			}
		}

		// GetIrreversibleHeight is the frozen finality watermark: the last height
		// a legacy confirm finalised. Logging it at retire is the only chance an
		// operator has to see where legacy finality ended, because nothing
		// advances it past the fork.
		var fork uint64
		if e.legacy != nil {
			fork = e.legacy.ForkHeight
		}
		log.Info("PBFT: Parlia fork reached, PBFT machinery retired",
			"height", height, "forkHeight", fork,
			"irreversible", e.GetIrreversibleHeight())
	})
}

// Retired reports whether the engine has taken its machinery down at the
// Parlia fork.
//
// It is the observable half of retire: the assembly layer (eth.pbftLoop's
// startup reconciliation), the RPC status and the tests all need to ask "is
// this engine still in service" without poking the unexported flag. A node that
// restarted already past the fork must answer true here exactly like one that
// crossed the fork while running -- before the reconciliation line existed the
// restart case left the flag false and every downstream answer lied.
func (e *Engine) Retired() bool { return e.retired.Load() }

// OnBlockImported advances the irreversible height when an imported block is one
// this node has a confirm for, and re-anchors the view grid on every imported
// block -- every legacy block: at LegacyConfig.ForkHeight and above it retires
// the engine instead (see the gate below).
//
// It is driven by the node's chain-head subscription, NOT by Finalize: Finalize
// runs for every imported block including side branches, so advancing finality
// there would let a side chain move the watermark (see docs/design
// pbft-port-design.md D2).
//
// The confirm comparison is what makes the finality half idempotent and safe: a
// block we just sealed has already been recorded by addConfirmToBlock, so
// FinishedProposal runs once and the second call (from the event) returns early
// on the height guard.
//
// The two halves are deliberately split, and the split has a shape that is easy
// to get wrong: upstream bundles them in Finalize
// (pgp-chain/consensus/pbft/pbft.go:591-598, FinishedProposal(..., header.Time)),
// and this function unbundles them into "advance the watermark only when we hold
// the confirm" and "re-anchor the view for every imported block". Unbundling is
// correct -- that is D2, and D2's whole point is that a block this node has no
// confirm for must not move GetIrreversibleHeight, which is reported to RPC as
// the irreversible height. What was wrong was unbundling them and dropping the
// anchor with them: the port then re-anchored on the local clock instead, which
// put every node on a different grid and livelocked six producers at the same
// height with "have different view offset".
//
// So the anchor argument below is header.Time and not the local clock, and it is
// passed on the reset path too, not only on the confirm path. header.Time is the
// only value both branches share and every node computes identically; see the
// contract on Dispatcher.ResetConsensus.
func (e *Engine) OnBlockImported(header *types.Header) {
	if header == nil || header.Number == nil {
		return
	}
	height := header.Number.Uint64()
	// The Parlia fork ends the legacy era: from LegacyConfig.ForkHeight up, the
	// block belongs to Parlia and the PBFT/DPoS machinery must stop working
	// instead of re-anchoring a view grid nothing will use. This is the runtime
	// half of the two retire gates; the other is eth.startPBFTNetwork, which
	// covers a node that restarts already past the fork. The gate sits above the
	// dispatcher nil check on purpose: a verifier-shaped engine with no
	// dispatcher still has to take its (nil) network down.
	//
	// Below the fork nothing changes: legacy sync drives a chain-head event per
	// imported block, and dropping the ResetConsensus re-anchor is what put
	// nodes on different view grids -- see the comment above. e.legacy == nil
	// is the no-fork compatibility path (New fills it in, so only a
	// hand-built engine reaches it): no fork to retire at, keep the old
	// behaviour everywhere.
	if e.legacy != nil && height >= e.legacy.ForkHeight {
		e.retire(height)
		return
	}
	if e.dispatcher == nil {
		return
	}
	if confirm, ok := e.blockPool.GetConfirmByHeight(height); ok {
		if sealHash := pgp.SealHash(header); common.Hash(confirm.Proposal.BlockHash) == sealHash {
			e.dispatcher.FinishedProposal(height, elacom.Uint256(sealHash), header.Time)
			e.blockPool.CleanFinalConfirmedBlock(height)
			return
		}
	}
	e.dispatcher.ResetConsensus(height, header.Time)
	// Upstream does this in Finalize, for every imported block, one line after
	// FinishedProposal (pgp-chain/consensus/pbft/pbft.go:599). Unbundling the two
	// halves of Finalize moved FinishedProposal here and left this behind, which
	// matters because the pool's confirm maps are only ever pruned through this
	// call.
	//
	// It is a WINDOW prune, not a per-height one -- it drops entries older than
	// height-cachedCount -- so on its own it does not clear the height that stalled
	// the chain: a confirm for a height the chain never reached is not older than
	// anything, it is ahead. That case needs reclaimOrphanedConfirm below, and the
	// two are not substitutes. This one keeps the pool from growing without bound
	// on a chain that is moving; that one unsticks a chain that has stopped.
	e.blockPool.CleanFinalConfirmedBlock(height)
}

// confirmOrphanGrace is how long a confirm may be held for a height this node does
// not have the block of, before it is treated as unreachable and dropped.
//
// Two view windows. The normal case is not close to this: the proposer holds the
// block in its own pool before proposing, and every other node receives it on the
// DPoS block channel just before the proposal, so a node that has the confirm and
// not the block is already the exception. Two windows gives a slow block delivery
// room to arrive without leaving the chain stuck for anything like long enough to
// matter -- and "stuck" here is permanent, not slow, because nothing else in the
// tree ever drops that entry.
const confirmOrphanGrace = 2 * signTolerance

// reclaimOrphanedConfirm breaks the confirm-without-block deadlock.
//
// # The stall
//
// A height this node holds a confirm for but has no block of is unreachable. Two
// independent gates treat the confirm as the decision:
//
//	seal.go: "height already has a confirm, refusing to propose"      -> nobody proposes
//	network_listener.go: "already finalised with another block"       -> nobody accepts
//
// and the only thing that ever removes a height confirm is
// BlockPool.CleanFinalConfirmedBlock, which prunes a window six blocks behind the
// height being imported. A confirm for a height the chain never reached is ahead of
// that window, not behind it, so it is kept forever. Measured at 23:04 on the
// six-producer chain: every node held a confirm for h22, none had the block, the
// head was pinned at 21, and all six refused both to propose and to accept. Four
// minutes later it had not moved and it never would have.
//
// # When dropping is safe
//
// The condition is the stall itself, and it has to be: a confirm for head+1 while
// the head does not move for confirmOrphanGrace.
//
// The first version of this also required that the pool not hold the block, on the
// reasoning that a node which has the block is merely late to import it. That is
// wrong for the case that actually happened, and wrong for the proposer in
// particular: Seal puts the block in the pool before proposing, so the proposer
// always "has" it, and its import is exactly what stopped. Requiring the pool to be
// missing the block made the watchdog silent in the one state it exists for --
// measured: the test below held the confirm for 20s and nothing happened.
//
// What makes dropping safe is not the pool, it is that finality is not held here.
// Finality is FinishedProposal, driven by importing a block; a block that is not in
// the chain is not final on this node no matter what the votes said. Re-proposing
// the same block afterwards is the normal shape -- it is the pool's copy, and a
// competitor cannot outrun it while this node is on duty.
func (e *Engine) reclaimOrphanedConfirm(now time.Time) bool {
	if e.dispatcher == nil || e.blockPool == nil {
		return false
	}
	head, _, ok := e.headAnchor()
	if !ok {
		// No chain reader yet: nothing to conclude about what we are missing.
		e.forgetOrphanConfirm()
		return false
	}
	height := head + 1

	if _, has := e.blockPool.GetConfirmByHeight(height); !has {
		e.forgetOrphanConfirm()
		return false
	}

	e.orphanMu.Lock()
	if e.orphanHeight != height {
		e.orphanHeight, e.orphanSince = height, now
		e.orphanMu.Unlock()
		return false
	}
	held := now.Sub(e.orphanSince)
	e.orphanMu.Unlock()
	if held < confirmOrphanGrace {
		return false
	}

	e.blockPool.DropHeightConfirm(height)
	e.forgetOrphanConfirm()
	log.Warn("PBFT: dropping a confirm whose block never arrived",
		"height", height, "held", held.String(),
		"reason", "a height cannot stay undecided; the confirm would block every proposal forever")
	if e.StartMine != nil {
		e.StartMine()
	}
	return true
}

// forgetOrphanConfirm clears the watchdog so the next height starts its own clock.
func (e *Engine) forgetOrphanConfirm() {
	e.orphanMu.Lock()
	e.orphanHeight = 0
	e.orphanMu.Unlock()
}

// IsProductionEnabled reports whether this node may produce blocks.
func (e *Engine) IsProductionEnabled() bool { return e.cfg.productionEnabled() }

// DPOSAccount returns the producer key the engine signs with, or nil when no
// keystore could be opened.
//
// The assembly layer needs it to build dpos.Config: the DAddr records Routes
// gossips are signed with the same arbiter key the engine seals with, and the PID
// they are filed under is its public key. Opening the keystore a second time in
// eth/backend.go would duplicate the password handling that attachAccount's
// comment says should stay in one place, and would let the two copies disagree
// after a restart.
//
// It is a getter, not a setter: the only ways to obtain an account stay
// dpos.GetDposAccount and this package's tests.
func (e *Engine) DPOSAccount() daccount.Account { return e.account }

// IsProducer reports whether the configured key belongs to the producer set.
func (e *Engine) IsProducer() bool {
	if e.account == nil || e.dispatcher == nil {
		return false
	}
	return e.dispatcher.IsProducer(e.account.PublicKeyBytes())
}

// IsOnduty reports whether this node's producer is the proposer of the current
// view.
func (e *Engine) IsOnduty() bool {
	if e.dispatcher == nil {
		return false
	}
	return e.dispatcher.ProducerIsOnDuty()
}

// SpvHeightAt implements spv.PbftEngineView: the ELA height anchored in the
// block at the given pgp height, read from header.Nonce.
//
// The narrow interface exists in spv rather than here because spv must not
// import core/types (it would drag the cgo BLS toolchain into the SPV service).
// Implementing it on the engine is what finally lets spv.PbftEngine stop being
// nil.
func (e *Engine) SpvHeightAt(height uint64) uint64 {
	chain := e.chainReader()
	if chain == nil {
		return 0
	}
	header := chain.GetHeaderByNumber(height)
	if header == nil {
		return 0
	}
	return header.Nonce.Uint64()
}

// Producers implements spv.PbftEngineView: the statically configured producer
// set, i.e. the one that applies to blocks whose header.Nonce is zero.
func (e *Engine) Producers() []string { return e.legacy.Producers }

// OnViewChanged implements dpos.ViewListener: a view change just happened and
// this hook decides whether the node may now build a block.
//
// The SetReady below is not bookkeeping -- it is the only way back to "may
// prepare" without a round having finished, and without it the set deadlocks on
// the first height. Prepare marks the view Running on every producer that asks
// (it has to: the view loop that times out a stalled round only exists once a
// round is open), and Prepare refuses to start a second round while one is
// Running. A round that ends without a block -- because it was someone else's
// turn, because the proposal was rejected, because the view timed out -- leaves
// that mark behind, so the next turn would be refused by a mark nobody clears.
// FinishedProposal and ResetConsensus do clear it, but both need an imported
// block, and importing a block is what production is for. So the rotation itself
// has to clear it, which is exactly what upstream does here
// (pgp-chain/consensus/pbft/pbft.go:1182-1197).
//
// CleanProposals(true) rides along and is not decoration: every vote collected
// during the view that just ended belongs to a view offset that no longer exists
// and must not be counted in the new one. This is also the first caller in the
// tree to pass true, the branch dpos.Dispatcher.CleanProposals documents as
// ported-but-not-wired until the view-change path landed.
//
// Forced resets do NOT clear the Running mark, and that is not an oversight: force
// is used by Dispatcher.FinishedProposal and Dispatcher.ResetConsensus, both of
// which have already called SetReady and rebuilt the view from a real block.
// Clearing it a second time there would let the miner prepare a second round on
// top of a height that just became final.
//
// They do poke the miner, though, and that difference matters: the mark is about
// the round, the poke is about the work item. A forced reset fires immediately
// after a block was imported, which is the one moment this node's pending work is
// guaranteed to be built on a stale head. See the note at the call site.
func (e *Engine) OnViewChanged(isOnDuty bool, force bool) {
	log.Debug("PBFT view changed", "onDuty", isOnDuty, "force", force)
	if e.dispatcher == nil {
		return
	}
	// Replay before cleaning, which is upstream's order (pbft.go:1182-1197). A
	// proposal that was ahead of our view is now current, and it is the second vote
	// a proposer one view ahead of us is waiting for. Cleaning first would be
	// harmless -- CleanProposals(true) only drops proposals from lower offsets --
	// but the order is kept so a future reader comparing the two files sees the
	// same three steps in the same sequence.
	e.processPrecociousProposal()
	if force {
		// A forced change is fired right after a block was imported, so the work
		// item this node is holding was built on the block before it. Rebuilding it
		// here is the difference between "the next slot can use this node" and "the
		// next slot has to wait for a timer".
		//
		// Skipping it is what pinned the six-producer rig to 12s a block. The view
		// grid is signTolerance = 6s and a block may only carry the grid point its
		// view starts on, so the rebuild has to happen inside the new window. With
		// no poke at import time the only remaining trigger was
		// minerconfig.defaultRecommit = 10s, which is longer than one window: the
		// rebuild always landed on the grid point after next. Measured on this rig,
		// same binary otherwise: recommit 10s -> 12s intervals, recommit 2s -> 6s.
		//
		// The Running mark is deliberately left alone; FinishedProposal owns it.
		if e.IsCurrent() && e.StartMine != nil {
			e.StartMine()
		}
		return
	}
	e.dispatcher.CleanProposals(true)
	if !isOnDuty || !e.IsCurrent() {
		// Our turn has not come yet, or the chain is still catching up: the view
		// stays Running and the next rotation will ask again.
		return
	}
	e.dispatcher.GetConsensusView().SetReady()
	if e.StartMine != nil {
		e.StartMine()
	}
}

// sealHashOf is the dpos.BlockPool adapter: a pool keyed by anything other than
// the PBFT seal hash would not match confirm.Proposal.BlockHash.
func sealHashOf(block dpos.DBlock) (elacom.Uint256, error) {
	blk, ok := block.(*dBlock)
	if !ok {
		return elacom.Uint256{}, errors.New("pbft: pool block is not an ethereum block")
	}
	return elacom.Uint256(pgp.SealHash(blk.Header())), nil
}

// The engine must be usable as a consensus.Engine, and its view listener hook
// must match what the dispatcher calls. Both are checked at compile time because
// a signature drift in either direction is otherwise a silent no-op: a listener
// that no longer matches the interface is simply never called, and the miner
// then never learns that its turn arrived.
var (
	_ consensus.Engine  = (*Engine)(nil)
	_ dpos.ViewListener = (*Engine)(nil)
)

// Engine deliberately does NOT implement consensus.PoSA. Implementing it makes
// eth/backend.go:509-520 type-assert the engine to *parlia.Parlia and panic,
// because PoSA is BSC's Parlia interface, not a general "can produce blocks"
// marker. The producing capability is advertised through the narrower
// GetIrreversibleHeight / IsProductionEnabled surface instead. See
// docs/design/pbft-port-design.md D7.
