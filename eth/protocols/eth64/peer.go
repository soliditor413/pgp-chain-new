// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
)

const (
	// maxKnownBlocks is the number of block hashes a peer remembers.
	maxKnownBlocks = 1024

	// responseTimeout bounds how long a single request waits for its response.
	// A late response is not correlated with the next request by the protocol, so
	// the wait must be short and requests must never be pipelined.
	responseTimeout = 20 * time.Second
)

var (
	errPeerClosed     = errors.New("eth64: peer closed")
	errRespTimeout    = errors.New("eth64: response timeout")
	errAlreadyDone    = errors.New("eth64: peer already closed")
	errNoMatchingPeer = errors.New("eth64: no matching peer")
)

// Peer is an eth/64 peer. It is deliberately minimal: requests only send, the
// responses are read by the single dispatch goroutine and handed over through the
// channels below, because eth/64 has no request ids to correlate concurrent
// answers. Callers must therefore issue one request at a time.
type Peer struct {
	id      string
	version uint
	rw      p2p.MsgReadWriter

	head common.Hash
	td   *big.Int

	lock        sync.RWMutex
	knownBlocks *lru.Cache[common.Hash, struct{}]
	lagging     bool

	recvHeaders chan []*types.Header
	recvBodies  chan []*types.Body

	closed    chan struct{}
	closeOnce sync.Once
}

// NewPeer creates an eth/64 peer wrapper around a devp2p connection.
func NewPeer(version uint, id string, rw p2p.MsgReadWriter) *Peer {
	return &Peer{
		id:          id,
		version:     version,
		rw:          rw,
		td:          new(big.Int),
		knownBlocks: lru.NewCache[common.Hash, struct{}](maxKnownBlocks),
		recvHeaders: make(chan []*types.Header, 1),
		recvBodies:  make(chan []*types.Body, 1),
		closed:      make(chan struct{}),
	}
}

// ID returns the peer identifier.
func (p *Peer) ID() string { return p.id }

// Version returns the negotiated eth protocol version.
func (p *Peer) Version() uint { return p.version }

// Log returns a logger scoped to this peer.
func (p *Peer) Log() log.Logger { return log.New("peer", p.id, "eth", p.version) }

// Head returns the last known head hash and total difficulty of the peer.
func (p *Peer) Head() (common.Hash, *big.Int) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.head, new(big.Int).Set(p.td)
}

// SetHead updates the last known head of the peer.
func (p *Peer) SetHead(hash common.Hash, td *big.Int) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.head = hash
	if td != nil {
		p.td = new(big.Int).Set(td)
	}
}

// TD returns the peer's total difficulty.
func (p *Peer) TD() *big.Int {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return new(big.Int).Set(p.td)
}

// MarkBlock remembers an announced block hash.
func (p *Peer) MarkBlock(hash common.Hash) { p.knownBlocks.Add(hash, struct{}{}) }

// KnownBlock reports whether the peer already announced the given block.
func (p *Peer) KnownBlock(hash common.Hash) bool {
	_, ok := p.knownBlocks.Get(hash)
	return ok
}

// MarkLagging flags the peer as unhelpful for the current sync round.
func (p *Peer) MarkLagging() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.lagging = true
}

// Lagging reports whether the peer was flagged during this sync round.
func (p *Peer) Lagging() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.lagging
}

// ResetLagging clears the lagging flag at the start of a sync round.
func (p *Peer) ResetLagging() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.lagging = false
}

// RequestHeadersByNumber asks for headers starting at the given block number.
// It only sends; the caller must wait with WaitHeaders before issuing anything
// else on this peer.
func (p *Peer) RequestHeadersByNumber(origin uint64, amount, skip int, reverse bool) error {
	return p.send(GetBlockHeadersMsg, &GetBlockHeadersRequest{
		Origin:  HashOrNumber{Number: origin},
		Amount:  uint64(amount),
		Skip:    uint64(skip),
		Reverse: reverse,
	})
}

// RequestHeadersByHash asks for headers starting at the given block hash.
func (p *Peer) RequestHeadersByHash(origin common.Hash, amount, skip int, reverse bool) error {
	return p.send(GetBlockHeadersMsg, &GetBlockHeadersRequest{
		Origin:  HashOrNumber{Hash: origin},
		Amount:  uint64(amount),
		Skip:    uint64(skip),
		Reverse: reverse,
	})
}

// RequestBodies asks for the bodies of the given blocks.
func (p *Peer) RequestBodies(hashes []common.Hash) error {
	return p.send(GetBlockBodiesMsg, hashes)
}

// SendBlockHeaders answers a GetBlockHeaders request.
func (p *Peer) SendBlockHeaders(headers []*types.Header) error {
	return p.send(BlockHeadersMsg, headers)
}

