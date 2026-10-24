// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
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

// TestLegacyRechargeRejectedByStateTransition drives the rule through the entry
// point that actually enforces it.
//
// It used to call ApplyMessage, because the guard lived in the state
// transition and fired per transaction. It does not live there any more: the
// verdict is taken once per block against the pre-block state (see
// legacySettledRecharges), so the unit under test is that function. What the old
// test protected is preserved: a settled hash is rejected, an unsettled one is
// not.
//
// The three cases that follow are the ones a per-transaction check gets wrong,
// and every one of them is a block that exists on the network.
func TestLegacyRechargeRejectedByStateTransition(t *testing.T) {
	var (
		settled   = common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
		elaMinter = params.ELAMINTER
		blockNum  = big.NewInt(616157)
	)
	rechargeTx := func(h common.Hash) *types.Transaction {
		return types.NewTx(&types.LegacyTx{To: &elaMinter, Data: rechargeCalldata(h)})
	}

	// A settled hash is rejected; an unsettled one is not. This is the rule
	// pgp-chain enforced and the control that keeps a "reject everything" guard
	// from passing.
	t.Run("settled is rejected, unsettled is not", func(t *testing.T) {
		settledRejects := legacySettledRecharges(params.PGPChainConfig, blockNum,
			legacyGuardState(t, settled), types.Transactions{rechargeTx(settled)})
		if len(settledRejects) != 1 || !settledRejects[0] {
			t.Fatalf("a Recharge whose ELA side is already completed must be rejected, got %v", settledRejects)
		}
		unsettledRejects := legacySettledRecharges(params.PGPChainConfig, blockNum,
			legacyGuardState(t), types.Transactions{rechargeTx(settled)})
		if len(unsettledRejects) != 1 || unsettledRejects[0] {
			t.Fatalf("a Recharge whose slot is empty must be allowed, got %v", unsettledRejects)
		}
	})

	// THE REGRESSION. Block 616157 of pgp mainnet carries a Recharge of a hash
	// that the same block had already recharged. Judged per transaction, the
	// second one looks settled and the block is declared invalid - which is
	// exactly what happened before this test existed. Judged against the
	// pre-block state, both are unsettled, both run, and the contract decides
	// what the duplicate does.
	t.Run("duplicate inside one block is not rejected", func(t *testing.T) {
		got := legacySettledRecharges(params.PGPChainConfig, blockNum,
			legacyGuardState(t), types.Transactions{rechargeTx(settled), rechargeTx(settled)})
		if len(got) != 2 {
			t.Fatalf("settled slice = %v, want two entries", got)
		}
		if got[0] || got[1] {
			t.Fatalf("a hash that the block itself recharges twice was judged settled before the block: %v", got)
		}
	})

	// The other direction: a hash settled by an earlier block is still rejected,
	// including when the block also recharges a fresh one. Without this, the fix
	// above would have to be "stop checking", which accepts history pgp-chain
	// refused.
	t.Run("settled by an earlier block is still rejected", func(t *testing.T) {
		fresh := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
		got := legacySettledRecharges(params.PGPChainConfig, blockNum,
			legacyGuardState(t, settled),
			types.Transactions{rechargeTx(fresh), rechargeTx(settled)})
		if len(got) != 2 || got[0] || !got[1] {
			t.Fatalf("settled slice = %v, want [false true]", got)
		}
	})

	// Non-Recharge transactions must not be flagged, whatever they call. A block
	// with no Recharge at all takes the "nothing to do" path and allocates
	// nothing; one that mixes a Recharge with a refundWithdraw of the same hash
	// must flag only the Recharge.
	t.Run("other transactions are not flagged", func(t *testing.T) {
		refund := types.NewTx(&types.LegacyTx{To: &elaMinter, Data: refundWithdrawCalldata(settled)})
		plain := types.NewTx(&types.LegacyTx{To: &elaMinter})
		if got := legacySettledRecharges(params.PGPChainConfig, blockNum,
			legacyGuardState(t, settled), types.Transactions{refund, plain}); got != nil {
			t.Fatalf("a block with no Recharge produced %v, want nil", got)
		}

		got := legacySettledRecharges(params.PGPChainConfig, blockNum,
			legacyGuardState(t, settled), types.Transactions{refund, rechargeTx(settled)})
		if len(got) != 2 || got[0] || !got[1] {
			t.Fatalf("settled slice = %v, want [false true] (refund not flagged, recharge flagged)", got)
		}
	})

	// Outside the legacy range the rule does not exist, and a block with no
	// Recharge at all must not allocate.
	t.Run("outside the legacy rules", func(t *testing.T) {
		if got := legacySettledRecharges(params.PGPChainConfig, big.NewInt(0), legacyGuardState(t, settled), nil); got != nil {
			t.Fatalf("empty block produced %v, want nil", got)
		}
		if got := legacySettledRecharges(nil, blockNum, legacyGuardState(t, settled), types.Transactions{rechargeTx(settled)}); got != nil {
			t.Fatalf("nil config produced %v, want nil", got)
		}
	})
}
