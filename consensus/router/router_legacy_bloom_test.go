// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package router_test

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/consensus/router"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

// TestFinalizeAndAssembleLegacyDelegationKeepsTheLegacyBloom covers the block
// assembly path a pgp node actually runs, which none of the core-side bloom
// tests reach.
//
// The chain of facts, in order:
//
//   - core.AssembleBlock prefers the optional blockAssembler interface and only
//     falls back to plain Finalize when the engine does not implement it
//     (core/state_processor.go:559-572).
//   - router.Engine implements FinalizeAndAssemble, so on a node wired with the
//     height router the delegation always happens (consensus/router/router.go,
//     FinalizeAndAssemble).
//   - For a legacy block the engine that answers is *pgp.Engine, which now
//     implements the interface itself (consensus/pgp/pgp.go, FinalizeAndAssemble),
//     so the router hands the block over instead of assembling it inline.
//
// The test therefore enters the delegation branch and exercises
// pgp.Engine.FinalizeAndAssemble — which is the code production executes, through
// the router. That makes it a behavioural test of the production assembly path
// rather than a test of a branch: it stays meaningful whether the delegation is
// direct or one hop, and it is the reason the router's own copy of the rule could
// be deleted without a replacement test.
//
// This test does not discriminate between delegation and the fallback that used
// to live in router.go. That was measured, not assumed: with
// pgp.Engine.FinalizeAndAssemble renamed away in a throwaway worktree (so the
// router fallback was taken), the test still passed; with the method present (so
// the router delegated), it still passed. What it pins is the bloom behaviour of
// the legacy assembly path, not the identity of the function that produces it.
//
// Reproducing the renamed-away half needs one more step than "rename the method":
// block_assembler_assert_test.go has to be moved aside in the same worktree,
// because its compile-time assertion stops this package from building at all, so
// the test never gets to run. Renaming alone gives a build failure, which is the
// assertion working, not the discrimination being measured.
//
// The delegation branch is what runs today. Measured the same way: an error
// injected as the first statement of pgp.Engine.FinalizeAndAssemble surfaced here
// ("DELEGATION-PROBE"), so the router is not taking any other path. The probe is
// not kept in the tree; the permanent guarantee is that the fallback no longer
// exists and router.go returns errNoBlockAssembler instead — see
// block_assembler_assert_test.go for the assertion that keeps it that way.
//
// It uses a real *pgp.Engine rather than a fake on purpose. The property being
// relied on is "the router delegates to the engine that owns the height": a fake
// would prove that the router can delegate to something, not that it delegates to
// the engine a node actually wires in. And because *pgp.Engine used to have no
// assembler of its own — deliberately, until this change made the method
// necessary — a fake would also have had to reproduce the absence of a method,
// which is not something a type assertion can be talked into.
//
// It is an external test package (package router_test) on purpose: router imports
// consensus/parlia, parlia imports the top-level core package, so a test inside
// package router could not import core to get at the assembly helper. Nothing
// here needs unexported state either, so the external package keeps the coupling
// honest.
//
// The receipts are built by hand, with the legacy bloom spelled out through
// CreateBloomWithTx, instead of being produced by core.MakeReceipt. That keeps
// the test aimed at the assembly step alone: if it went through MakeReceipt, a
// regression there could mask, or be masked by, a regression here.
func TestFinalizeAndAssembleLegacyDelegationKeepsTheLegacyBloom(t *testing.T) {
	cfg := &params.ChainConfig{
		ChainID: big.NewInt(1),
		Legacy: &params.LegacyConfig{
			ForkHeight: math.MaxUint64,
			VerifySeal: false,
		},
	}
	// forkHeight 0 means "no fork scheduled", so every height is legacy
	// (router.go:82-92, :139-145). A nil PoSA engine is allowed and is what a
	// pre-fork node has.
	legacy := pgp.New(cfg.Legacy)
	eng := router.New(legacy, nil, 0)

	txs, receipts := legacyBloomFixture(t)

	if len(txs) != len(receipts) {
		t.Fatalf("fixture problem: %d transactions against %d receipts", len(txs), len(receipts))
	}
	// Fixture self-checks. The interesting receipt is the one without logs: its
	// bloom is non-empty on the legacy rules because it carries the sender, and a
	// logs-only implementation cannot reproduce it.
	if len(receipts[0].Logs) != 0 {
		t.Fatalf("fixture problem: receipt 0 carries %d logs, it has to be the log-less one", len(receipts[0].Logs))
	}
	if receipts[0].Bloom == types.EmptyBloom {
		t.Fatal("fixture problem: the log-less receipt has an empty bloom, so a logs-only assembly would match it")
	}

	header := &types.Header{
		ParentHash: common.HexToHash("0x11"),
		Number:     big.NewInt(1),
		GasLimit:   30_000_000,
		Time:       1_700_000_000,
		Coinbase:   common.HexToAddress("0x00000000000000000000000000000000000000c0"),
		Difficulty: big.NewInt(1),
	}
	header.GasUsed = 21_000 + 30_000

	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("cannot open an empty state: %v", err)
	}
	chain := &bloomTestChain{config: cfg, header: header}

	body := &types.Body{Transactions: txs}
	block, outReceipts, err := eng.FinalizeAndAssemble(chain, header, statedb, body, receipts, nil)
	if err != nil {
		t.Fatalf("FinalizeAndAssemble failed: %v", err)
	}

	// The assertion that decides whether a peer accepts the block. The validator
	// rebuilds the header bloom from the logs plus the sender and recipient of
	// each transaction (core/block_validator.go:176, CreateBloomWithTxList) and
	// never looks at receipt.Bloom.
	wantBloom := types.CreateBloomWithTxList(receipts, txs)
	if got := block.Bloom(); got != wantBloom {
		t.Fatalf("assembled header bloom = %x\nvalidator formula      = %x\n"+
			"the assembly lost the from/to bits of the log-less receipt, which is the "+
			"\"invalid bloom\" rejections every peer would send back", got, wantBloom)
	}
	// Guards the fixture: if the logs-only value happened to equal the legacy one,
	// the assertion above would pass on a broken assembly too.
	var logsOnly types.Bloom
	for _, r := range receipts {
		for i := range logsOnly {
			logsOnly[i] |= types.CreateBloom(r)[i]
		}
	}
	if logsOnly == wantBloom {
		t.Fatalf("fixture problem: the logs-only bloom equals the legacy bloom (%x), so this case "+
			"cannot tell the two apart", logsOnly)
	}
	// The receipt bloom also enters the receipts trie (core/types/receipt.go:380,
	// EncodeIndex encodes r.Bloom), so an assembly that only fixed the header side
	// would still produce a block peers reject, under the other error message.
	//
	// On this fixture the assertion is tautological: NewBlock derives the root from
	// the very receipts handed in, and the assembly never mutates them, so it
	// cannot fail here. It is a tripwire for a future change that makes the
	// assembly touch receipt.Bloom, not evidence for today's fix. The assertion
	// that actually discriminates in this test is the bloom one above.
	if got, want := block.ReceiptHash(), types.DeriveSha(types.Receipts(receipts), trie.NewStackTrie(nil)); got != want {
		t.Fatalf("assembled receipt root = %x\nroot over the receipts handed in = %x", got, want)
	}
	// And the assembly has to carry the body through; an assembly that dropped
	// transactions would satisfy neither of the two assertions above for the right
	// reason.
	if got := block.Transactions().Len(); got != len(txs) {
		t.Fatalf("assembled block carries %d transactions, want %d", got, len(txs))
	}
	if len(outReceipts) != len(receipts) {
		t.Fatalf("assembled block returned %d receipts, want %d", len(outReceipts), len(receipts))
	}
}