// SendBlockBodies answers a GetBlockBodies request.
func (p *Peer) SendBlockBodies(bodies []*types.Body) error {
	return p.send(BlockBodiesMsg, bodies)
}

// SendStatus sends the handshake status packet.
func (p *Peer) SendStatus(status *StatusPacket) error { return p.send(StatusMsg, status) }

// SendNewBlockHashes announces blocks.
func (p *Peer) SendNewBlockHashes(hashes []struct {
	Hash   common.Hash
	Number uint64
}) error {
	return p.send(NewBlockHashesMsg, NewBlockHashesPacket(hashes))
}

func (p *Peer) send(code uint64, data interface{}) error {
	p.lock.RLock()
	closed := p.isClosedLocked()
	p.lock.RUnlock()
	if closed {
		return errPeerClosed
	}
	return p2p.Send(p.rw, code, data)
}

func (p *Peer) isClosedLocked() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// WaitHeaders blocks until a BlockHeaders response arrives, the peer dies or the
// timeout expires. Only the sync loop calls this.
func (p *Peer) WaitHeaders(timeout time.Duration) ([]*types.Header, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case headers := <-p.recvHeaders:
		return headers, nil
	case <-p.closed:
		return nil, errPeerClosed
	case <-timer.C:
		return nil, errRespTimeout
	}
}

// WaitBodies blocks until a BlockBodies response arrives.
func (p *Peer) WaitBodies(timeout time.Duration) ([]*types.Body, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case bodies := <-p.recvBodies:
		return bodies, nil
	case <-p.closed:
		return nil, errPeerClosed
	case <-timer.C:
		return nil, errRespTimeout
	}
}

// deliverHeaders hands a response to the waiting sync loop, dropping it if the
// slot is full (a stale response nobody asked for anymore).
func (p *Peer) deliverHeaders(headers []*types.Header) {
	select {
	case p.recvHeaders <- headers:
	default:
		p.Log().Debug("Dropping unsolicited block headers", "count", len(headers))
	}
}

// deliverBodies is deliverHeaders for bodies.
func (p *Peer) deliverBodies(bodies []*types.Body) {
	select {
	case p.recvBodies <- bodies:
	default:
		p.Log().Debug("Dropping unsolicited block bodies", "count", len(bodies))
	}
}

// Close terminates the peer, unblocking every waiter.
func (p *Peer) Close() {
	p.closeOnce.Do(func() { close(p.closed) })
}

// peerSet is a thread safe collection of eth/64 peers. Peers are selected for
// sync by total difficulty.
type peerSet struct {
	peers  map[string]*Peer
	lock   sync.RWMutex
	closed bool
}

// newPeerSet creates an empty peer set.
func newPeerSet() *peerSet {
	return &peerSet{peers: make(map[string]*Peer)}
}

// Register adds a peer to the set.
func (ps *peerSet) Register(p *Peer) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errAlreadyDone
	}
	if _, ok := ps.peers[p.ID()]; ok {
		return errors.New("eth64: peer already registered")
	}
	ps.peers[p.ID()] = p
	peerCountMeter.Mark(1)
	return nil
}

// Unregister removes a peer from the set and closes it.
func (ps *peerSet) Unregister(id string) error {
	ps.lock.Lock()
	p, ok := ps.peers[id]
	if ok {
		delete(ps.peers, id)
	}
	ps.lock.Unlock()

	if !ok {
		return errors.New("eth64: peer not registered")
	}
	p.Close()
	peerCountMeter.Mark(-1)
	return nil
}

// Peer retrieves a registered peer by id.
func (ps *peerSet) Peer(id string) *Peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()
	return ps.peers[id]
}

// Len returns the number of registered peers.
func (ps *peerSet) Len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()
	return len(ps.peers)
}

// Peers returns all registered peers.
func (ps *peerSet) Peers() []*Peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	peers := make([]*Peer, 0, len(ps.peers))
	for _, peer := range ps.peers {
		peers = append(peers, peer)
	}
	return peers
}

// BestPeer returns the peer with the highest total difficulty, or nil.
func (ps *peerSet) BestPeer() *Peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var best *Peer
	var bestTD *big.Int
	for _, peer := range ps.peers {
		if best == nil {
			best, bestTD = peer, peer.TD()
			continue
		}
		if td := peer.TD(); td.Cmp(bestTD) > 0 {
			best, bestTD = peer, td
		}
	}
	return best
}

// Close unregisters and closes every peer.
func (ps *peerSet) Close() {
	ps.lock.Lock()
	peers := make([]*Peer, 0, len(ps.peers))
	for _, peer := range ps.peers {
		peers = append(peers, peer)
	}
	ps.peers = make(map[string]*Peer)
	ps.closed = true
	ps.lock.Unlock()

	for _, peer := range peers {
		peer.Close()
	}
}
