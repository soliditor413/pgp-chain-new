// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"encoding/hex"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/spv"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

// The pgp-chain mainnet block 30115 fixture.
//
// Why this block: it is the first block of the chain that redeems an ELA
// deposit, so it is the only block whose header commits to the answer of the
// whole legacy gateway stack — the 0x3f1 recharge lookup, the ELAMinter contract
// at 0x…64, the ELA token at 0x…65, and the legacy gas-refund rule in
// core/state_transition.go. It is also the block that settles how much the
// gateway mints.
//
// The values below were read off the live chain (eth_getBlockByNumber 0x75a3,
// eth_getTransactionByHash) and cross-checked against the raw block RLP, which
// is checked in as core/testdata/pgp-block-30115.rlp.
const (
	pgpBlock30115Hash      = "0x3a839548de1b163f1c70912e45dedc951d9cd6a38ea7eec42c1b0f0359ef9853"
	pgpBlock30115Parent    = "0xa59784ef6e7432a70320e62cc602fa3e9b4cada7f6b0a85c565ccd98d93dc923"
	pgpBlock30115StateRoot = "0x427f9ea5dfb73586ed722b04f0bc3c1f184184f4c1971c16d824b8cc089b63b2"
	pgpBlock30115RcptRoot  = "0x617beb86033e35420b8aeb0996711a98d785dc4ed71f57f9ad5060e1fe360d17"
	pgpBlock30115TxHash    = "0xe760777ecf7232b6a4d692c2d9bbb578401795cc9842f439a8b96e72f21da579"
	pgpBlock30115TxFrom    = "0x3a0b0ba9c9b698cbf8058aa8a28c39f26e574171"

	// The ELA transaction the block redeems, i.e. the bytes32 argument of the
	// Recharge call.
	elaHash30115 = "0x1b7e2877238323b58f5240bcf5e6797df3aae5684142c075572e145ee3d25898"

	// The ELA height that transaction sits at. Note it is 271 blocks ahead of
	// the block's own header.Nonce (2,076,267): the sealer stamped the ELA height
	// it had already observed, then executed a deposit it had just learned
	// about. Any implementation that caps the recharge read at header.Nonce
	// rejects this block — see spv.RechargeAny.
	elaHeightOfRecharge30115 = 2_076_538

	// header.Nonce of block 30115 = the ELA height the block is pinned to.
	pgpBlock30115ElaAnchor = 2_076_267

	// The deposit as ELAMinter saw it: one output of 10,150,000 satoshi paying
	// 0xa20f5a22…, whose fee is the 150,000 satoshi the ELA transaction kept.
	rechargeTarget30115 = "0xa20f5A22eF423b0e5c2Db79A5475D9512d989971"
	deposit30115        = 10_150_000
	depositFee30115     = 150_000
)

// readPGPBlock30115 decodes the checked-in block and pins its identity.
func readPGPBlock30115(t *testing.T) *types.Block {
	t.Helper()

	raw, err := os.ReadFile("testdata/pgp-block-30115.rlp")
	if err != nil {
		t.Fatalf("cannot read the block fixture: %v", err)
	}
	bin, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	if err != nil {
		t.Fatalf("the block fixture is not hex: %v", err)
	}
	block := new(types.Block)
	if err := rlp.DecodeBytes(bin, block); err != nil {
		t.Fatalf("cannot decode the block fixture: %v", err)
	}

	// Everything below reasons about this specific block, so the fixture has to
	// be the block it claims to be before a single assertion runs.
	header := block.Header()
	switch {
	case block.Hash().Hex() != pgpBlock30115Hash:
		t.Fatalf("fixture hash = %s, want %s", block.Hash().Hex(), pgpBlock30115Hash)
	case header.ParentHash.Hex() != pgpBlock30115Parent:
		t.Fatalf("fixture parent = %s", header.ParentHash.Hex())
	case header.Number.Uint64() != 30115:
		t.Fatalf("fixture number = %d", header.Number.Uint64())
	case header.ReceiptHash.Hex() != pgpBlock30115RcptRoot:
		t.Fatalf("fixture receipts root = %s", header.ReceiptHash.Hex())
	case header.Nonce.Uint64() != pgpBlock30115ElaAnchor:
		t.Fatalf("fixture ELA anchor = %d", header.Nonce.Uint64())
	case len(block.Transactions()) != 1:
		t.Fatalf("fixture carries %d transactions, want 1", len(block.Transactions()))
	}
	// The transactions root is recomputed rather than trusted, so a fixture that
	// silently loses or reorders a transaction cannot pass as the real block.
	if got := types.DeriveSha(block.Transactions(), trie.NewStackTrie(nil)); got != header.TxHash {
		t.Fatalf("fixture transactions root = %s, want %s", got.Hex(), header.TxHash.Hex())
	}
	return block
}

