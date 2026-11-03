// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/spv"
	"github.com/ethereum/go-ethereum/trie"
)

// Blocks produced by this client on the pgp legacy rules carry a header bloom
// that mixes the sender and the recipient of every transaction into the
// receipt's logs (pgp-chain/core/types/bloom9.go:104-134). The block producer
// assembles that bloom one way (core/types/block.go:327, MergeBloom over the
// per-receipt blooms) and every validator recomputes it another
// (core/block_validator.go:176, CreateBloomWithTxList). If those two disagree the
// producer is the only node that accepts its own block: devnet run 3 accepted
// block 15 on node0 and dropped it on the other five with
//
//	invalid bloom (remote: 000…000 local: …0400…0080…)
//
// The tests below reproduce that as InsertChain returning an "invalid bloom"
// error, i.e. at the level the symptom actually appeared, rather than by
// comparing bloom values in isolation.
//
// WHY THE ASSERTIONS LOOK THE WAY THEY DO
//
//   - The interesting cases are the ones whose receipts have NO logs. A legacy
//     receipt's bloom is non-empty even then, because it also carries from/to.
//     A fixture where every receipt logs something cannot tell the two
//     implementations apart, which is why core/types/bloom9_test.go's benchmark
//     fixtures never caught this.
//   - Bloom values are compared with == (byte for byte) and never with
//     Bloom.Test(addr). A bloom that is missing bits can still answer Test()
//     true for a particular address, so a Test()-based assertion passes on a
//     partially wrong bloom — exactly the shape of this bug when only some of a
//     block's transactions emit logs.
//   - Every case states whether it must pass BEFORE the fix ("control"), so a
//     green run cannot be explained by the fixture having drifted into a
//     segment the bug never touched.

// legacyBloomEmitterAddr holds a contract whose runtime code emits one LOG0 and
// stops: PUSH1 0x00 PUSH1 0x00 LOG0 STOP. It exists so a transaction can be made
// to produce exactly one log without deploying anything mid-test.
var legacyBloomEmitterAddr = common.HexToAddress("0x00000000000000000000000000000000000000e1")

var legacyBloomEmitterCode = []byte{
	0x60, 0x00, // PUSH1 0x00  (offset)
	0x60, 0x00, // PUSH1 0x00  (size)
	0xa0, // LOG0
	0x00, // STOP
}

// legacyBloomTestChainID is a chain ID of its own, not pgp-chain mainnet's
// (860621). Reusing the mainnet ID would trip core/genesis.go's
// checkPGPDatabaseMatch as soon as the genesis is not the built-in mainnet one,
// which is a guard worth keeping rather than working around: these tests build a
// small chain, they are not replaying mainnet.
var legacyBloomTestChainID = big.NewInt(8606211)

// legacyBloomConfig returns a copy of the pgp legacy chain config, so a test can
// flip single fields without touching the process-wide params.PGPChainConfig.
// ForkHeight is math.MaxUint64 in the copy, i.e. every block produced here is a
// legacy block, which the tests assert per case rather than assume.
func legacyBloomConfig() *params.ChainConfig {
	legacy := *params.PGPChainConfig.Legacy
	cfg := *params.PGPChainConfig
	cfg.ChainID = new(big.Int).Set(legacyBloomTestChainID)
	cfg.Legacy = &legacy
	return &cfg
}

// standardBloomConfig is the same chain with the legacy rules switched off. It
// is the control for the direction of the fix: the same transactions on the same
// heights must keep getting a logs-only bloom, because that is what every
// non-legacy chain (Parlia included) requires.
func standardBloomConfig() *params.ChainConfig {
	cfg := legacyBloomConfig()
	cfg.Legacy = nil
	return cfg
}

// legacyBloomChain builds a chain on top of a genesis that funds the sender and
// deploys the log emitter. The block produced by assembleLegacyBlock is block 1,
// which is before every fork that would need a base fee or withdrawals, so the
// header shape stays the one the legacy producer actually writes
// (miner/worker.go:1145-1153 deliberately leaves BaseFee out).
type legacyBloomChain struct {
	chain  *BlockChain
	engine consensus.Engine
	cfg    *params.ChainConfig
}

