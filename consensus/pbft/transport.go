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
	"sync/atomic"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	"github.com/ethereum/go-ethereum/core/types"
)

// Transport is the outbound half of the DPoS channel.
//
// It is an interface with an optional implementation, not a hard dependency,
// because the round state machine must be testable and must run on a single node
// before any peer networking exists. A nil Transport means "sealed locally, told
// nobody", which is exactly the M1/M2 shape of the port: one producer, its own
// vote, its own confirm, and a block that the verifier accepts.
//
// The inbound half is deliberately NOT in this interface. Peers call
// ProcessProposal / ProcessVote / AcceptConfirm on the engine directly, because
// those are ordinary methods with real preconditions, and hiding them behind an
// interface would only make the network layer look decoupled when it is not.
//
// See docs/design/pbft-port-design.md M4.
type Transport interface {
	// BroadcastBlock announces a sealed block over the DPoS block channel. It is
	// separate from devp2p block propagation: a legacy peer accepts a block from
	// the PBFT channel with `confirmed=true` and imports it without a confirm of
	// its own.
	BroadcastBlock(block *types.Block)

	// BroadcastProposal announces a proposal (seal hash + view offset) so peers
	// can vote for it.
	BroadcastProposal(proposal *payload.DPOSProposal)

	// BroadcastVote sends an accept or reject vote.
	BroadcastVote(vote *emsg.Vote)

	// BroadcastConfirm sends a confirm carrying the 2/3 signature set. Peers
	// that missed the vote round learn the block is final from this alone.
	//
	// height is an argument rather than a field because the ConfirmMsg on the
	// wire carries it separately: payload.Confirm has no height of its own (it
	// commits to a block by seal hash), and a peer needs the height before it
	// does any signature work, to reject a future confirm early. Deriving it
	// inside the transport would mean resolving the seal hash against a block
	// pool the transport does not own.
	BroadcastConfirm(confirm *payload.Confirm, height uint64)

	// BroadcastResetView asks the producer set to abandon the current view. It
	// is only sent once the view offset has run past maxViewOffset, i.e. when
	// view rotation alone is no longer making progress.
	BroadcastResetView(msg *emsg.ResetView)

	// BroadcastRaw sends an arbitrary DPoS message to the producer set.
	//
	// The five methods above cover the consensus round, where naming the message
	// buys a doc comment and a type check. The sync path does not fit that shape:
	// it carries Inventory, GetBlock, ResponseBlocks, RequestConsensus and
	// ResponseConsensus, none of which is a round message, and one method per
	// message would double the interface for no extra safety. pbft.engine's
	// OnInv / OnGetBlock / OnBlockReceived are the callers.
	BroadcastRaw(msg elap2p.Message)

	// BroadcastRawExcept is BroadcastRaw minus one peer, used by the block relay
	// path (a block just received is not sent back to its sender).
	BroadcastRawExcept(msg elap2p.Message, except peer.PID)

	// SendToPeer sends one DPoS message to one producer.
	//
	// Required because the sync path is request/response: OnInv answers one peer
	// with one GetBlock, OnGetBlock answers with one BlockMsg. Broadcasting those
	// would make every producer answer the same request.
	SendToPeer(pid peer.PID, msg elap2p.Message) error
}

// SetTransport installs the network implementation.
//
// It must be called before the miner starts: the engine treats a missing
// transport as "single node" rather than as an error, so installing one late
// would silently drop the proposals of the round that is already in flight. The
// lock is here for the race detector, not for a supported late call.
func (e *Engine) SetTransport(transport Transport) {
	e.transportMu.Lock()
	e.transport = transport
	e.transportMu.Unlock()
}

func (e *Engine) getTransport() Transport {
	e.transportMu.RLock()
	defer e.transportMu.RUnlock()
	return e.transport
}

// HasTransport reports whether the engine is wired to the DPoS network.
//
// Production without a transport is legitimate (a single-producer test chain,
// or the M1 acceptance run); a *multi*-producer node without one is a
// misconfiguration that stalls the chain, so the caller uses this to decide
// whether to warn at startup.
func (e *Engine) HasTransport() bool { return e.getTransport() != nil }

// MarkRecovered releases the recovery gate.
//
// Until it is called a producing node refuses to seal. The gate exists because
// a restarted producer does not know which height the rest of the set has
// already confirmed: sealing at a height that a peer has a confirm for is a
// double proposal, and the honest answer is to wait for the consensus-status
// exchange to say where the set is.
//
// A node WITH a transport is released by the network layer once the status
// exchange completes (docs/design/pbft-port-design.md M4/R9). A node WITHOUT one
// has nobody to synchronise with and nobody to race, but nothing in New opens the
// gate for it either: New only marks the recovered flag when production is
// disabled entirely (config.Enable=false). So a single-producer node -- the
// --pbft.enable-without-networking shape of M2 -- MUST be released by the
// assembly layer, and eth/backend.go does exactly that in StartMining when
// HasTransport is false. Forget that call and the node answers
// ErrWaitRecoverStatus on every Prepare and never seals, with no error anywhere.
func (e *Engine) MarkRecovered() { atomic.StoreInt32(&e.recovered, 1) }

// IsRecovered reports whether the recovery gate is open. It is exported for the
// pbft RPC namespace's status call, which is what an operator looks at when a
// producer is up but not sealing.
func (e *Engine) IsRecovered() bool { return atomic.LoadInt32(&e.recovered) == 1 }
