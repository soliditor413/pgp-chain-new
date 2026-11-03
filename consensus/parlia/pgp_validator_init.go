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

package parlia

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// This file adapts Parlia to a chain whose history was produced by another
// consensus and only switches to Parlia at a non-zero height.
//
// Upstream Parlia assumes it owns the chain from block 0: the validator set is
// parsed out of the genesis header's extra field (parlia.snapshot, number == 0),
// and the system contracts are initialised in the block whose number is 1
// (parlia.go:1465, :1573). Neither holds here. The pgp genesis header carries a
// PBFT producer set and a P-256 confirm signature, not a 20-byte address list
// and a 65-byte secp256k1 seal, so parsing it as a Parlia header yields garbage;
// and the first Parlia block is block ForkHeight, not block 1, so the system
// contracts would never be initialised at all.
//
// Two things are therefore added:
//
//  1. A snapshot anchor at ForkHeight-1 whose validator set is read from the
//     chain config instead of from a header. It is exact rather than
//     approximate: before ForkHeight the validator set is a constant, because
//     it comes from LegacyConfig.
//  2. An init block predicate so BSCValidatorSet.init() runs in block
//     ForkHeight, and it is fed the validator set as an argument rather than
//     reading it from a compile-time constant.

const (
	// maxPGPGenesisValidators bounds the genesis set by the width of the
	// validator-count byte in the header extra field. prepareValidators writes
	// it as byte(len(newValidators)) (parlia.go:1034), so anything above 255
	// would wrap around silently and produce a header that cannot be parsed
	// back.
	maxPGPGenesisValidators = 255

	// pgpValidatorPackageType is the packageType discriminator of
	// BSCValidatorSet's validator set RLP. decodeValidatorSet dispatches on it,
	// and 0 selects decodeValidatorSetSynPackage, which is the 4-tuple-per-
	// validator layout this encoder produces.
	pgpValidatorPackageType = 0
)

// ErrNoPGPGenesisValidators is returned when a chain has a Parlia fork height
// but no genesis validator set configured.
//
// It is a hard error and not "no validators yet": BSCValidatorSet.init requires
// a non-empty set, and a zero numOfCabinets makes getMiningValidators
// underflow (BSCValidatorSet.sol:366-367 allocates a zero-length array and then
// indexes it). A chain that reaches ForkHeight without a validator set can only
// stop, so it is better to refuse to start.
var ErrNoPGPGenesisValidators = errors.New("parlia fork height is set but parliaGenesisValidators is empty")

// pgpGenesisValidatorSet is the validated, ready-to-encode form of
// LegacyConfig's four validator fields.
//
// The addresses are kept as three parallel slices rather than a struct because
// that is the order the RLP tuple wants; keeping them separate makes the
// mapping to the contract's field names explicit at the one place that matters
// (pgpValidatorInitArgs).
type pgpGenesisValidatorSet struct {
	// Consensus addrs, in config order. This order is consensus critical: it
	// is the order the RLP payload is built in, so it fixes the state root
	// written at ForkHeight.
	Consensus []common.Address

	// BSCFee addrs, parallel to Consensus. Decoded by the contract as
	// BBCFeeAddress (index 2 of the tuple) - see the ordering note in
	// pgpValidatorInitArgs.
	BSCFee []common.Address

	// Fee addrs, parallel to Consensus. Decoded by the contract as feeAddress
	// (index 1 of the tuple).
	Fee []common.Address

	// Voting powers, parallel to Consensus.
	Powers []uint64

	// Cabinets is the value seeded into BSCValidatorSet.numOfCabinets.
	Cabinets uint64
}

