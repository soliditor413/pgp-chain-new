// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

// TestGenerateChainLegacyBloomMatchesTheValidator is the only test that pins
// BOTH halves of the legacy bloom defect on the plain GenerateChain path, and the
// only one that keeps them pinned separately.
//
// Half one is core/types/bloom9.go's MergeBloom, which used to skip every receipt
// without logs; half two is core/receipt_processor.go's synchronous
// ReceiptBloomGenerator, whose Apply overwrote the bloom core.MakeReceipt had just
// computed for a legacy block with the logs-only CreateBloom.
// core/chain_makers.go:128 is the caller of that synchronous generator, which is
// why GenerateChain is the second path (the miner uses the asynchronous one, see
// core/legacy_bloom_block_test.go) that produced blocks every peer rejected with
// "invalid bloom".
//
// # WHY THE ASSERTIONS SIT AT THE ASSEMBLY LAYER
//
// The end-to-end assertion (InsertChain returns nil) is below, but it is not the
// primary one. Reaching it with the real pgp engine would need three pieces of
// scenery that have nothing to do with bloom — Extra long enough for a PBFT
// confirm (consensus/pgp/confirm.go:56), LegacyConfig.VerifySeal off
// (params/pgp_config.go ships it on, and a zero-filled Extra cannot decode into a
// confirm), and an SPV recharge store installed
// (core/state_processor.go:131-136). Any one of those breaking would look like
// "the bloom bug is back".
//
// This test takes none of that scenery: it runs ethash.NewFaker (:101), so the list
// above is what a swap to the real engine would cost, not what this test
// requires. Note what is *not* on it: the bloom does not depend on which engine
// assembles the block, because every assembly path ends in types.NewBlock
// (core/types/block.go:327), so a bare *pgp.Engine and the height router over it
// merge the same blooms. The router was on this list for a different reason —
// on a real node the engine *is* the router, and the router used to run its own
// inline copy of core.AssembleBlock's fallback; giving pgp.Engine the assembler
// method is what let that copy be deleted.
//
// The two assertions on the assembled block need none of it and fail for exactly
// one reason each:
//
//	                          header.Bloom   header.ReceiptHash
//	MergeBloom only fixed         red              red
//	sync generator only fixed     red              green
//	both fixed                    green            green
//
// The middle row is the point: ReceiptHash is green there, so the two assertions
// are not restating each other. ReceiptHash is red in the first row because
// receipt.Bloom also travels inside the receipts trie (core/types/receipt.go:380,
// EncodeIndex encodes r.Bloom) — fixing only the header side of the merge leaves
// the trie built from wiped blooms, which is itself a consensus break.
func TestGenerateChainLegacyBloomMatchesTheValidator(t *testing.T) {
	installLegacyBloomRechargeStore(t)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)

	// A plain transfer: no code, no logs. That is what makes the receipt's bloom
	// carry information the logs-only recompute cannot reproduce, and it is the
	// exact shape of the transaction that took down the devnet.
	tx, err := types.SignTx(
		types.NewTransaction(0, common.HexToAddress("0x00000000000000000000000000000000000000aa"), big.NewInt(1), 21000, big.NewInt(1), nil),
		types.FrontierSigner{}, key)
	if err != nil {
		t.Fatalf("failed to sign the transaction: %v", err)
	}

	gspec := &Genesis{
		Config:   legacyBloomConfig(),
		GasLimit: 30_000_000,
		Alloc: GenesisAlloc{
			sender: {Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))},
		},
	}

	// ethash.NewFaker is deliberate, not a shortcut: it keeps the block
	// production and import machinery (gas accounting, receipt derivation, block
	// assembly, the block validator) while leaving out pgp's header scenery. The
	// bloom check itself is config driven — core/block_validator.go:175-179 picks
	// the formula from IsPGPLegacy, not from the engine — so swapping in a real
	// pgp.Engine here would add the three prerequisites listed above without
	// testing any more of the bloom code. See that comment before "making the test
	// more realistic".
	engine := ethash.NewFaker()
	db, blocks, receiptLists := GenerateChainWithGenesis(gspec, engine, 1, func(_ int, b *BlockGen) {
		b.AddTx(tx)
	})

	if len(blocks) != 1 || len(receiptLists) != 1 || len(receiptLists[0]) != 1 {
		t.Fatalf("fixture problem: generated %d blocks with %d receipt lists", len(blocks), len(receiptLists))
	}
	block, receipts := blocks[0], receiptLists[0]

	// Fixture self-checks. None of these can catch the bug; they only prove the
	// assertions below are aimed at a block the bug can reach.
	if !gspec.Config.IsPGPLegacy(block.Number()) {
		t.Fatalf("fixture problem: block %d is not in the legacy segment, the test proves nothing", block.NumberU64())
	}
	if len(receipts[0].Logs) != 0 {
		t.Fatalf("fixture problem: the receipt carries %d logs, so a logs-only bloom would not differ",
			len(receipts[0].Logs))
	}
	// GenerateChain re-derives the receipts after the block has been assembled
	// (core/chain_makers.go:509-527, Receipts.DeriveFields), so this receipt bloom
	// is the legacy one even before the fix. It is green either way and must not
	// be mistaken for the regression pin.
	if want := types.CreateBloomWithTx(receipts[0], tx); receipts[0].Bloom != want {
		t.Fatalf("fixture problem: the re-derived receipt bloom is %x, want the legacy bloom %x",
			receipts[0].Bloom, want)
	}

	// Pin one: the header bloom the chain maker produced must be the one the
	// validator recomputes. CreateBloomWithTxList never reads receipt.Bloom, it
	// rebuilds from logs + from/to, so it is the independent side of the
	// comparison.
	if got, want := block.Bloom(), types.CreateBloomWithTxList(receipts, block.Transactions()); got != want {
		t.Fatalf("generated header bloom = %x\nvalidator formula      = %x\n"+
			"the two sides of the legacy bloom disagree, so every peer rejects this block", got, want)
	}
	// Pin two: the receipts trie. This is the assertion that stays red when only
	// MergeBloom is fixed, because the block was hashed over the blooms the
	// synchronous generator had already overwritten with the logs-only value.
	if got, want := block.ReceiptHash(), types.DeriveSha(types.Receipts(receipts), trie.NewStackTrie(nil)); got != want {
		t.Fatalf("generated receipt root = %x\nroot from the returned receipts = %x\n"+
			"the block was hashed over receipts whose bloom was overwritten after MakeReceipt computed it", got, want)
	}

	// End to end, secondary: the same block has to survive the validation path a
	// peer runs. See the file comment for why this assertion is not the primary
	// one; with the fake engine it needs no scenery at all, and it is the shape the
	// devnet actually failed in.
	chain, err := NewBlockChain(db, gspec, engine, DefaultConfig())
	if err != nil {
		t.Fatalf("cannot create the chain: %v", err)
	}
	t.Cleanup(chain.Stop)

	if _, err := chain.InsertChain(blocks); err != nil {
		if strings.Contains(err.Error(), "invalid bloom") {
			t.Fatalf("block %d was rejected with %q.\nThat is the devnet failure, reached through "+
				"GenerateChain instead of the miner.", block.NumberU64(), err)
		}
		t.Fatalf("block %d was rejected: %v", block.NumberU64(), err)
	}
	if head := chain.CurrentBlock(); head.Hash() != block.Hash() {
		t.Fatalf("chain head = %s, want the inserted block %s", head.Hash(), block.Hash())
	}
}
