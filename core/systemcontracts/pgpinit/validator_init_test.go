// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// Tests for the consensus-critical validator-set encoder.
//
// The encoder had no tests at all before this file. That is worth stating
// plainly, because the bytes it produces are installed by BSCValidatorSet.init
// at block D and fix the state root from that block onward: an encoder bug does
// not fail a request, it forks the chain or - worse - installs a validator set
// that nobody can correct afterwards (audit F-05).
package pgpinit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// ---------------------------------------------------------------------------
// A hand-rolled RLP encoder.
//
// Deliberately independent of github.com/ethereum/go-ethereum/rlp: the point of
// TestPayloadMatchesIndependentEncoder is to catch a framing mistake in the
// production encoder, and that is impossible if the expectation is produced by
// the same library. These three helpers implement just enough of RLP (strings,
// integers-as-strings, lists) to build the payload by hand.
// ---------------------------------------------------------------------------

func rlpLen(n int, shortOffset byte) []byte {
	if n <= 55 {
		return []byte{shortOffset + byte(n)}
	}
	var lenBytes []byte
	for x := n; x > 0; x >>= 8 {
		lenBytes = append([]byte{byte(x)}, lenBytes...)
	}
	out := []byte{shortOffset + 55 + byte(len(lenBytes))}
	return append(out, lenBytes...)
}

func rlpStr(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpLen(len(b), 0x80), b...)
}

func rlpUint(v uint64) []byte {
	if v == 0 {
		return []byte{0x80}
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	i := 0
	for i < 8 && buf[i] == 0 {
		i++
	}
	return rlpStr(buf[i:])
}

func rlpList(items ...[]byte) []byte {
	var body []byte
	for _, it := range items {
		body = append(body, it...)
	}
	return append(rlpLen(len(body), 0xC0), body...)
}

// encodeByHand builds the expected payload from tuples whose field order is
// written out explicitly, mirroring what BSCValidatorSet.decodeValidator reads
// (idx 0 consensus, idx 1 BBCFee, idx 2 fee, idx 3 voting power).
func encodeByHand(tuples []validatorTuple) []byte {
	items := make([][]byte, 0, len(tuples))
	for _, t := range tuples {
		items = append(items, rlpList(
			rlpStr(t.Consensus.Bytes()),
			rlpStr(t.BSCFee.Bytes()),
			rlpStr(t.Fee.Bytes()),
			rlpUint(t.Power),
		))
	}
	return rlpList(rlpUint(pgpValidatorPackageType), rlpList(items...))
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// threeSeatConfig is the fixture every test shares, so the golden payload below
// refers to exactly these bytes.
//
// Fee addresses are set and differ from the consensus addresses on purpose: when
// parliaGenesisFeeAddrs is empty, ValidatorInitArgs writes the same address into
// slots 0, 1 and 2, and a field-order mistake would then be invisible.
func threeSeatConfig() *params.LegacyConfig {
	return &params.LegacyConfig{
		ParliaGenesisValidators: []string{
			"0x0000000000000000000000000000000000000001",
			"0x0000000000000000000000000000000000000002",
			"0x0000000000000000000000000000000000000003",
		},
		ParliaGenesisFeeAddrs: []string{
			"0x0000000000000000000000000000000000000101",
			"0x0000000000000000000000000000000000000102",
			"0x0000000000000000000000000000000000000103",
		},
		ParliaGenesisVotingPowers: []uint64{7, 8, 9},
		ParliaGenesisCabinets:     3,
	}
}

func seatConfig(n int) *params.LegacyConfig {
	cfg := &params.LegacyConfig{ParliaGenesisCabinets: uint64(n)}
	for i := 1; i <= n; i++ {
		cfg.ParliaGenesisValidators = append(cfg.ParliaGenesisValidators,
			fmt.Sprintf("0x%040x", i))
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestPayloadMatchesIndependentEncoder is the cross-implementation check: the
// expectation comes from encodeByHand, not from geth's rlp package.
func TestPayloadMatchesIndependentEncoder(t *testing.T) {
	payload, cabinets, err := ValidatorInitArgs(threeSeatConfig())
	if err != nil {
		t.Fatalf("ValidatorInitArgs: %v", err)
	}
	if cabinets != 3 {
		t.Fatalf("cabinets = %d, want 3", cabinets)
	}

	// Slots 0 and 1 both carry the consensus address (BSCFee defaults to it),
	// slot 2 carries the configured fee address. Written out by hand so a
	// reordering of validatorTuple is a test failure rather than a silent
	// state-root change.
	want := encodeByHand([]validatorTuple{
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000101"), Power: 7},
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000102"), Power: 8},
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000003"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000003"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000103"), Power: 9},
	})
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload does not match the hand-rolled encoder\n got: %x\nwant: %x", payload, want)
	}

	// And the comparison has teeth: swapping the two fee slots must not produce
	// the same bytes. Without this, a test that compares two encoders which both
	// emit the same wrong order would still pass.
	swapped := encodeByHand([]validatorTuple{
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000101"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000001"), Power: 7},
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000102"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000002"), Power: 8},
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000003"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000103"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000003"), Power: 9},
	})
	if bytes.Equal(payload, swapped) {
		t.Fatal("a tuple with the fee slots swapped produces the same payload; the test cannot detect field-order mistakes")
	}
	if len(payload) != len(swapped) {
		t.Fatalf("swapping two 20-byte fields changed the payload length (%d vs %d); the check above is not comparing like with like",
			len(payload), len(swapped))
	}
}

