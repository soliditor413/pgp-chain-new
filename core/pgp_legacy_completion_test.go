// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// TestBlockContextCarriesELAHeight pins the one job vm.BlockContext.Nonce has
// left: it must carry the ELA SPV height the block itself recorded
// (types.EncodeNonce(GetSpvHeight()), pgp-chain/consensus/pbft/pbft.go:492) and
// nothing else.
//
// The recharge completion check no longer reads it - that verdict is a slot in
// the state being executed, see legacyRechargeAlreadyCompleted - but the legacy
// ELA precompiles still have to know which ELA height the block is pinned to,
// and taking that from the block context is what keeps a replay independent of
// the node's head. If this link breaks, the precompiles start answering against
// "whatever the node knew when it synced" again.
func TestBlockContextCarriesELAHeight(t *testing.T) {
	const spvHeight = 483920

	// The nonce encodes big endian, so the round trip also pins the direction
	// pgp-chain uses (types.EncodeNonce).
	nonce := types.EncodeNonce(spvHeight)
	if got := nonce.Uint64(); got != spvHeight {
		t.Fatalf("nonce round trip mismatch: have %d, want %d", got, spvHeight)
	}

	header := &types.Header{
		Number:     big.NewInt(1923),
		Difficulty: big.NewInt(params.MinimumDifficulty.Int64()),
		Time:       1762224252,
		Nonce:      nonce,
	}
	if ctx := NewEVMBlockContext(header, nil, new(common.Address)); ctx.Nonce != spvHeight {
		t.Fatalf("NewEVMBlockContext dropped the header nonce: have %d, want %d", ctx.Nonce, spvHeight)
	}

	// A header with no nonce set must report height 0 rather than something
	// derived from the head, so an unanchored block fails loudly instead of
	// silently guessing.
	bare := &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(1)}
	if got := NewEVMBlockContext(bare, nil, new(common.Address)).Nonce; got != 0 {
		t.Fatalf("a header without a nonce must yield height 0, got %d", got)
	}
}

// legacyGuardState builds a StateDB whose ELAMinter account holds exactly the
// storage a settled hash would leave behind, and nothing else.
//
// The state is the only input the guard has, so this is the whole fixture: no
// EVM, no block, no ELA database.
func legacyGuardState(t *testing.T, settled ...common.Hash) *state.StateDB {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("cannot build a test StateDB: %v", err)
	}
	for _, elaHash := range settled {
		statedb.SetState(params.ELAMINTER, params.CompletedSlot(elaHash), params.CompletedSetValue)
	}
	return statedb
}

// rechargeCalldata is a well formed Recharge(bytes32,bytes) call: selector,
// the bytes32 hash, then the offset (0x40) and length (0) of the empty bytes
// argument.
func rechargeCalldata(elaHash common.Hash) []byte {
	selector := crypto.Keccak256([]byte("Recharge(bytes32,bytes)"))[:4]
	input := make([]byte, 0, 4+32+64)
	input = append(input, selector...)
	input = append(input, elaHash[:]...)
	input = append(input, make([]byte, 64)...)
	input[4+32+31] = 0x40 // offset of the bytes argument, last byte of word 2
	return input
}

// refundWithdrawCalldata is refundWithdraw(bytes32): selector plus the one
// bytes32 argument.
func refundWithdrawCalldata(elaHash common.Hash) []byte {
	selector := crypto.Keccak256([]byte("refundWithdraw(bytes32)"))[:4]
	return append(append([]byte{}, selector...), elaHash[:]...)
}