// newLegacyBloomChain funds every sender in senders on top of a genesis that also
// deploys the log emitter.
func newLegacyBloomChain(t *testing.T, cfg *params.ChainConfig, senders ...common.Address) *legacyBloomChain {
	t.Helper()

	alloc := GenesisAlloc{
		legacyBloomEmitterAddr: {Code: legacyBloomEmitterCode, Balance: new(big.Int)},
	}
	for _, sender := range senders {
		alloc[sender] = types.Account{Balance: new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))}
	}
	genesis := &Genesis{
		Config:   cfg,
		GasLimit: 30_000_000,
		Alloc:    alloc,
	}
	// ethash.NewFaker on purpose. The bloom check is config driven —
	// core/block_validator.go:175-179 picks the formula from IsPGPLegacy, not from
	// the engine — so a real pgp.Engine would add scenery (an Extra long enough for
	// a PBFT confirm, consensus/pgp/confirm.go:56, and LegacyConfig.VerifySeal off)
	// without testing any more of the bloom code, and a failure in that scenery
	// reads exactly like the bloom bug.
	//
	// No *legacy* engine choice can change the bloom here:
	// pgp.Engine.FinalizeAndAssemble and core.AssembleBlock's own fallback both end
	// in types.NewBlock (core/types/block.go:327, MergeBloom over the per-receipt
	// blooms), and the per-receipt bloom comes from the chain config
	// (core/block_validator.go:175-179, DeriveFields), not from the engine. A
	// Parlia assembler would change it — it appends system transactions in
	// systemTxMining mode (consensus/parlia/parlia.go:1617-1623) — but this
	// fixture is not Parlia.
	//
	// Which assembly branch ethash reaches: Ethash has no FinalizeAndAssemble
	// (consensus/ethash/consensus.go:522 accumulates rewards and never touches the
	// bloom), so core.AssembleBlock takes its own fallback at
	// core/state_processor.go:567-571 instead of the blockAssembler branch above
	// it. Nothing in this file asserts anything about the router. The delegation is
	// pinned by consensus/router/block_assembler_assert_test.go (compile time) and
	// TestFinalizeAndAssembleWithoutAssemblerFailsLoudly (runtime);
	// router_legacy_bloom_test.go pins the bloom behaviour of that path, not the
	// identity of the function behind it.
	engine := ethash.NewFaker()
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, DefaultConfig())
	if err != nil {
		t.Fatalf("cannot create the chain: %v", err)
	}
	t.Cleanup(chain.Stop)

	if got := chain.CurrentBlock().Number.Uint64(); got != 0 {
		t.Fatalf("fixture problem: the chain starts at block %d, not genesis", got)
	}
	return &legacyBloomChain{chain: chain, engine: engine, cfg: cfg}
}

// installLegacyBloomRechargeStore installs the process-wide SPV store that
// legacy block execution requires.
//
// core/state_processor.go:131-136 calls spv.InstallHeightView for every legacy
// block, and that answers ErrRechargeStoreNotInitialised when SpvDbInit has not
// run. Without this the block is rejected long before the bloom check and the
// test would be measuring the wrong thing.
func installLegacyBloomRechargeStore(t *testing.T) {
	t.Helper()

	prevStore := spv.GetRechargeStore()
	prevView := spv.CurrentRechargeView()
	t.Cleanup(func() {
		spv.UninstallHeightView()
		spv.SetRechargeStore(prevStore)
		spv.SetStateView(prevView)
	})
	spv.SetRechargeStore(spv.NewRechargeStore(rawdb.NewMemoryDatabase()))
}

