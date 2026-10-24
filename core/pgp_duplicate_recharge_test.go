// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// The two sides of the duplicate-Recharge rule, on mainnet block 616157.
//
// That block carries two Recharge calls to ELAMINTER (0x…64) naming the same ELA
// transaction, from two different senders. On the real chain the first one
// succeeded and the second one FAILED — the block itself was valid, it imported,
// and every node agrees on it:
//
//	tx0 0x11896a6f…dff8b6 from 0x0b78D09a…31e864  Recharge  Success  fee 0
//	tx1 0xa7e53488…047c54 from 0x3A0b0Ba9…e574171  Recharge  Failed   fee 0.00148415
//	block gasUsed 29,683, gasLimit 200,000,000
//
// This test pins the local behaviour that makes that possible, because the two
// halves live in different files and either one can be "fixed" into a consensus
// split on its own:
//
//   - the HOST must not reject the block (commit 50697591b): the "already
//     completed" verdict is taken once per block against the pre-block state, so
//     a duplicate inside the block is not a reason to invalidate it;
//   - the CONTRACT then reverts the duplicate, and the execution half must let
//     that revert happen instead of turning it into a rejected block.
//
// A regression in either half does not corrupt the state quietly: it stops the
// node at block 616157 forever, or worse, imports a block the rest of the network
// considers invalid.
func TestDuplicateRechargeInOneBlockFailsInsteadOfRejectingTheBlock(t *testing.T) {
	block := readPGPBlock30115(t)
	installRechargeStore30115(t)

	txA := block.Transactions()[0]
	txB := duplicateRechargeTx(t, block.Header(), txA)

	// A is the chain's own transaction; B is the same deposit claimed by someone
	// else. Both must reach the contract: if the host filtered B out, the block
	// would be declared invalid, which is the 616157 bug coming back.
	statedb := pgpGenesisState(t)
	receipts := applyBlockTransactions(t, statedb, block, txA, txB)
	receiptA, receiptB := receipts[0], receipts[1]

	// ---- Half one: the duplicate is not the host's business ----
	//
	// Receipt A is the known-good baseline from TestReplayPGPBlock30115: the
	// gateway call succeeded and pgp-chain refunded its gas in full.
	if receiptA.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("the first Recharge failed; without it this test proves nothing about duplicates")
	}
	if receiptA.GasUsed != 0 {
		t.Fatalf("the first Recharge used %d gas, want 0 (pgp-chain refunds a successful Recharge in full)",
			receiptA.GasUsed)
	}

	// ---- Half two: the contract reverts it, and the revert is the right answer ----
	//
	// ELAMinter's completed[elaHash] mapping is already true when B runs, so the
	// call reverts. Two independent consequences are asserted, because they fail
	// for different reasons:
	//
	//   - the receipt says Failed, i.e. the block committed a failed transaction;
	//   - the gas was NOT refunded, because core/state_transition.go:717 only
	//     refunds a fully-executed Recharge. This is the same signal a replay
	//     driver uses to notice that the ELA data was missing, so it has to mean
	//     "the call failed in the VM" and nothing else.
	if receiptB.Status != types.ReceiptStatusFailed {
		t.Fatalf("the duplicate Recharge reported status %d, want %d (failed): the contract must revert it, "+
			"and a successful receipt here means the second call minted against the same deposit",
			receiptB.Status, types.ReceiptStatusFailed)
	}
	if receiptB.GasUsed == 0 {
		t.Fatal("the duplicate Recharge was refunded in full; the refund rule must only fire when vmerr == nil")
	}
	t.Logf("duplicate Recharge: status=%d gasUsed=%d (first Recharge: status=%d gasUsed=%d)",
		receiptB.Status, receiptB.GasUsed, receiptA.Status, receiptA.GasUsed)

	// The whole transaction is rewound, not just the call: pgp-chain takes the
	// pass-balance snapshot at the top of TransitionDb, before preCheck, so a
	// reverted Recharge gives back the nonce and the gas payment too, while the
	// receipt still reports the gas as used.
	//
	// This is the counterintuitive part and it is deliberate. Anyone who reads
	// "the fee was charged" off the receipt and decides the balance bookkeeping
	// must have been forgotten will be tempted to move the snapshot later or to
	// settle the fee separately — which changes the state root of every legacy
	// block that contains a failed Recharge, 616157 included.
	bSender := senderOf(t, txB, block.Header())
	if nonce := statedb.GetNonce(bSender); nonce != 0 {
		t.Fatalf("the reverted Recharge left nonce %d on %s, want 0: the snapshot predates preCheck, "+
			"so the nonce increment is rewound with everything else", nonce, bSender.Hex())
	}
	if balance := statedb.GetBalance(bSender); balance.Sign() != 0 {
		t.Fatalf("the reverted Recharge left %s wei on %s, want 0: the gas payment is rewound too, "+
			"even though the receipt reports %d gas used", balance, bSender.Hex(), receiptB.GasUsed)
	}

	// The first transaction's effect survives the second one's revert. If B's
	// rollback reached past its own snapshot it would erase A's write, the block
	// would import with the deposit unsettled, and a replay of the same block
	// would mint it a second time later.
	if got := statedb.GetState(params.ELAMINTER, params.CompletedSlot(common.HexToHash(elaHash30115))); got != params.CompletedSetValue {
		t.Fatalf("completed[%s] = %s after the revert, want %s: the successful Recharge must stay settled",
			elaHash30115, got.Hex(), params.CompletedSetValue.Hex())
	}

	// ---- The host-side rule itself, asserted directly ----
	//
	// This is the property commit 50697591b introduced, and it is worth pinning
	// independently of the receipts: judged against the PRE-BLOCK state, neither
	// transaction is "already settled", so the block is importable. Judged against
	// the running state (the old per-transaction check), the second would be true
	// and Process would return errLegacyTxAlreadyCompleted instead of receipts.
	before := pgpGenesisState(t)
	settled := legacySettledRecharges(params.PGPChainConfig, block.Number(), before, types.Transactions{txA, txB})
	if len(settled) != 2 {
		t.Fatalf("legacySettledRecharges returned %d verdicts for a 2-transaction block, want 2", len(settled))
	}
	if settled[0] || settled[1] {
		t.Fatalf("legacySettledRecharges = %v, want [false false]: both Recharges must be judged against the "+
			"block's opening state, where the ELA transaction is still unsettled", settled)
	}

	// ---- Determinism ----
	//
	// The failure of B depends on gas consumed up to the revert, so re-running the
	// same two transactions on a fresh copy of the same starting state has to
	// produce the same receipt. A difference here would mean the outcome depends on
	// something outside the block — a wall clock, a map iteration order, a store
	// lookup — which is the one class of bug a single run cannot show.
	second := applyBlockTransactions(t, pgpGenesisState(t), block, txA, duplicateRechargeTx(t, block.Header(), txA))
	if second[1].GasUsed != receiptB.GasUsed {
		t.Fatalf("replaying the duplicate used %d gas the second time and %d the first: the outcome is not a "+
			"function of the block", second[1].GasUsed, receiptB.GasUsed)
	}
}

