// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Package pgp_test is an external test package on purpose: it imports
// core/systemcontracts to compare the image's addresses with the chain's own
// constants, and the deployment code in that package will import core/
// systemcontracts/pgp in turn. An internal test file would make that pair an
// import cycle the day the wiring lands; an external one cannot.
package pgp_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/systemcontracts/pgp"
	"github.com/ethereum/go-ethereum/crypto"
)

// canonicalJSON is the imageHash serialisation (design doc §7.4), written here
// independently of the generator: keys sorted, no whitespace, UTF-8, arrays in
// order, and integers emitted VERBATIM as JSON numbers (unquoted) -- plain
// compact JSON, which is what every stdlib reproduces. Independence is the
// point: if the test reused the generator's or image.go's encoder, a shared
// mistake would agree with itself and prove nothing.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		// json.Number.String() is the decimal text exactly as it appeared;
		// emitting it unquoted is the rule, and quoting it here is the mistake
		// the pinning test below exists to catch.
		return x.String()
	case string:
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatalf("canonicalJSON: %v", err)
		}
		return string(b)
	case []any:
		var out bytes.Buffer
		out.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(canonicalJSON(t, item))
		}
		out.WriteByte(']')
		return out.String()
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out bytes.Buffer
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				t.Fatalf("canonicalJSON: %v", err)
			}
			out.Write(kb)
			out.WriteByte(':')
			out.WriteString(canonicalJSON(t, x[k]))
		}
		out.WriteByte('}')
		return out.String()
	default:
		t.Fatalf("canonicalJSON: unsupported type %T", v)
		return ""
	}
}