// newPGPGenesisValidatorSet validates LegacyConfig's validator fields and fills
// in the optional ones.
//
// Validation is strict on purpose. Every one of these conditions, if let
// through, produces a node that starts fine and then writes a state root that
// no other node agrees with, which is far harder to debug than a refusal to
// start.
func newPGPGenesisValidatorSet(cfg *params.LegacyConfig) (*pgpGenesisValidatorSet, error) {
	if cfg == nil {
		return nil, errors.New("no legacy config: cannot build the parlia genesis validator set")
	}
	n := len(cfg.ParliaGenesisValidators)
	if n == 0 {
		return nil, ErrNoPGPGenesisValidators
	}
	if n > maxPGPGenesisValidators {
		return nil, fmt.Errorf("too many parlia genesis validators: %d > %d (the header extra count byte is a uint8)",
			n, maxPGPGenesisValidators)
	}

	// The optional parallel lists are either absent or exactly n long. A
	// shorter or longer list is a configuration mistake, not something to pad:
	// padding would silently attach the wrong fee address to the wrong
	// validator, and the resulting payload would still be well-formed.
	if l := len(cfg.ParliaGenesisFeeAddrs); l != 0 && l != n {
		return nil, fmt.Errorf("parliaGenesisFeeAddrs has %d entries but parliaGenesisValidators has %d; must be empty or match exactly", l, n)
	}
	if l := len(cfg.ParliaGenesisVotingPowers); l != 0 && l != n {
		return nil, fmt.Errorf("parliaGenesisVotingPowers has %d entries but parliaGenesisValidators has %d; must be empty or match exactly", l, n)
	}

	set := &pgpGenesisValidatorSet{
		Consensus: make([]common.Address, n),
		BSCFee:    make([]common.Address, n),
		Fee:       make([]common.Address, n),
		Powers:    make([]uint64, n),
	}

	// Duplicate detection. A repeated consensus address makes
	// currentValidatorSetMap point the second copy at the first, so the
	// contract would report a smaller mining set than the header announces and
	// every epoch block would fail verifyValidators.
	seen := make(map[common.Address]int, n)

	for i, raw := range cfg.ParliaGenesisValidators {
		addr, err := parsePGPValidatorAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("parliaGenesisValidators[%d] %q: %w", i, raw, err)
		}
		if addr == (common.Address{}) {
			return nil, fmt.Errorf("parliaGenesisValidators[%d] is the zero address", i)
		}
		if first, ok := seen[addr]; ok {
			return nil, fmt.Errorf("parliaGenesisValidators[%d] and [%d] are both %s; the set must be unique", first, i, addr)
		}
		seen[addr] = i
		set.Consensus[i] = addr
		set.Fee[i] = addr
		set.BSCFee[i] = addr
	}

	for i, raw := range cfg.ParliaGenesisFeeAddrs {
		addr, err := parsePGPValidatorAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("parliaGenesisFeeAddrs[%d] %q: %w", i, raw, err)
		}
		if addr == (common.Address{}) {
			return nil, fmt.Errorf("parliaGenesisFeeAddrs[%d] is the zero address", i)
		}
		set.Fee[i] = addr
	}

	if len(cfg.ParliaGenesisVotingPowers) == 0 {
		for i := range set.Powers {
			set.Powers[i] = params.DefaultParliaVotingPower
		}
	} else {
		for i, power := range cfg.ParliaGenesisVotingPowers {
			// Zero is rejected rather than defaulted: a zero voting power is a
			// valid uint64 that the contract accepts, and it produces a
			// validator that can never win an election once StakeHub takes
			// over. Refusing it here is the only place it can be caught.
			if power == 0 {
				return nil, fmt.Errorf("parliaGenesisVotingPowers[%d] is zero", i)
			}
			set.Powers[i] = power
		}
	}

	// Zero means "one seat per validator", which is what both the devnet and
	// mainnet want; anything else must at least be non-zero, or
	// getMiningValidators underflows.
	set.Cabinets = cfg.ParliaGenesisCabinets
	if set.Cabinets == 0 {
		set.Cabinets = uint64(n)
	}
	if set.Cabinets > uint64(n) {
		return nil, fmt.Errorf("parliaGenesisCabinets is %d but only %d validators are configured", set.Cabinets, n)
	}

	return set, nil
}

// parsePGPValidatorAddress accepts the address forms that appear in a genesis
// JSON file and rejects everything else.
//
// common.HexToAddress is deliberately not used on its own: it never fails. It
// truncates or zero-pads whatever it is given, so "0xzz" silently becomes the
// zero address and a 39-character address silently becomes a different,
// perfectly valid-looking address. Either would change the RLP payload and
// fork the chain against every node that did read the file correctly.
func parsePGPValidatorAddress(s string) (common.Address, error) {
	const hexLen = 2 * common.AddressLength

	body := s
	if len(body) >= 2 && (body[:2] == "0x" || body[:2] == "0X") {
		body = body[2:]
	}
	if len(body) != hexLen {
		return common.Address{}, fmt.Errorf("expected %d hex characters, got %d", hexLen, len(body))
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return common.Address{}, fmt.Errorf("invalid hex character %q at offset %d", c, i)
		}
	}
	return common.HexToAddress(s), nil
}

