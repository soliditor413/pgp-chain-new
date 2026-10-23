// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package pgp

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

// testChain is a minimal consensus.ChainHeaderReader that only knows about the
// genesis header, which is all the legacy verification needs.
type testChain struct {
	genesis *types.Header
}

func (c *testChain) Config() *params.ChainConfig             { return params.PGPChainConfig }
func (c *testChain) CurrentHeader() *types.Header            { return c.genesis }
func (c *testChain) GenesisHeader() *types.Header            { return c.genesis }
func (c *testChain) GetHighestVerifiedHeader() *types.Header { return c.genesis }
func (c *testChain) GetVerifiedBlockByHash(common.Hash) *types.Header {
	return c.genesis
}
func (c *testChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if number == 0 && hash == c.genesis.Hash() {
		return c.genesis
	}
	return nil
}
func (c *testChain) GetHeaderByNumber(number uint64) *types.Header {
	if number == 0 {
		return c.genesis
	}
	return nil
}
func (c *testChain) GetHeaderByHash(hash common.Hash) *types.Header {
	if hash == c.genesis.Hash() {
		return c.genesis
	}
	return nil
}
func (c *testChain) GetTd(common.Hash, uint64) *big.Int { return big.NewInt(1) }

// sigHeaderFields returns the header fields that participate in the seal hash,
// in the exact order pgp-chain's encodeSigHeader uses.
func sigHeaderFields(h *types.Header) []interface{} {
	return []interface{}{
		h.ParentHash, h.UncleHash, h.Coinbase, h.Root, h.TxHash, h.ReceiptHash,
		h.Bloom, h.Difficulty, h.Number, h.GasLimit, h.GasUsed, h.Time,
		h.MixDigest, h.Nonce,
	}
}

func newTestHeader(parent *types.Header) *types.Header {
	return &types.Header{
		ParentHash:  parent.Hash(),
		UncleHash:   types.CalcUncleHash(nil),
		Coinbase:    common.HexToAddress("0x4909fcaf28f539a0307a306d2788174aa733d911"),
		Root:        common.HexToHash("0x01"),
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Difficulty:  big.NewInt(2),
		Number:      big.NewInt(1),
		GasLimit:    30_000_000,
		GasUsed:     0,
		Time:        parent.Time + 3,
		Extra:       make([]byte, 32+65),
	}
}

// TestSealHashShape pins the exact shape of the sealed header encoding: 14
// fields, with Extra and the chain id left out. This is the single invariant that
// has to match pgp-chain byte for byte, because it is what the PBFT confirm
// signatures were produced over.
func TestSealHashShape(t *testing.T) {
	header := newTestHeader(&types.Header{Number: big.NewInt(0), Time: 1762218000})

	enc, err := rlp.EncodeToBytes(sigHeaderFields(header))
	if err != nil {
		t.Fatalf("failed to encode the sig header: %v", err)
	}
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(enc, &fields); err != nil {
		t.Fatalf("failed to decode the sig header: %v", err)
	}
	if len(fields) != 14 {
		t.Fatalf("sig header field count mismatch: have %d, want 14", len(fields))
	}
	// Tie the shape above to the production encoder, otherwise this test only
	// checks its own helper and would keep passing if SealHash changed.
	if got, want := SealHash(header), crypto.Keccak256Hash(enc); got != want {
		t.Fatalf("SealHash does not hash the 14 field reference encoding: have %s, want %s", got, want)
	}
}

// pgpReferenceSealHash is a verbatim reimplementation of pgp-chain's
// encodeSigHeader (pgp-chain/consensus/pbft/pbft.go:744-765) followed by a
// keccak256. It is deliberately duplicated here instead of calling SealHash:
// field ordering is the one invariant a regression would be nearly impossible to
// diagnose from a syncing node, so the test has to compare two independent
// encoders rather than trust the one under test.
func pgpReferenceSealHash(h *types.Header) common.Hash {
	var buf bytes.Buffer
	if err := rlp.Encode(&buf, sigHeaderFields(h)); err != nil {
		panic("pgp: can't encode the reference sig header: " + err.Error())
	}
	return crypto.Keccak256Hash(buf.Bytes())
}

