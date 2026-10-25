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

package pgp

// Golden tests over recorded pgp mainnet headers.
//
// The synthetic tests in confirm_test.go prove the rules reject what they
// should, but they cannot prove the rules accept what the chain actually
// produced: they sign with keys the test invents. These tests run the same code
// over twelve real headers, so a change that is self consistent but wrong -
// a flipped byte order, a wrong vote threshold, a mistyped producer key - shows
// up as a rejected mainnet block.
//
// The vectors were extracted from a synced pgp mainnet datadir and cross checked
// through two independent implementations: one rebuilt types.Header from these
// bytes and compared Hash() against the canonical block hash, the other
// re-derived the seal hash with a hand written RLP encoder that never touches
// this package's types. All 96 votes (eight per block) were also verified
// individually with pgp-chain's own ELA crypto, each with a flip-one-byte
// negative control that had to fail.
//
// The blocks cover every nonce step the chain ever took, because header.Nonce is
// the ELA height the block was sealed against and it selects which producer set
// the confirm is checked against:
//
//	1 .. 16404        nonce 0          the configured producers  (five samples)
//	16405 .. 19160    nonce 2076195    the ELA arbiters of 2076195 (two samples)
//	19161 .. 79458    nonce 2076267    the ELA arbiters of 2076267 (five samples)
//
// Two of the samples carry a non-zero proposal view offset (block 1 has 1, block
// 16405 has 2). Those are real view changes, which is what makes the fork guard
// testable against the chain rather than only against synthetic data.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	elaCrypto "github.com/elastos/Elastos.ELA/crypto"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// sealVectorsDigest is sha256 over the canonical JSON of the "vectors" array,
// i.e. keys sorted and no insignificant whitespace:
//
//	python3 -c 'import json,hashlib;v=json.load(open("testdata/seal_vectors.json"))["vectors"];print(hashlib.sha256(json.dumps(v,sort_keys=True,separators=(",",":")).encode()).hexdigest())'
//
// Go's encoding/json reproduces it exactly for a decoded map[string]any, because
// it sorts map keys and emits compact output.
const sealVectorsDigest = "81619f5aeaeb64513624354353c5c42059cdbf1dc23c52d1523100337b66d033"

// goldenHeader is one recorded header. Only the seal fields are kept: these are
// exactly the fourteen that SealHash commits to, plus Extra.
type goldenHeader struct {
	ParentHash  string `json:"parentHash"`
	UncleHash   string `json:"uncleHash"`
	Coinbase    string `json:"coinbase"`
	Root        string `json:"root"`
	TxHash      string `json:"txHash"`
	ReceiptHash string `json:"receiptHash"`
	Bloom       string `json:"bloom"`
	Difficulty  string `json:"difficulty"`
	GasLimit    string `json:"gasLimit"`
	GasUsed     string `json:"gasUsed"`
	Time        uint64 `json:"time"`
	MixDigest   string `json:"mixDigest"`
	Nonce       string `json:"nonce"`
	Extra       string `json:"extra"`
}

type goldenVote struct {
	ProposalHash string `json:"proposalHash"`
	Signer       string `json:"signer"`
	Accept       bool   `json:"accept"`
	Sign         string `json:"sign"`
}

type goldenConfirm struct {
	VoteCount int `json:"voteCount"`
	Proposal  struct {
		Sponsor    string `json:"sponsor"`
		BlockHash  string `json:"blockHash"`
		ViewOffset uint32 `json:"viewOffset"`
		Sign       string `json:"sign"`
	} `json:"proposal"`
	Votes []goldenVote `json:"votes"`
}

type goldenVector struct {
	Number    uint64        `json:"number"`
	BlockHash string        `json:"blockHash"`
	Header    goldenHeader  `json:"header"`
	Confirm   goldenConfirm `json:"confirm"`
}

// loadGoldenVectors reads testdata/seal_vectors.json. Only the "vectors" key is
// mapped; the explanatory note keys are documentation and are free to change.
func loadGoldenVectors(t *testing.T) []goldenVector {
	t.Helper()

	raw, err := os.ReadFile("testdata/seal_vectors.json")
	if err != nil {
		t.Fatalf("read the seal vectors: %v", err)
	}
	var doc struct {
		Vectors []goldenVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the seal vectors: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("the seal vector file carries no vectors")
	}
	return doc.Vectors
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()

	raw := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	b, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatalf("not hex: %q: %v", s, err)
	}
	return b
}