// assembleLegacyBlock builds the block the miner would build for txs, in the
// same order and through the same entry points:
//
//	ApplyTransaction per transaction, with the asynchronous bloom generator the
//	miner uses (miner/worker.go:922) — NOT GenerateChain, which goes through the
//	synchronous generator and the chain maker's own header preparation;
//	then core.AssembleBlock (miner/worker.go:1776).
//
// It returns the block and the receipts the producer hands to its sealer, so a
// test can compare the two blooms that have to agree.
func (lc *legacyBloomChain) assembleLegacyBlock(t *testing.T, txs types.Transactions) (*types.Block, types.Receipts) {
	t.Helper()

	parent := lc.chain.CurrentBlock()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		GasLimit:   parent.GasLimit,
		Time:       parent.Time + 1,
		Coinbase:   common.Address{0x01},
		Difficulty: lc.engine.CalcDifficulty(lc.chain, parent.Time+1, parent),
	}
	// The legacy producer writes no base fee (miner/worker.go:1151) and no
	// withdrawals hash, so a config that activated London at this height would be
	// a fixture bug: the header would no longer be the shape being tested.
	if lc.cfg.IsLondon(header.Number) {
		t.Fatalf("fixture problem: block %d has London active, the legacy header would need a base fee", header.Number.Uint64())
	}
	statedb, err := lc.chain.StateAt(parent)
	if err != nil {
		t.Fatalf("cannot open the parent state: %v", err)
	}
	gp := NewGasPool(header.GasLimit)
	evm := vm.NewEVM(NewEVMBlockContext(header, lc.chain, &header.Coinbase), statedb, lc.cfg, vm.Config{})

	// Capacity must cover every transaction, otherwise Apply blocks on the
	// channel while this goroutine is still feeding it.
	blooms := NewAsyncReceiptBloomGenerator(len(txs) + 1)
	receipts := make(types.Receipts, 0, len(txs))
	for i, tx := range txs {
		statedb.SetTxContext(tx.Hash(), i)
		receipt, err := ApplyTransaction(evm, gp, statedb, header, tx, blooms)
		if err != nil {
			blooms.Close()
			t.Fatalf("transaction %d was rejected by the legacy rules: %v", i, err)
		}
		receipts = append(receipts, receipt)
	}
	// The miner closes its generator when fillTransactions returns, i.e. before
	// AssembleBlock reads the blooms, so the values are final by then.
	blooms.Close()
	header.GasUsed = gp.Used()

	body := types.Body{Transactions: txs}
	block, receipts, err := AssembleBlock(lc.engine, lc.chain, header, statedb, &body, receipts)
	if err != nil {
		t.Fatalf("cannot assemble the block: %v", err)
	}
	return block, receipts
}

// insert asserts the block is accepted by this chain. The error is returned
// rather than fataled so the caller can assert on its text.
func (lc *legacyBloomChain) insert(block *types.Block) error {
	_, err := lc.chain.InsertChain([]*types.Block{block})
	return err
}

// loglessReceipts counts the receipts that carry no logs. Every interesting case
// below has at least one, and the number is asserted per case: a case that claims
// to exercise the merge of a log-less receipt but no longer has one would pass
// before the fix as well and prove nothing.
func loglessReceipts(receipts types.Receipts) int {
	n := 0
	for _, r := range receipts {
		if len(r.Logs) == 0 {
			n++
		}
	}
	return n
}

// orBloom merges src into dst. Used to spell out the expected bloom instead of
// deriving it from the function under test.
func orBloom(dst *types.Bloom, src types.Bloom) {
	for i := range dst {
		dst[i] |= src[i]
	}
}

// legacyFastPathMerge reproduces the merge that shipped before the fix: it skips
// every receipt that carries no logs.
//
// It exists only so each case can prove it is able to tell the two
// implementations apart. A case whose bloom comes out the same with and without
// the fast path would have passed before the fix as well, and would then be
// evidence of nothing — that is exactly how the old benchmark fixtures (every
// receipt with two logs) managed to look healthy for so long. Two traps this
// catches that reasoning alone does not:
//
//   - a block whose log-less transactions all come from the sender that also
//     logged something: the from/to bits are already in the merge, so skipping
//     them changes nothing;
//   - an accidental collision between the bits of different receipts.
func legacyFastPathMerge(receipts types.Receipts) types.Bloom {
	var bin types.Bloom
	for _, r := range receipts {
		if len(r.Logs) != 0 {
			orBloom(&bin, r.Bloom)
		}
	}
	return bin
}

