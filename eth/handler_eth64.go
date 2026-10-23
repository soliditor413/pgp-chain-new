// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

import (
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
	"github.com/ethereum/go-ethereum/p2p"
)

// eth64Handler adapts the internal handler to the minimal interface the eth/64
// protocol needs. It deliberately exposes nothing but the chain: the eth/64
// implementation is self contained (own peer set, own sync loop) so it cannot
// corrupt the eth/68 or bsc peer sets.
type eth64Handler handler

// Chain implements eth64.Backend.
func (h *eth64Handler) Chain() *core.BlockChain { return h.chain }

// makeETH64Protocols returns the pgp-chain eth/64 protocol entry. It is only
// registered for chains that carry the pgp legacy history, never for
// BSC/Chapel/Rialto.
func (s *Ethereum) makeETH64Protocols() []p2p.Protocol {
	if s.blockchain == nil || s.blockchain.Config().Legacy == nil {
		return nil
	}
	return eth64.MakeProtocols((*eth64Handler)(s.handler), s.networkID)
}