// pgpValidatorSetInitSelector is the 4-byte selector of
// BSCValidatorSet.init(bytes,uint256), i.e. keccak256 of the canonical
// signature.
//
// It is derived rather than hardcoded so that it stays auditable, and it is
// asserted against the compiled ABI in New() so that a stale ABI - one still
// exposing the pre-C1-a no-argument init() - fails at startup instead of at
// ForkHeight.
var pgpValidatorSetInitSelector = crypto.Keccak256([]byte("init(bytes,uint256)"))[:4]

// pgpPlainInitSelector is the 4-byte selector of the no-argument init() that the
// other six system contracts still expose.
//
// initContract() feeds the same calldata to seven addresses. Upstream could pack
// "init" once from validatorSetABI because all seven took no arguments. Now only
// the validator contract does, and the ABI no longer describes a no-argument
// init() at all, so Pack("init") cannot produce these four bytes any more. They
// are derived here instead.
//
// It evaluates to 0xe1c7392a, which is what signer/fourbyte/4byte.json records
// for init() and what the previous no-argument ABI method id was - so the six
// non-validator contracts keep receiving exactly the calldata they always did.
var pgpPlainInitSelector = crypto.Keccak256([]byte("init()"))[:4]

// pgpValidatorTuple is one element of the validator set list as it is encoded
// into the RLP payload.
//
// Field order is consensus critical and does NOT match the field names in
// LegacyConfig. The contract's decodeValidator (BSCValidatorSet.sol) reads:
//
//	idx 0 -> consensusAddress
//	idx 1 -> feeAddress
//	idx 2 -> BBCFeeAddress
//	idx 3 -> votingPower
//
// so the second slot is the *BBC* fee address and the third is the *fee*
// address. Encoders that align by name instead of by position swap those two
// addresses. The payload stays well-formed, decodes cleanly, and produces a
// different state root - which is why every implementation of this tuple
// (this one, scripts/gen-pgp-genesis-config.py, scripts/validators.js) carries
// the order explicitly.
//
// Only four fields are emitted. decodeValidator sets success = true once idx
// reaches 3, so the trailing BLS public key is optional, and leaving it out is
// what keeps the payload identical to the one produced by the pre-Luban
// tooling. A 5-tuple with a BLS key would encode the same 4 fields plus 48
// bytes, i.e. a different payload.
type pgpValidatorTuple struct {
	Consensus common.Address
	BSCFee    common.Address
	Fee       common.Address
	Power     uint64
}