func mustHexUint64(t *testing.T, s string) uint64 {
	t.Helper()

	raw := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, err := strconv.ParseUint(raw, 16, 64)
	if err != nil {
		t.Fatalf("not a hex integer: %q: %v", s, err)
	}
	return v
}

func mustHexBig(t *testing.T, s string) *big.Int {
	t.Helper()

	raw := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	v, ok := new(big.Int).SetString(raw, 16)
	if !ok {
		t.Fatalf("not a hex integer: %q", s)
	}
	return v
}

// header rebuilds the recorded header and checks that it hashes back to the
// recorded block hash.
//
// That check is the cold start of every test in this file. If it fails the
// vectors are incomplete or misparsed, and any assertion built on top of them
// would be measuring the test rather than the code, so it fails loudly and first
// instead of letting a later, more confusing assertion take the blame.
func (v goldenVector) header(t *testing.T) *types.Header {
	t.Helper()

	nonce := mustHex(t, v.Header.Nonce)
	if len(nonce) != len(types.BlockNonce{}) {
		t.Fatalf("block %d: nonce is %d bytes, want %d", v.Number, len(nonce), len(types.BlockNonce{}))
	}
	h := &types.Header{
		ParentHash:  common.HexToHash(v.Header.ParentHash),
		UncleHash:   common.HexToHash(v.Header.UncleHash),
		Coinbase:    common.HexToAddress(v.Header.Coinbase),
		Root:        common.HexToHash(v.Header.Root),
		TxHash:      common.HexToHash(v.Header.TxHash),
		ReceiptHash: common.HexToHash(v.Header.ReceiptHash),
		Bloom:       types.BytesToBloom(mustHex(t, v.Header.Bloom)),
		Difficulty:  mustHexBig(t, v.Header.Difficulty),
		Number:      new(big.Int).SetUint64(v.Number),
		GasLimit:    mustHexUint64(t, v.Header.GasLimit),
		GasUsed:     mustHexUint64(t, v.Header.GasUsed),
		Time:        v.Header.Time,
		MixDigest:   common.HexToHash(v.Header.MixDigest),
		Extra:       mustHex(t, v.Header.Extra),
	}
	copy(h.Nonce[:], nonce)

	if want := common.HexToHash(v.BlockHash); h.Hash() != want {
		t.Fatalf("block %d: rebuilt header hashes to %s, want %s; the vector file does not "+
			"describe the header the rest of this test assumes, so nothing below it can be trusted",
			v.Number, h.Hash(), want)
	}
	return h
}

// decodeGoldenConfirm decodes the recorded confirm from the rebuilt header.
func (v goldenVector) decodeGoldenConfirm(t *testing.T, header *types.Header) *payload.Confirm {
	t.Helper()

	confirm, err := decodeConfirm(header.Extra)
	if err != nil {
		t.Fatalf("block %d: decodeConfirm rejected a mainnet header: %v", v.Number, err)
	}
	return confirm
}

