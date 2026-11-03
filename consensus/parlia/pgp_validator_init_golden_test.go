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

// C1-d: golden-file tests.
//
// The tests in pgp_validator_init_test.go check the encoder against a payload
// string that lives next to them, i.e. Go against a Go-owned constant. That
// catches accidental edits to the encoder, but it cannot catch the deeper
// question: does the payload the node actually sends parse on the real
// Solidity decoder?
//
// A golden file answers both questions because it is a shared artefact:
//
//	Go encoder  --writes-->  testdata/pgp_validators_6.hex  --reads-->  Solidity decoder
//
// The file is copied verbatim into
// bsc-genesis-contract/test/fixtures/pgp_validators_6.hex, where
// test/PGPValidatorInit.t.sol feeds it to BSCValidatorSetTool.decodeInitPayload
// -- the same decodeValidator code path that BSCValidatorSet.init runs. Neither
// side re-implements the other; they meet at the bytes.
//
// Regenerate after an intentional encoder change:
//
//	go test ./consensus/parlia/ -run Golden -update-golden
//
// then copy the file across (or run bsc-genesis-contract/.baseline/check_pgp_fixture.py,
// which fails if the two copies have drifted).

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// goldenUpdate rewrites the golden files in place instead of comparing.
//
// It is a flag rather than an always-on rewrite so that CI (and a plain
// `go test`) fails loudly when the encoder output changes, forcing whoever
// changed it to say so in a commit that also touches the fixture.
var goldenUpdate = flag.Bool("update-golden", false, "rewrite testdata golden files instead of comparing against them")

// goldenFilePGPValidators6 is the six-validator devnet payload shared with the
// contract repo.
const goldenFilePGPValidators6 = "pgp_validators_6.hex"

// goldenFilePGPValidators21 is the mainnet-sized payload (C1-d G6).
//
// The addresses are synthetic -- they are keccak256 of a fixed label, not real
// validators -- because G6 asks a question about *seat count*, not about
// identity: does anything on either side still assume a particular set size?
// The 6-seat fixture answers "are the bytes right"; this one answers "does a
// set three and a half times larger go through the same path".
const goldenFilePGPValidators21 = "pgp_validators_21.hex"

// mainnetValidatorCount is the seat count pgp mainnet is planned to run.
const mainnetValidatorCount = 21

// syntheticValidators returns n deterministic, valid, distinct addresses.
//
// Deterministic matters: the fixture is checked in, so a generator that
// reshuffled on every run would make the golden comparison meaningless.
func syntheticValidators(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		h := crypto.Keccak256([]byte(fmt.Sprintf("pgp-devnet-validator-%02d", i)))
		out[i] = common.BytesToAddress(h[12:]).Hex()
	}
	return out
}

// mainnetLegacyConfig is the 21-seat equivalent of devnetLegacyConfig.
func mainnetLegacyConfig() *params.LegacyConfig {
	addrs := syntheticValidators(mainnetValidatorCount)
	powers := make([]uint64, len(addrs))
	for i := range powers {
		powers[i] = params.DefaultParliaVotingPower
	}
	return &params.LegacyConfig{
		ForkHeight:                1100,
		ParliaGenesisValidators:   addrs,
		ParliaGenesisVotingPowers: powers,
		ParliaGenesisCabinets:     uint64(len(addrs)),
	}
}

// pinnedDevnetPayloadHash pins the payload the whole C1-c/C1-d chain is built
// around.
//
// It is recorded in the design doc and was produced independently by
// bsc-genesis-contract/scripts/gen-pgp-genesis-config.py. If a future change to
// the encoder alters this hash, every node that had already forked at that
// height would compute a different state root, so the hash is a deliberate trip
// wire rather than a redundant assertion: it fails even when the golden file
// has been regenerated to match a broken encoder.
const pinnedDevnetPayloadHash = "0xe18ed4d5bd8063537f924de9a7700ec2343d4c5c48de231bc704d15aed46ce03"

// readGoldenFile loads a hex-encoded golden file, tolerating a 0x prefix, the
// trailing newline an editor adds, and blank lines.
//
// The Solidity side uses vm.parseBytes, which requires the 0x prefix, so the
// file carries one. Here it is optional so the file stays readable to both.
func readGoldenFile(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file %s: %v (regenerate with -update-golden)", path, err)
	}
	text := strings.TrimSpace(string(raw))
	text = strings.TrimPrefix(strings.TrimPrefix(text, "0x"), "0X")
	text = strings.Join(strings.Fields(text), "")
	blob, err := hex.DecodeString(text)
	if err != nil {
		t.Fatalf("golden file %s is not valid hex: %v", path, err)
	}
	return blob
}

