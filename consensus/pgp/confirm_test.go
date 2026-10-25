// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package pgp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	elacommon "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	elaCrypto "github.com/elastos/Elastos.ELA/crypto"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/spv"
)

// testProducer is one PBFT producer: the raw private key ELA signs with and the
// compressed public key that goes into a proposal or a vote.
type testProducer struct {
	priv []byte
	pub  []byte
}

// newTestProducers generates n independent P256 producers.
//
// Real keys, not fixtures: the whole point of these tests is that the signature
// checks are actually performed, so the signatures have to be real signatures.
// elaCrypto.GenerateKeyPair uses crypto.DefaultCurve, i.e. P256, which is what
// the mainnet producers use.
func newTestProducers(t *testing.T, n int) []testProducer {
	t.Helper()

	out := make([]testProducer, 0, n)
	for i := 0; i < n; i++ {
		priv, pub, err := elaCrypto.GenerateKeyPair()
		if err != nil {
			t.Fatalf("cannot generate producer %d: %v", i, err)
		}
		encoded, err := pub.EncodePoint(true)
		if err != nil {
			t.Fatalf("cannot encode producer %d: %v", i, err)
		}
		if len(encoded) != 33 {
			t.Fatalf("compressed public key is %d bytes, want 33", len(encoded))
		}
		out = append(out, testProducer{priv: priv, pub: encoded})
	}
	return out
}

// elaSign produces the raw 64 byte r||s signature ELA verifies, over sha256(data)
// on P256.
//
// It does not call elaCrypto.Sign because that function is unusable on this
// toolchain: it builds an ecdsa.PrivateKey with only D set and leaves the public
// half nil (Elastos.ELA crypto/crypto.go:112-118), and Go 1.24+ derives the
// public key on the way into the FIPS signer and nil-derefs doing it. Nothing in
// the node signs with it — the engine only ever verifies — so this is a test-side
// workaround, and the *verification* still goes through elaCrypto.Verify.
func elaSign(t *testing.T, priv []byte, data []byte) []byte {
	t.Helper()

	curve := elaCrypto.DefaultCurve
	x, y := curve.ScalarBaseMult(priv)
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         new(big.Int).SetBytes(priv),
	}
	digest := sha256.Sum256(data)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("cannot sign: %v", err)
	}
	sig := make([]byte, elaCrypto.SignatureLength)
	r.FillBytes(sig[:elaCrypto.SignerLength])
	s.FillBytes(sig[elaCrypto.SignerLength:])
	return sig
}

// signProposal fills in the sponsor and the signature of a proposal.
func signProposal(t *testing.T, p *testProducer, blockHash common.Hash, viewOffset uint32) payload.DPOSProposal {
	t.Helper()

	proposal := payload.DPOSProposal{
		Sponsor:    p.pub,
		BlockHash:  elacommon.Uint256(blockHash),
		ViewOffset: viewOffset,
	}
	proposal.Sign = elaSign(t, p.priv, proposal.Data())
	return proposal
}

// signVote fills in the signer and the signature of a vote.
func signVote(t *testing.T, p *testProducer, proposalHash elacommon.Uint256, accept bool) payload.DPOSProposalVote {
	t.Helper()

	vote := payload.DPOSProposalVote{
		ProposalHash: proposalHash,
		Signer:       p.pub,
		Accept:       accept,
	}
	vote.Sign = elaSign(t, p.priv, vote.Data())
	return vote
}

// buildConfirm assembles the confirm the first `voters` producers would have
// produced for blockHash.
func buildConfirm(t *testing.T, producers []testProducer, blockHash common.Hash, viewOffset uint32, voters int) *payload.Confirm {
	t.Helper()

	proposal := signProposal(t, &producers[0], blockHash, viewOffset)
	confirm := &payload.Confirm{Proposal: proposal}
	hash := proposal.Hash()
	for i := 0; i < voters; i++ {
		confirm.Votes = append(confirm.Votes, signVote(t, &producers[i], hash, true))
	}
	return confirm
}

// serializeConfirm returns the bytes that belong in header.Extra.
func serializeConfirm(t *testing.T, confirm *payload.Confirm) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := confirm.Serialize(&buf); err != nil {
		t.Fatalf("cannot serialize the confirm: %v", err)
	}
	return buf.Bytes()
}

