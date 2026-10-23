// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
)

// ChainReader is the chain surface eth/64 actually uses.
//
// It exists instead of *core.BlockChain so the syncer's decisions can be tested
// without a real blockchain: the branches that matter (a batch refused outright,
// a batch accepted only in part, a batch accepted but not inserted) are all
// decided from the return value of InsertChain, and *core.BlockChain cannot
// produce them on demand.
//
// Every method here is already on *core.BlockChain, so the eth package keeps
// passing its concrete chain. The set is a superset of forkid.Blockchain, which
// is what lets handshake.go hand it to forkid.NewFilter unchanged.
type ChainReader interface {
	Config() *params.ChainConfig
	Genesis() *types.Block
	CurrentHeader() *types.Header
	CurrentBlock() *types.Header
	GetHeaderByNumber(number uint64) *types.Header
	GetHeaderByHash(hash common.Hash) *types.Header
	GetBody(hash common.Hash) *types.Body
	GetTd(hash common.Hash, number uint64) *big.Int
	InsertChain(chain types.Blocks) (int, error)
}

// Backend is the minimal chain access eth/64 needs. It is intentionally tiny so
// that the eth package can adapt its handler without exposing its internals.
type Backend interface {
	// Chain returns the local blockchain.
	Chain() ChainReader
}

// handler owns the eth/64 peer set and the single sync loop.
type handler struct {
	backend Backend
	network uint64
	peers   *peerSet

	closeCh   chan struct{}
	closeOnce sync.Once
	startOnce sync.Once
	syncer    *chainSyncer
}

func newHandler(backend Backend, network uint64) *handler {
	return &handler{
		backend: backend,
		network: network,
		peers:   newPeerSet(),
		closeCh: make(chan struct{}),
	}
}

// MakeProtocols returns the eth/64 protocol entry.
//
// The handler (peer set + syncer) is created once and shared by every peer, so
// BestPeer sees all of them.
func MakeProtocols(backend Backend, network uint64) []p2p.Protocol {
	h := newHandler(backend, network)
	h.start()

	return []p2p.Protocol{{
		Name:    ProtocolName,
		Version: ETH64,
		// Length must stay 17, see protocolLengths.
		Length: protocolLengths[ETH64],
		Run: func(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
			return h.run(peer, rw)
		},
		PeerInfo: func(id enode.ID) interface{} {
			if peer := h.peers.Peer(id.String()); peer != nil {
				head, td := peer.Head()
				return struct {
					Version uint
					Head    string
					TD      string
				}{peer.Version(), head.Hex(), td.String()}
			}
			return nil
		},
	}}
}

// start launches the sync loop. Safe to call more than once.
func (h *handler) start() {
	h.startOnce.Do(func() {
		h.syncer = newChainSyncer(h)
		go h.syncer.loop()
	})
}

// Close stops the syncer and disconnects every peer.
func (h *handler) Close() {
	h.closeOnce.Do(func() {
		close(h.closeCh)
		h.peers.Close()
	})
}

// run handles a single eth/64 connection: handshake, then a single reader that
// dispatches everything. There is exactly one goroutine reading from rw, which is
// what makes the id-less eth/64 request/response correlation safe.
func (h *handler) run(p2pPeer *p2p.Peer, rw p2p.MsgReadWriter) error {
	peer := NewPeer(ETH64, p2pPeer.ID().String(), rw)
	registered := false
	defer func() {
		peer.Close()
		if registered {
			if err := h.peers.Unregister(peer.ID()); err != nil {
				peer.Log().Debug("Failed to unregister peer", "err", err)
			}
		}
	}()

	if err := h.handshake(peer); err != nil {
		peer.Log().Debug("eth/64 handshake failed", "err", err)
		return err
	}
	if err := h.peers.Register(peer); err != nil {
		peer.Log().Debug("Failed to register eth/64 peer", "err", err)
		return err
	}
	registered = true
	// Nudge the syncer so a fresh peer is used immediately.
	h.syncer.trigger()

	return h.dispatch(peer)
}

// dispatch is the single reader of the peer connection.
func (h *handler) dispatch(peer *Peer) error {
	for {
		msg, err := peer.rw.ReadMsg()
		if err != nil {
			return err
		}
		if msg.Size > protocolMaxMsgSize {
			msg.Discard()
			return fmt.Errorf("%w: %d > %d", errMsgTooLarge, msg.Size, protocolMaxMsgSize)
		}
		switch msg.Code {
		case StatusMsg:
			// A second status message is a protocol violation, but not worth
			// killing the connection over on a legacy network.
			msg.Discard()

		case BlockHeadersMsg:
			var headers []*types.Header
			if err := msg.Decode(&headers); err != nil {
				return fmt.Errorf("eth64: invalid block headers: %w", err)
			}
			headerRespMeter.Mark(1)
			peer.deliverHeaders(headers)

		case BlockBodiesMsg:
			var bodies []*types.Body
			if err := msg.Decode(&bodies); err != nil {
				return fmt.Errorf("eth64: invalid block bodies: %w", err)
			}
			bodyRespMeter.Mark(1)
			peer.deliverBodies(bodies)

		case GetBlockHeadersMsg:
			if err := h.handleGetBlockHeaders(peer, msg); err != nil {
				return err
			}
		case GetBlockBodiesMsg:
			if err := h.handleGetBlockBodies(peer, msg); err != nil {
				return err
			}
		case NewBlockHashesMsg:
			h.handleNewBlockHashes(peer, msg)
		case NewBlockMsg:
			h.handleNewBlock(peer, msg)

		default:
			// 0x08 is ELAMSG on pgp-chain (dpos traffic) and would be
			// NewPooledTransactionHashes on eth/68. Never return an error here:
			// one dpos broadcast must not kill the connection.
			discardedMeter.Mark(1)
			peer.Log().Trace("Discarding unsupported eth64 message", "code", msg.Code, "size", msg.Size)
			if err := msg.Discard(); err != nil {
				return err
			}
		}
	}
}

// syncLoopInterval is how often the syncer retries when idle.
const syncLoopInterval = 10 * time.Second
