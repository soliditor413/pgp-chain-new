// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos/elamsg"
	"github.com/ethereum/go-ethereum/log"
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

// DPOSHooks is what eth/64 must hand to the layers above it: the two peer
// lifecycle edges and the ELAMSG envelope.
//
// It exists so that this package does NOT import dpos. The ELAMSG type lives in
// dpos/elamsg precisely to keep the Elastos DPoS p2p stack out of the protocol
// layer (see that package's doc comment), but the DPoS EVENT TYPES
// (dpos.ETNewPeer and friends) live in dpos, and notifying with a look-alike
// number is not an option: Elastos.ELA/events has an ETDirectPeersChanged of its
// own on a different numbering space, so a hardcoded literal here would deliver
// to nobody and the only symptom would be producers that never peer.
//
// So the translation lives one layer up, in eth/handler_eth64.go, which may
// import dpos freely.
//
// A nil hooks value is legal and means "this node does not run DPoS": a BSC or
// Rialto chain registers eth/64 only for legacy history and has no producer set.
type DPOSHooks interface {
	// PeerUp is called once per connection after the handshake and peer
	// registration succeed, i.e. when the peer is usable.
	PeerUp(peer *Peer)

	// PeerDown is called when a registered peer goes away, on every exit path.
	//
	// It is called from the connection's own goroutine while that goroutine is
	// unwinding, so an implementation that blocks delays the teardown. The eth
	// layer dispatches it to a new goroutine for that reason.
	PeerDown(peer *Peer)

	// ElaMsg is called with a decoded ELAMSG payload.
	//
	// The peer is passed because a DAddr reply goes back to the peer that asked;
	// the ELAMSG envelope carries no sender.
	ElaMsg(peer *Peer, msg *elamsg.ElaMsg)
}

// handler owns the eth/64 peer set and the single sync loop.
type handler struct {
	backend Backend
	network uint64
	peers   *peerSet
	hooks   DPOSHooks

	closeCh   chan struct{}
	closeOnce sync.Once
	startOnce sync.Once
	syncer    *chainSyncer
}

func newHandler(backend Backend, network uint64, hooks DPOSHooks) *handler {
	return &handler{
		backend: backend,
		network: network,
		peers:   newPeerSet(),
		hooks:   hooks,
		closeCh: make(chan struct{}),
	}
}

// MakeProtocols returns the eth/64 protocol entry and the elamsg entry that
// carries the DPoS address channel.
//
// Two entries, one handler. They cannot share a capability name: p2p keys
// matched protocols by name, so "eth"/64 is overwritten by eth/68 or eth/70 on
// any connection that speaks them (see ElaMsgProtocolName in protocol.go).
// Sharing the handler is what keeps the second entry useful -- a peer registered
// by either Run is visible to BroadcastElaMsg, PeerByID, PeerCount and the
// syncer, because they all read the same peer set.
func MakeProtocols(backend Backend, network uint64, hooks DPOSHooks) []p2p.Protocol {
	h := newHandler(backend, network, hooks)
	h.start()
	// Publish this handler so eth_syncing can ask the syncer how far behind it
	// is. The eth package already imports this one (eth/handler_eth64.go), so
	// this pointer is the one-way channel that avoids an import cycle.
	currentHandler.Store(h)

	// One closure for both entries: which capability served the connection is an
	// implementation detail of the transport, not something to report per entry.
	peerInfo := func(id enode.ID) interface{} {
		if peer := h.peers.Peer(id.String()); peer != nil {
			head, td := peer.Head()
			return struct {
				Version uint
				Head    string
				TD      string
			}{peer.Version(), head.Hex(), td.String()}
		}
		return nil
	}

	return []p2p.Protocol{{
		Name:    ProtocolName,
		Version: ETH64,
		// Length must stay 17, see protocolLengths.
		Length:   protocolLengths[ETH64],
		Run:      func(peer *p2p.Peer, rw p2p.MsgReadWriter) error { return h.run(peer, rw) },
		PeerInfo: peerInfo,
	}, {
		Name:    ElaMsgProtocolName,
		Version: ElaMsgProtocolVersion,
		// Same window size as eth/64: the message code table is the pgp-chain
		// one, and matchProtocols accumulates Length onto the next protocol's
		// offset, so any other value would shift snap/bsc codes.
		Length:   protocolLengths[ETH64],
		Run:      func(peer *p2p.Peer, rw p2p.MsgReadWriter) error { return h.runElaMsgOnly(peer, rw) },
		PeerInfo: peerInfo,
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
			// PeerDown before Unregister, and outside the peer-set lock, so the
			// DPoS subscriber sees the drop while the session is still in the
			// set it resolves peers in. dpos keys its per-peer request cache by
			// peer id and removes that entry here.
			if h.hooks != nil {
				h.hooks.PeerDown(peer)
			}
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
	if h.hooks != nil {
		h.hooks.PeerUp(peer)
	}
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

		case ELAMSG:
			// 0x08 is the DPoS address-routing channel on a pgp-chain connection.
			// It carries peer.PID to IP:port mappings -- nothing else -- and the
			// independent DPoS TCP server (:20338) is what actually exchanges
			// consensus messages. See docs/design/m4-network-layer-breakdown.md.
			//
			// An undecodable payload is dropped rather than returned as an error,
			// which is a deliberate deviation from pgp-chain (eth/handler.go:750
			// returns ErrDecode). Not killing the connection is the property
			// TestDispatchKeepsTheConnectionOnABadELAMSG pins, and it costs
			// nothing: the routing layer simply never learns that address.
			hooks := h.hooks
			if hooks == nil {
				msg.Discard()
				break
			}
			elaMsg := new(elamsg.ElaMsg)
			if err := msg.Decode(&elaMsg); err != nil {
				peer.Log().Debug("Discarding undecodable ELAMSG", "err", err)
				break
			}
			hooks.ElaMsg(peer, elaMsg)

		default:
			// Never return an error here: one dpos broadcast must not kill the
			// connection.
			discardedMeter.Mark(1)
			peer.Log().Trace("Discarding unsupported eth64 message", "code", msg.Code, "size", msg.Size)
			if err := msg.Discard(); err != nil {
				return err
			}
		}
	}
}