// legacyBloomFixture returns one plain transfer (no logs) and one call that emits
// a single log, with the legacy bloom already written onto each receipt.
func legacyBloomFixture(t *testing.T) (types.Transactions, types.Receipts) {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	if sender == (common.Address{}) {
		t.Fatal("fixture problem: the recovered sender is the zero address")
	}

	transfer, err := types.SignTx(
		types.NewTransaction(0, common.HexToAddress("0x00000000000000000000000000000000000000aa"), big.NewInt(1), 21000, big.NewInt(1), nil),
		types.FrontierSigner{}, key)
	if err != nil {
		t.Fatalf("failed to sign the transfer: %v", err)
	}
	logging, err := types.SignTx(
		types.NewTransaction(1, common.HexToAddress("0x00000000000000000000000000000000000000e1"), new(big.Int), 30_000, big.NewInt(1), nil),
		types.FrontierSigner{}, key)
	if err != nil {
		t.Fatalf("failed to sign the logging call: %v", err)
	}

	receipts := types.Receipts{
		{
			Type:              transfer.Type(),
			Status:            types.ReceiptStatusSuccessful,
			CumulativeGasUsed: 21_000,
			TxHash:            transfer.Hash(),
			Logs:              []*types.Log{},
		},
		{
			Type:              logging.Type(),
			Status:            types.ReceiptStatusSuccessful,
			CumulativeGasUsed: 51_000,
			TxHash:            logging.Hash(),
			Logs: []*types.Log{{
				Address: common.HexToAddress("0x00000000000000000000000000000000000000e1"),
				Topics:  []common.Hash{common.HexToHash("0xdead")},
			}},
		},
	}
	txs := types.Transactions{transfer, logging}
	for i, tx := range txs {
		receipts[i].Bloom = types.CreateBloomWithTx(receipts[i], tx)
	}
	return txs, receipts
}

// bloomTestChain is the smallest consensus.ChainHeaderReader the assembly needs.
type bloomTestChain struct {
	config *params.ChainConfig
	header *types.Header
}

func (c *bloomTestChain) Config() *params.ChainConfig  { return c.config }
func (c *bloomTestChain) CurrentHeader() *types.Header { return c.header }
func (c *bloomTestChain) GenesisHeader() *types.Header { return c.header }
func (c *bloomTestChain) GetHighestVerifiedHeader() *types.Header {
	return c.header
}
func (c *bloomTestChain) GetVerifiedBlockByHash(common.Hash) *types.Header { return c.header }
func (c *bloomTestChain) GetHeaderByHash(common.Hash) *types.Header        { return c.header }
func (c *bloomTestChain) GetHeaderByNumber(uint64) *types.Header           { return c.header }
func (c *bloomTestChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if number == 1 && hash == c.header.Hash() {
		return c.header
	}
	return nil
}
func (c *bloomTestChain) GetTd(common.Hash, uint64) *big.Int { return big.NewInt(1) }