// pgpValidatorInitArgs builds the two arguments BSCValidatorSet.init takes.
//
// The payload is RLP([packageType, validators]) where packageType is encoded as
// the empty string per RLP's integer rule, i.e. 0x80 and not 0x00. Both decode
// to 0, but only 0x80 is what every other encoder produces and therefore what
// the payload hash in the design doc was computed over. Getting this wrong does
// not break the contract - it breaks cross-implementation comparison, which is
// the only way to check an encoder before it has ever run on chain.
func pgpValidatorInitArgs(cfg *params.LegacyConfig) (payload []byte, cabinets uint64, err error) {
	set, err := newPGPGenesisValidatorSet(cfg)
	if err != nil {
		return nil, 0, err
	}

	tuples := make([]pgpValidatorTuple, len(set.Consensus))
	for i := range set.Consensus {
		tuples[i] = pgpValidatorTuple{
			Consensus: set.Consensus[i],
			BSCFee:    set.BSCFee[i],
			Fee:       set.Fee[i],
			Power:     set.Powers[i],
		}
	}

	// rlp.EncodeToBytes on a struct encodes its fields as a list, which is the
	// tuple. The outer []interface{} is a list of two elements: the package
	// type and the validator list.
	payload, err = rlp.EncodeToBytes([]interface{}{
		uint8(pgpValidatorPackageType),
		tuples,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("failed to RLP-encode the parlia genesis validator set: %w", err)
	}
	return payload, set.Cabinets, nil
}

// pgpInitPlan is the pre-computed init() calldata, built once at engine
// construction so that a bad validator configuration fails at startup rather
// than at ForkHeight, i.e. right when the chain is supposed to switch over.
type pgpInitPlan struct {
	// payload is the RLP blob passed as the first init() argument.
	payload []byte

	// cabinets is the value passed as the second init() argument.
	cabinets uint64
}

// newPGPInitPlan returns the init plan for a chain config, or nil when no
// injection is needed.
//
// A nil Legacy block, or a ForkHeight still at math.MaxUint64, means "this
// chain never switches", so there is nothing to inject and no validator set to
// demand. That case must stay silent: PGPChainConfig ships with
// ForkHeight = math.MaxUint64 precisely so that a node syncing pgp mainnet
// today does not need a Parlia validator set in its config.
func newPGPInitPlan(chainConfig *params.ChainConfig) (*pgpInitPlan, error) {
	if chainConfig == nil || chainConfig.Legacy == nil {
		return nil, nil
	}
	if chainConfig.Legacy.ForkHeight == math.MaxUint64 {
		return nil, nil
	}
	payload, cabinets, err := pgpValidatorInitArgs(chainConfig.Legacy)
	if err != nil {
		return nil, err
	}
	return &pgpInitPlan{payload: payload, cabinets: cabinets}, nil
}

// isPGPInitBlock reports whether the system contracts have to be initialised in
// this block, i.e. whether it is the first Parlia block.
//
// Upstream only ever inits in block 1. On this chain the role of block 1 is
// played by block ForkHeight, because that is where the Parlia state - and with
// it the system contracts that Parlia creates - begins.
func (p *Parlia) isPGPInitBlock(number *big.Int) bool {
	if p.pgpInit == nil || number == nil {
		return false
	}
	return number.Uint64() == p.chainConfig.Legacy.ForkHeight
}

// pgpSnapshotAnchorNumber returns the height at which the Parlia snapshot
// chain is anchored, i.e. the last block produced before the switch.
//
// ok is false when the chain has no Parlia switch, in which case upstream's
// number == 0 anchor is the right one and this file stays out of the way.
func (p *Parlia) pgpSnapshotAnchorNumber() (number uint64, ok bool) {
	// A fork at height 0 would mean Parlia from genesis, where upstream's own
	// anchor applies and there is no ForkHeight-1 block to anchor at.
	if p.pgpInit == nil || p.chainConfig.Legacy.ForkHeight == 0 || p.chainConfig.Legacy.ForkHeight == math.MaxUint64 {
		return 0, false
	}
	return p.chainConfig.Legacy.ForkHeight - 1, true
}

// pgpAnchorSnapshot builds the snapshot for the last legacy block.
//
// It is built from the chain config rather than by parsing a header, because
// that header was produced by the legacy consensus and does not contain a
// Parlia validator list. This is exact, not an approximation: before ForkHeight
// the validator set is constant by construction.
//
// Recents is empty, which upstream explicitly tolerates for snapshots built by
// newSnapshot from a header (parlia.go:896-899): the double-sign check becomes
// more lenient, the block time check is not consulted during apply, and the
// slash decision is never verified against a transaction list. The cost is that
// a validator may sign twice in the first few blocks after the switch; the
// benefit is not having to invent a history that never happened.
func (p *Parlia) pgpAnchorSnapshot(number uint64, hash common.Hash) (*Snapshot, error) {
	cfg := p.chainConfig.Legacy
	set, err := newPGPGenesisValidatorSet(cfg)
	if err != nil {
		return nil, err
	}

	// Zero BLS public keys, in the same shape parseValidators produces for a
	// post-Luban epoch header whose validators have no BLS key registered yet.
	//
	// This is not a placeholder that upstream never exercises: verifyValidators
	// and prepareValidators both substitute a zero key explicitly on the Luban
	// fork block (parlia.go:1271-1277, :1035-1041), so a set of zero keys is a
	// state BSC itself passes through. Vote attestation verification cannot be
	// reached from here, because it returns early when a header carries no
	// attestation, and a zero key cannot be turned into a signature.
	voteAddrs := make([]types.BLSPublicKey, len(set.Consensus))

	snap := newSnapshot(p.config, p.signatures, number, hash, set.Consensus, voteAddrs, p.ethAPI)
	snap.BlockInterval = defaultBlockInterval
	snap.TurnLength = defaultTurnLength
	// EpochLength is left at newSnapshot's defaultEpochLength. On a chain where
	// Lorentz is not activated - which is the case for this fork, since its
	// time-based forks cannot be pinned in genesis - the epoch length never
	// changes away from that default, so the anchor matches what apply() will
	// go on to compute.
	return snap, nil
}
