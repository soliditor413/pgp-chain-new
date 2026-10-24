// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

import (
	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
	"github.com/ethereum/go-ethereum/p2p"
)

// eth64Handler adapts the internal handler to the minimal interface the eth/64
// protocol needs. It deliberately exposes nothing but the chain: the eth/64
// implementation is self contained (own peer set, own sync loop) so it cannot
// corrupt the eth/68 or bsc peer sets.
type eth64Handler handler

// Chain implements eth64.Backend. The concrete chain satisfies eth64.ChainReader,
// which is the narrowest surface eth/64 uses.
func (h *eth64Handler) Chain() eth64.ChainReader { return h.chain }

// makeETH64Protocols returns the pgp-chain eth/64 protocol entry. It is only
// registered for chains that carry the pgp legacy history, never for
// BSC/Chapel/Rialto.
//
// The result is memoised because eth64.MakeProtocols is not a pure descriptor: it
// allocates a peer set and starts a sync goroutine. Protocols() is called twice
// per start - once to register (eth/backend.go:534) and once to name the protocols
// for a metrics gauge (cmd/geth/config.go:345) - so creating the entry twice would
// leave a second eth/64 syncer running with no peers for the life of the process,
// and would point progress reporting at whichever handler happened to be built
// last.
func (s *Ethereum) makeETH64Protocols() []p2p.Protocol {
	if s.blockchain == nil || s.blockchain.Config().Legacy == nil {
		return nil
	}
	s.eth64ProtosOnce.Do(func() {
		s.eth64Protos = eth64.MakeProtocols((*eth64Handler)(s.handler), s.networkID)
	})
	return s.eth64Protos
}