// TestSealVectorsDigestMatchesTheAuditedData guards the provenance of the golden
// data, not the code.
//
// The vectors were cross checked by two independent implementations and the
// generator was lost afterwards, so a silent edit to a hex digit would be
// unrecoverable and would quietly weaken every other test in this file. Rewording
// the explanatory notes stays allowed because the digest covers "vectors" only.
func TestSealVectorsDigestMatchesTheAuditedData(t *testing.T) {
	raw, err := os.ReadFile("testdata/seal_vectors.json")
	if err != nil {
		t.Fatalf("read the seal vectors: %v", err)
	}
	var doc struct {
		Vectors json.RawMessage `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the seal vectors: %v", err)
	}

	// UseNumber keeps every numeric field as its literal digits. Decoding into
	// float64 would re-encode through strconv and silently change any value that
	// is not exactly representable, which is how a fingerprint test turns into a
	// flaky one. The current data has no such value, but a future vector might.
	dec := json.NewDecoder(bytes.NewReader(doc.Vectors))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		t.Fatalf("decode the vectors: %v", err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("re-encode the vectors: %v", err)
	}

	if got := fmt.Sprintf("%x", sha256.Sum256(canonical)); got != sealVectorsDigest {
		t.Fatalf("the golden data changed:\n  have %s\n  want %s\n\n"+
			"The explanatory notes may be reworded freely, but the vectors themselves are golden "+
			"data. If the change is deliberate, for instance a new vector was added, recompute the "+
			"digest with the command in this file's header comment and update both this constant "+
			"and the one recorded in the file's note field.", got, sealVectorsDigest)
	}
}

// TestGoldenConfirmDecodesRealMainnetExtra proves the wire format is right.
//
// Every rule downstream assumes Extra is one serialized payload.Confirm, 1199
// bytes for these blocks. If the layout drifts, the confirm would decode into
// something plausible but wrong and the signature checks would fail for a reason
// that looks like a bad block.
func TestGoldenConfirmDecodesRealMainnetExtra(t *testing.T) {
	for _, v := range loadGoldenVectors(t) {
		header := v.header(t)

		if len(header.Extra) != 1199 {
			t.Fatalf("block %d: Extra is %d bytes, want 1199", v.Number, len(header.Extra))
		}
		confirm := v.decodeGoldenConfirm(t, header)

		if len(confirm.Votes) != v.Confirm.VoteCount {
			t.Fatalf("block %d: decoded %d votes, the file records %d", v.Number, len(confirm.Votes), v.Confirm.VoteCount)
		}
		if len(confirm.Votes) != 8 {
			t.Fatalf("block %d: %d votes, every mainnet confirm carries eight", v.Number, len(confirm.Votes))
		}
		if got, want := confirm.Proposal.ViewOffset, v.Confirm.Proposal.ViewOffset; got != want {
			t.Fatalf("block %d: view offset %d, the file records %d", v.Number, got, want)
		}

		seen := make(map[string]struct{}, len(confirm.Votes))
		for i, vote := range confirm.Votes {
			if !vote.Accept {
				t.Fatalf("block %d: vote %d is a reject vote", v.Number, i)
			}
			key := string(vote.Signer)
			if _, dup := seen[key]; dup {
				t.Fatalf("block %d: vote %d repeats signer %x", v.Number, i, vote.Signer)
			}
			seen[key] = struct{}{}
		}

		// The sponsor votes for its own proposal, and it does so by construction
		// rather than by convention: StartProposal and StartVote both take the
		// sealing account's public key (pgp-chain/dpos/proposal.go:18,
		// dpos/proposalvote.go:22). Counting the sponsor out would drop a twelve
		// producer confirm from eight votes to seven.
		if _, ok := seen[string(confirm.Proposal.Sponsor)]; !ok {
			t.Fatalf("block %d: sponsor %x is not among its own voters; pgp seals the proposal and "+
				"the vote with the same account, so this can only be a parsing bug",
				v.Number, confirm.Proposal.Sponsor)
		}
	}
}

// TestGoldenSealHashBindsRealMainnetHeaders pins the byte order of the binding.
//
// The proposal commits to SealHash, and the hash is compared as a plain array
// conversion with no reversal. Someone adding a BytesReverse here would make
// every historical block unverifiable, and the synthetic tests would not notice
// because they build the proposal from the same function they check against.
func TestGoldenSealHashBindsRealMainnetHeaders(t *testing.T) {
	for _, v := range loadGoldenVectors(t) {
		header := v.header(t)
		confirm := v.decodeGoldenConfirm(t, header)

		if want := SealHash(header); common.Hash(confirm.Proposal.BlockHash) != want {
			t.Fatalf("block %d: the proposal commits to %x, SealHash is %x",
				v.Number, common.Hash(confirm.Proposal.BlockHash), want)
		}
	}
}

// TestGoldenConfirmSatisfiesTheConfiguredProducerSet runs the real accepts
// through the real configuration.
//
// This is the assertion that catches a mistyped producer key: the signature check
// would still pass, but the signer would not be a member.
//
// It also pins the threshold at its shipped value of 8, which is what the chain
// needs. It does NOT pin the rounding rule behind that value: for twelve
// producers, ceil(12*2/3) and floor(12*2/3) are both 8, so a switch to rounding
// up leaves these twelve blocks verifying. TestLegacyMajorityCount is what
// distinguishes the two, using 25 and 2 producers where they diverge.
func TestGoldenConfirmSatisfiesTheConfiguredProducerSet(t *testing.T) {
	producers, err := decodeProducers(params.DefaultLegacyProducers)
	if err != nil {
		t.Fatalf("the built-in producer set is unusable: %v", err)
	}
	if len(producers) != 12 {
		t.Fatalf("the built-in producer set has %d keys, want 12", len(producers))
	}
	if got, want := legacyMajorityCount(len(producers)), 8; got != want {
		t.Fatalf("threshold for twelve producers is %d, want %d", got, want)
	}

	for _, v := range loadGoldenVectors(t) {
		header := v.header(t)
		confirm := v.decodeGoldenConfirm(t, header)

		if err := checkConfirm(confirm, producers, legacyMajorityCount(len(producers))); err != nil {
			t.Fatalf("block %d: mainnet confirm rejected: %v", v.Number, err)
		}
	}
}

// TestGoldenVerifySealAcceptsRealMainnetHeadersWithNoSPV runs the full seal check
// over every recorded block that predates the ELA chain reaching a height.
//
// These blocks carry header.Nonce == 0, so verifyConfirm takes the configured
// producer branch and never touches the SPV database. That is the property being
// asserted: the first 16404 blocks stay verifiable on a node whose ELA side has
// not started at all.
//
// The non-zero nonce blocks are deliberately left out: they resolve their
// producer set through the SPV service, which a test process does not have, so
// they are covered by checkConfirm above instead.
func TestGoldenVerifySealAcceptsRealMainnetHeadersWithNoSPV(t *testing.T) {
	engine := New(&params.LegacyConfig{
		VerifySeal: true,
		Producers:  params.DefaultLegacyProducers,
	})
	// A fresh chain: no block at any height yet, head at genesis. verifySeal
	// therefore has no canonical header to compare the view offset against.
	chain := &testChain{genesis: &types.Header{Number: big.NewInt(0), Time: 1761657649}}

	checked := 0
	for _, v := range loadGoldenVectors(t) {
		header := v.header(t)
		if header.Nonce != (types.BlockNonce{}) {
			continue
		}
		checked++

		if err := engine.verifySeal(chain, header); err != nil {
			t.Fatalf("block %d (nonce 0) rejected: %v", v.Number, err)
		}
	}
	if checked == 0 {
		t.Fatal("no recorded block carries nonce 0, so this test asserted nothing")
	}
	if checked != 5 {
		t.Fatalf("expected the five pre-ELA samples to carry nonce 0, got %d", checked)
	}
}

// TestGoldenHeaderOneAnchorsToTheConfiguredGenesis recovers the genesis hash from
// the vectors themselves.
//
// The genesis block never goes through header verification, so a typo in
// params.PGPGenesisHash cannot be caught by any other test. Block 1's parent is
// the genesis, which makes the recorded chain able to contradict the constant.
func TestGoldenHeaderOneAnchorsToTheConfiguredGenesis(t *testing.T) {
	found := false
	for _, v := range loadGoldenVectors(t) {
		if v.Number != 1 {
			continue
		}
		found = true
		if got := common.HexToHash(v.Header.ParentHash); got != params.PGPGenesisHash {
			t.Fatalf("block 1 is a child of %s, but params.PGPGenesisHash is %s", got, params.PGPGenesisHash)
		}
	}
	if !found {
		t.Fatal("the vectors carry no block 1, so the genesis anchor is unchecked")
	}
}

// TestGoldenTamperedMainnetHeadersAreRejected is the negative half.
//
// Accepting real blocks proves the rules are not too strict. These cases prove
// they are not too lax, on the same real data.
func TestGoldenTamperedMainnetHeadersAreRejected(t *testing.T) {
	producers, err := decodeProducers(params.DefaultLegacyProducers)
	if err != nil {
		t.Fatalf("the built-in producer set is unusable: %v", err)
	}
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: params.DefaultLegacyProducers})
	chain := &testChain{genesis: &types.Header{Number: big.NewInt(0), Time: 1761657649}}

	vectors := loadGoldenVectors(t)
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}

	t.Run("header-time-shifts-the-seal-hash", func(t *testing.T) {
		// An ordinary header field, excluded from Extra but included in SealHash.
		// A confirm therefore cannot be lifted onto another block.
		for _, v := range vectors {
			header := v.header(t)
			header.Time++

			err := engine.verifySeal(chain, header)
			if !errors.Is(err, ErrInvalidConfirm) {
				t.Fatalf("block %d: error = %v, want %v", v.Number, err, ErrInvalidConfirm)
			}
		}
	})

	t.Run("vote-signer-swapped-for-another-producer", func(t *testing.T) {
		// The replacement is a real producer, so membership passes and only the
		// signature check can catch it. This keeps the membership test from
		// looking like the sole line of defence.
		for _, v := range vectors {
			header := v.header(t)
			confirm := v.decodeGoldenConfirm(t, header)

			voted := make(map[string]struct{}, len(confirm.Votes))
			for _, vote := range confirm.Votes {
				voted[string(vote.Signer)] = struct{}{}
			}
			var replacement []byte
			for _, p := range producers {
				if _, ok := voted[string(p)]; !ok {
					replacement = p
					break
				}
			}
			if replacement == nil {
				t.Fatalf("block %d: every producer already voted, cannot pick a replacement", v.Number)
			}

			confirm.Votes[0].Signer = replacement
			extra := serializeConfirm(t, confirm)
			tampered, err := decodeConfirm(extra)
			if err != nil {
				t.Fatalf("block %d: the tampered confirm does not decode: %v", v.Number, err)
			}
			err = checkConfirm(tampered, producers, legacyMajorityCount(len(producers)))
			if !errors.Is(err, ErrInvalidConfirm) {
				t.Fatalf("block %d: a reattributed vote was accepted: %v", v.Number, err)
			}
		}
	})

	t.Run("one-signature-byte-flipped", func(t *testing.T) {
		for _, v := range vectors {
			header := v.header(t)
			extra := common.CopyBytes(header.Extra)

			// First vote starts after the proposal and the vote count; inside it
			// the signature is the last field, after ProposalHash (32), the
			// length prefixed signer and the accept byte.
			voteStart := legacyProposalWireLen + 8
			sigStart := voteStart + 32 + 1 + elaCrypto.NegativeBigLength + 1 + 1
			if sigStart >= len(extra) {
				t.Fatalf("block %d: computed signature offset %d is out of range", v.Number, sigStart)
			}
			extra[sigStart+3] ^= 0xff

			tampered, err := decodeConfirm(extra)
			if err != nil {
				t.Fatalf("block %d: the tampered confirm does not decode: %v", v.Number, err)
			}
			err = checkConfirm(tampered, producers, legacyMajorityCount(len(producers)))
			if !errors.Is(err, ErrInvalidConfirm) {
				t.Fatalf("block %d: a corrupt signature was accepted: %v", v.Number, err)
			}
		}
	})

	t.Run("seven-votes-are-not-enough", func(t *testing.T) {
		for _, v := range vectors {
			header := v.header(t)
			confirm := v.decodeGoldenConfirm(t, header)
			confirm.Votes = confirm.Votes[:7]

			extra := serializeConfirm(t, confirm)
			tampered, err := decodeConfirm(extra)
			if err != nil {
				t.Fatalf("block %d: the seven vote confirm does not decode: %v", v.Number, err)
			}
			err = checkConfirm(tampered, producers, legacyMajorityCount(len(producers)))
			if !errors.Is(err, ErrInvalidConfirm) {
				t.Fatalf("block %d: seven valid votes were accepted, want a threshold failure: %v", v.Number, err)
			}
			if !strings.Contains(err.Error(), "need 8 votes, got 7") {
				t.Fatalf("block %d: error = %v, want it to name the threshold", v.Number, err)
			}
		}
	})

	t.Run("absurd-vote-count-does-not-allocate", func(t *testing.T) {
		// The decoder must bound the count before payload.Confirm.Deserialize
		// turns it into a make(). A terabyte allocation here would take the node
		// down from a single relayed header.
		for _, v := range vectors {
			header := v.header(t)
			extra := common.CopyBytes(header.Extra)
			binary.LittleEndian.PutUint64(extra[legacyProposalWireLen:], ^uint64(0))

			started := time.Now()
			_, err := decodeConfirm(extra)
			elapsed := time.Since(started)

			if err == nil {
				t.Fatalf("block %d: a vote count of 2^64-1 was accepted", v.Number)
			}
			if elapsed > time.Second {
				t.Fatalf("block %d: rejecting the flood took %v, it should be immediate", v.Number, elapsed)
			}
		}
	})
}