// runElaMsgOnly serves a connection on the elamsg capability: no handshake, no
// block traffic, just the DPoS address channel.
//
// It differs from run in four deliberate ways:
//
//   - no handshake. This capability has no Status exchange to perform; both ends
//     are this client by construction, so the peer is registered immediately.
//   - no h.syncer.trigger(). eth/70 owns syncing on the connections that speak
//     it; triggering from here would put two downloaders on one head.
//   - only ELAMSG is dispatched. Everything else is discarded, never returned as
//     an error, so a stray frame cannot kill the address channel.
//   - it registers into the same peer set as run, which is what makes the peer
//     visible to BroadcastElaMsg and PeerByID.
//
// Register can only fail with "already registered" if eth/64 also accepted this
// connection. That cannot happen while this client offers eth/68 and eth/70 --
// matchProtocols keeps one version per name, so on a connection that negotiates
// eth/70 the eth/64 entry never runs. If it ever does (a peer that speaks eth/64
// but not eth/70), eth/64's own dispatch already handles 0x08, so losing this
// entry costs nothing; returning the error just stops the duplicate reader.
func (h *handler) runElaMsgOnly(p2pPeer *p2p.Peer, rw p2p.MsgReadWriter) error {
	// Version is the elamsg capability's, not ETH64: it is what PeerInfo reports
	// for this entry, and reporting 64 there would claim an eth/64 session that
	// this connection does not have.
	peer := NewPeer(ElaMsgProtocolVersion, p2pPeer.ID().String(), rw)
	registered := false
	defer func() {
		peer.Close()
		if registered {
			// Same ordering as run: PeerDown before Unregister, outside the
			// peer-set lock, so the DPoS subscriber sees the drop while the
			// session is still in the set it resolves peers in.
			if h.hooks != nil {
				h.hooks.PeerDown(peer)
			}
			if err := h.peers.Unregister(peer.ID()); err != nil {
				peer.Log().Debug("Failed to unregister DPoS peer", "err", err)
			}
		}
	}()

	if err := h.peers.Register(peer); err != nil {
		peer.Log().Debug("Failed to register DPoS peer", "err", err)
		return err
	}
	registered = true
	if h.hooks != nil {
		h.hooks.PeerUp(peer)
	}

	return h.dispatchElaMsgOnly(peer)
}

// dispatchElaMsgOnly is the single reader of an elamsg connection.
func (h *handler) dispatchElaMsgOnly(peer *Peer) error {
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
		case ELAMSG:
			// Same policy as dispatch: an undecodable payload is dropped, not
			// returned as an error. The routing layer only loses that one
			// address, while tearing the connection down would lose the channel.
			hooks := h.hooks
			if hooks == nil {
				msg.Discard()
				continue
			}
			elaMsg := new(elamsg.ElaMsg)
			if err := msg.Decode(&elaMsg); err != nil {
				peer.Log().Debug("Discarding undecodable ELAMSG", "err", err)
				continue
			}
			hooks.ElaMsg(peer, elaMsg)

		default:
			// Never return an error for an unknown code: this capability exists
			// to stay alive next to block-sync traffic, and a version skew must
			// not turn into a peer-churn loop.
			discardedMeter.Mark(1)
			peer.Log().Trace("Discarding unsupported elamsg message", "code", msg.Code, "size", msg.Size)
			if err := msg.Discard(); err != nil {
				return err
			}
		}
	}
}

