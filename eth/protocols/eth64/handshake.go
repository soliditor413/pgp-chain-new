// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/p2p"
)

// handshakeTimeout is how long the status exchange may take.
const handshakeTimeout = 5 * time.Second

var (
	errNoStatusMsg      = errors.New("eth64: no status message")
	errMsgTooLarge      = errors.New("eth64: message too large")
	errHandshakeTimeout = errors.New("eth64: handshake timeout")
)

// ourStatus assembles the status packet describing the local chain.
func (h *handler) ourStatus() (*StatusPacket, error) {
	chain := h.backend.Chain()

	head := chain.CurrentHeader()
	if head == nil {
		return nil, errors.New("eth64: missing current header")
	}
	td := chain.GetTd(head.Hash(), head.Number.Uint64())
	if td == nil {
		td = head.Difficulty
	}
	return &StatusPacket{
		ProtocolVersion: ETH64,
		NetworkID:       h.network,
		TD:              td,
		Head:            head.Hash(),
		Genesis:         chain.Genesis().Hash(),
		ForkID:          forkid.NewIDWithChain(chain),
	}, nil
}

// handshake performs the eth/64 status exchange.
//
// eth/64 has exactly one status message: there is no UpgradeStatus handshake
// (eth/67+) and no snap/bsc extension barrier, so waiting for either would make
// a pgp peer time out and drop us after 5 seconds.
//
// Validation failures are reported by returning a p2p.DiscReason: p2p's run loop
// detects that type and sends the proper Disconnect message, which is what
// pgp-chain expects (simply closing the socket leaves it waiting).
func (h *handler) handshake(p *Peer) error {
	status, err := h.ourStatus()
	if err != nil {
		return err
	}
	if err := p.SendStatus(status); err != nil {
		return err
	}
	msg, err := p.readWithTimeout(handshakeTimeout)
	if err != nil {
		return err
	}
	if msg.Size > protocolMaxMsgSize {
		msg.Discard()
		p.Log().Debug("Status message too large", "size", msg.Size)
		return p2p.DiscProtocolError
	}
	if msg.Code != StatusMsg {
		msg.Discard()
		p.Log().Debug("Expected a status message", "code", msg.Code)
		return p2p.DiscProtocolError
	}
	var remote StatusPacket
	if err := msg.Decode(&remote); err != nil {
		p.Log().Debug("Invalid status message", "err", err)
		return p2p.DiscProtocolError
	}
	if remote.ProtocolVersion != uint32(ETH64) {
		p.Log().Debug("Protocol version mismatch", "remote", remote.ProtocolVersion, "local", ETH64)
		return p2p.DiscIncompatibleVersion
	}
	if remote.NetworkID != h.network {
		p.Log().Debug("Network id mismatch", "remote", remote.NetworkID, "local", h.network)
		return p2p.DiscNetworkError
	}
	localGenesis := h.backend.Chain().Genesis().Hash()
	if remote.Genesis != localGenesis {
		p.Log().Debug("Genesis mismatch", "remote", remote.Genesis, "local", localGenesis)
		return p2p.DiscNetworkError
	}
	// EIP-2124 fork id validation, same rules as the eth/68 handshake.
	if err := forkid.NewFilter(h.backend.Chain())(remote.ForkID); err != nil {
		p.Log().Debug("Fork id rejected", "remote", remote.ForkID, "err", err)
		return p2p.DiscSubprotocolError
	}
	p.SetHead(remote.Head, remote.TD)
	handshakeMeter.Mark(1)

	p.Log().Info("eth/64 handshake done", "head", remote.Head, "td", remote.TD, "forkid", remote.ForkID, "network", remote.NetworkID)
	return nil
}

// readWithTimeout reads one message, giving up after the given timeout. The
// underlying MsgReadWriter exposes no deadline, so the read runs in a goroutine;
// the connection is torn down by the caller on timeout, which unblocks it.
func (p *Peer) readWithTimeout(timeout time.Duration) (msg p2p.Msg, err error) {
	type result struct {
		msg p2p.Msg
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := p.rw.ReadMsg()
		ch <- result{msg: msg, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.msg, r.err
	case <-timer.C:
		p.Close()
		return p2p.Msg{}, fmt.Errorf("%w after %v", errHandshakeTimeout, timeout)
	}
}