// pgpGenesisState builds the state block 30115's transactions would run on if the
// chain had done nothing at all before them, i.e. the genesis alloc.
//
// This is deliberately NOT the state of block 30114, and the test does not
// pretend otherwise: the parent state cannot be reconstructed without replaying
// 30114 blocks, and none of the values this test asserts depend on it. What the
// execution of block 30115 does depend on is in here:
//
//   - the two gateway contracts exist with the code they were deployed with,
//     otherwise the call to 0x…64 reverts;
//   - the sender has no code and nonce 0, which is the nonce the transaction
//     carries;
//   - the sender does not need a balance, because a Recharge borrows
//     PassBalance before it buys gas (core/state_transition.go borrowPassBalance).
//
// Anything the parent state legitimately differs in — balances of unrelated
// accounts, ELAMinter's completed mapping — is either not read by this
// transaction or asserted to be irrelevant by the receipts root, which commits
// only to status, gas and logs.
func pgpGenesisState(t *testing.T) *state.StateDB {
	t.Helper()

	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("cannot build a test StateDB: %v", err)
	}
	for addr, account := range DefaultPGPGenesisBlock().Alloc {
		if len(account.Code) > 0 {
			statedb.SetCode(addr, account.Code, tracing.CodeChangeGenesis)
		}
		if account.Nonce != 0 {
			statedb.SetNonce(addr, account.Nonce, tracing.NonceChangeGenesis)
		}
		if account.Balance != nil && account.Balance.Sign() != 0 {
			statedb.SetBalance(addr, uint256.MustFromBig(account.Balance), tracing.BalanceIncreaseGenesisBalance)
		}
		for key, value := range account.Storage {
			statedb.SetState(addr, key, value)
		}
	}
	// Guard the fixture: without the minter's code the deposit call reverts for a
	// reason that has nothing to do with the deposit.
	if len(statedb.GetCode(params.ELAMINTER)) == 0 {
		t.Fatal("fixture problem: the genesis alloc carries no ELAMinter code")
	}
	return statedb
}

// installRechargeStore30115 puts the process in the state a synced node is in
// when it executes block 30115: the ELA listener has indexed the deposit at ELA
// height 2,076,538, the store has been advanced past it, and block execution has
// pinned the ELA height view to header.Nonce.
//
// SpvDbInit is used rather than building the store by hand because it is also
// what registers the 0x3F1 precompile; an unregistered 0x3F1 is a stub that
// charges its gas and fails, which would make this test measure nothing.
func installRechargeStore30115(t *testing.T) {
	t.Helper()

	prevStore := spv.GetRechargeStore()
	prevView := spv.CurrentRechargeView()
	t.Cleanup(func() {
		spv.UninstallHeightView()
		spv.SetRechargeStore(prevStore)
		spv.SetStateView(prevView)
	})

	spv.SpvDbInit(t.TempDir(), "", [20]byte{}, nil)
	store := spv.GetRechargeStore()
	if store == nil {
		t.Fatal("SpvDbInit did not install a recharge store")
	}
	err := store.PutRecharge(elaHeightOfRecharge30115, elaHash30115, spv.RechargeDatas{{
		TargetAddress: common.HexToAddress(rechargeTarget30115),
		TargetAmount:  big.NewInt(deposit30115),
		Fee:           big.NewInt(depositFee30115),
	}})
	if err != nil {
		t.Fatalf("cannot index the deposit: %v", err)
	}
	// RechargeAny refuses to answer for a store that was never advanced, which is
	// the state a node with no ELA sync at all is in.
	if err := store.AdvanceCoverage(elaHeightOfRecharge30115, common.HexToHash(elaHash30115)); err != nil {
		t.Fatalf("cannot advance the store: %v", err)
	}
	if err := spv.InstallHeightView(pgpBlock30115ElaAnchor); err != nil {
		t.Fatalf("cannot pin the ELA height view: %v", err)
	}
}