// currentHandler is the handler of the one eth/64 protocol set this process runs,
// published by MakeProtocols so Progress can find it.
//
// A package-level pointer rather than a field on the eth backend because the two
// packages are wired in one direction only: eth imports eth64
// (eth/handler_eth64.go), and reaching back the other way would be a cycle. There
// is at most one such handler per process - MakeProtocols is called once, from
// Ethereum.Protocols - so "the current one" is exactly the right model.
var currentHandler atomic.Pointer[handler]

// BroadcastElaMsg relays one DPoS address envelope to every connected eth/64
// peer. It is the outbound half of the DAddr channel: dpos.Routes asks for it
// through its RelayAddr hook when it has an Inv to gossip.
//
// Telling everyone is correct here, unlike for consensus traffic. An address
// record is public by construction -- it is the IP:port a producer wants the
// world to dial -- and the point of the Inv is to reach peers this node is not
// (yet) peered with on the DPoS side, which is exactly the set a producer-only
// unicast would miss.
//
// No-op before MakeProtocols has run, which is the shape of a node whose chain
// does not carry legacy history.
func BroadcastElaMsg(msg *elamsg.ElaMsg) {
	h := currentHandler.Load()
	if h == nil || msg == nil {
		return
	}
	for _, peer := range h.peers.Peers() {
		peer.SendELAMessage(msg)
	}
}

// PeerByID returns the established eth/64 peer with the given id, or nil when
// there is none -- either this process runs no eth/64 protocol set at all, or
// the session has not finished its handshake yet.
//
// It is the read side of the bridge from the p2p layer to the protocol layer.
// The p2p server is what knows a connection exists, but only an eth/64 peer can
// carry an ELAMSG, so the DPoS adapter in the eth package (p2p_dpos_peer.go)
// turns an enode.ID into the session that can send. The lookup goes through
// currentHandler because the handler type stays unexported; eth already imports
// this package, so there is no cycle.
func PeerByID(id string) *Peer {
	h := currentHandler.Load()
	if h == nil {
		return nil
	}
	return h.peers.Peer(id)
}

// PeerCount is the number of established eth/64 peers.
//
// It is exported for the DPoS node's peer counter: dpos.Routes only gossips to
// peers in its cache, and an ELAMSG can only leave this protocol set, so this is
// also a reasonable "is the devp2p side alive" probe for a status endpoint.
func PeerCount() int {
	h := currentHandler.Load()
	if h == nil {
		return 0
	}
	return h.peers.Len()
}

// SyncProgress is how far behind the eth/64 peer set the chain is.
type SyncProgress struct {
	// Current is our chain head.
	Current uint64
	// Highest is the head height the peer announced, or Current when it is not
	// known. It is never lower than Current: a stale target must not make the
	// node look like it is syncing backwards.
	Highest uint64
	// Start is the head height at which the current catch-up began, or 0 when
	// the chain is not catching up.
	Start uint64
}

// Progress reports the eth/64 syncer's view of the sync.
//
// It exists because eth_syncing is blind to this package. The RPC handler reads
// ethereum.SyncProgress from the built-in downloader
// (eth/api_backend.go:441-453), and that downloader only ever runs for the eth/68
// and eth/70 protocols it registers peers for. The pgp network speaks eth/64, so
// the downloader's HighestBlock stays 0 while this syncer imports millions of
// blocks, SyncProgress.Done (interfaces.go:148-153) compares 0 against the real
// head, and eth_syncing answers false throughout a full replay.
//
// ok is false when this process runs no eth/64 protocol set at all - a BSC,
// Chapel or Rialto node - and the caller must then report nothing, because it has
// no better answer than the downloader's.
func Progress() (SyncProgress, bool) {
	h := currentHandler.Load()
	if h == nil || h.syncer == nil {
		log.Debug("eth/64 progress: no syncer", "handler", h != nil)
		return SyncProgress{}, false
	}
	p := h.syncer.progress()
	log.Debug("eth/64 progress", "current", p.Current, "highest", p.Highest, "start", p.Start)
	return p, true
}

// progress takes a consistent-enough snapshot of the syncer's bookkeeping.
//
// Current is read from the chain rather than remembered, so it advances during a
// single long catch-up even though nothing in this struct is updated per batch.
// Highest is floored at Current so a target left over from a peer that turned out
// to be behind - or from a reorg - cannot report a gap that does not exist.
func (s *chainSyncer) progress() SyncProgress {
	current := s.localHeight()
	highest := s.target.Load()
	if highest < current {
		highest = current
	}
	start := s.start.Load()
	if start > current {
		start = 0
	}
	return SyncProgress{Current: current, Highest: highest, Start: start}
}

// syncLoopInterval is how often the syncer retries when idle.
const syncLoopInterval = 10 * time.Second