// writeGoldenFile stores a payload with a 0x prefix and one trailing newline, so
// that `cmp` against the copy in the contract repo stays byte-exact.
func writeGoldenFile(t *testing.T, name string, payload []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if err := os.WriteFile(path, []byte("0x"+hex.EncodeToString(payload)+"\n"), 0o644); err != nil {
		t.Fatalf("write golden file %s: %v", path, err)
	}
	t.Logf("rewrote %s (%d bytes)", path, len(payload))
}

// TestGoldenPGPValidators6 is the C1-d G1/G2 check: the encoder's output matches
// the fixture that the Solidity decoder test also consumes.
func TestGoldenPGPValidators6(t *testing.T) {
	payload, cabinets, err := pgpValidatorInitArgs(devnetLegacyConfig())
	if err != nil {
		t.Fatalf("pgpValidatorInitArgs: %v", err)
	}
	if cabinets != uint64(len(devnetValidators)) {
		t.Errorf("cabinets = %d, want %d", cabinets, len(devnetValidators))
	}

	if *goldenUpdate {
		writeGoldenFile(t, goldenFilePGPValidators6, payload)
		return
	}

	want := readGoldenFile(t, goldenFilePGPValidators6)
	if !bytes.Equal(payload, want) {
		t.Fatalf("encoder output does not match testdata/%s\n got (%d B): 0x%x\nwant (%d B): 0x%x\n"+
			"if the change is intentional, run: go test ./consensus/parlia/ -run Golden -update-golden, "+
			"then copy the file to bsc-genesis-contract/test/fixtures/",
			goldenFilePGPValidators6, len(payload), payload, len(want), want)
	}
	if len(payload) != 403 {
		t.Errorf("payload length = %d, want 403", len(payload))
	}
}

// TestGoldenPGPValidators21 is C1-d G6: the 21-seat set goes through exactly the
// same encoder as the 6-seat set, and the result still parses.
//
// The failure this guards against is a hardcoded 21 or a hardcoded 6 somewhere in
// the tuple loop, the count byte, or the ABI packing. Those are invisible in a
// single-size test because the wrong constant usually equals the right one.
func TestGoldenPGPValidators21(t *testing.T) {
	cfg := mainnetLegacyConfig()
	payload, cabinets, err := pgpValidatorInitArgs(cfg)
	if err != nil {
		t.Fatalf("pgpValidatorInitArgs: %v", err)
	}
	if cabinets != mainnetValidatorCount {
		t.Errorf("cabinets = %d, want %d", cabinets, mainnetValidatorCount)
	}

	if *goldenUpdate {
		writeGoldenFile(t, goldenFilePGPValidators21, payload)
		return
	}

	want := readGoldenFile(t, goldenFilePGPValidators21)
	if !bytes.Equal(payload, want) {
		t.Fatalf("encoder output does not match testdata/%s\n got (%d B): 0x%x\nwant (%d B): 0x%x",
			goldenFilePGPValidators21, len(payload), payload, len(want), want)
	}

	// 3 (outer list header) + 1 (packageType) + 3 (inner list header) + 21*66.
	// The two headers widen from one byte to two because the body no longer fits
	// in 55 bytes -- that transition is exactly where an off-by-one in the RLP
	// header appears, so the total length is asserted rather than derived.
	const wantLen = 3 + 1 + 3 + mainnetValidatorCount*66
	if len(payload) != wantLen {
		t.Fatalf("payload length = %d, want %d", len(payload), wantLen)
	}
	if payload[0] != 0xf9 || payload[3] != 0x80 {
		t.Fatalf("expected f9 .. .. 80 prefix, got 0x%x", payload[:4])
	}
	t.Logf("21-seat payload keccak256 = 0x%x", crypto.Keccak256(payload))
}

// TestGoldenPGPValidators6Hash pins the payload hash that the design doc and the
// Python reference agree on.
//
// This is separate from the byte comparison above on purpose: the byte
// comparison can be satisfied by regenerating the fixture, whereas this one
// cannot. A deliberate encoder change therefore has to edit this constant as
// well, which is the point where a reviewer sees it.
func TestGoldenPGPValidators6Hash(t *testing.T) {
	payload := readGoldenFile(t, goldenFilePGPValidators6)
	got := "0x" + hex.EncodeToString(crypto.Keccak256(payload))
	if got != pinnedDevnetPayloadHash {
		t.Fatalf("golden payload hash = %s, want %s\n"+
			"the fixture no longer describes the payload recorded in the design doc; "+
			"either the fixture was regenerated from a changed encoder (revert it) "+
			"or the change is intentional (update pinnedDevnetPayloadHash and the doc together)",
			got, pinnedDevnetPayloadHash)
	}
}

