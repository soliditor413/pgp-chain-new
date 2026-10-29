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

// Config holds the per-node parameters the engine needs to sign, listen and
// announce itself.
//
// None of these belong on params.ChainConfig, and that is a deliberate split
// (docs/design/pbft-port-design.md D9):
//
//   - ChainConfig is written to disk and compared against the built-in genesis
//     config on every start, so a per-node port or keystore path in there would
//     cause a "stored config mismatch" rewrite on every boot;
//   - ChainConfig feeds the EIP-2124 fork checksum through core/forkid's
//     reflection over *Block/*Time field names, so every added field is a
//     handshake hazard for no reason;
//   - a keystore path is not consensus. Two nodes must agree on the block
//     period; they must not agree on where one of them keeps its key.
//
// Magic and NodeVersion are the exception to "these are not consensus": they are
// still per-node settings, but every producer has to use the same values or the
// DPoS TCP handshake fails. They are documented as such below rather than being
// moved into ChainConfig, because a wrong Magic is a loud connection failure,
// not a silent chain split.
type Config struct {
	// Enable turns block production on. When false the engine still verifies
	// headers and confirms exactly as consensus/pgp does, but Prepare and Seal
	// refuse: the node is a follower.
	//
	// Default false. A node with no pbft configuration at all therefore behaves
	// like the current read-only replay setup, which is what makes the rolling
	// upgrade in docs/design/pbft-port-design.md M5 reversible.
	Enable bool

	// KeyStore is the ELA keystore holding the arbiter key. It is a file, not a
	// key: the password comes from KeyStorePassword.
	KeyStore string

	// KeyStorePassword is the keystore password in clear text, because that is
	// the only form pgp-chain has ever supported (--pbft.keystore.password).
	// Prefer a file the operator can chmod over a command line argument that
	// lands in the shell history.
	//
	// It is excluded from the TOML form (toml:"-") and therefore from both
	// `geth dumpconfig` output and any config file the node rewrites: a password
	// that can be read back out of a file on disk is not a secret. It can only be
	// supplied through the flag (or its GETH_PBFT_KEYSTORE_PASSWORD environment
	// variable), which also keeps it out of --config entirely.
	KeyStorePassword string `toml:"-"`

	// DataDir is where the DPoS peer address database is kept, relative to the
	// node datadir when not absolute.
	DataDir string

	// MinerAddress overrides the etherbase. header.Coinbase is part of SealHash,
	// so the address that ends up in the block is the one the miner is told to
	// use; this field exists so a PBFT node can be told to sign with its arbiter
	// address instead of whatever account happens to be first in the keystore.
	MinerAddress string

	// IPAddress is the address this node advertises to the other arbiters.
	IPAddress string

	// Port is the DPoS TCP listen port (pgp-chain --pbft.net.port).
	Port uint16

	// Magic is the network magic of the DPoS channel. Every producer must use the
	// same value; a mismatch is rejected during the peer handshake.
	Magic uint32

	// MaxNodePerHost caps how many arbiters may share one IP.
	MaxNodePerHost uint32

	// NodeVersion is exchanged in the DPoS version message.
	NodeVersion string
}

// productionEnabled reports whether this node was configured to produce blocks.
//
// It is deliberately about intent only, not about whether a usable key was
// found. The two questions have different answers and different consequences:
//
//   - intent decides whether the node waits for the consensus-status exchange
//     before it may seal (see recovered in engine.go);
//   - a key decides whether it can sign at all.
//
// Folding the keystore path into this predicate would make a node whose keystore
// failed to open look like a follower, i.e. it would silently stop waiting for
// recovery and start sealing the moment a key appeared. Keeping them apart means
// the missing key is reported by Prepare/Seal, where an operator can see it,
// while the recovery gate still behaves as configured.
func (c *Config) productionEnabled() bool {
	return c != nil && c.Enable
}

// hasKeyStore reports whether a keystore location was configured.
func (c *Config) hasKeyStore() bool {
	return c != nil && c.KeyStore != ""
}
