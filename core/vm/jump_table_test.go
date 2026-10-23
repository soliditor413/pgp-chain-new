// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

// TestJumpTableCopy tests that deep copy is necessary to prevent modify shared jump table
func TestJumpTableCopy(t *testing.T) {
	tbl := newMergeInstructionSet()
	require.Equal(t, uint64(0), tbl[SLOAD].constantGas)

	// a deep copy won't modify the shared jump table
	deepCopy := copyJumpTable(&tbl)
	deepCopy[SLOAD].constantGas = 100
	require.Equal(t, uint64(100), deepCopy[SLOAD].constantGas)
	require.Equal(t, uint64(0), tbl[SLOAD].constantGas)
}

// TestLegacyInstructionSet pins 0x44 on the pgp-chain legacy rules.
//
// pgp-chain builds its Shanghai table on top of the merge table, so 0x44 there is
// PREVRANDAO/opRandom and pushes 0 when Context.Random is nil
// (pgp-chain/core/vm/instructions.go:488-495). This tree builds Shanghai on top
// of London, where 0x44 is DIFFICULTY and reads the block difficulty, which on
// the legacy chain is always 2. Without the dedicated table every contract that
// reads 0x44 would produce a different state root.
func TestLegacyInstructionSet(t *testing.T) {
	legacyEVM := func(difficulty *big.Int, random *common.Hash) *EVM {
		ctx := BlockContext{Difficulty: difficulty, Random: random}
		return NewEVM(ctx, nil, params.TestChainConfig, Config{})
	}
	run44 := func(op *operation, evm *EVM) *uint256.Int {
		t.Helper()
		var (
			pc    uint64
			stack = newStackForTesting()
		)
		if _, err := op.execute(&pc, evm, &ScopeContext{Stack: stack}); err != nil {
			t.Fatalf("0x44 execution failed: %v", err)
		}
		if len(stack.Data()) != 1 {
			t.Fatalf("0x44 left %d items on the stack, want 1", len(stack.Data()))
		}
		return new(uint256.Int).Set(&stack.Data()[0])
	}

	// Legacy rules: difficulty 2 and no randomness must yield 0, not 2.
	if got := run44(legacyInstructionSet[PREVRANDAO], legacyEVM(big.NewInt(2), nil)); !got.IsZero() {
		t.Fatalf("legacy 0x44: have %v, want 0 (pgp-chain pushes 0 when Random is nil)", got)
	}
	// The shared Shanghai table must be untouched: BSC still reads the
	// difficulty there, and patching the global table would have broken it.
	if got := run44(shanghaiInstructionSet[PREVRANDAO], legacyEVM(big.NewInt(2), nil)); got.Uint64() != 2 {
		t.Fatalf("shanghai 0x44 was polluted: have %v, want 2", got)
	}
	// With randomness present both variants agree on it.
	random := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000000ff")
	if got := run44(legacyInstructionSet[PREVRANDAO], legacyEVM(big.NewInt(2), &random)); got.Uint64() != 0xff {
		t.Fatalf("legacy 0x44 with Random: have %v, want 0xff", got)
	}
	// Everything else must be identical to the Shanghai table the legacy rules
	// otherwise inherit (PUSH0 and BASEFEE are the two forks pgp activated).
	for _, op := range []OpCode{PUSH0, BASEFEE, SLOAD, MLOAD} {
		if legacyInstructionSet[op] == nil {
			t.Fatalf("legacy instruction set is missing op %v", op)
		}
		if legacyInstructionSet[op].constantGas != shanghaiInstructionSet[op].constantGas {
			t.Fatalf("op %v gas mismatch: legacy %d, shanghai %d",
				op, legacyInstructionSet[op].constantGas, shanghaiInstructionSet[op].constantGas)
		}
	}
}