// TestLegacyBlockBloomAcceptedByImport is the block-level shape of the devnet
// failure: the producer assembles a block, then hands it to the same validation
// path a peer would use.
//
// The assertion is InsertChain returning nil. "The bloom is not zero" is
// deliberately not the assertion: it holds for some of the broken shapes below
// (a block where only one transaction logs gets a partially filled bloom) and so
// cannot tell a real fix from a partial one.
func TestLegacyBlockBloomAcceptedByImport(t *testing.T) {
	installLegacyBloomRechargeStore(t)

	// Two accounts on purpose. The log-less transactions must come from an account
	// that does not also appear as the sender of a logging transaction: otherwise
	// its from bits are already in the merge and skipping its receipt costs
	// nothing, which would make the case unable to tell the two merges apart.
	// legacyFastPathMerge below is what turns that reasoning into an assertion.
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	secondKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate the second key: %v", err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	secondSender := crypto.PubkeyToAddress(secondKey.PublicKey)
	signer := types.FrontierSigner{}

	signWith := func(t *testing.T, k *ecdsa.PrivateKey, tx *types.Transaction) *types.Transaction {
		t.Helper()
		signed, err := types.SignTx(tx, signer, k)
		if err != nil {
			t.Fatalf("failed to sign the transaction: %v", err)
		}
		return signed
	}
	// A call into the emitter: the recipient has code, the transaction returns
	// successfully, and the receipt carries exactly one log.
	loggingCall := func(t *testing.T, nonce uint64) *types.Transaction {
		return signWith(t, key, types.NewTransaction(nonce, legacyBloomEmitterAddr, new(big.Int), 200_000, big.NewInt(1), nil))
	}
	// A plain value transfer from the second account: no code, no logs.
	transfer := func(t *testing.T, nonce uint64) *types.Transaction {
		return signWith(t, secondKey, types.NewTransaction(nonce, common.HexToAddress("0x00000000000000000000000000000000000000aa"), big.NewInt(1), 21000, big.NewInt(1), nil))
	}
	// A contract creation from the second account that emits nothing: the
	// constructor is empty, so the runtime code is empty and the receipt has no
	// logs. Only the sender is mixed into its bloom, which is the half of
	// CreateBloomWithTx a plain transfer cannot exercise.
	creation := func(t *testing.T, nonce uint64) *types.Transaction {
		return signWith(t, secondKey, types.NewContractCreation(nonce, new(big.Int), 100_000, big.NewInt(1), nil))
	}

	tests := []struct {
		name string
		// txs are built per case so nonces stay contiguous per subtest.
		txs func(t *testing.T) types.Transactions
		// wantLogless is asserted on the receipts the producer holds.
		wantLogless int
		// control marks a case that must pass with and without the fix, i.e. a
		// case the bug could never reach.
		control bool
	}{
		{
			// The shape of the reported incident: one transaction with logs and one
			// without. Only the first used to contribute, so the header bloom came
			// out missing the second account's from/to bits — a partially filled
			// bloom, not a zero one.
			name:        "one logging call plus one plain transfer",
			txs:         func(t *testing.T) types.Transactions { return types.Transactions{loggingCall(t, 0), transfer(t, 0)} },
			wantLogless: 1,
		},
		{
			// Block 15 of the devnet run: no transaction emits a log, so the old
			// fast path dropped every receipt and the header bloom was all zero.
			name:        "no transaction emits a log",
			txs:         func(t *testing.T) types.Transactions { return types.Transactions{transfer(t, 0), creation(t, 1)} },
			wantLogless: 2,
		},
		{
			// Mixed shapes: a log-less creation (sender only, no recipient) in front
			// of a logging call from a different account. Exercises both halves of
			// CreateBloomWithTx.
			name:        "log-less creation plus logging call",
			txs:         func(t *testing.T) types.Transactions { return types.Transactions{creation(t, 0), loggingCall(t, 0)} },
			wantLogless: 1,
		},
		{
			// The same two transactions in the other order, so a fix that only
			// handled "the first receipt" or "the last receipt" cannot pass.
			name:        "plain transfer before the logging call",
			txs:         func(t *testing.T) types.Transactions { return types.Transactions{transfer(t, 0), loggingCall(t, 0)} },
			wantLogless: 1,
		},
		{
			// Control: every transaction logs, so the old fast path never fired and
			// this case passed before the fix too. If it ever goes red the fix has
			// broken the merge that always worked.
			name:        "every transaction emits a log",
			txs:         func(t *testing.T) types.Transactions { return types.Transactions{loggingCall(t, 0), loggingCall(t, 1)} },
			wantLogless: 0,
			control:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lc := newLegacyBloomChain(t, legacyBloomConfig(), sender, secondSender)
			txs := tt.txs(t)

			block, receipts := lc.assembleLegacyBlock(t, txs)

			// Fixture self-checks, before any assertion about the bloom.
			if !lc.cfg.IsPGPLegacy(block.Number()) {
				t.Fatalf("fixture problem: block %d is not in the legacy segment, the test proves nothing",
					block.NumberU64())
			}
			if got := loglessReceipts(receipts); got != tt.wantLogless {
				t.Fatalf("fixture problem: %d log-less receipts, want %d", got, tt.wantLogless)
			}
			for i, r := range receipts {
				if r.Bloom == types.EmptyBloom {
					t.Fatalf("fixture problem: legacy receipt %d has an empty bloom; MakeReceipt must have "+
						"mixed in the sender and the recipient", i)
				}
			}
			// The case has to be able to tell the fixed merge from the broken one.
			// Without this the suite could quietly rot into a set of fixtures that
			// pass both ways — which is what the old benchmark fixtures were.
			want := types.CreateBloomWithTxList(receipts, block.Transactions())
			if old := legacyFastPathMerge(receipts); tt.control && old != want {
				t.Fatalf("this is marked a control, but the pre-fix merge %x differs from %x, so it would have "+
					"failed before the fix too", old, want)
			} else if !tt.control && old == want {
				t.Fatalf("fixture problem: the pre-fix merge produces the same bloom as the fixed one (%x), "+
					"so this case cannot catch the bug", want)
			}

			// The symptom first: the producer's block has to survive the same
			// validation path a peer runs.
			if err := lc.insert(block); err != nil {
				// Name the failure mode explicitly: an "invalid bloom" here is the
				// devnet symptom, anything else is a fixture problem.
				if strings.Contains(err.Error(), "invalid bloom") {
					t.Fatalf("block %d was rejected with %q.\nThat is the devnet failure: the producer and "+
						"the validator disagree about the header bloom (produced %x, validator %x).",
						block.NumberU64(), err, block.Bloom(), want)
				}
				t.Fatalf("block %d was rejected: %v", block.NumberU64(), err)
			}
			if head := lc.chain.CurrentBlock(); head.Hash() != block.Hash() {
				t.Fatalf("chain head = %s, want the inserted block %s", head.Hash(), block.Hash())
			}

			// Then the diagnosis, in case the block was accepted for a reason the
			// bloom comparison would have caught more directly.
			if got := block.Bloom(); got != want {
				t.Fatalf("produced header bloom = %x\nvalidator formula      = %x", got, want)
			}
			// And the receipts trie: receipt.Bloom travels inside it as well
			// (core/types/receipt.go:380 encodes r.Bloom), so a change that only
			// corrected the header bloom would leave the receipt root wrong —
			// which is the same consensus break under another name. Peers report
			// that one as "invalid receipt root hash".
			if got, wantRoot := block.ReceiptHash(), types.DeriveSha(receipts, trie.NewStackTrie(nil)); got != wantRoot {
				t.Fatalf("produced receipt root = %x\nroot over the produced receipts = %x", got, wantRoot)
			}
		})
	}
}