// TestValidatorsTruncationIsWellFormed pins the F-05 failure mode itself: a
// payload describing two validators is a perfectly valid payload. Nothing in the
// bytes says a third one was dropped, which is why the contract cannot tell
// "the config has two" from "the config had three and one was truncated".
func TestValidatorsTruncationIsWellFormed(t *testing.T) {
	two := encodeByHand([]validatorTuple{
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000101"), Power: 7},
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000102"), Power: 8},
	})

	pkg, tuples, err := decodeValidatorInitPayload(two)
	if err != nil {
		t.Fatalf("a two-validator payload must decode without error, got: %v", err)
	}
	if pkg != 0 {
		t.Fatalf("package type = %d, want 0", pkg)
	}
	if len(tuples) != 2 {
		t.Fatalf("decoded %d validators, want 2", len(tuples))
	}

	// Same payload built by the production encoder for a genuine two-seat
	// config: byte-identical. There is no marker, checksum or count that
	// distinguishes "intentionally two" from "truncated from three".
	twoSeat, _, err := ValidatorInitArgs(seatConfig(2))
	if err != nil {
		t.Fatalf("ValidatorInitArgs(2 seats): %v", err)
	}
	if !bytes.Equal(twoSeat, encodeByHand([]validatorTuple{
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000001"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000001"), Power: params.DefaultParliaVotingPower},
		{Consensus: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			BSCFee: common.HexToAddress("0x0000000000000000000000000000000000000002"),
			Fee:    common.HexToAddress("0x0000000000000000000000000000000000000002"), Power: params.DefaultParliaVotingPower},
	})) {
		t.Fatal("a real two-seat payload differs from the truncated one; the two cases are distinguishable after all")
	}
}

// TestValidatorInitArgsRoundTrip exercises the sizes that matter: the smallest
// legal set, the size both devnet and mainnet use, a full-cabinet set, and the
// 255 limit imposed by the header count byte.
func TestValidatorInitArgsRoundTrip(t *testing.T) {
	for _, n := range []int{1, 3, 21, 255} {
		t.Run(fmt.Sprintf("%d seats", n), func(t *testing.T) {
			cfg := seatConfig(n)
			payload, cabinets, err := ValidatorInitArgs(cfg)
			if err != nil {
				t.Fatalf("ValidatorInitArgs: %v", err)
			}
			if cabinets != uint64(n) {
				t.Fatalf("cabinets = %d, want %d (ParliaGenesisCabinets=0 means one seat each)", cabinets, n)
			}
			pkg, tuples, err := decodeValidatorInitPayload(payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pkg != pgpValidatorPackageType {
				t.Fatalf("package type = %d, want %d", pkg, pgpValidatorPackageType)
			}
			if len(tuples) != n {
				t.Fatalf("decoded %d validators, want %d", len(tuples), n)
			}
			// Length is fully determined by the seat count: 0x80 + list header
			// for the package type, then N*(4 fields of 21 bytes + power).
			//
			// Not asserting an exact formula here - the 1-byte integer encoding
			// of a voting power below 0x80 makes it depend on the values. What
			// matters is that the framing is self-consistent, which decode
			// already proved by rejecting trailing bytes.
		})
	}

	t.Run("256 seats is rejected", func(t *testing.T) {
		if _, _, err := ValidatorInitArgs(seatConfig(256)); err == nil {
			t.Fatal("256 validators must be rejected: the header count byte is a uint8")
		}
	})

	t.Run("0 seats is rejected", func(t *testing.T) {
		if _, _, err := ValidatorInitArgs(&params.LegacyConfig{}); err == nil {
			t.Fatal("an empty validator set must be rejected")
		}
	})
}

// TestValidatorInitArgsGoldenPayload is a tripwire, not an external authority.
//
// If this fails, the bytes injected at block D changed, which means the state
// root at D changed. That may be intentional - but then it has to be re-derived
// and re-pinned in docs/design/pgp-system-contract-deploy-design.md, and every
// node has to be upgraded together. Failing here is the cheap version of that
// discovery.
func TestValidatorInitArgsGoldenPayload(t *testing.T) {
	const (
		// 3 seats, each a 4-tuple of 20-byte addresses and a small voting power:
		// 64 bytes of body per tuple (21+21+21+1) -> long-form 2-byte header -> 66;
		// three of them = 198 -> inner list header 2 bytes = 200; plus the 0x80
		// package type = 201 -> outer header 2 bytes = 203.
		wantLen  = 203
		wantHash = "0x61df20405d6542868e27f3c2fb592f035e60eaab41077839a5a9e04b3ae86355"
	)
	payload, _, err := ValidatorInitArgs(threeSeatConfig())
	if err != nil {
		t.Fatalf("ValidatorInitArgs: %v", err)
	}
	gotHash := fmt.Sprintf("0x%x", crypto.Keccak256(payload))
	if len(payload) != wantLen {
		t.Fatalf("payload length = %d, want %d", len(payload), wantLen)
	}
	if gotHash != wantHash {
		t.Fatalf("payload hash = %s, want %s\npayload: %x", gotHash, wantHash, payload)
	}
}
