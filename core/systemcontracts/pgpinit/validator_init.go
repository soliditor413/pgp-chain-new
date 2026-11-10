// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package pgpinit

// This file builds the arguments of BSCValidatorSet.init(bytes,uint256).
//
// It used to live in consensus/parlia, which is where the values come from, but
// the protocol's deployment step (core/pgp_systemcontract_deploy.go) needs the
// same encoding and core cannot import parlia (parlia already imports core).
// The package split keeps one encoder rather than two: two encoders of a
// consensus-critical payload diverge, and the divergence only shows up as a
// state root that differs between implementations.
//
// Everything below is moved verbatim. Do not "simplify" the RLP call or the
// package type: the bytes are compared against other implementations opcode by
// opcode, and a well-formed but different payload is the worst possible
// outcome.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	blscrypto "github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

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

	// voteAddressLength is the size of a compressed BLS12-381 public key, which
	// is what BSCValidatorSet stores as a validator's vote address and what an
	// aggregate attestation is verified against.
	voteAddressLength = 48
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

// ValidatorSet is the validated, ready-to-encode form of LegacyConfig's
// validator fields.
//
// The addresses are kept as three parallel slices rather than a struct because
// that is the order the RLP tuple wants; keeping them separate makes the
// mapping to the contract's field names explicit at the one place that matters
// (ValidatorInitArgs).
type ValidatorSet struct {
	// Consensus addrs, in config order. This order is consensus critical: it
	// is the order the RLP payload is built in, so it fixes the state root
	// written at ForkHeight.
	Consensus []common.Address

	// BSCFee addrs, parallel to Consensus. Decoded by the contract as
	// BBCFeeAddress (index 2 of the tuple) - see the ordering note in
	// ValidatorInitArgs.
	BSCFee []common.Address

	// Fee addrs, parallel to Consensus. Decoded by the contract as feeAddress
	// (index 1 of the tuple).
	Fee []common.Address

	// Voting powers, parallel to Consensus.
	Powers []uint64

	// VoteAddrs is the per-validator BLS public key (48 bytes), parallel to
	// Consensus. Empty means "this chain has no vote addresses configured",
	// which is the mainnet case and keeps the RLP payload at four fields.
	//
	// It is a [][]byte rather than [][48]byte so the empty case is unambiguous:
	// a nil slice means "omit the field", which is different from "48 zero
	// bytes", and the encoder must be able to tell those apart.
	VoteAddrs [][]byte

	// Cabinets is the value seeded into BSCValidatorSet.numOfCabinets.
	Cabinets uint64
}