// goldenSealHeader returns a header whose 14 sealed fields all hold distinct,
// non-zero values, so that any dropped or reordered field changes the hash.
func goldenSealHeader() *types.Header {
	seq := func(fill byte, n int) []byte { return bytes.Repeat([]byte{fill}, n) }
	bloom := types.Bloom{}
	copy(bloom[:], seq(0x77, types.BloomByteLength))
	nonce := types.BlockNonce{}
	copy(nonce[:], seq(0x88, 8))

	return &types.Header{
		ParentHash:  common.BytesToHash(seq(0x11, 32)),
		UncleHash:   common.BytesToHash(seq(0x22, 32)),
		Coinbase:    common.BytesToAddress(seq(0x33, 20)),
		Root:        common.BytesToHash(seq(0x44, 32)),
		TxHash:      common.BytesToHash(seq(0x55, 32)),
		ReceiptHash: common.BytesToHash(seq(0x66, 32)),
		Bloom:       bloom,
		Difficulty:  big.NewInt(2),
		Number:      big.NewInt(12345),
		GasLimit:    30_000_000,
		GasUsed:     21000,
		Time:        1762218000,
		Extra:       seq(0x99, 32+65),
		MixDigest:   common.BytesToHash(seq(0xaa, 32)),
		Nonce:       nonce,
	}
}

// wantGoldenSealHash was produced by the pgp-chain reference encoder
// (pgpReferenceSealHash), not by SealHash, so it pins the exact field order
// independently of the implementation under test.
const wantGoldenSealHash = "0x288fa8a3218b55bd48ef2e0c477075bcfd1ed4575c21ffed7716c2af57a0dabc"

// TestSealHashGolden is the regression guard for the field order. A swapped pair
// (e.g. Time and GasUsed) keeps the field count at 14 and still makes every
// single-field mutation change the hash, so the other seal hash tests cannot
// catch it. This one can.
func TestSealHashGolden(t *testing.T) {
	header := goldenSealHeader()

	ref := pgpReferenceSealHash(header)
	if got := SealHash(header); got != ref {
		t.Fatalf("SealHash diverged from the pgp-chain reference encoder: have %s, want %s", got, ref)
	}
	if ref.Hex() != wantGoldenSealHash {
		t.Fatalf("pinned seal hash is stale: reference encoder produces %s, pinned value is %s", ref.Hex(), wantGoldenSealHash)
	}
}

// TestSealHashIgnoresExtra makes sure the ELA producer payload never leaks into
// the seal hash: pgp-chain comments header.Extra out of encodeSigHeader.
func TestSealHashIgnoresExtra(t *testing.T) {
	parent := &types.Header{Number: big.NewInt(0), Time: 1762218000}

	short := newTestHeader(parent)
	long := newTestHeader(parent)
	long.Extra = append(long.Extra, bytes.Repeat([]byte{0xab}, 128)...)

	if SealHash(short) != SealHash(long) {
		t.Fatal("SealHash depends on header.Extra, PBFT signatures can no longer be replayed")
	}
}

// TestSealHashCoversEveryField makes sure no sealed field was dropped by mistake:
// mutating any of the 14 fields must change the hash.
func TestSealHashCoversEveryField(t *testing.T) {
	parent := &types.Header{Number: big.NewInt(0), Time: 1762218000}
	base := SealHash(newTestHeader(parent))

	mutations := map[string]func(*types.Header){
		"ParentHash":  func(h *types.Header) { h.ParentHash = common.HexToHash("0x02") },
		"UncleHash":   func(h *types.Header) { h.UncleHash = common.HexToHash("0x02") },
		"Coinbase":    func(h *types.Header) { h.Coinbase = common.HexToAddress("0x02") },
		"Root":        func(h *types.Header) { h.Root = common.HexToHash("0x02") },
		"TxHash":      func(h *types.Header) { h.TxHash = common.HexToHash("0x02") },
		"ReceiptHash": func(h *types.Header) { h.ReceiptHash = common.HexToHash("0x02") },
		"Bloom":       func(h *types.Header) { h.Bloom[0] = 0x01 },
		"Difficulty":  func(h *types.Header) { h.Difficulty = big.NewInt(3) },
		"Number":      func(h *types.Header) { h.Number = big.NewInt(2) },
		"GasLimit":    func(h *types.Header) { h.GasLimit = 30_000_001 },
		"GasUsed":     func(h *types.Header) { h.GasUsed = 21_000 },
		"Time":        func(h *types.Header) { h.Time = 1762218100 },
		"MixDigest":   func(h *types.Header) { h.MixDigest = common.HexToHash("0x02") },
		"Nonce":       func(h *types.Header) { h.Nonce[0] = 0x01 },
	}
	for name, mutate := range mutations {
		header := newTestHeader(parent)
		mutate(header)
		if SealHash(header) == base {
			t.Fatalf("%s does not participate in the seal hash", name)
		}
	}
}

