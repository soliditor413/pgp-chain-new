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

package vm

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// pgp0x3F1Golden is the expected return data of one real 0x3f1 call: the single
// recharge carried by ELA main chain block 30115.
//
//	address     0xa20f5A22eF423b0e5c2Db79A5475D9512d989971
//	targetAmount 10,150,000 = 0x9ae070
//	fee          150,000    = 0x249f0
//	targetData   empty       -> 0x00
//
// Six words, 192 bytes:
//
//	word0  count = 1
//	word1  targetAddress
//	word2  targetAmount
//	word3  fee
//	word4  offset to targetData = 0xa0 (5 head words)
//	word5  length of targetData = 0
//
// It is a hex literal, not a re-encoding, on purpose: re-encoding the same values
// with the same library would pass even if the shape were wrong.
const pgp0x3F1Golden = "" +
	"0000000000000000000000000000000000000000000000000000000000000001" + // count
	"000000000000000000000000a20f5a22ef423b0e5c2db79a5475d9512d989971" + // targetAddress
	"00000000000000000000000000000000000000000000000000000000009ae070" + // targetAmount
	"00000000000000000000000000000000000000000000000000000000000249f0" + // fee
	"00000000000000000000000000000000000000000000000000000000000000a0" + // offset
	"0000000000000000000000000000000000000000000000000000000000000000" // length

// pgp0x3F1GoldenData is the input that must produce pgp0x3F1Golden.
func pgp0x3F1GoldenData() []PGPRechargeData {
	return []PGPRechargeData{{
		TargetAddress: common.HexToAddress("0xa20f5A22eF423b0e5c2Db79A5475D9512d989971"),
		TargetAmount:  big.NewInt(10150000),
		Fee:           big.NewInt(150000),
		TargetData:    nil,
	}}
}

// pgpWord slices word i out of ABI output. It fails rather than panics so a
// length regression shows up as the real problem instead of an index error.
func pgpWord(t *testing.T, out []byte, i int) []byte {
	t.Helper()
	if len(out) < (i+1)*32 {
		t.Fatalf("encoded output is %d bytes, too short to hold word %d", len(out), i)
	}
	return out[i*32 : (i+1)*32]
}

// TestPGP0x3F1EncodingGolden byte-compares the encoder against the observed
// 192-byte return data, then re-checks the two numbers as integers so a typo in
// the hex literal above reports the value instead of just "bytes differ".
func TestPGP0x3F1EncodingGolden(t *testing.T) {
	want, err := hex.DecodeString(pgp0x3F1Golden)
	if err != nil {
		t.Fatalf("golden vector is not valid hex: %v", err)
	}
	if len(want) != 192 {
		t.Fatalf("golden vector is %d bytes, want 192", len(want))
	}

	data := pgp0x3F1GoldenData()
	got, err := EncodePGPRechargeDatas(data)
	if err != nil {
		t.Fatalf("EncodePGPRechargeDatas: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("encoded length is %d bytes, want %d\n got %x\nwant %x", len(got), len(want), got, want)
	}
	if !bytes.Equal(got, want) {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("first differing byte at offset %d: got 0x%02x, want 0x%02x\n got %x\nwant %x",
					i, got[i], want[i], got, want)
			}
		}
	}

	// The numbers, spelled out in decimal, so the literal above is checked
	// against block 30115 rather than against itself.
	if n := new(big.Int).SetBytes(pgpWord(t, got, 0)); n.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("count word = %s, want 1", n)
	}
	if addr := common.BytesToAddress(pgpWord(t, got, 1)); addr != data[0].TargetAddress {
		t.Errorf("targetAddress word = %s, want %s", addr.Hex(), data[0].TargetAddress.Hex())
	}
	if n := new(big.Int).SetBytes(pgpWord(t, got, 2)); n.Cmp(big.NewInt(10150000)) != 0 {
		t.Errorf("targetAmount word = %s, want 10150000", n)
	}
	if n := new(big.Int).SetBytes(pgpWord(t, got, 3)); n.Cmp(big.NewInt(150000)) != 0 {
		t.Errorf("fee word = %s, want 150000", n)
	}
	if n := new(big.Int).SetBytes(pgpWord(t, got, 4)); n.Cmp(big.NewInt(0xa0)) != 0 {
		t.Errorf("targetData offset word = %s, want 0xa0", n)
	}
	// An empty targetData still occupies its length word. A caller that found no
	// word here would read past the head, so this is not dead padding.
	if n := new(big.Int).SetBytes(pgpWord(t, got, 5)); n.Sign() != 0 {
		t.Errorf("empty targetData length word = %s, want 0", n)
	}
}

// TestPGP0x3F1EmptyReturns32ZeroBytes pins the difference between "no recharge"
// and "no return data".
//
// pgp packs a single uint256 count even when the slice is empty
// (pgp-chain/core/vm/contracts.go:1556-1557), so the result is 32 zero bytes.
// Returning nil instead would make a Solidity caller's abi.decode read zero
// returndata and revert, turning an ordinary "this tx recharged nothing" into a
// failed transaction.
func TestPGP0x3F1EmptyReturns32ZeroBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []PGPRechargeData
	}{
		{"nil", nil},
		{"empty", []PGPRechargeData{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodePGPRechargeDatas(tc.in)
			if err != nil {
				t.Fatalf("EncodePGPRechargeDatas: %v", err)
			}
			if len(got) != 32 {
				t.Fatalf("empty input encoded to %d bytes, want 32: %x", len(got), got)
			}
			if !bytes.Equal(got, make([]byte, 32)) {
				t.Fatalf("empty input encoded to %x, want 32 zero bytes", got)
			}
		})
	}
}