// pgpLegacyChainStub is the smallest ChainContext that makes
// NewEVMBlockContext behave the way it does on a real node.
//
// It exists for one reason: for a pgp legacy header the block context must carry
// BaseFee 0 instead of nil, and that substitution is gated on `chain != nil`
// (core/evm.go). Passing nil here would leave BaseFee nil and the state
// transition would die on a nil dereference inside CmpBig before the transaction
// runs — see the note in core/state_transition.go.
type pgpLegacyChainStub struct{ header *types.Header }

func (c *pgpLegacyChainStub) Config() *params.ChainConfig                 { return params.PGPChainConfig }
func (c *pgpLegacyChainStub) CurrentHeader() *types.Header                { return c.header }
func (c *pgpLegacyChainStub) GetHeader(common.Hash, uint64) *types.Header { return nil }
func (c *pgpLegacyChainStub) GetHeaderByNumber(uint64) *types.Header      { return nil }
func (c *pgpLegacyChainStub) GetHeaderByHash(common.Hash) *types.Header   { return nil }

// Engine is never consulted: the block context is built with an explicit author,
// which skips the engine's Author() call. Returning nil keeps the stub honest
// about what it supports.
func (c *pgpLegacyChainStub) Engine() consensus.Engine { return nil }

// replayPGPBlock30115 executes the block's single transaction against statedb
// through the same entry point insertChain uses.
func replayPGPBlock30115(t *testing.T, statedb *state.StateDB, block *types.Block) *types.Receipt {
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
	// SetTxContext is what Process does before every transaction, and it cannot
	// be skipped here: MakeReceipt collects the logs with statedb.GetLogs(txHash),
	// so without the context the logs of this transaction are filed under the
	// zero hash, the receipt comes back empty, and the receipt root is wrong for
	// a reason that looks like a contract bug.
	statedb.SetTxContext(block.Transactions()[0].Hash(), 0)

	receipt, err := ApplyTransaction(evm, gp, statedb, header, block.Transactions()[0])
	if err != nil {
		t.Fatalf("the block's transaction was rejected outright: %v\n"+
			"A rejection here means the legacy rules in core/state_transition.go disagree with the block "+
			"pgp-chain sealed: a wrong pass balance, a wrong recharge selector, or a precompile that could not answer.",
			err)
	}
	return receipt
}

