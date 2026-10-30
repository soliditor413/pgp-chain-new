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

// Defaults for the two DPoS network settings that have no safe zero value.
//
// They are declared here, in the package that consumes them, because they are
// read from two places that must not drift: the flag defaults in cmd/utils and
// the fallback in eth.startPBFTNetwork. Two copies of "the default" is how a
// rig ends up with one node on 3 and another on 300.
const (
	// DefaultNetMagic is pgp-chain mainnet's DPoS magic
	// (pgp-chain/params/config.go:106, Pbft.Magic = 202511).
	//
	// Its only job is to be equal on every producer: dpos/p2p/hub.Intercept
	// compares it against the magic in the incoming version message, and equal
	// means "this connection belongs to the local DPoS service". A mismatch does
	// not raise an error -- the connection is instead treated as a sub-service,
	// fails the state.index lookup in handleInbound, and is closed at Debug
	// level (dpos/p2p/hub/hub.go:320).
	//
	// Zero would technically work just as well as 202511, since it too can be
	// agreed on. It is rejected as the default because it is indistinguishable
	// from an uninitialised field: a bug that drops the config entirely then
	// looks exactly like a deliberate zero, and the symptom is the silent
	// no-connection above.
	DefaultNetMagic uint32 = 202511

	// DefaultMaxNodePerHost is pgp-chain mainnet's value
	// (pgp-chain/params/config.go:107, Pbft.MaxNodePerHost = 300).
	//
	// It caps inbound connections per source IP in the DPoS server
	// (dpos/p2p/server.go:584: hostConnManger[ip] < MaxNodePerHost). Zero is not
	// "unlimited" -- it rejects every inbound connection and logs it only at
	// Debug -- so this default has to be non-zero, and it has to be generous
	// enough for a bench that runs the whole producer set on one host.
	//
	// A low cap (3, say) is not a security boundary worth having: a peer that
	// shares an IP still has to complete the PID handshake to be accepted, so
	// the cap bounds connection count, not identity. What it does do is fail a
	// 4-node single-host rig intermittently, when a producer reconnects before
	// the previous socket is reaped.
	DefaultMaxNodePerHost uint32 = 300
)

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
	//
	// "Rejected" overstates how visible it is: dpos/p2p/hub.Intercept compares the
	// magic against the one in the incoming version message and, on a mismatch,
	// treats the connection as a sub-service rather than refusing it
	// (dpos/p2p/hub/hub.go:383). The handoff then fails the state.index lookup and
	// closes the socket, logged at Debug (hub.go:320). Two nodes with different
	// magics therefore both look healthy and never connect. DefaultNetMagic is
	// non-zero for exactly this reason.
	Magic uint32

	// MaxNodePerHost caps how many arbiters may share one IP. Default
	// DefaultMaxNodePerHost; zero rejects every inbound connection.
	MaxNodePerHost uint32

	// NodeVersion is exchanged in the DPoS version message.
	NodeVersion string

	// DPoSV2StartHeight is carried for parity with pgp-chain's
	// --pbft.dposv2startheight, but it decides nothing here and is not exposed as
	// a flag.
	//
	// It exists upstream to raise the DPoS payload version to V2 once the chain
	// passes a height (dpos/p2p/peer/peer.go:814). That gate is dead in practice:
	// pgp-chain/eth/backend.go:190 calls SetPayloadVersion(DPoSV2Version)
	// unconditionally, so the per-peer branch only re-does what already happened.
	// This port keeps the unconditional call (pbft.SetWireVersionV2) and therefore
	// never reads this field -- see wireversion.go for why the version has to be
	// pinned before the first handshake.
	//
	// It is NOT on ChainConfig (D9): the value that actually matters is a
	// process-wide switch, not a chain parameter, and putting a dead field into
	// ChainConfig would mean a config that can disagree with itself at no
	// diagnostic benefit.
	DPoSV2StartHeight uint32
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