// TestVerifyHeaderRejects pins every rule the legacy history relies on. Each case
// must fail for its own reason.
func TestVerifyHeaderRejects(t *testing.T) {
	genesis := &types.Header{Number: big.NewInt(0), Time: 1762218000}
	chain := &testChain{genesis: genesis}

	cases := []struct {
		name   string
		config *params.LegacyConfig
		mutate func(*types.Header)
		want   error
	}{
		{"genesis", nil, func(h *types.Header) { h.Number = big.NewInt(0) }, errUnknownBlock},
		{"difficulty-1", nil, func(h *types.Header) { h.Difficulty = big.NewInt(1) }, errInvalidDifficulty},
		{"difficulty-nil", nil, func(h *types.Header) { h.Difficulty = nil }, errInvalidDifficulty},
		{"mixdigest", nil, func(h *types.Header) { h.MixDigest = common.HexToHash("0x01") }, errInvalidMixDigest},
		{"uncle-hash", nil, func(h *types.Header) { h.UncleHash = common.HexToHash("0x01") }, errInvalidUncleHash},
		{"gas-used", nil, func(h *types.Header) { h.GasUsed = h.GasLimit + 1 }, errInvalidGasUsed},
		{"short-extra", nil, func(h *types.Header) { h.Extra = make([]byte, 32+64) }, errInvalidExtra},
		{"period", nil, func(h *types.Header) { h.Time = genesis.Time + 2 }, errInvalidTimestamp},
		{"future", nil, func(h *types.Header) { h.Time = uint64(^uint64(0) >> 1) }, consensus.ErrFutureBlock},
		{"seal", &params.LegacyConfig{VerifySeal: true}, func(h *types.Header) {}, ErrSealNotImplemented},
	}
	for _, c := range cases {
		header := newTestHeader(genesis)
		c.mutate(header)

		err := New(c.config).VerifyHeader(chain, header)
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: have error %v, want %v", c.name, err, c.want)
		}
	}
}

// TestVerifyHeaderAccepts makes sure a well formed legacy header passes.
func TestVerifyHeaderAccepts(t *testing.T) {
	genesis := &types.Header{Number: big.NewInt(0), Time: 1762218000}
	chain := &testChain{genesis: genesis}

	if err := New(&params.LegacyConfig{}).VerifyHeader(chain, newTestHeader(genesis)); err != nil {
		t.Fatalf("valid legacy header rejected: %v", err)
	}
}