// TestReplayPGPBlock30115 replays pgp-chain mainnet block 30115 and checks the
// result against the commitments the block itself carries.
//
// WHAT THIS PROVES AND WHAT IT DOES NOT. It proves the gateway executes
// identically: the call succeeds, no gas is charged (pgp-chain refunds the whole
// cost of a Recharge, and core/state_transition.go only does that when the VM
// returned no error), and the receipt — status, cumulative gas, bloom and every
// log, including the minted amounts — hashes to the receipts root in the header.
// The receipts root is the right arbiter for the deposit amount because it
// commits to the log data, which is where the minted number appears.
//
// It does NOT prove the state root. Block 30115's state includes 30114 blocks of
// history this fixture does not have, so the test cannot rebuild it and does not
// claim to; the state root is only reachable by replaying the chain from genesis,
// which is the job of a full sync, not of a unit test.
func TestReplayPGPBlock30115(t *testing.T) {
	block := readPGPBlock30115(t)
	installRechargeStore30115(t)

	statedb := pgpGenesisState(t)
	receipt := replayPGPBlock30115(t, statedb, block)

	// The first gate: pgp-chain's refundCostGas returns the entire gas of a
	// Recharge, so a zero gasUsed means the call completed without a VM error.
	// Any failure of the gateway (missing record, missing view, unregistered
	// precompile) shows up here first, and far more legibly than as a root
	// mismatch.
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("the Recharge call failed; the block cannot be replayed without ELA data it does not have")
	}
	if receipt.GasUsed != 0 {
		t.Fatalf("gasUsed = %d, want 0: pgp-chain refunds a Recharge in full only when vmerr == nil "+
			"(pgp-chain/core/state_transition.go:325), so a non-zero value means the gateway call errored",
			receipt.GasUsed)
	}
	if block.GasUsed() != 0 {
		t.Fatalf("the fixture block itself reports gasUsed %d", block.GasUsed())
	}

	// The second gate: the events themselves. The block's logsBloom commits to
	// the address and topics of every log, so an implementation that minted to
	// the wrong account, or emitted a different set of events, is caught here
	// with a diagnosis that names the events instead of a root diff.
	if receipt.Bloom != block.Bloom() {
		for i, entry := range receipt.Logs {
			t.Logf("log[%d] address=%s topics=%v data=%x", i, entry.Address.Hex(), entry.Topics, entry.Data)
		}
		t.Fatalf("receipt bloom = %x, want %x: a different set of events was emitted than the chain recorded",
			receipt.Bloom, block.Bloom())
	}

	// The third gate, and the one that settles the minted amount: status,
	// cumulative gas, bloom and every byte of log data hash to the root in the
	// header. This is the only check here that a plausible-looking but wrong
	// implementation cannot satisfy.
	if got := types.DeriveSha(types.Receipts{receipt}, trie.NewStackTrie(nil)); got.Hex() != pgpBlock30115RcptRoot {
		for i, entry := range receipt.Logs {
			t.Logf("log[%d] address=%s topics=%v data=%x", i, entry.Address.Hex(), entry.Topics, entry.Data)
		}
		t.Fatalf("receipts root = %s, want %s: the execution produced different receipts than the chain did; "+
			"the logs above are what this implementation claims happened", got.Hex(), pgpBlock30115RcptRoot)
	}

	// Report what the gateway actually did, because the numbers are the answer to
	// "how much does a deposit mint" and reading them out of a log is the whole
	// point of this replay.
	if len(receipt.Logs) == 0 {
		t.Fatal("the Recharge emitted no logs; the block's logsBloom is not empty, so something is wrong")
	}
	for i, entry := range receipt.Logs {
		t.Logf("log[%d] address=%s topics=%v data=%x", i, entry.Address.Hex(), entry.Topics, entry.Data)
	}

	// How much the gateway mints, read out of the token's storage.
	//
	// This is the question this replay was written to settle, and the answer is
	// not the obvious one: the deposit is 10,150,000 satoshi and the ELA
	// transaction kept a 150,000 satoshi fee, so ELAMinter mints 10,000,000 to
	// the recipient and 150,000 to the transaction's sender. The fee is carved
	// out of the deposit, it is not minted on top of it — the supply grows by
	// exactly the deposit. Both minted amounts are visible as Transfer events
	// from the zero address in the logs above, which is what makes them
	// enforceable: they are inside the receipts root.
	recipient := common.HexToAddress(rechargeTarget30115)
	sender := common.HexToAddress(pgpBlock30115TxFrom)
	credited := erc20BalanceOf(statedb, params.ELATOKEN, recipient)
	feeCredited := erc20BalanceOf(statedb, params.ELATOKEN, sender)

	if want := big.NewInt(deposit30115 - depositFee30115); credited.Cmp(want) != 0 {
		t.Fatalf("the deposit recipient was credited %s, want %s (deposit %d minus the %d satoshi ELA fee)",
			credited.String(), want.String(), deposit30115, depositFee30115)
	}
	if want := big.NewInt(depositFee30115); feeCredited.Cmp(want) != 0 {
		t.Fatalf("the transaction sender was credited %s, want the %s satoshi ELA fee",
			feeCredited.String(), want.String())
	}
	supply := new(big.Int).SetBytes(statedb.GetState(params.ELATOKEN, totalSupplySlot()).Bytes())
	if want := big.NewInt(deposit30115); supply.Cmp(want) != 0 {
		t.Fatalf("ELAToken totalSupply = %s, want %s: the deposit is the only supply created, "+
			"and the fee comes out of it rather than adding to it", supply.String(), want.String())
	}

	// The sender must not have paid for the gas: the pass balance is borrowed, the
	// gas is refunded in full and the leftover borrow is settled back out. A dust
	// balance here would be a consensus split the moment the account is spent
	// from — and the sender does hold a balance now, in ELAToken, from the fee.
	if balance := statedb.GetBalance(sender); balance.Sign() != 0 {
		t.Fatalf("the Recharge sender kept %s wei of gas money; a Recharge is free "+
			"(pgp-chain refunds the fee to the sender)", balance.String())
	}
}