// TestBlockBloomOnStandardRulesStaysLogsOnly is the reversal guard.
//
// The fix for the legacy bloom must not leak the from/to bits into the bloom of
// a chain that does not use the legacy rules — BSC/Parlia reads header.Bloom as
// the logs-only filter, and mixing accounts in would silently change every
// Parlia block hash. Same transactions, same heights, legacy rules off: the
// header bloom has to be exactly the merge of the logs-only blooms.
func TestBlockBloomOnStandardRulesStaysLogsOnly(t *testing.T) {
	installLegacyBloomRechargeStore(t)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	signer := types.FrontierSigner{}
	sign := func(t *testing.T, tx *types.Transaction) *types.Transaction {
		t.Helper()
		signed, err := types.SignTx(tx, signer, key)
		if err != nil {
			t.Fatalf("failed to sign the transaction: %v", err)
		}
		return signed
	}

	txs := types.Transactions{
		sign(t, types.NewTransaction(0, legacyBloomEmitterAddr, new(big.Int), 200_000, big.NewInt(1), nil)),
		sign(t, types.NewTransaction(1, common.HexToAddress("0x00000000000000000000000000000000000000aa"), big.NewInt(1), 21000, big.NewInt(1), nil)),
	}

	cfg := standardBloomConfig()
	lc := newLegacyBloomChain(t, cfg, sender)
	block, receipts := lc.assembleLegacyBlock(t, txs)

	if cfg.IsPGPLegacy(block.Number()) {
		t.Fatalf("fixture problem: block %d is still in the legacy segment", block.NumberU64())
	}
	if got := loglessReceipts(receipts); got != 1 {
		t.Fatalf("fixture problem: %d log-less receipts, want 1", got)
	}
	// The log-less receipt must contribute nothing at all on these rules.
	if receipts[1].Bloom != types.EmptyBloom {
		t.Fatalf("receipt 1 (a plain transfer) has bloom %x; on the standard rules a receipt's bloom is "+
			"derived from its logs alone and must stay empty", receipts[1].Bloom)
	}

	var want types.Bloom
	for _, r := range receipts {
		orBloom(&want, types.CreateBloom(r))
	}
	if got := block.Bloom(); got != want {
		t.Fatalf("header bloom = %x\nlogs-only merge = %x\nthe standard rules must not mix accounts in", got, want)
	}
	// And it must not be the legacy value either, otherwise the assertion above
	// could be satisfied by both formulas agreeing.
	if legacy := types.CreateBloomWithTxList(receipts, block.Transactions()); legacy == want {
		t.Fatal("fixture problem: the legacy and the standard bloom came out equal, so this case cannot " +
			"distinguish the two formulas")
	}
	if err := lc.insert(block); err != nil {
		t.Fatalf("block %d was rejected: %v", block.NumberU64(), err)
	}
}