// applyBlockTransactions runs txs against statedb the way Process does, and
// fails the test if any of them is rejected outright.
//
// A rejection is exactly what must not happen here: ApplyTransaction returns an
// error only when the transaction cannot be applied at all, and Process turns
// that into an invalid block. A failed-but-applied transaction comes back as a
// receipt with status Failed, which is the distinction this whole test is about.
func applyBlockTransactions(t *testing.T, statedb *state.StateDB, block *types.Block, txs ...*types.Transaction) []*types.Receipt {
	t.Helper()

	header := block.Header()
	blockCtx := NewEVMBlockContext(header, &pgpLegacyChainStub{header: header}, new(common.Address))
	if blockCtx.BaseFee == nil {
		t.Fatal("fixture problem: the legacy block context has no base fee, the transition will dereference nil")
	}
	evm := vm.NewEVM(blockCtx, statedb, params.PGPChainConfig, vm.Config{})
	gp := new(GasPool)
	if err := gp.AddGas(header.GasLimit); err != nil {
		t.Fatalf("cannot fund the block gas pool: %v", err)
	}

	receipts := make([]*types.Receipt, 0, len(txs))
	for i, tx := range txs {
		// SetTxContext is not optional: MakeReceipt files the logs under the
		// current context, so without it every receipt comes back with no logs.
		statedb.SetTxContext(tx.Hash(), i)
		receipt, err := ApplyTransaction(evm, gp, statedb, header, tx)
		if err != nil {
			t.Fatalf("transaction %d [%s] was rejected outright: %v\n"+
				"A rejection makes the whole block invalid, which is precisely what block 616157 shows "+
				"the network does NOT do with a duplicate Recharge.", i, tx.Hash().Hex(), err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

// duplicateRechargeTx rebuilds the fixture transaction with the same calldata,
// destination, value, gas and gas price, signed by a fresh key.
//
// The sender has to differ: two transactions from the same account with nonce 0
// cannot both be valid, and the point here is the duplicate ELA hash, not a
// duplicate nonce. A different sender also puts the pass-balance borrow to work
// for real — the account starts with a zero balance and no code, which is the
// case that borrow exists for.
func duplicateRechargeTx(t *testing.T, header *types.Header, original *types.Transaction) *types.Transaction {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("cannot generate a throwaway key: %v", err)
	}
	// Astronomically unlikely, but a coincidental match would make the second
	// transaction unapplicable for the wrong reason and the failure would look
	// like a bug in the duplicate rule.
	if crypto.PubkeyToAddress(key.PublicKey) == senderOf(t, original, header) {
		t.Fatal("the throwaway key reproduced the fixture sender")
	}

	unsigned := types.NewTx(&types.LegacyTx{
		Nonce:    original.Nonce(),
		GasPrice: original.GasPrice(),
		Gas:      original.Gas(),
		To:       original.To(),
		Value:    original.Value(),
		Data:     original.Data(),
	})
	signed, err := types.SignTx(unsigned, types.MakeSigner(params.PGPChainConfig, header.Number, header.Time), key)
	if err != nil {
		t.Fatalf("cannot sign the duplicate transaction: %v", err)
	}
	return signed
}

// senderOf recovers the sender with the same signer Process uses, so the test
// cannot disagree with the code under test about who signed a transaction.
func senderOf(t *testing.T, tx *types.Transaction, header *types.Header) common.Address {
	t.Helper()

	sender, err := types.Sender(types.MakeSigner(params.PGPChainConfig, header.Number, header.Time), tx)
	if err != nil {
		t.Fatalf("cannot recover the sender of %s: %v", tx.Hash().Hex(), err)
	}
	return sender
}