// readManifest decodes manifest.json with numbers preserved as text, which is
// what the canonical rule needs.
func readManifest(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatalf("cannot read manifest.json: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	return doc
}

// TestCodeHashesMatchImageFiles is the primary integrity test: the bytes on disk
// hash to what the manifest claims, and decode to the claimed length.
//
// It also enforces two properties of the build output that the generator gates
// on, because a hand-edited image would bypass the generator: no unresolved
// library placeholder, and no ipfs metadata hash (foundry.toml sets
// bytecode_hash='none', so an ipfs marker means a different build).
func TestCodeHashesMatchImageFiles(t *testing.T) {
	entries := pgp.Entries()
	if len(entries) != 17 {
		t.Fatalf("image has %d entries, want 17", len(entries))
	}
	for _, e := range entries {
		hexCode, err := os.ReadFile("image/" + e.Name)
		if err != nil {
			t.Fatalf("%s: cannot read image/%s: %v", e.Name, e.Name, err)
		}
		if bytes.ContainsAny(hexCode, "\r\n") {
			t.Fatalf("%s: image file contains a line break; the format is a single bare hex string", e.Name)
		}
		code, err := hex.DecodeString(string(hexCode))
		if err != nil {
			t.Fatalf("%s: image is not valid hex: %v", e.Name, err)
		}
		if len(code) != e.CodeLen {
			t.Fatalf("%s: image decodes to %d bytes, entry says %d", e.Name, len(code), e.CodeLen)
		}
		if got := crypto.Keccak256Hash(code); got != e.CodeHash {
			t.Fatalf("%s: keccak(code) = %s, entry says %s", e.Name, got, e.CodeHash)
		}
		if bytes.Contains(code, []byte("__")) {
			t.Fatalf("%s: bytecode carries a library link placeholder", e.Name)
		}
		tail := code[len(code)-64:]
		if bytes.Contains(tail, []byte("ipfs")) {
			t.Fatalf("%s: metadata tail carries an ipfs hash; this image is expected to be built with "+
				"bytecode_hash='none'", e.Name)
		}
	}
}

// TestImageHashRecomputes pins imageHash to the canonical encoding of the
// entries array. Provenance is excluded by construction: only entries are
// serialised, so rebuilding the same image from a different git state keeps the
// same hash.
func TestImageHashRecomputes(t *testing.T) {
	doc := readManifest(t)

	entries, ok := doc["entries"].([]any)
	if !ok {
		t.Fatalf("manifest entries is not an array")
	}
	declared, ok := doc["imageHash"].(string)
	if !ok {
		t.Fatalf("manifest imageHash is not a string")
	}

	calc := crypto.Keccak256Hash([]byte(canonicalJSON(t, entries)))
	if calc != common.HexToHash(declared) {
		t.Fatalf("imageHash mismatch: canonical entries hash to %s, manifest says %s", calc, declared)
	}
	if calc != pgp.ImageHash() {
		t.Fatalf("pgp.ImageHash() = %s, canonical recomputation says %s", pgp.ImageHash(), calc)
	}
}

// TestAddressesMatchConstants requires the image's address set to be exactly the
// chain's system-contract constants. A missing or extra account is a deployment
// that either skips an initialization or writes code where nothing expects it.
func TestAddressesMatchConstants(t *testing.T) {
	want := []string{
		systemcontracts.ValidatorContract,
		systemcontracts.SlashContract,
		systemcontracts.SystemRewardContract,
		systemcontracts.LightClientContract,
		systemcontracts.TokenHubContract,
		systemcontracts.RelayerIncentivizeContract,
		systemcontracts.RelayerHubContract,
		systemcontracts.GovHubContract,
		systemcontracts.TokenManagerContract,
		systemcontracts.CrossChainContract,
		systemcontracts.StakingContract,
		systemcontracts.StakeHubContract,
		systemcontracts.StakeCreditContract,
		systemcontracts.GovernorContract,
		systemcontracts.GovTokenContract,
		systemcontracts.TimelockContract,
		systemcontracts.TokenRecoverPortalContract,
	}

	have := make(map[common.Address]string, 17)
	for _, e := range pgp.Entries() {
		have[e.Address] = e.Name
	}
	if len(have) != len(want) {
		t.Fatalf("image has %d distinct addresses, the constant set has %d", len(have), len(want))
	}
	for _, s := range want {
		addr := common.HexToAddress(s)
		if _, ok := have[addr]; !ok {
			t.Fatalf("image is missing %s (%s)", s, addr)
		}
	}
	for addr, name := range have {
		if _, ok := pgp.Code(addr); !ok {
			t.Fatalf("%s: Entries() lists it but Code() does not", name)
		}
	}
}

// TestPaymentLaneIsAbsent keeps a deliberate omission deliberate: bsc-genesis-
// contract has no source for 0x2007, so shipping the unattributed jenner blob
// under this image's provenance would be a lie about where the bytes came from.
func TestPaymentLaneIsAbsent(t *testing.T) {
	addr := common.HexToAddress(systemcontracts.PaymentLaneContract)
	if code, ok := pgp.Code(addr); ok {
		t.Fatalf("image contains 0x2007 PaymentLane (%d bytes); it is excluded on purpose", len(code))
	}
	for _, e := range pgp.Entries() {
		if e.Address == addr {
			t.Fatalf("Entries() lists 0x2007 PaymentLane")
		}
	}
}

// TestEntriesAndCodeAreCopies pins the defensive-copy contract: a caller that
// mutates what it got must not change what the next caller sees.
func TestEntriesAndCodeAreCopies(t *testing.T) {
	first := pgp.Entries()
	if len(first) == 0 {
		t.Fatal("no entries")
	}
	original := first[0].CodeHash
	first[0].Code[0] ^= 0xff

	second := pgp.Entries()
	if second[0].CodeHash != original {
		t.Fatalf("mutating a returned entry changed the image")
	}
	if got := crypto.Keccak256Hash(second[0].Code); got != original {
		t.Fatalf("mutating a returned Code slice changed the stored bytes")
	}

	code, ok := pgp.Code(second[0].Address)
	if !ok {
		t.Fatal("Code() does not know an address it just listed")
	}
	code[0] ^= 0xff
	again, _ := pgp.Code(second[0].Address)
	if got := crypto.Keccak256Hash(again); got != original {
		t.Fatalf("mutating Code()'s return value changed the stored bytes")
	}
}

// TestValidatorBytecodeContainsInitSelector is the F-1 regression, and it is
// deliberately about the BYTECODE rather than the ABI: an ABI that declares
// init(bytes,uint256) is useless if the deployed code does not dispatch it.
//
// 0x63 is PUSH4, so the five-byte sequence 63 1663df00 can only appear where the
// contract compares the incoming selector. Before the fork's ABI fix the
// validator contract dispatched only 63 e1c7392a (init()), and Parlia refused to
// start with "invalid validator set init selector".
//
// The loop then requires the same of every entry the protocol will initialise:
// a deployInit entry whose selector is absent from its own bytecode would fail
// at the deploy height, on chain, in front of every node at once.
func TestValidatorBytecodeContainsInitSelector(t *testing.T) {
	validator := common.HexToAddress(systemcontracts.ValidatorContract)
	code, ok := pgp.Code(validator)
	if !ok {
		t.Fatalf("image has no code for the validator contract %s", validator)
	}
	want := []byte{0x63, 0x16, 0x63, 0xdf, 0x00} // PUSH4 0x1663df00
	if !bytes.Contains(code, want) {
		t.Fatalf("validator bytecode does not dispatch init(bytes,uint256) (0x1663df00); the ABI fix is not " +
			"in this artifact and Parlia will refuse to start")
	}
	old := []byte{0x63, 0xe1, 0xc7, 0x39, 0x2a} // PUSH4 0xe1c7392a = init()
	if bytes.Contains(code, old) {
		t.Logf("validator bytecode also dispatches init() (0xe1c7392a); acceptable as long as init(bytes,uint256) " +
			"is present, which it is")
	}

	for _, e := range pgp.Entries() {
		if !e.DeployInit || e.InitSignature == "" {
			continue
		}
		sel := append([]byte{0x63}, e.InitSelector[:]...)
		if !bytes.Contains(e.Code, sel) {
			t.Fatalf("%s: bytecode does not dispatch %s (%x), so the deploy-time initialization would fail",
				e.Name, e.InitSignature, sel)
		}
	}
}

// TestEntriesAreSorted keeps the documented ordering honest; callers rely on it
// instead of sorting the deployment sequence themselves.
func TestEntriesAreSorted(t *testing.T) {
	entries := pgp.Entries()
	for i := 1; i < len(entries); i++ {
		prev := entries[i-1].Address
		cur := entries[i].Address
		if new(big.Int).SetBytes(prev[:]).Cmp(new(big.Int).SetBytes(cur[:])) >= 0 {
			t.Fatalf("entries are not in ascending address order: %s before %s", prev, cur)
		}
	}
}

// wantImageHash is the imageHash literal: the third opinion.
//
// TestImageHashRecomputes above proves that the encoder and manifest.json agree
// with each other -- which a shared mistake would also satisfy, and did: an
// earlier revision of the comments described "numbers as decimal strings" while
// the code emitted them verbatim, so a reader self-checking against the comment
// would have concluded the opposite of what the bytes said. This constant is not
// derived at test time from anything in the tree: it was produced when the image
// landed and independently cross-checked with `cast keccak`. Any drift -- in the
// generator's rules, in this encoder, or in the entries themselves -- turns red
// here, which is what design doc §7.4 means by "规范形态由钉子测试保证".
const wantImageHash = "0xa8c971b64963244867be207c1ddebad4c4e6a1a214127c9907be92829a1c3484"

// TestImageHashIsPinned breaks the circularity: encoder-vs-manifest, encoder-vs-
// literal, and a negative control proving the literal is not satisfied by
// anything at all.
func TestImageHashIsPinned(t *testing.T) {
	want := common.HexToHash(wantImageHash)

	doc := readManifest(t)
	entries, ok := doc["entries"].([]any)
	if !ok {
		t.Fatalf("manifest entries is not an array")
	}

	// 1. The encoder in this file must produce the pinned value. A failure here
	//    is an implementation drift: the serialisation rule changed under us.
	if got := crypto.Keccak256Hash([]byte(canonicalJSON(t, entries))); got != want {
		t.Fatalf("canonical encoder drifted: it hashes to %s, the pinned imageHash is %s. "+
			"Design doc §7.4 fixes the form (keys sorted, compact, integers verbatim, entries only); "+
			"do not update this constant to match a new encoder without deciding that change explicitly",
			got, want)
	}

	// 2. The manifest's own claim must equal the same literal. A failure here
	//    means manifest.json was edited by hand or regenerated from different
	//    artifacts -- not that the encoder is wrong.
	declared, ok := doc["imageHash"].(string)
	if !ok {
		t.Fatalf("manifest imageHash is not a string")
	}
	if common.HexToHash(declared) != want {
		t.Fatalf("manifest.json declares imageHash %s, but the pinned value is %s: the file was edited or "+
			"regenerated; regenerate with scripts/pgp-image-gen.py instead of hand-editing", declared, want)
	}

	// 3. Negative control: if a one-byte change to an entry still hashed to the
	//    pinned value, the assertion above would be vacuous (a constant-returning
	//    encoder would pass it). Mutating codeLen by one changes the canonical
	//    bytes without needing any keccak of its own.
	if got := crypto.Keccak256Hash([]byte(canonicalJSON(t, decrementCodeLen(t, entries, 0)))); got == want {
		t.Fatalf("pinned imageHash is satisfied by a mutated entry: the canonical encoder or this test is " +
			"vacuous")
	}
}

// decrementCodeLen returns a deep copy of entries with entries[idx].codeLen
// reduced by one, leaving the caller's decoded manifest untouched.
func decrementCodeLen(t *testing.T, entries []any, idx int) []any {
	t.Helper()

	out := make([]any, len(entries))
	copy(out, entries)
	for i, raw := range entries {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("entry %d is not an object", i)
		}
		clone := make(map[string]any, len(m))
		for k, v := range m {
			clone[k] = v
		}
		out[i] = clone
	}
	entry, ok := out[idx].(map[string]any)
	if !ok {
		t.Fatalf("entry %d is not an object", idx)
	}
	num, ok := entry["codeLen"].(json.Number)
	if !ok {
		t.Fatalf("entry %d has no numeric codeLen, so the canonical form under test is not the shipped one", idx)
	}
	n, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		t.Fatalf("entry %d codeLen %q is not an integer: %v", idx, num, err)
	}
	entry["codeLen"] = json.Number(strconv.FormatInt(n-1, 10))
	return out
}

// TestImageProvenanceIsCommitted is the F-04 gate.
//
// The 17 runtime bytecodes are consensus data: every node installs exactly these
// bytes at block D. A manifest that says sourceDirty: true is admitting they
// cannot be rebuilt from any commit, which is what made the image impossible to
// audit independently -- the manifest could only vouch for itself.
//
// If this fails, the workflow is: commit and tag bsc-genesis-contract, check the
// tag out cleanly, and regenerate from that build:
//
//	python3 scripts/pgp-image-gen.py --artifacts <clean-checkout>/out
func TestImageProvenanceIsCommitted(t *testing.T) {
	doc := readManifest(t)

	commit, _ := doc["sourceCommit"].(string)
	if len(commit) != 40 {
		t.Fatalf("sourceCommit = %q, want a 40-character commit id", commit)
	}
	if dirty, _ := doc["sourceDirty"].(bool); dirty {
		t.Fatal("sourceDirty is true: these bytes came from an uncommitted tree and nobody else can rebuild them")
	}
	if tag, _ := doc["sourceTag"].(string); tag == "" {
		t.Fatal("sourceTag is empty: tag the source commit so the image traces back to a release point")
	}
}