// TestPGP0x3F1EncodingShape is the discriminator between the flat argument list
// pgp uses and the tidy abi.encode(RechargeData[]) it looks like.
//
// The two differ in word 0 (count vs 0x20 offset to the array) and in total
// length, and a port that "cleans this up" to the array form compiles, looks
// right, and mis-decodes every field on the ELA side. So the shape is asserted
// explicitly rather than being implied by the golden vector.
func TestPGP0x3F1EncodingShape(t *testing.T) {
	two := []PGPRechargeData{
		{
			TargetAddress: common.HexToAddress("0x00000000000000000000000000000000000000aa"),
			TargetAmount:  big.NewInt(1),
			Fee:           big.NewInt(2),
			TargetData:    nil,
		},
		{
			TargetAddress: common.HexToAddress("0x00000000000000000000000000000000000000bb"),
			TargetAmount:  big.NewInt(3),
			Fee:           big.NewInt(4),
			// 33 bytes: one full word plus one byte, so the length is not a
			// multiple of 32 and the rounding has to happen.
			TargetData: bytes.Repeat([]byte{0xcc}, 33),
		},
	}

	got, err := EncodePGPRechargeDatas(two)
	if err != nil {
		t.Fatalf("EncodePGPRechargeDatas: %v", err)
	}

	// Head: 1 count word + 2 * 4 field words = 9 words = 288 bytes.
	// Dynamic section: element 0 contributes 32 (empty length word),
	// element 1 contributes 32 (length) + 64 (33 bytes rounded to two words).
	const wantLen = 288 + 32 + 96
	if len(got) != wantLen {
		t.Fatalf("two-element encoding is %d bytes, want %d: %x", len(got), wantLen, got)
	}
	if n := new(big.Int).SetBytes(pgpWord(t, got, 0)); n.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("word0 = %s, want 2 (the count); 0x20 here would mean abi.encode(T[])", n)
	}
	// Element 0's bytes offset: head is 9 words and element 0's empty payload
	// sits right after it.
	if n := new(big.Int).SetBytes(pgpWord(t, got, 4)); n.Cmp(big.NewInt(9*32)) != 0 {
		t.Errorf("element0 targetData offset = %s, want %d (9 head words)", n, 9*32)
	}
	if n := new(big.Int).SetBytes(pgpWord(t, got, 8)); n.Cmp(big.NewInt(10*32)) != 0 {
		t.Errorf("element1 targetData offset = %s, want %d", n, 10*32)
	}
	if n := new(big.Int).SetBytes(pgpWord(t, got, 10)); n.Cmp(big.NewInt(33)) != 0 {
		t.Errorf("element1 targetData length = %s, want 33", n)
	}
	// The payload itself: 33 * 0xcc followed by 31 bytes of right padding.
	body := got[11*32:]
	if !bytes.Equal(body[:33], bytes.Repeat([]byte{0xcc}, 33)) {
		t.Errorf("element1 targetData payload = %x, want 33 bytes of 0xcc", body[:33])
	}
	if !bytes.Equal(body[33:], make([]byte, 31)) {
		t.Errorf("element1 targetData padding = %x, want 31 zero bytes", body[33:])
	}

	// The empty-payload element still gets its length word, at offset 288.
	if !bytes.Equal(got[9*32:10*32], make([]byte, 32)) {
		t.Errorf("element0 empty targetData = %x, want a zero length word", got[9*32:10*32])
	}
}

// TestPGP0x3F1EncodingRejectsBadAmounts covers the three inputs that would
// otherwise reach reflect or the mint path as a surprise.
//
// A nil *big.Int is not a zero amount: abi.Pack dereferences it and panics, and a
// panic inside EVM execution is far harder to attribute than an error. A negative
// amount cannot be expressed as uint256 at all.
func TestPGP0x3F1EncodingRejectsBadAmounts(t *testing.T) {
	addr := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	for _, tc := range []struct {
		name string
		data PGPRechargeData
		want error
	}{
		{"nil targetAmount", PGPRechargeData{TargetAddress: addr, Fee: big.NewInt(1)}, ErrPGPRechargeNilAmount},
		{"nil fee", PGPRechargeData{TargetAddress: addr, TargetAmount: big.NewInt(1)}, ErrPGPRechargeNilFee},
		{"negative targetAmount", PGPRechargeData{TargetAddress: addr, TargetAmount: big.NewInt(-1), Fee: big.NewInt(1)}, ErrPGPRechargeNegative},
		{"negative fee", PGPRechargeData{TargetAddress: addr, TargetAmount: big.NewInt(1), Fee: big.NewInt(-1)}, ErrPGPRechargeNegative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EncodePGPRechargeDatas([]PGPRechargeData{tc.data})
			if !errors.Is(err, tc.want) {
				t.Fatalf("EncodePGPRechargeDatas returned (%x, %v), want error %v", out, err, tc.want)
			}
			if out != nil {
				t.Fatalf("failed encode still returned %d bytes", len(out))
			}
		})
	}
}