// TestLegacyMakeReceiptBloomSurvivesTheProcessors is the second root cause,
// asserted where it happens rather than at the block level.
//
// core.MakeReceipt computes the legacy bloom and only then runs the receipt
// processors (core/state_processor.go:390-395). The asynchronous generator the
// miner and the importer use refuses to overwrite a bloom that is already set
// (core/receipt_processor.go:62-64), but the synchronous one used to overwrite it
// unconditionally with the logs-only CreateBloom. Any caller of the synchronous
// generator — core/chain_makers.go:128 and miner/bid_simulator.go:1398 — therefore
// produced receipts whose bloom disagreed with the header the legacy rules
// require.
func TestLegacyMakeReceiptBloomSurvivesTheProcessors(t *testing.T) {
	installLegacyBloomRechargeStore(t)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	sender := crypto.PubkeyToAddress(key.PublicKey)
	tx, err := types.SignTx(
		types.NewTransaction(0, common.HexToAddress("0x00000000000000000000000000000000000000aa"), big.NewInt(1), 21000, big.NewInt(1), nil),
		types.FrontierSigner{}, key)
	if err != nil {
		t.Fatalf("failed to sign the transaction: %v", err)
	}

	lc := newLegacyBloomChain(t, legacyBloomConfig(), sender)
	parent := lc.chain.CurrentBlock()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		GasLimit:   parent.GasLimit,
		Time:       parent.Time + 1,
		Coinbase:   common.Address{0x01},
		Difficulty: lc.engine.CalcDifficulty(lc.chain, parent.Time+1, parent),
	}
	if !lc.cfg.IsPGPLegacy(header.Number) {
		t.Fatalf("fixture problem: block %d is not a legacy block", header.Number.Uint64())
	}

	for _, processors := range []struct {
		name string
		p    ReceiptProcessor
	}{
		{"synchronous generator (core/chain_makers.go:128)", NewReceiptBloomGenerator()},
		{"asynchronous generator (miner/worker.go:922)", NewAsyncReceiptBloomGenerator(1)},
	} {
		t.Run(processors.name, func(t *testing.T) {
			if async, ok := processors.p.(*AsyncReceiptBloomGenerator); ok {
				defer async.Close()
			}
			// A fresh state and EVM per subtest: both runs send the same nonce-0
			// transaction, so sharing the state would reject the second one.
			statedb, err := lc.chain.StateAt(parent)
			if err != nil {
				t.Fatalf("cannot open the parent state: %v", err)
			}
			statedb.SetTxContext(tx.Hash(), 0)
			evm := vm.NewEVM(NewEVMBlockContext(header, lc.chain, &header.Coinbase), statedb, lc.cfg, vm.Config{})

			receipt, err := ApplyTransaction(evm, NewGasPool(header.GasLimit), statedb, header, tx, processors.p)
			if err != nil {
				t.Fatalf("the transaction was rejected: %v", err)
			}
			want := types.CreateBloomWithTx(receipt, tx)
			if receipt.Bloom == types.EmptyBloom {
				t.Fatal("the processor wiped the legacy bloom: this receipt has no logs, so a logs-only " +
					"recompute produces the zero bloom and the header bloom assembled from it is zero too")
			}
			if receipt.Bloom != want {
				t.Fatalf("receipt bloom = %x, want the legacy bloom %x: a processor must not overwrite the "+
					"value core.MakeReceipt computed", receipt.Bloom, want)
			}
		})
	}
}