// TestGoldenPGPValidators6Order checks that the fixture lists the addresses in
// configuration order.
//
// Order is consensus critical: the payload is built by iterating the config
// slice, and the contract assigns currentValidatorSetMap entries from it, so a
// permutation yields a valid payload, a successful init, and a state root that
// disagrees with every other node. A plain linear search over the payload is
// enough to pin it, because the three address slots inside a tuple are copies of
// the same address for the devnet config.
func TestGoldenPGPValidators6Order(t *testing.T) {
	payload := readGoldenFile(t, goldenFilePGPValidators6)

	prev := -1
	for i, raw := range devnetValidators {
		addr := common.HexToAddress(raw)
		// Skip the RLP string prefix: search for the 20 address bytes only, so
		// the match is independent of how the string header is encoded.
		pos := bytes.Index(payload, addr.Bytes())
		if pos < 0 {
			t.Fatalf("validator %d (%s) is missing from the payload", i, addr.Hex())
		}
		if pos <= prev {
			t.Fatalf("validator %d (%s) appears at offset %d, not after the previous one at %d: "+
				"the payload is not in config order", i, addr.Hex(), pos, prev)
		}
		prev = pos
	}
}

// TestGoldenPGPValidators6InitCalldata reproduces the calldata shape that the
// H-block system transaction carries, without starting a chain.
//
// Measured on the running devnet at ForkHeight=1 with a single validator, the
// transaction to 0x…1000 was 196 bytes: 4 selector + 32 offset + 32 cabinets +
// 32 length + 96 padded payload (71 bytes in one word). The arithmetic below is
// the same shape with six validators, and is what the eventual block-level
// assertion compares against.
func TestGoldenPGPValidators6InitCalldata(t *testing.T) {
	payload := readGoldenFile(t, goldenFilePGPValidators6)

	plan, err := newPGPInitPlan(&params.ChainConfig{ChainID: big.NewInt(8606211), Legacy: devnetLegacyConfig()})
	if err != nil {
		t.Fatalf("newPGPInitPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("newPGPInitPlan returned nil for a forked chain")
	}
	if !bytes.Equal(plan.payload, payload) {
		t.Fatal("the init plan payload differs from the golden fixture")
	}

	// Parse the ABI the same way New() does, so the selector check below runs
	// against what the engine will actually use at ForkHeight.
	vABI, err := abi.JSON(strings.NewReader(validatorSetABI))
	if err != nil {
		t.Fatalf("validatorSetABI does not parse: %v", err)
	}
	if !bytes.Equal(vABI.Methods["init"].ID, pgpValidatorSetInitSelector) {
		t.Fatalf("ABI init selector = 0x%x, want 0x%x", vABI.Methods["init"].ID, pgpValidatorSetInitSelector)
	}
	packed, err := vABI.Pack("init", plan.payload, new(big.Int).SetUint64(plan.cabinets))
	if err != nil {
		t.Fatalf("pack init(bytes,uint256): %v", err)
	}

	if !bytes.Equal(packed[:4], pgpValidatorSetInitSelector) {
		t.Errorf("selector = 0x%x, want 0x%x", packed[:4], pgpValidatorSetInitSelector)
	}

	// 4 + 3 head words + the payload padded up to a 32-byte boundary.
	wantPadded := (len(payload) + 31) / 32 * 32
	wantLen := 4 + 3*32 + wantPadded
	if len(packed) != wantLen {
		t.Fatalf("calldata length = %d, want %d (4 + 3*32 + %d)", len(packed), wantLen, wantPadded)
	}

	// ABI layout: selector | offset-to-bytes (0x40) | cabinets | bytes length | data.
	if off := new(big.Int).SetBytes(packed[4:36]); off.Cmp(big.NewInt(0x40)) != 0 {
		t.Errorf("bytes offset = %#x, want 0x40", off)
	}
	if got := new(big.Int).SetBytes(packed[36:68]); got.Cmp(new(big.Int).SetUint64(plan.cabinets)) != 0 {
		t.Errorf("cabinets word = %s, want %d", got, plan.cabinets)
	}
	if got := new(big.Int).SetBytes(packed[68:100]); got.Cmp(big.NewInt(int64(len(payload)))) != 0 {
		t.Errorf("bytes length word = %s, want %d", got, len(payload))
	}
	if !bytes.HasPrefix(packed[100:], payload) {
		t.Error("the payload is not embedded verbatim after the length word")
	}
	for i := 100 + len(payload); i < len(packed); i++ {
		if packed[i] != 0 {
			t.Fatalf("padding byte at %d is 0x%02x, want 0x00", i, packed[i])
		}
	}
	if len(packed) != 516 {
		t.Errorf("calldata length = %d, want 516 for the six-validator devnet set", len(packed))
	}
}