// TestVerifyHeaderRulesWithoutHappyPath covers the remaining legacy rules that
// have no case in TestVerifyHeaderRejects. Every one of them would silently let
// an invalid block through if the check were dropped, so each needs its own
// negative assertion.
func TestVerifyHeaderRulesWithoutHappyPath(t *testing.T) {
	genesis := &types.Header{Number: big.NewInt(0), Time: 1762218000}
	chain := &testChain{genesis: genesis}
	engine := New(&params.LegacyConfig{})

	t.Run("gas-limit-too-high", func(t *testing.T) {
		header := newTestHeader(genesis)
		header.GasLimit = maxGasLimit + 1
		if err := engine.VerifyHeader(chain, header); !errors.Is(err, errInvalidGasLimit) {
			t.Fatalf("have error %v, want %v", err, errInvalidGasLimit)
		}
	})
	t.Run("unknown-ancestor", func(t *testing.T) {
		header := newTestHeader(genesis)
		header.Number = big.NewInt(5)                        // no such parent in the test chain
		header.ParentHash = common.BytesToHash([]byte{0x01}) // and the hash points nowhere either
		if err := engine.VerifyHeader(chain, header); !errors.Is(err, consensus.ErrUnknownAncestor) {
			t.Fatalf("have error %v, want %v", err, consensus.ErrUnknownAncestor)
		}
	})
	t.Run("uncles-not-allowed", func(t *testing.T) {
		block := types.NewBlockWithHeader(newTestHeader(genesis)).WithBody(
			types.Body{Uncles: []*types.Header{{Number: big.NewInt(1)}}},
		)
		// VerifyUncles never reads the chain, it only inspects the block body.
		if err := engine.VerifyUncles(nil, block); !errors.Is(err, errUnclesNotAllowed) {
			t.Fatalf("have error %v, want %v", err, errUnclesNotAllowed)
		}
		// A block without uncles must still be accepted.
		if err := engine.VerifyUncles(nil, types.NewBlockWithHeader(newTestHeader(genesis))); err != nil {
			t.Fatalf("uncle-less block rejected: %v", err)
		}
	})
	t.Run("requests-not-allowed", func(t *testing.T) {
		header := newTestHeader(genesis)
		header.RequestsHash = &common.Hash{}
		if err := engine.VerifyRequests(header, [][]byte{{0x01}}); !errors.Is(err, ErrRequestsNotAllowed) {
			t.Fatalf("have error %v, want %v", err, ErrRequestsNotAllowed)
		}
		// Each field is rejected on its own. Both are unreachable for a valid
		// legacy block (15 field headers carry no RequestsHash and postExecution
		// only fills the request list when Prague is active), so a lone field
		// means the block does not belong to the legacy chain.
		if err := engine.VerifyRequests(header, nil); !errors.Is(err, ErrRequestsNotAllowed) {
			t.Fatalf("header with RequestsHash but no requests was accepted, want %v", ErrRequestsNotAllowed)
		}
		if err := engine.VerifyRequests(newTestHeader(genesis), [][]byte{{0x01}}); !errors.Is(err, ErrRequestsNotAllowed) {
			t.Fatalf("requests without RequestsHash were accepted, want %v", ErrRequestsNotAllowed)
		}
		// A plain legacy header must still pass.
		if err := engine.VerifyRequests(newTestHeader(genesis), nil); err != nil {
			t.Fatalf("plain legacy header rejected: %v", err)
		}
	})
}

// TestProductionNotSupported makes sure the read-only engine never pretends it
// produced a block.
func TestProductionNotSupported(t *testing.T) {
	e := New(&params.LegacyConfig{})
	if err := e.Seal(nil, types.NewBlockWithHeader(newTestHeader(&types.Header{})), nil, nil); err == nil {
		t.Fatal("Seal must fail on the read-only legacy engine")
	}
}

// TestFinalizeRootsOnly pins the exact scope of Finalize: commit the state root
// and clear the uncle list, and nothing else.
//
// The 65:35 fee split deliberately lives in the per transaction state
// transition (pgp-chain/core/state_transition.go:331-352, mirrored in
// core/state_transition.go), so a Finalize that moved any balance would double
// count it. The producer balance assertion is what stops a future refactor from
// quietly moving the split back here, which would keep every receipt and the
// state root valid while silently inflating the producer's balance.
func TestFinalizeRootsOnly(t *testing.T) {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("failed to create state: %v", err)
	}
	header := newTestHeader(&types.Header{})
	header.Root = common.Hash{}
	header.UncleHash = common.HexToHash("0xdeadbeef") // must be overwritten

	// The producer is the header coinbase, i.e. exactly the account a
	// re-introduced fee split would credit.
	producer := header.Coinbase
	deposit := uint256.NewInt(1_000_000)
	statedb.AddBalance(producer, deposit, tracing.BalanceChangeUnspecified)
	wantRoot := statedb.IntermediateRoot(true)

	var (
		txs         []*types.Transaction
		receipts    []*types.Receipt
		systemTxs   []*types.Transaction
		withdrawals []*types.Withdrawal
		usedGas     uint64
	)
	engine := New(&params.LegacyConfig{})
	if err := engine.Finalize(&testChain{}, header, statedb, &txs, nil, withdrawals, &receipts, &systemTxs, &usedGas, nil); err != nil {
		t.Fatalf("Finalize returned %v, want nil", err)
	}
	if header.Root != wantRoot {
		t.Fatalf("header root %v, want %v", header.Root, wantRoot)
	}
	if want := types.CalcUncleHash(nil); header.UncleHash != want {
		t.Fatalf("header uncle hash %v, want %v", header.UncleHash, want)
	}
	if got := statedb.GetBalance(producer); got.Cmp(deposit) != 0 {
		t.Fatalf("producer balance %v, want it untouched at %v (fee split belongs to the state transition)", got, deposit)
	}
}
