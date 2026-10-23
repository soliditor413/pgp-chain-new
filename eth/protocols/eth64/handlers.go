// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p"
)

const (
	// maxHeadersServe is the maximum number of headers we serve in one answer,
	// matching pgp-chain's downloader.MaxHeaderFetch.
	maxHeadersServe = 1024

	// maxBodiesServe is the maximum number of bodies we serve in one answer.
	maxBodiesServe = 256

	// softResponseLimit caps the total payload size of one answer.
	softResponseLimit = 2 * 1024 * 1024

	// maxNewBlockHashes caps how many announcements we accept in one message.
	maxNewBlockHashes = 1024
)

// handleGetBlockHeaders answers a GetBlockHeaders request.
func (h *handler) handleGetBlockHeaders(peer *Peer, msg p2p.Msg) error {
	var req GetBlockHeadersRequest
	if err := msg.Decode(&req); err != nil {
		return errBadRequest("eth64: invalid GetBlockHeaders request", err)
	}
	chain := h.backend.Chain()

	// Resolve the origin block.
	var (
		origin *types.Header
		number uint64
		hash   common.Hash
	)
	if req.Origin.Hash != (common.Hash{}) {
		hash = req.Origin.Hash
		origin = chain.GetHeaderByHash(hash)
		if origin == nil {
			peer.Log().Debug("GetBlockHeaders origin not found", "hash", hash)
			return peer.SendBlockHeaders(nil)
		}
	} else {
		number = req.Origin.Number
		origin = chain.GetHeaderByNumber(number)
		if origin == nil {
			peer.Log().Debug("GetBlockHeaders origin not found", "number", number)
			return peer.SendBlockHeaders(nil)
		}
		hash = origin.Hash()
	}

	// Collect the headers. This mirrors pgp-chain's eth/handler.go logic: the
	// skip distance is applied between entries, and Reverse walks backwards.
	amount := min(req.Amount, maxHeadersServe)
	headers := make([]*types.Header, 0, min(amount, 64))
	bytes := 0

	for i := uint64(0); i < amount && origin != nil; i++ {
		if bytes > softResponseLimit {
			break
		}
		headers = append(headers, origin)
		bytes += int(origin.Size())

		// Advance by skip+1 blocks in the requested direction.
		step := req.Skip + 1
		if req.Reverse {
			if origin.Number.Uint64() < step {
				break
			}
			origin = chain.GetHeaderByNumber(origin.Number.Uint64() - step)
		} else {
			origin = chain.GetHeaderByNumber(origin.Number.Uint64() + step)
		}
	}
	peer.Log().Trace("Serving GetBlockHeaders", "origin", hash, "amount", amount,
		"skip", req.Skip, "reverse", req.Reverse, "served", len(headers))
	return peer.SendBlockHeaders(headers)
}

// handleGetBlockBodies answers a GetBlockBodies request.
func (h *handler) handleGetBlockBodies(peer *Peer, msg p2p.Msg) error {
	var hashes []common.Hash
	if err := msg.Decode(&hashes); err != nil {
		return errBadRequest("eth64: invalid GetBlockBodies request", err)
	}
	if len(hashes) > maxBodiesServe {
		hashes = hashes[:maxBodiesServe]
	}
	chain := h.backend.Chain()

	bodies := make([]*types.Body, 0, len(hashes))
	bytes := 0
	for _, hash := range hashes {
		if bytes > softResponseLimit {
			break
		}
		body := chain.GetBody(hash)
		if body == nil {
			continue
		}
		bodies = append(bodies, body)
		// types.Body has no Size() in this tree, so approximate the payload from
		// the transactions; it only feeds the soft response limit.
		for _, tx := range body.Transactions {
			bytes += int(tx.Size())
		}
		bytes += 512 * len(body.Uncles)
	}
	peer.Log().Trace("Serving GetBlockBodies", "requested", len(hashes), "served", len(bodies))
	return peer.SendBlockBodies(bodies)
}

// handleNewBlockHashes records announced blocks. Nothing is stored: the legacy
// history has to be replayed from genesis anyway.
func (h *handler) handleNewBlockHashes(peer *Peer, msg p2p.Msg) {
	var announces NewBlockHashesPacket
	if err := msg.Decode(&announces); err != nil {
		peer.Log().Debug("Invalid NewBlockHashes", "err", err)
		return
	}
	if len(announces) > maxNewBlockHashes {
		announces = announces[:maxNewBlockHashes]
	}
	for _, announce := range announces {
		if peer.KnownBlock(announce.Hash) {
			continue
		}
		peer.MarkBlock(announce.Hash)
		peer.Log().Trace("Peer announced a new block", "number", announce.Number, "hash", announce.Hash)
	}
}

// handleNewBlock records a full block announcement.
func (h *handler) handleNewBlock(peer *Peer, msg p2p.Msg) {
	var announce NewBlockPacket
	if err := msg.Decode(&announce); err != nil {
		peer.Log().Debug("Invalid NewBlock", "err", err)
		return
	}
	if announce.Block == nil {
		return
	}
	peer.MarkBlock(announce.Block.Hash())

	// The announced header is unverified and unverifiable here (no seal check), so
	// it must never move our head. Only the peer bookkeeping is updated.
	if td := announce.TD; td != nil {
		if curTD := peer.TD(); td.Cmp(curTD) >= 0 {
			peer.SetHead(announce.Block.Hash(), td)
		}
	}
}

// errBadRequest is a decoding/validation error raised by the server side. It is
// returned to the dispatch loop, which tears the connection down because a peer
// that sends malformed requests is not worth keeping.
type badRequest struct {
	msg string
	err error
}

func (e *badRequest) Error() string { return e.msg + ": " + e.err.Error() }
func (e *badRequest) Unwrap() error { return e.err }

func errBadRequest(msg string, err error) error { return &badRequest{msg: msg, err: err} }