// TestLegacyRechargeAlreadyCompleted covers the verdicts the legacy state
// transition must reach, and the one it must not.
//
// pgp-chain rejected a Recharge whose ELA side was already settled
// (pgp-chain/core/state_transition.go:256-261). It did so by asking its own
// ELAMinter contract over eth_call with nil call options
// (pgp-chain/spv/elaminter.go:113-134), i.e. it read this chain's storage at
// "latest" - an answer that depends on when the node ran and therefore cannot be
// replayed. The guard reads the same slot out of the state being executed.
//
// The cases are separate subtests so a failure names the rule that broke. The
// RefundWithdrawSettled one is the reason the split exists at all: ELAMinter
// stores completed[hash] for Recharge *and* refundWithdraw in the same mapping
// (slot 1), so a storage-only check would reject a withdraw the moment the
// withdraw itself landed. pgp-chain never did that, because IsCompletedByTxInput
// matched the Recharge method id and answered "not completed" for everything
// else (pgp-chain/spv/elaminter.go:86-96).
func TestLegacyRechargeAlreadyCompleted(t *testing.T) {
	var (
		settled    = common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
		otherHash  = common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
		elaMinter  = params.ELAMINTER
		notMinter  = common.HexToAddress("0x0000000000000000000000000000000000000063")
		settledSt  = legacyGuardState(t, settled)
		emptyState = legacyGuardState(t)
	)

	// A Recharge for a hash that is already marked completed is rejected. This is
	// the rule pgp-chain enforced and the reason the guard exists at all.
	t.Run("RechargeSettled", func(t *testing.T) {
		if !legacyRechargeAlreadyCompleted(settledSt, rechargeCalldata(settled), &elaMinter) {
			t.Fatal("a Recharge whose ELA side is already completed must be rejected")
		}
	})

	// Same calldata, same settled slot, but addressed to another contract: the
	// rule does not apply. Only params.IsPGPRechargeTx checks the recipient
	// (params.LegacyRechargeElaHash does not), so this case is what keeps that
	// gate honest - without it a look-alike call to an unrelated contract would be
	// rejected and the transaction lost.
	t.Run("RechargeOtherRecipient", func(t *testing.T) {
		if legacyRechargeAlreadyCompleted(settledSt, rechargeCalldata(settled), &notMinter) {
			t.Fatal("the completion rule must only fire for a call to the ELA Minter contract")
		}
		if legacyRechargeAlreadyCompleted(settledSt, rechargeCalldata(settled), nil) {
			t.Fatal("a contract creation carrying Recharge calldata must not hit the rule")
		}
	})

	// A Recharge for a hash the contract never settled is allowed. An untouched
	// slot reads as zero, which is why "not completed" needs no special case.
	t.Run("RechargeUnsettled", func(t *testing.T) {
		if legacyRechargeAlreadyCompleted(emptyState, rechargeCalldata(settled), &elaMinter) {
			t.Fatal("a Recharge whose slot is empty must be allowed")
		}
	})

	// A settled hash must not vouch for a different hash.
	t.Run("RechargeOtherHash", func(t *testing.T) {
		if legacyRechargeAlreadyCompleted(settledSt, rechargeCalldata(otherHash), &elaMinter) {
			t.Fatal("an unrelated ELA hash must not be treated as completed")
		}
	})

	// refundWithdraw for a hash that is marked completed is still allowed. The
	// very same hashed slot is set, because refundWithdraw writes the same
	// mapping; pgp-chain accepted these transactions and they are already in the
	// chain, so rejecting one here would invalidate history.
	t.Run("RefundWithdrawSettled", func(t *testing.T) {
		if legacyRechargeAlreadyCompleted(settledSt, refundWithdrawCalldata(settled), &elaMinter) {
			t.Fatal("refundWithdraw must not be rejected by the Recharge completion rule, " +
				"even though ELAMinter stores both under the same slot")
		}
	})

	// Input too short to carry the argument is allowed, and must not panic.
	t.Run("MalformedInput", func(t *testing.T) {
		if legacyRechargeAlreadyCompleted(settledSt, rechargeCalldata(settled)[:35], &elaMinter) {
			t.Fatal("a truncated Recharge must not be treated as completed")
		}
		if legacyRechargeAlreadyCompleted(settledSt, nil, &elaMinter) {
			t.Fatal("nil calldata must not be treated as completed")
		}
	})
}

// TestLegacyRechargeRejectedByStateTransition drives the rule through the real
// entry point. The test above proves what the guard answers; this one proves
// execute() actually asks it. Without this, the guard could be perfect and never
// called, and a Recharge pgp-chain rejected would be accepted on replay - a bad
// block, not a rejected transaction.
//
// Both directions are asserted. The reject case is the one whose failure mode
// silently forks the node away from the chain; the accept case is the control -
// without it a guard that rejected every Recharge would satisfy the first
// assertion. The accept case does not need to succeed end to end (this fixture
// is not a funded account, so the transition stops somewhere after the guard),
// only to not stop *at* the guard.
func TestLegacyRechargeRejectedByStateTransition(t *testing.T) {
	settled := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	elaMinter := params.ELAMINTER

	header := &types.Header{
		Number:     big.NewInt(1923),
		Difficulty: big.NewInt(params.MinimumDifficulty.Int64()),
		Time:       1762224252,
		BaseFee:    big.NewInt(0),
		Nonce:      types.EncodeNonce(483920),
	}
	// Guard against a fixture that quietly drifts off the legacy rules: if it
	// did, legacyGasRefund would be false and the test would "pass" by never
	// reaching the rule.
	if rules := params.PGPChainConfig.Rules(header.Number, false, header.Time); !rules.IsPGPLegacy {
		t.Fatalf("fixture problem: block %d is not on the legacy rules", header.Number)
	}

	blockCtx := NewEVMBlockContext(header, nil, new(common.Address))
	evm := vm.NewEVM(blockCtx, legacyGuardState(t, settled), params.PGPChainConfig, vm.Config{})

	msg := &Message{
		From:      common.HexToAddress("0x00000000000000000000000000000000000000aa"),
		To:        &elaMinter,
		Data:      rechargeCalldata(settled),
		GasLimit:  1_000_000,
		GasPrice:  uint256.NewInt(1),
		GasFeeCap: uint256.NewInt(1),
		GasTipCap: uint256.NewInt(0),
		Value:     uint256.NewInt(0),
	}
	if _, err := ApplyMessage(evm, msg, nil); !errors.Is(err, errLegacyTxAlreadyCompleted) {
		t.Fatalf("a settled Recharge must be rejected by the state transition, got err=%v", err)
	}

	// Same call against a state where nothing is settled: whatever happens next,
	// the completion rule must not be what stops it.
	evm = vm.NewEVM(blockCtx, legacyGuardState(t), params.PGPChainConfig, vm.Config{})
	if _, err := ApplyMessage(evm, msg, nil); errors.Is(err, errLegacyTxAlreadyCompleted) {
		t.Fatalf("an unsettled Recharge must not be rejected by the completion rule, got err=%v", err)
	}
}
