// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package params

import (
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// rechargeCalldata builds a Recharge(bytes32,bytes) call the way a wallet would:
// the selector, then the bytes32 hash, then the offset/length of the dynamic
// second argument. Only the first 36 bytes matter to LegacyRechargeElaHash, but
// the call has to be shaped like the real thing so the fixture cannot pass by
// accident on a truncated input.
func rechargeCalldata(elaHash string) []byte {
	raw, err := hex.DecodeString(elaHash)
	if err != nil || len(raw) != 32 {
		panic("rechargeCalldata needs a 32 byte hex hash")
	}
	input := make([]byte, 0, 4+32+32+32)
	input = append(input, elaMinterRechargeSelector...)
	input = append(input, raw...)
	// offset of the bytes argument (0x40) and its length (0) - never read here,
	// present so the fixture is a well formed call.
	input = append(input, make([]byte, 32)...)
	input[len(input)-2] = 0x00
	input[len(input)-1] = 0x40
	input = append(input, make([]byte, 32)...)
	return input
}

const testElaHash = "1111111111111111111111111111111111111111111111111111111111111111"

// TestLegacyRechargeElaHash pins the extraction of the first ABI argument.
//
// It must match pgp-chain's IsCompletedByTxInput, which only understands
// Recharge and answers "not completed" for everything else - refundWithdraw
// included (pgp-chain/spv/elaminter.go:86-96). If this ever started returning a
// hash for refundWithdraw, the completion check would begin rejecting gateway
// calls pgp-chain accepted.
func TestLegacyRechargeElaHash(t *testing.T) {
	hash, ok := LegacyRechargeElaHash(rechargeCalldata(testElaHash))
	if !ok {
		t.Fatal("a Recharge call must yield its ELA hash")
	}
	if want := "0x" + testElaHash; hash != want {
		t.Fatalf("ELA hash mismatch: have %s, want %s", hash, want)
	}

	// refundWithdraw carries a hash too, but pgp-chain never looked it up there.
	refund := append(append([]byte{}, elaMinterRefundWithdrawSelector...), make([]byte, 32)...)
	if hash, ok := LegacyRechargeElaHash(refund); ok || hash != "" {
		t.Fatalf("refundWithdraw must not be treated as a recharge lookup, got (%q, %v)", hash, ok)
	}

	// Truncated input: a selector with no room for the bytes32 argument.
	if hash, ok := LegacyRechargeElaHash(rechargeCalldata(testElaHash)[:35]); ok || hash != "" {
		t.Fatalf("a truncated Recharge must not decode, got (%q, %v)", hash, ok)
	}
	if _, ok := LegacyRechargeElaHash(nil); ok {
		t.Fatal("nil input must not decode")
	}
}

// TestCompletedSlot pins the storage key of ELAMinter's completed mapping.
//
// Everything downstream - the "is this recharge already settled" rule, and with
// it whether a replayed block is accepted - hinges on this one keccak. A wrong
// slot does not fail loudly: it reads an always-zero slot, every recharge looks
// unsettled, and the chain silently accepts transactions pgp-chain rejected.
// Hence golden values rather than a re-derivation in the test.
//
// The slot is 1 and the argument goes first, both read out of the deployed
// bytecode (params/pgp_contracts.go, ELAMINTER_CODE):
//
//	getter  completed(bytes32)    0x0182  PUSH1 1 PUSH1 0x20 MSTORE ... SLOAD
//	Recharge(bytes32,bytes)       0x04a5  PUSH1 1 PUSH1 0x20 MSTORE ... SSTORE
//	refundWithdraw(bytes32)       0x0905  PUSH1 1 PUSH1 0x20 MSTORE ... SSTORE
//
// The argument is stored at memory 0x00 and the slot at 0x20, so the hash is
// keccak256(arg . slot) - argument first, exactly as solidity does it.
func TestCompletedSlot(t *testing.T) {
	// Direction check, independent of the golden values below: the canonical
	// solidity vector for a mapping read. keccak256(abi.encode(uint256(0))) is
	// the well known 0x290decd9..., and keccak256 of 64 zero bytes (key 0, slot
	// 0) is 0xad3228b6.... Both must hold before any of the golden values can be
	// trusted, because they prove the implementation is real Keccak-256 and that
	// a 64 byte input is laid out key-first.
	if got, want := crypto.Keccak256Hash(make([]byte, 32)),
		common.HexToHash("0x290decd9548b62a8d60345a988386fc84ba6bc95484008f6362f93160ef3e563"); got != want {
		t.Fatalf("keccak self check on 32 zero bytes failed: have %s, want %s", got, want)
	}
	if got, want := crypto.Keccak256Hash(make([]byte, 64)),
		common.HexToHash("0xad3228b676f7d3cd4284a5443f17f1962b36e491b30a40b2405849e597ba5fb5"); got != want {
		t.Fatalf("keccak self check on 64 zero bytes failed: have %s, want %s", got, want)
	}

	for _, tc := range []struct{ name, elaHash, slot string }{
		{
			name:    "all zero hash",
			elaHash: "0x0000000000000000000000000000000000000000000000000000000000000000",
			slot:    "0xa6eef7e35abe7026729641147f7915573c7e97b47efa546f5f6e3230263bcb49",
		},
		{
			name:    "repeated 0x11 hash",
			elaHash: "0x1111111111111111111111111111111111111111111111111111111111111111",
			slot:    "0x7deb3b60ec0f1bf56dbdd0ffedbadafddeaa08947884ff0f215ce93ee1826102",
		},
		{
			name:    "mixed hash",
			elaHash: "0x7841c0f6cd8f2fb1ac1e4d0ecf2b0e9b8d24c0a7b3c5c9c6f0d1e2a3b4c5d6e7",
			slot:    "0x4efcd1aff5c730be442fdc937b7c3980aefeae32f4245c0f7479a2aab6af44ec",
		},
	} {
		if got := CompletedSlot(common.HexToHash(tc.elaHash)); got != common.HexToHash(tc.slot) {
			t.Errorf("%s: CompletedSlot(%s) = %s, want %s", tc.name, tc.elaHash, got, tc.slot)
		}
	}

	// The reversed layout (slot first, hash second) is a real trap: it produces a
	// perfectly plausible looking keccak that reads an empty slot. Guard against
	// it explicitly so nobody "simplifies" the derivation into it.
	var reversed [64]byte
	reversed[31] = byte(completedMappingSlot)
	copy(reversed[32:], common.HexToHash("0x"+testElaHash).Bytes())
	if got, wrong := CompletedSlot(common.HexToHash("0x"+testElaHash)), crypto.Keccak256Hash(reversed[:]); got == wrong {
		t.Fatalf("CompletedSlot hashes the slot before the argument (%s); the contract hashes arg . slot", got)
	}

	// The mapping is a bool, so a settled hash reads 0x00..01 and an untouched
	// slot reads all zero. Anything else would never compare equal.
	if CompletedSetValue != (common.Hash{31: 1}) {
		t.Fatalf("CompletedSetValue must be 0x00..01, got %s", CompletedSetValue)
	}
}