// FromLegacy validates LegacyConfig's validator fields and fills in the optional
// ones.
//
// Validation is strict on purpose. Every one of these conditions, if let
// through, produces a node that starts fine and then writes a state root that
// no other node agrees with, which is far harder to debug than a refusal to
// start.
func FromLegacy(cfg *params.LegacyConfig) (*ValidatorSet, error) {
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

	set := &ValidatorSet{
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
		addr, err := parseValidatorAddress(raw)
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
		addr, err := parseValidatorAddress(raw)
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

	// Vote addresses are all-or-nothing, exactly like the fee addresses: a list
	// shorter than the validator set would attach the wrong BLS key to the wrong
	// validator, and the resulting payload would still be well-formed, so the
	// mistake would only surface as attestations that never verify.
	if l := len(cfg.ParliaGenesisVoteAddrs); l != 0 {
		if l != n {
			return nil, fmt.Errorf("parliaGenesisVoteAddrs has %d entries but parliaGenesisValidators has %d; must be empty or match exactly", l, n)
		}
		set.VoteAddrs = make([][]byte, n)
		for i, raw := range cfg.ParliaGenesisVoteAddrs {
			vote, err := parseVoteAddress(raw)
			if err != nil {
				return nil, fmt.Errorf("parliaGenesisVoteAddrs[%d] %q: %w", i, raw, err)
			}
			set.VoteAddrs[i] = vote
		}
	}

	return set, nil
}

// parseVoteAddress accepts a 48-byte BLS public key in hex, with or without the
// 0x prefix, and rejects everything else.
//
// The all-zero key is rejected on purpose. It is what the contract stores when
// no vote address is supplied, so accepting it here would let an operator
// "configure" finality that can never work: the payload would carry 48 zero
// bytes, the epoch block would look identical to the one measured in
// 04-verification.md §1.2, and every aggregate attestation would fail to verify
// with no hint that the keys were the problem.
func parseVoteAddress(s string) ([]byte, error) {
	const hexLen = 2 * voteAddressLength

	body := s
	if len(body) >= 2 && (body[:2] == "0x" || body[:2] == "0X") {
		body = body[2:]
	}
	if len(body) != hexLen {
		return nil, fmt.Errorf("expected %d hex characters (%d-byte BLS public key), got %d", hexLen, voteAddressLength, len(body))
	}
	if _, err := hex.DecodeString(body); err != nil {
		return nil, fmt.Errorf("not hex: %w", err)
	}
	vote := common.FromHex(body)
	allZero := true
	for _, b := range vote {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil, errors.New("is the zero key, which is the same as not configuring a vote address at all")
	}
	// The curve check is not paranoia. A 96-character hex string that is not a
	// point on BLS12-381 passes every length check, is stored by the contract
	// verbatim, and then every aggregate attestation fails to verify - with
	// nothing on chain explaining why. This is the last point at which that
	// mistake costs a restart instead of a debugging session.
	if _, err := blscrypto.PublicKeyFromBytes(vote); err != nil {
		return nil, fmt.Errorf("not a BLS12-381 public key: %w", err)
	}
	return vote, nil
}

// parseValidatorAddress accepts the address forms that appear in a genesis
// JSON file and rejects everything else.
//
// common.HexToAddress is deliberately not used on its own: it never fails. It
// truncates or zero-pads whatever it is given, so "0xzz" silently becomes the
// zero address and a 39-character address silently becomes a different,
// perfectly valid-looking address. Either would change the RLP payload and
// fork the chain against every node that did read the file correctly.
func parseValidatorAddress(s string) (common.Address, error) {
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

// ValidatorInitSelector is the 4-byte selector of
// BSCValidatorSet.init(bytes,uint256), i.e. keccak256 of the canonical
// signature. Derived rather than hardcoded so it stays auditable.
var ValidatorInitSelector = crypto.Keccak256([]byte("init(bytes,uint256)"))[:4]

// validatorTuple is one element of the validator set list as it is encoded
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
type validatorTuple struct {
	Consensus common.Address
	BSCFee    common.Address
	Fee       common.Address
	Power     uint64
}

// ValidatorInitArgs builds the two arguments BSCValidatorSet.init takes.
//
// The payload is RLP([packageType, validators]) where packageType is encoded as
// the empty string per RLP's integer rule, i.e. 0x80 and not 0x00. Both decode
// to 0, but only 0x80 is what every other encoder produces and therefore what
// the payload hash in the design doc was computed over. Getting this wrong does
// not break the contract - it breaks cross-implementation comparison, which is
// the only way to check an encoder before it has ever run on chain.
func ValidatorInitArgs(cfg *params.LegacyConfig) (payload []byte, cabinets uint64, err error) {
	set, err := FromLegacy(cfg)
	if err != nil {
		return nil, 0, err
	}

	tuples := make([]validatorTuple, len(set.Consensus))
	for i := range set.Consensus {
		tuples[i] = validatorTuple{
			Consensus: set.Consensus[i],
			BSCFee:    set.BSCFee[i],
			Fee:       set.Fee[i],
			Power:     set.Powers[i],
		}
	}

	// Two shapes, chosen by whether a vote address was configured.
	//
	// The four-field shape is not a legacy fallback - it is the mainnet shape,
	// and it has to stay byte-identical to what previous releases produced,
	// because its bytes are what block D's state root is computed over. Adding a
	// fifth field unconditionally (even with an empty value) would change the
	// payload of every existing chain for no reason. The contract accepts both:
	// BSCValidatorSet.decodeValidator treats index 4 as an optional vote address
	// (contracts/BSCValidatorSet.sol:1168-1169).
	var encodeTarget interface{}
	if len(set.VoteAddrs) == 0 {
		encodeTarget = tuples
	} else {
		// Vote addresses are what make fast finality work at all: attestations
		// are BLS aggregate signatures and the validator set has to know the
		// public keys. An empty list here is "no finality on this chain", not
		// "an error" - the devnet before this field existed ran exactly that
		// way, with 48 zero bytes per seat in every epoch block.
		withVotes := make([]validatorTupleWithVote, len(tuples))
		for i := range tuples {
			withVotes[i] = validatorTupleWithVote{
				Consensus: tuples[i].Consensus,
				BSCFee:    tuples[i].BSCFee,
				Fee:       tuples[i].Fee,
				Power:     tuples[i].Power,
				Vote:      set.VoteAddrs[i],
			}
		}
		encodeTarget = withVotes
	}

	// rlp.EncodeToBytes on a struct encodes its fields as a list, which is the
	// tuple. The outer []interface{} is a list of two elements: the package
	// type and the validator list.
	payload, err = rlp.EncodeToBytes([]interface{}{
		uint8(pgpValidatorPackageType),
		encodeTarget,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("failed to RLP-encode the parlia genesis validator set: %w", err)
	}

	// Round-trip self-check (audit F-05). Read-only: the bytes above are the
	// bytes that get injected, and nothing below changes them.
	//
	// The reason this exists is narrower than "defensive coding". The contract's
	// decodeValidatorSet walks the RLP list and trusts its length, so a payload
	// that is well-formed but one validator short decodes cleanly and installs a
	// smaller set. init() is one-shot: after it, alreadyInit is true and there is
	// no governance path to correct the set (see F-02) - only a hard fork. The
	// failure therefore has to happen here, before block D is produced, and it
	// has to be loud.
	//
	// What this catches: framing errors (a length prefix that describes fewer
	// elements than were written), field-order slips, and a tuple/address list
	// that drifted out of sync. What it cannot catch: a bug that encodes and
	// decodes symmetrically wrong. That is what the independent encoder in
	// validator_init_test.go is for.
	//
	// The expectation is built here rather than re-encoded, and it is built in
	// the shape-independent form, so a mistake in *which* shape was chosen (four
	// fields where five were meant, or the vote addresses attached to the wrong
	// validators) shows up as a mismatch rather than as an agreement.
	expected := make([]decodedValidator, len(tuples))
	for i := range tuples {
		expected[i] = decodedValidator{
			Consensus: tuples[i].Consensus,
			BSCFee:    tuples[i].BSCFee,
			Fee:       tuples[i].Fee,
			Power:     tuples[i].Power,
		}
		if len(set.VoteAddrs) != 0 {
			expected[i].Vote = set.VoteAddrs[i]
		}
	}
	decodedPkg, decoded, err := decodeValidatorInitPayload(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("internal: validator init payload failed its round-trip self-check: %w", err)
	}
	if int(decodedPkg) != pgpValidatorPackageType || len(decoded) != len(expected) {
		return nil, 0, fmt.Errorf("internal: validator init payload failed its round-trip self-check: "+
			"encoded %d validators (package type %d), decoded %d (package type %d)",
			len(expected), pgpValidatorPackageType, len(decoded), decodedPkg)
	}
	for i := range expected {
		if !sameDecodedValidator(decoded[i], expected[i]) {
			return nil, 0, fmt.Errorf("internal: validator init payload failed its round-trip self-check: "+
				"validator %d encoded as %+v, decoded as %+v", i, expected[i], decoded[i])
		}
	}

	return payload, set.Cabinets, nil
}

// validatorTupleWithVote is the five-field shape. It is a separate type rather
// than a nullable field on validatorTuple because RLP has no notion of an absent
// struct field: adding Vote to the tuple would emit an empty string for it and
// change the payload of every chain that has no vote addresses.
//
// The field order matches validatorTuple plus the vote address, which is what
// BSCValidatorSet.decodeValidator reads (index 4, optional).
type validatorTupleWithVote struct {
	Consensus common.Address
	BSCFee    common.Address
	Fee       common.Address
	Power     uint64
	Vote      []byte
}

// decodedValidator is the shape-independent form the self-check compares in.
//
// Vote is nil when the payload carried four fields and 48 bytes when it carried
// five. Keeping "nil" and "48 zero bytes" distinct is deliberate: the whole
// point of the vote field is that an all-zero key means "finality cannot work
// here", so the self-check must be able to tell the two apart.
type decodedValidator struct {
	Consensus common.Address
	BSCFee    common.Address
	Fee       common.Address
	Power     uint64
	Vote      []byte
}

func sameDecodedValidator(a, b decodedValidator) bool {
	return a.Consensus == b.Consensus &&
		a.BSCFee == b.BSCFee &&
		a.Fee == b.Fee &&
		a.Power == b.Power &&
		bytes.Equal(a.Vote, b.Vote)
}

// decodeValidatorInitPayload is the inverse of the encoder above.
//
// It exists for one caller: the round-trip self-check in ValidatorInitArgs. It
// is deliberately strict about the outer framing - decoding into []rlp.RawValue
// forces the top level to be a two-element list and turns trailing bytes into an
// error - because a lenient decoder here would reproduce exactly the failure the
// self-check is meant to prevent.
//
// It accepts both the four-field and five-field validator shapes, and rejects
// anything in between: the contract would tolerate a three-field tuple (success
// flips at index 3, so it would not) but there is no configuration that should
// ever produce one, and a decoder that quietly accepted it could not be used to
// prove the count.
//
// It is not a second encoder and must not grow into one: the payload is
// consensus-critical and must have one producer.
func decodeValidatorInitPayload(payload []byte) (uint8, []decodedValidator, error) {
	var outer []rlp.RawValue
	if err := rlp.DecodeBytes(payload, &outer); err != nil {
		return 0, nil, fmt.Errorf("outer list: %w", err)
	}
	if len(outer) != 2 {
		return 0, nil, fmt.Errorf("outer list has %d elements, want 2 (package type, validators)", len(outer))
	}
	var pkg uint8
	if err := rlp.DecodeBytes(outer[0], &pkg); err != nil {
		return 0, nil, fmt.Errorf("package type: %w", err)
	}
	var items []rlp.RawValue
	if err := rlp.DecodeBytes(outer[1], &items); err != nil {
		return 0, nil, fmt.Errorf("validator list: %w", err)
	}

	out := make([]decodedValidator, len(items))
	for i, item := range items {
		var fields []rlp.RawValue
		if err := rlp.DecodeBytes(item, &fields); err != nil {
			return 0, nil, fmt.Errorf("validator %d: %w", i, err)
		}
		if len(fields) != 4 && len(fields) != 5 {
			return 0, nil, fmt.Errorf("validator %d has %d fields, want 4 or 5", i, len(fields))
		}
		var d decodedValidator
		if err := rlp.DecodeBytes(fields[0], &d.Consensus); err != nil {
			return 0, nil, fmt.Errorf("validator %d consensus address: %w", i, err)
		}
		if err := rlp.DecodeBytes(fields[1], &d.BSCFee); err != nil {
			return 0, nil, fmt.Errorf("validator %d BBC fee address: %w", i, err)
		}
		if err := rlp.DecodeBytes(fields[2], &d.Fee); err != nil {
			return 0, nil, fmt.Errorf("validator %d fee address: %w", i, err)
		}
		if err := rlp.DecodeBytes(fields[3], &d.Power); err != nil {
			return 0, nil, fmt.Errorf("validator %d voting power: %w", i, err)
		}
		if len(fields) == 5 {
			if err := rlp.DecodeBytes(fields[4], &d.Vote); err != nil {
				return 0, nil, fmt.Errorf("validator %d vote address: %w", i, err)
			}
			if len(d.Vote) != voteAddressLength {
				return 0, nil, fmt.Errorf("validator %d vote address is %d bytes, want %d", i, len(d.Vote), voteAddressLength)
			}
		}
		out[i] = d
	}
	return pkg, out, nil
}