// TestReplayPGPBlock30115FailsWithoutTheRechargeRecord is the control for the
// test above: it proves the ELA data is load-bearing rather than decorative.
//
// Without the record, 0x3F1 must fail the call. If it instead returned an empty
// list, ELAMinter would mint nothing, the call would succeed, the gas would still
// be refunded to zero and the block would be accepted with a silently wrong
// state — the exact failure mode this whole subsystem exists to avoid. So the
// assertion is "the gateway call fails", not merely "the root differs".
func TestReplayPGPBlock30115FailsWithoutTheRechargeRecord(t *testing.T) {
	block := readPGPBlock30115(t)

	// A store that has been advanced but holds no deposit: the node has synced
	// ELA up to the anchor height and this transaction is not in it.
	prevStore := spv.GetRechargeStore()
	prevView := spv.CurrentRechargeView()
	t.Cleanup(func() {
		spv.UninstallHeightView()
		spv.SetRechargeStore(prevStore)
		spv.SetStateView(prevView)
	})
	spv.SpvDbInit(t.TempDir(), "", [20]byte{}, nil)
	store := spv.GetRechargeStore()
	if err := store.AdvanceCoverage(elaHeightOfRecharge30115, common.HexToHash(elaHash30115)); err != nil {
		t.Fatalf("cannot advance the store: %v", err)
	}
	if err := spv.InstallHeightView(pgpBlock30115ElaAnchor); err != nil {
		t.Fatalf("cannot pin the ELA height view: %v", err)
	}

	statedb := pgpGenesisState(t)
	receipt := replayPGPBlock30115(t, statedb, block)

	if receipt.Status != types.ReceiptStatusFailed {
		t.Fatal("the gateway call succeeded without the deposit; it must fail instead of minting nothing silently")
	}
	// The refund is conditional on vmerr == nil, so a failed gateway call is
	// visible in the gas: this is the signal a replay driver uses to say "the
	// ELA data for this block is missing" rather than "the state root is wrong".
	if receipt.GasUsed == 0 {
		t.Fatal("a failed Recharge must not have its gas refunded; gasUsed = 0 means the refund rule fired anyway")
	}
}

// totalSupplySlot is storage slot 2 of the ELAToken contract, which is where the
// OpenZeppelin ERC20 the genesis deploys keeps _totalSupply: slot 0 is
// _balances, slot 1 is _allowances, slot 2 is _totalSupply.
//
// The value is not assumed — pgpGenesisState asserts the account carries the
// deployed code, and TestReplayPGPBlock30115 only compares totalSupply against
// the recipient's balance, so a wrong slot makes both read zero and the test
// fail loudly instead of passing vacuously.
func totalSupplySlot() common.Hash { return common.BigToHash(big.NewInt(2)) }

// erc20BalanceOf reads _balances[owner] out of an ERC20 deployed at token.
//
// An ERC20 balance lives in the contract's storage, not in the account's wei
// balance, and the mapping key is keccak256(pad32(owner) ++ pad32(slot)) — the
// owner first, which is the opposite of the usual EVM word order and the reason
// this is a named helper rather than an inline expression.
func erc20BalanceOf(statedb *state.StateDB, token, owner common.Address) *big.Int {
	key := crypto.Keccak256Hash(common.LeftPadBytes(owner.Bytes(), 32), common.BigToHash(big.NewInt(0)).Bytes())
	return new(big.Int).SetBytes(statedb.GetState(token, key).Bytes())
}