// zeroConfirmExtra returns a structurally valid confirm with no votes whose
// signatures are all zero. It costs 143 bytes and is enough for every test that
// only cares about the envelope: the header binding runs before any signature is
// looked at.
func zeroConfirmExtra(blockHash common.Hash) []byte {
	confirm := payload.Confirm{
		Proposal: payload.DPOSProposal{
			Sponsor:    bytes.Repeat([]byte{0x02}, 33),
			BlockHash:  elacommon.Uint256(blockHash),
			ViewOffset: 0,
			Sign:       make([]byte, 64),
		},
	}
	var buf bytes.Buffer
	if err := confirm.Serialize(&buf); err != nil {
		panic("pgp: cannot serialize an empty confirm: " + err.Error())
	}
	return buf.Bytes()
}

// bindConfirm writes a confirm for this header into its Extra.
func bindConfirm(t *testing.T, header *types.Header, producers []testProducer, viewOffset uint32, voters int) {
	t.Helper()
	header.Extra = serializeConfirm(t, buildConfirm(t, producers, SealHash(header), viewOffset, voters))
}

// TestLegacyMajorityCount pins the two-thirds threshold, including the rounding
// direction and the degenerate case.
//
// 12 -> 8 and 36 -> 24 do not distinguish floor from ceiling; 25 -> 16 does, and
// 1 -> 0 pins the reference behaviour that a one member producer set accepts a
// vote-less confirm. That last one reads like a bug and is not: it is what
// int(float64(n)*2/3) does, and "fixing" it would reject confirms upstream
// accepts.
func TestLegacyMajorityCount(t *testing.T) {
	for _, tc := range []struct {
		total int
		want  int
	}{
		{12, 8},  // mainnet: 8 of 12
		{36, 24}, // 36 * 2 / 3
		{24, 16}, // 24 * 2 / 3
		{25, 16}, // floor(16.67), NOT 17 -- this is the floor/ceiling discriminator
		{2, 1},   // floor(1.33)
		{1, 0},   // degenerate: reference behaviour, not a bug
		{0, 0},
	} {
		if got := legacyMajorityCount(tc.total); got != tc.want {
			t.Errorf("legacyMajorityCount(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}

// TestDecodeConfirmRejectsVoteCountFlood is the remote-crash guard.
//
// payload.Confirm.Deserialize allocates make([]DPOSProposalVote, signCount)
// straight from the wire with no upper bound, so an Extra advertising 2^64-1
// votes would try to allocate the address space inside header verification. The
// count has to be bounded by the bytes that remain before Deserialize runs.
//
// The elapsed-time assertion is part of the test on purpose: an implementation
// that allocated first and failed later would still return an error, just not
// fast enough to be a fix.
func TestDecodeConfirmRejectsVoteCountFlood(t *testing.T) {
	base := zeroConfirmExtra(common.HexToHash("0xdead"))
	// zeroConfirmExtra is exactly a proposal plus a zero count word.
	if len(base) != legacyConfirmMinLen {
		t.Fatalf("fixture is %d bytes, want %d", len(base), legacyConfirmMinLen)
	}
	flood := make([]byte, len(base))
	copy(flood, base)
	binary.LittleEndian.PutUint64(flood[legacyProposalWireLen:], math.MaxUint64)

	start := time.Now()
	confirm, err := decodeConfirm(flood)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a confirm claiming %d votes in %d bytes was accepted", uint64(math.MaxUint64), len(flood))
	}
	if !errors.Is(err, errConfirmVoteCount) {
		t.Fatalf("error = %v, want %v", err, errConfirmVoteCount)
	}
	if confirm != nil {
		t.Fatal("rejected confirm still returned a value")
	}
	if elapsed > time.Second {
		t.Fatalf("rejecting the flood took %v; the count is not bounded before allocation", elapsed)
	}

	// A count that does fit must still be accepted, otherwise the bound is so
	// tight it rejects the real thing.
	eight := buildConfirm(t, newTestProducers(t, 8), common.HexToHash("0xdead"), 0, 8)
	if _, err := decodeConfirm(serializeConfirm(t, eight)); err != nil {
		t.Fatalf("a legitimate 8 vote confirm was rejected: %v", err)
	}
}

// TestDecodeConfirmRejectsTrailingBytes pins exact consumption.
//
// Deserialize stops after the last vote and never checks the reader, so without
// this rule an attacker can append arbitrary bytes: the header hash changes, the
// Extra changes, and the same confirm still verifies for two different blocks.
func TestDecodeConfirmRejectsTrailingBytes(t *testing.T) {
	producers := newTestProducers(t, 8)
	extra := serializeConfirm(t, buildConfirm(t, producers, common.HexToHash("0xdead"), 0, 8))

	if _, err := decodeConfirm(extra); err != nil {
		t.Fatalf("the exact serialization was rejected: %v", err)
	}
	for _, pad := range [][]byte{{0x00}, {0xff, 0xff}} {
		padded := append(append([]byte{}, extra...), pad...)
		_, err := decodeConfirm(padded)
		if !errors.Is(err, errConfirmTrailing) {
			t.Fatalf("%d trailing bytes: error = %v, want %v", len(pad), err, errConfirmTrailing)
		}
	}
}

// TestDecodeConfirmRejectsNonCanonicalLength checks that the CompactSize rules of
// ELA's serializer are enforced while the layout is being walked.
//
// Without it, the same confirm has several legal encodings and therefore several
// different header hashes.
func TestDecodeConfirmRejectsNonCanonicalLength(t *testing.T) {
	// A confirm whose sponsor length is written with a wider discriminant than
	// the value needs. Bitcoin's CompactSize allows exactly one encoding per
	// value and ELA inherits that, so all three of these are non-canonical even
	// though each one decodes to the real sponsor length of 33 bytes.
	//
	// All three discriminants are covered because they are three separate
	// branches in readCompactSize, each with its own threshold. Leaving two of
	// them untested let a `<` become `<=` on those branches without any test
	// noticing.
	cases := []struct {
		name   string
		prefix []byte
	}{
		{"0xfd for a value that fits in one byte", []byte{0xfd, 0x21, 0x00}},
		{"0xfe for a value that fits in one byte", []byte{0xfe, 0x21, 0x00, 0x00, 0x00}},
		{"0xff for a value that fits in one byte", []byte{0xff, 0x21, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nonCanonical := append([]byte{}, tc.prefix...)
			nonCanonical = append(nonCanonical, make([]byte, 33+32+4)...)
			nonCanonical = append(nonCanonical, 0x40)
			nonCanonical = append(nonCanonical, make([]byte, 64+8)...)
			// Padding keeps the buffer above the minimum length, so the
			// canonicality check is what fails and not the size floor.
			nonCanonical = append(nonCanonical, make([]byte, 64)...)

			if len(nonCanonical) < legacyConfirmMinLen {
				t.Fatalf("the fixture is %d bytes, below the %d byte floor, so it would "+
					"fail on length before reaching the canonicality check",
					len(nonCanonical), legacyConfirmMinLen)
			}
			_, err := decodeConfirm(nonCanonical)
			if !errors.Is(err, errCompactNonCanon) {
				t.Fatalf("error = %v, want %v", err, errCompactNonCanon)
			}
		})
	}

	// The same value written the canonical way must be walked successfully; the
	// point of the cases above is the encoding, not the length itself.
	if _, err := proposalWireLen(append([]byte{0x21}, make([]byte, 33+32+4+1+64)...)); err != nil {
		t.Fatalf("canonical encoding was rejected: %v", err)
	}

	// Canonicality and the 33 byte sponsor ceiling are two independent gates,
	// and this is the case that proves it: 0xfe00010000 is a perfectly legal
	// 0xfe encoding of 65536, so readCompactSize returns it happily, and only
	// the ceiling check in proposalWireLen can reject it. If the canonicality
	// branch ever swallowed values above its own threshold, this would report
	// errCompactNonCanon instead.
	//
	// The second entry is the same argument for the 0xff branch. Both are load
	// bearing for a second reason: 0x10000 and 0x100000000 are exactly the
	// thresholds those branches compare against, so these two values are the
	// only inputs that can tell `<` from `<=` there. Anything smaller is
	// rejected by both and the mutation survives.
	for _, enc := range [][]byte{
		{0xfe, 0x00, 0x00, 0x01, 0x00},                         // 65536
		{0xff, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}, // 4294967296
	} {
		if _, err := proposalWireLen(enc); !errors.Is(err, errSponsorTooLong) {
			t.Fatalf("%x: a canonically encoded length sitting exactly on the "+
				"discriminant's threshold must be accepted by readCompactSize and then "+
				"rejected by the sponsor ceiling, got %v", enc, err)
		}
	}

	// Truncation inside a multi byte length must be an error, never a panic.
	for _, buf := range [][]byte{{0xfd}, {0xfd, 0x21}, {0xfe, 0x00}, {0xff, 0x00, 0x00, 0x00}} {
		if _, err := proposalWireLen(buf); err == nil {
			t.Fatalf("%x: truncated length prefix was accepted", buf)
		}
	}
}

// TestCheckConfirmRejections walks every way a forged confirm can be refused.
//
// Each case breaks exactly one rule, so a regression that drops one rule shows up
// as exactly one failing subtest instead of a general "something is wrong".
func TestCheckConfirmRejections(t *testing.T) {
	producers := newTestProducers(t, 12)
	byBytes := make([][]byte, len(producers))
	for i := range producers {
		byBytes[i] = producers[i].pub
	}
	blockHash := common.HexToHash("0xabcdef")
	const viewOffset = 3

	good := func() *payload.Confirm {
		return buildConfirm(t, producers, blockHash, viewOffset, 8)
	}

	t.Run("empty producer set", func(t *testing.T) {
		if err := checkConfirm(good(), nil, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("producer set of only empty entries", func(t *testing.T) {
		if err := checkConfirm(good(), [][]byte{nil, make([]byte, 33)}, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("sponsor not a producer", func(t *testing.T) {
		stranger := newTestProducers(t, 1)[0]
		confirm := buildConfirm(t, producers, blockHash, viewOffset, 0)
		// Rebuild with a sponsor from outside the set, keeping the votes valid.
		proposal := signProposal(t, &stranger, blockHash, viewOffset)
		confirm.Proposal = proposal
		hash := proposal.Hash()
		confirm.Votes = nil
		for i := 0; i < 8; i++ {
			confirm.Votes = append(confirm.Votes, signVote(t, &producers[i], hash, true))
		}
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("reject vote", func(t *testing.T) {
		confirm := good()
		confirm.Votes[0] = signVote(t, &producers[0], confirm.Proposal.Hash(), false)
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("vote for another proposal", func(t *testing.T) {
		confirm := good()
		other := buildConfirm(t, producers, common.HexToHash("0x1234"), viewOffset, 8)
		confirm.Votes[0] = other.Votes[0]
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("vote from outside the producer set", func(t *testing.T) {
		confirm := good()
		stranger := newTestProducers(t, 1)[0]
		confirm.Votes[0] = signVote(t, &stranger, confirm.Proposal.Hash(), true)
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("duplicate signer", func(t *testing.T) {
		// Nine votes but only eight distinct signers: without deduplication one
		// key alone could satisfy an 8-of-12 threshold.
		confirm := good()
		confirm.Votes = append(confirm.Votes, signVote(t, &producers[0], confirm.Proposal.Hash(), true))
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("bad vote signature", func(t *testing.T) {
		confirm := good()
		confirm.Votes[0].Sign[0] ^= 0xff
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("bad proposal signature", func(t *testing.T) {
		confirm := good()
		confirm.Proposal.Sign[0] ^= 0xff
		if err := checkConfirm(confirm, byBytes, 8); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("error = %v, want %v", err, ErrInvalidConfirm)
		}
	})
	t.Run("too few votes", func(t *testing.T) {
		confirm := buildConfirm(t, producers, blockHash, viewOffset, 7)
		if err := checkConfirm(confirm, byBytes, legacyMajorityCount(12)); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("7 of 12 votes were accepted, want %v", ErrInvalidConfirm)
		}
	})
	t.Run("exactly enough votes", func(t *testing.T) {
		confirm := buildConfirm(t, producers, blockHash, viewOffset, 8)
		if err := checkConfirm(confirm, byBytes, legacyMajorityCount(12)); err != nil {
			t.Fatalf("8 of 12 votes were rejected: %v", err)
		}
	})
	t.Run("threshold is computed from the unfiltered signer count", func(t *testing.T) {
		// 25 seats, 16 votes. floor(25*2/3) = 16 accepts, ceil would need 17.
		many := newTestProducers(t, 25)
		manyBytes := make([][]byte, len(many))
		for i := range many {
			manyBytes[i] = many[i].pub
		}
		confirm := buildConfirm(t, many, blockHash, viewOffset, 16)
		if err := checkConfirm(confirm, manyBytes, legacyMajorityCount(25)); err != nil {
			t.Fatalf("16 of 25 votes were rejected, the threshold is not the floor of two thirds: %v", err)
		}
		if err := checkConfirm(confirm, manyBytes, 17); !errors.Is(err, ErrInvalidConfirm) {
			t.Fatalf("the 16 vote confirm still passed at a threshold of 17, so the assertion above proves nothing: %v", err)
		}
	})
}

// TestVerifySealAccepts is the end-to-end positive case: a header whose Extra is
// a real 8-of-12 confirm over its own seal hash, with header.Nonce zero so the
// producer set comes from the configuration and no SPV database is involved.
//
// It also asserts the byte count, because 1199 is the length every mainnet legacy
// header carries and a change here means the layout drifted.
func TestVerifySealAccepts(t *testing.T) {
	producers := newTestProducers(t, 12)
	hexKeys := make([]string, len(producers))
	for i := range producers {
		hexKeys[i] = common.Bytes2Hex(producers[i].pub)
	}
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: hexKeys})

	genesis := &types.Header{Number: common.Big0, Time: 1762218000}
	header := newTestHeader(genesis)
	header.Nonce = types.EncodeNonce(0)
	bindConfirm(t, header, producers, 0, 8)

	if len(header.Extra) != 1199 {
		t.Fatalf("extra is %d bytes, want 1199 (proposal 135 + count 8 + 8 votes of 132)", len(header.Extra))
	}
	chain := &testChain{genesis: genesis}
	if err := engine.VerifyHeader(chain, header); err != nil {
		t.Fatalf("a valid 8-of-12 confirm was rejected: %v", err)
	}

	// One vote fewer must fail, otherwise the positive case above proves nothing
	// about the threshold being applied.
	short := newTestHeader(genesis)
	short.Nonce = types.EncodeNonce(0)
	bindConfirm(t, short, producers, 0, 7)
	if err := engine.VerifyHeader(chain, short); !errors.Is(err, ErrInvalidConfirm) {
		t.Fatalf("a 7-of-12 confirm returned %v, want %v", err, ErrInvalidConfirm)
	}
}

// TestVerifyConfirmFailsClosedWithoutSPV pins the gate in front of the arbiter
// lookup.
//
// A block with header.Nonce != 0 needs the ELA arbiters of that height to check
// its confirm. When the SPV service cannot answer, the header must be rejected
// with ErrSealSPVBehind rather than accepted: an unverifiable seal that passes
// is worse than a block that has to be retried, because nothing downstream ever
// looks again.
//
// The test drives verifyConfirm directly instead of verifyHeader: the point is
// the producer-set resolution, and going through a full header would make the
// assertion depend on the header rules too.
func TestVerifyConfirmFailsClosedWithoutSPV(t *testing.T) {
	const elaHeight = 2_076_267

	// Fixture precondition. A fresh test process has no SPV service, so
	// GetSpvHeight reports 0; assert that instead of skipping, because a
	// non-zero height here would mean some other test started the service and
	// this one would then be testing nothing.
	if got := spv.GetSpvHeight(); got >= elaHeight {
		t.Fatalf("fixture problem: the SPV service reports height %d, this test needs it below %d", got, elaHeight)
	}

	producers := newTestProducers(t, 12)
	hexKeys := make([]string, len(producers))
	for i := range producers {
		hexKeys[i] = common.Bytes2Hex(producers[i].pub)
	}
	confirm := buildConfirm(t, producers, common.HexToHash("0xdead"), 0, 8)
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: hexKeys})

	err := engine.verifyConfirm(confirm, elaHeight)
	if !errors.Is(err, ErrSealSPVBehind) {
		t.Fatalf("error = %v, want %v: an unresolvable producer set must fail closed", err, ErrSealSPVBehind)
	}
	// The height gate and the GetProducers failure carry the same sentinel, so
	// the sentinel alone cannot tell them apart. Pin the gate's own wording:
	// without it the error comes back as "spv is not start" instead, and a mutant
	// that deletes the gate leaves every assertion above green.
	if !strings.Contains(err.Error(), "SPV is at") {
		t.Fatalf("error = %v, want the height gate to be what rejected this header", err)
	}

	// The same confirm with header.Nonce == 0 must be checkable with no SPV at
	// all, otherwise the first 16404 blocks become unsyncable on a node whose ELA
	// side is still catching up.
	if err := engine.verifyConfirm(buildConfirm(t, producers, common.HexToHash("0xdead"), 0, 1), 0); !errors.Is(err, ErrInvalidConfirm) {
		t.Fatalf("height 0 returned %v, want a confirm verdict (%v) rather than an SPV error", err, ErrInvalidConfirm)
	}
}

// TestVerifySealBindsConfirmToHeader proves the confirm cannot be moved to
// another block.
//
// The confirm is correct in every way except the block it names, and the only
// changed field (Time) is one that is inside the seal hash but outside Extra. A
// verifier that skipped the binding would accept this header, and then any
// captured confirm could be recycled onto a forged block.
func TestVerifySealBindsConfirmToHeader(t *testing.T) {
	producers := newTestProducers(t, 12)
	hexKeys := make([]string, len(producers))
	for i := range producers {
		hexKeys[i] = common.Bytes2Hex(producers[i].pub)
	}
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: hexKeys})

	genesis := &types.Header{Number: common.Big0, Time: 1762218000}
	header := newTestHeader(genesis)
	header.Nonce = types.EncodeNonce(0)
	bindConfirm(t, header, producers, 0, 8)

	forged := *header
	forged.Time += 3
	if err := engine.verifySeal(&testChain{genesis: genesis}, &forged); !errors.Is(err, ErrInvalidConfirm) {
		t.Fatalf("a confirm was accepted for a header it does not commit to: %v", err)
	}
}

// TestVerifySealToleratesUndecodableOldExtra pins a soft failure: when the
// canonical block at the same height carries an Extra that is not a confirm, the
// new header must be accepted.
//
// Returning an error there would stop the sync on a historical block that the
// network considers canonical, for a reason that has nothing to do with the block
// being verified.
func TestVerifySealToleratesUndecodableOldExtra(t *testing.T) {
	producers := newTestProducers(t, 12)
	hexKeys := make([]string, len(producers))
	for i := range producers {
		hexKeys[i] = common.Bytes2Hex(producers[i].pub)
	}
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: hexKeys})

	genesis := &types.Header{Number: common.Big0, Time: 1762218000}
	header := newTestHeader(genesis)
	header.Nonce = types.EncodeNonce(0)
	bindConfirm(t, header, producers, 0, 8)

	old := newTestHeader(genesis)
	old.Extra = bytes.Repeat([]byte{0xfd}, 200) // not a confirm at all
	chain := &testChain{genesis: genesis, byNumber: map[uint64]*types.Header{1: old}, current: old}

	if err := engine.verifySeal(chain, header); err != nil {
		t.Fatalf("an undecodable canonical Extra blocked verification: %v", err)
	}
}

// TestVerifySealForkAndDoubleSign covers the two history guards and, just as
// importantly, the two conditions that keep them from firing.
//
// The guards are what stop a fork from being finalised twice at one height. Their
// escape hatches are what stop the node from rejecting the honest chain: a view
// change at the tip legitimately lowers the view offset, and a MaxUint64 nonce
// means the ELA height was not known, so two blocks there may differ.
func TestVerifySealForkAndDoubleSign(t *testing.T) {
	producers := newTestProducers(t, 12)
	hexKeys := make([]string, len(producers))
	for i := range producers {
		hexKeys[i] = common.Bytes2Hex(producers[i].pub)
	}
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: hexKeys})
	genesis := &types.Header{Number: common.Big0, Time: 1762218000}

	// canonicalAndCandidate builds two different headers at the same height, each
	// correctly bound to its own confirm, and returns them plus a chain whose
	// canonical block is the first.
	canonicalAndCandidate := func(t *testing.T, canonicalView, candidateView uint32, canonicalNonce, candidateNonce uint64, tipHeight uint64) (*types.Header, *testChain) {
		t.Helper()
		canonical := newTestHeader(genesis)
		canonical.Time = genesis.Time + 3
		canonical.Nonce = types.EncodeNonce(canonicalNonce)
		bindConfirm(t, canonical, producers, canonicalView, 8)

		candidate := newTestHeader(genesis)
		candidate.Time = genesis.Time + 6 // a different header, same height
		candidate.Nonce = types.EncodeNonce(candidateNonce)
		bindConfirm(t, candidate, producers, candidateView, 8)

		if canonical.Hash() == candidate.Hash() {
			t.Fatal("the fixture produced two identical headers")
		}
		tip := &types.Header{Number: new(big.Int).SetUint64(tipHeight)}
		chain := &testChain{
			genesis:  genesis,
			current:  tip,
			byNumber: map[uint64]*types.Header{1: canonical},
		}
		return candidate, chain
	}

	t.Run("smaller view offset below the tip is a fork", func(t *testing.T) {
		candidate, chain := canonicalAndCandidate(t, 5, 3, 0, 0, 10)
		err := engine.verifySeal(chain, candidate)
		if !errors.Is(err, errChainForkBlock) {
			t.Fatalf("error = %v, want %v", err, errChainForkBlock)
		}
	})
	t.Run("smaller view offset at the tip is a normal view change", func(t *testing.T) {
		// tipHeight 1 == the header's own height, so `number < current.Number`
		// is false and the fork guard must not fire.
		candidate, chain := canonicalAndCandidate(t, 5, 3, 0, 0, 1)
		if err := engine.verifySeal(chain, candidate); err != nil {
			t.Fatalf("a view change at the tip was rejected as a fork: %v", err)
		}
	})
	t.Run("same view offset with different blocks is a double sign", func(t *testing.T) {
		candidate, chain := canonicalAndCandidate(t, 4, 4, 0, 0, 10)
		err := engine.verifySeal(chain, candidate)
		if !errors.Is(err, errDoubleSignBlock) {
			t.Fatalf("error = %v, want %v", err, errDoubleSignBlock)
		}
	})
	t.Run("same view offset with different blocks is allowed when the nonce is unknown", func(t *testing.T) {
		// The canonical block is the one with the unknown ELA height, which is
		// the reachable shape: a nonce-0 candidate can still be verified against
		// the configured producer set, while the canonical header only has to be
		// parsed from the database.
		candidate, chain := canonicalAndCandidate(t, 4, 4, math.MaxUint64, 0, 10)
		if err := engine.verifySeal(chain, candidate); err != nil {
			t.Fatalf("MaxUint64 nonce must exempt the double sign rule: %v", err)
		}
	})
}

// TestVerifySealForkAndDoubleSignOnRealBytes drives the two history guards with
// mainnet headers instead of synthetic ones.
//
// The test above proves the guards fire, but every byte in it is invented,
// including the seal hashes and the signatures. These cases keep the real header
// and change exactly one field inside its confirm, so the isolation is honest:
// SealHash covers fourteen fields and Extra is not one of them, which means the
// header binding at the top of verifySeal still passes and the guard under test
// is the only thing left that can reject.
//
// Block 1 is the only recorded block that can carry these cases. It has to reach
// the guards, and that means it has to survive verifyConfirm first, which for a
// non-zero header.Nonce needs the ELA SPV service (see the note on the
// non-zero-nonce case below). Block 1 has nonce 0 - it was sealed before the ELA
// chain had a height - so it is checked against the configured producer set and
// needs no SPV at all. It is also one of only two recorded blocks with a
// non-zero view offset, which is what makes a real fork comparison possible.
func TestVerifySealForkAndDoubleSignOnRealBytes(t *testing.T) {
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: params.DefaultLegacyProducers})

	byNumber := map[uint64]goldenVector{}
	for _, v := range loadGoldenVectors(t) {
		byNumber[v.Number] = v
	}
	blockOne, ok := byNumber[1]
	if !ok {
		t.Fatal("the vectors carry no block 1, so nothing here can run")
	}

	// canonicalNear rebuilds a recorded header with one field of its confirm
	// changed and every other byte left as the chain produced it.
	canonicalNear := func(t *testing.T, v goldenVector, mutate func(*payload.Confirm)) *types.Header {
		t.Helper()

		real := v.header(t)
		confirm := v.decodeGoldenConfirm(t, real)
		mutate(confirm)

		// A value copy is enough: types.Header has no hash cache in this tree,
		// Hash() re-encodes every call. Copying a cached digest here would make
		// the "hashes differ" assertions below pass for the wrong reason.
		altered := *real
		altered.Extra = serializeConfirm(t, confirm)
		if altered.Hash() == real.Hash() {
			t.Fatalf("block %d: the fixture failed to produce a different header", v.Number)
		}
		// verifySeal soft-fails (returns nil) when it cannot decode the canonical
		// header's Extra, so an unparseable mutant would silently pass the test.
		decoded, err := decodeConfirm(altered.Extra)
		if err != nil {
			t.Fatalf("block %d: the altered canonical header no longer decodes, so "+
				"verifySeal would soft-fail instead of exercising the guard: %v", v.Number, err)
		}
		if decoded.Proposal.ViewOffset != confirm.Proposal.ViewOffset {
			t.Fatalf("block %d: the altered confirm re-decoded to a different view offset", v.Number)
		}
		return &altered
	}

	// chainWith returns a chain whose canonical block at height v.Number is the
	// supplied header, with the head above it so the `number < current.Number`
	// half of the fork guard is satisfied.
	chainWith := func(v goldenVector, canonical *types.Header, tip uint64) *testChain {
		return &testChain{
			genesis:  &types.Header{Number: common.Big0, Time: 1761657649},
			current:  &types.Header{Number: new(big.Int).SetUint64(tip)},
			byNumber: map[uint64]*types.Header{v.Number: canonical},
		}
	}

	t.Run("real block 1 loses to a canonical block one view later", func(t *testing.T) {
		real := blockOne.header(t)
		if got := blockOne.decodeGoldenConfirm(t, real).Proposal.ViewOffset; got != 1 {
			t.Fatalf("block 1 carries view offset %d, want 1; the vector file changed", got)
		}
		// The canonical block is the same real header, one view later: exactly
		// what a fork looks like when the other branch won the view change.
		canonical := canonicalNear(t, blockOne, func(c *payload.Confirm) { c.Proposal.ViewOffset = 2 })
		if got := decodeConfirmMust(t, canonical).Proposal.ViewOffset; got != 2 {
			t.Fatalf("the canonical fixture carries view offset %d, want 2", got)
		}

		err := engine.verifySeal(chainWith(blockOne, canonical, 2), real)
		if !errors.Is(err, errChainForkBlock) {
			t.Fatalf("error = %v, want %v", err, errChainForkBlock)
		}
	})

	t.Run("real block 1 double signed at the same view", func(t *testing.T) {
		real := blockOne.header(t)
		// Same view offset, different confirm, therefore a different header at
		// the same height: the double sign shape. Flipping a byte in the
		// proposal signature is the cheapest way to move the header hash without
		// touching anything verifySeal parses, so the canonical header still
		// decodes and the fork guard cannot fire first.
		canonical := canonicalNear(t, blockOne, func(c *payload.Confirm) { c.Proposal.Sign[0] ^= 0xff })
		if got := decodeConfirmMust(t, canonical).Proposal.ViewOffset; got != 1 {
			t.Fatalf("the canonical fixture carries view offset %d, want 1", got)
		}

		err := engine.verifySeal(chainWith(blockOne, canonical, 2), real)
		if !errors.Is(err, errDoubleSignBlock) {
			t.Fatalf("error = %v, want %v", err, errDoubleSignBlock)
		}
	})

	t.Run("a real non-zero-nonce block stops before the guards", func(t *testing.T) {
		v, ok := byNumber[16405]
		if !ok {
			t.Fatal("the vectors carry no block 16405")
		}
		real := v.header(t)
		canonical := canonicalNear(t, v, func(c *payload.Confirm) { c.Proposal.ViewOffset = 3 })

		err := engine.verifySeal(chainWith(v, canonical, 20000), real)
		// Deliberately not errChainForkBlock. verifyConfirm runs before the two
		// history guards, and a non-zero header.Nonce resolves its producer set
		// through the ELA SPV service, which a test process does not have, so
		// this block never reaches the fork comparison. That makes the case a
		// pin on the *order* of the checks rather than on the guard itself - and
		// the order is worth pinning, because comparing the view offset of a
		// header whose confirm has not been authenticated yet would be comparing
		// a number the attacker chose.
		if !errors.Is(err, ErrSealSPVBehind) {
			t.Fatalf("error = %v, want %v; the history guards must not run before the "+
				"confirm has been authenticated", err, ErrSealSPVBehind)
		}
	})
}

// decodeConfirmMust decodes a header's Extra or fails the test.
func decodeConfirmMust(t *testing.T, header *types.Header) *payload.Confirm {
	t.Helper()

	confirm, err := decodeConfirm(header.Extra)
	if err != nil {
		t.Fatalf("decodeConfirm rejected the fixture's Extra: %v", err)
	}
	return confirm
}
