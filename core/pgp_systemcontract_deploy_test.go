// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package core

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/systemcontracts/pgp"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// deployTestChain is the ChainContext the deployment reads. Only Config is
// consulted, because NewEVMBlockContext is given an explicit author and does
// not ask the engine.
type deployTestChain struct{ cfg *params.ChainConfig }

func (c *deployTestChain) Config() *params.ChainConfig                 { return c.cfg }
func (c *deployTestChain) CurrentHeader() *types.Header                { return nil }
func (c *deployTestChain) GetHeader(common.Hash, uint64) *types.Header { return nil }
func (c *deployTestChain) GetHeaderByHash(common.Hash) *types.Header   { return nil }
func (c *deployTestChain) GetHeaderByNumber(uint64) *types.Header      { return nil }
func (c *deployTestChain) Engine() consensus.Engine                    { return nil }

const (
	deployTestFork   = uint64(1000)
	deployTestHeight = uint64(100)
)

func deployTestConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID: big.NewInt(860621),
		// The Ethereum fork schedule is part of what makes a system call work:
		// without EIP-150 the 63/64 gas cap does not apply, the initialisers
		// forward their whole remaining budget into a cold STATICCALL, and the
		// cost exceeds the budget before the call even starts. Same numbers as
		// PGPChainConfig.
		HomesteadBlock: big.NewInt(1),
		EIP150Block:    big.NewInt(2),
		EIP155Block:    big.NewInt(3),
		EIP158Block:    big.NewInt(3),
		ByzantiumBlock: big.NewInt(3),
		IstanbulBlock:  big.NewInt(3),
		BerlinBlock:    big.NewInt(3),
		LondonBlock:    big.NewInt(3),
		Legacy: &params.LegacyConfig{
			ForkHeight:                 deployTestFork,
			SystemContractDeployHeight: deployTestHeight,
			ParliaGenesisValidators: []string{
				"0x00000000000000000000000000000000000000a1",
				"0x00000000000000000000000000000000000000b2",
			},
		},
	}
}

func deployTestHeader(number uint64) *types.Header {
	return &types.Header{
		Number: new(big.Int).SetUint64(number),
		// Difficulty is dereferenced by NewEVMBlockContext (core/evm.go:90,
		// header.Difficulty.Sign()), so it must not be nil. BaseFee stays nil:
		// a pgp-legacy header has none, and that is the branch under test.
		Difficulty: big.NewInt(1),
		GasLimit:   30_000_000,
		Time:       1761657650,
	}
}

func deployTestState(t *testing.T) *state.StateDB {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	// A balance at the system address would surface an accidental transfer.
	statedb.SetBalance(params.SystemAddress, uint256.NewInt(1), 0)
	return statedb
}

// TestDeployInstallsAndInitialises is the positive path: every image entry has
// its code installed, and the validator contract answers with the configured
// set, which is only possible if init(bytes,uint256) ran with correctly encoded
// arguments.
func TestDeployInstallsAndInitialises(t *testing.T) {
	cfg := deployTestConfig()
	chain := &deployTestChain{cfg: cfg}
	statedb := deployTestState(t)

	nonceBefore := statedb.GetNonce(params.SystemAddress)
	balanceBefore := statedb.GetBalance(params.SystemAddress).ToBig()
	logsBefore := len(statedb.Logs())

	if err := TryDeployPGPSystemContracts(cfg, deployTestHeader(deployTestHeight), chain, statedb, vm.Config{}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	for _, entry := range pgp.Entries() {
		if got := len(statedb.GetCode(entry.Address)); got != entry.CodeLen {
			t.Fatalf("%s: code length %d, want %d", entry.Name, got, entry.CodeLen)
		}
		if got := statedb.GetNonce(entry.Address); got != 1 {
			t.Fatalf("%s: nonce %d, want 1", entry.Name, got)
		}
	}

	// getValidators() proves the init arguments were encoded correctly: the
	// contract can only report a non-empty set if it decoded payload+cabinets.
	selector := crypto.Keccak256([]byte("getValidators()"))[:4]
	validator := common.HexToAddress(systemcontracts.ValidatorContract)
	ret, err := callReadOnly(t, cfg, chain, statedb, deployTestHeight, validator, selector)
	if err != nil {
		t.Fatalf("getValidators() reverted: %v", err)
	}
	addrArray, err := abi.NewType("address[]", "", nil)
	if err != nil {
		t.Fatalf("abi type: %v", err)
	}
	values, err := abi.Arguments{{Type: addrArray}}.Unpack(ret)
	if err != nil {
		t.Fatalf("unpack getValidators(): %v", err)
	}
	got := values[0].([]common.Address)
	if want := len(cfg.Legacy.ParliaGenesisValidators); len(got) != want {
		t.Fatalf("getValidators() returned %d validators, want %d", len(got), want)
	}

	// No side effects on the caller and no logs.
	if got := statedb.GetNonce(params.SystemAddress); got != nonceBefore {
		t.Fatalf("system address nonce changed: %d -> %d", nonceBefore, got)
	}
	if got := statedb.GetBalance(params.SystemAddress).ToBig(); got.Cmp(balanceBefore) != 0 {
		t.Fatalf("system address balance changed: %s -> %s", balanceBefore, got)
	}
	// The initialisers do emit (about 21 events: Initialized, role and clock
	// changes). Logs are not part of the state tree, so that is consensus-safe;
	// what must hold is that none of them is attributable to a transaction.
	all := statedb.Logs()
	for i := logsBefore; i < len(all); i++ {
		if lg := all[i]; lg != nil && lg.TxHash != (common.Hash{}) {
			t.Fatalf("a block-begin log carries tx hash %s: it would be attributed to that transaction", lg.TxHash)
		}
	}
	t.Logf("deployment emitted %d block-begin logs with zero tx hash", len(all)-logsBefore)
}

// TestDeployIsNoOpOffHeight is the negative path, and the only trustworthy proof
// that the deployment is height-gated: at D+1 nothing may change, root included.
func TestDeployIsNoOpOffHeight(t *testing.T) {
	cfg := deployTestConfig()
	chain := &deployTestChain{cfg: cfg}
	statedb := deployTestState(t)
	rootBefore := statedb.IntermediateRoot(true)

	if err := TryDeployPGPSystemContracts(cfg, deployTestHeader(deployTestHeight+1), chain, statedb, vm.Config{}); err != nil {
		t.Fatalf("deploy off height returned %v, want nil", err)
	}
	if got := statedb.IntermediateRoot(true); got != rootBefore {
		t.Fatalf("state root changed at block %d: %s -> %s", deployTestHeight+1, rootBefore, got)
	}
	if code := statedb.GetCode(common.HexToAddress(systemcontracts.ValidatorContract)); len(code) != 0 {
		t.Fatalf("validator code installed at the wrong height (%d bytes)", len(code))
	}
}

// TestDeployFailureReturnsError pins the error contract: a bad validator set is
// an error, not a panic. A panic here would let one bad block take the node
// down, which is a network-wide DoS for anyone who can produce a block.
func TestDeployFailureReturnsError(t *testing.T) {
	cfg := deployTestConfig()
	cfg.Legacy.ParliaGenesisValidators = nil // init args cannot be encoded
	chain := &deployTestChain{cfg: cfg}
	statedb := deployTestState(t)

	err := TryDeployPGPSystemContracts(cfg, deployTestHeader(deployTestHeight), chain, statedb, vm.Config{})
	if err == nil {
		t.Fatalf("deploy with no validators returned nil, want an error")
	}
}

// TestDeployRejectsMismatchedChainID covers F-01. The system contracts carry
// bscChainID as a compile-time constant and SlashIndicator hands it to the 0x68
// precompile as the evidence domain. A chain that runs a different id recovers a
// different signer, so slashing reverts forever with "validator not migrated" --
// and nothing else notices. Block D is the last moment this can be caught: after
// it the bytecode is on chain and only a hard fork can fix it.
func TestDeployRejectsMismatchedChainID(t *testing.T) {
	cfg := deployTestConfig()
	cfg.ChainID = big.NewInt(8606211) // a devnet id that drifted from the bytecode
	chain := &deployTestChain{cfg: cfg}
	statedb := deployTestState(t)

	err := TryDeployPGPSystemContracts(cfg, deployTestHeader(deployTestHeight), chain, statedb, vm.Config{})
	if err == nil {
		t.Fatal("deploy with chain id 8606211 returned nil; the mismatch must stop block D")
	}
	if !strings.Contains(err.Error(), "bscChainID") {
		t.Fatalf("error does not name the mismatch: %v", err)
	}
}

// TestLegacyInitRejectsOrdinaryCaller covers F-03.
//
// Seven of the twelve initialisers belong to the 0.6.4 generation and used to
// carry onlyNotInit and nothing else, which made the deploy step's ordering the
// single thing standing between a user transaction and a one-shot write that
// cannot be undone. They now carry onlyCoinbase + onlyZeroGasPrice, the pair
// every 0.8.x initialiser already had.
//
// The assertions run against the shipped image, not against the Solidity source:
// the bytes installed here are exactly the bytes block D installs, so this also
// catches an image that drifted from the sources it claims to come from.
func TestLegacyInitRejectsOrdinaryCaller(t *testing.T) {
	cfg := deployTestConfig()
	chain := &deployTestChain{cfg: cfg}

	// Code only, no initialisers -- the state an attacker would find if the deploy
	// step ever ran in a different order, or on a node that skipped it.
	statedb := deployTestState(t)
	for _, e := range pgp.Entries() {
		statedb.SetCode(e.Address, e.Code, tracing.CodeChangeSystemContractUpgrade)
	}
	slash := common.HexToAddress(systemcontracts.SlashContract)
	initData := crypto.Keccak256([]byte("init()"))[:4]

	attempt := func(coinbase, from common.Address, gasPrice int64) (string, error) {
		ctx := NewEVMBlockContext(deployTestHeader(deployTestHeight), chain, &coinbase)
		if ctx.BaseFee == nil {
			ctx.BaseFee = new(big.Int)
		}
		evm := vm.NewEVM(ctx, statedb, cfg, vm.Config{})
		defer evm.Release()
		msg := &Message{
			From:      from,
			GasLimit:  1_000_000,
			GasPrice:  uint256.NewInt(uint64(gasPrice)),
			GasFeeCap: uint256.NewInt(uint64(gasPrice)),
			GasTipCap: uint256.NewInt(0),
			To:        &slash,
			Data:      initData,
		}
		evm.SetTxContext(NewEVMTxContext(msg))
		ret, _, err := evm.Call(msg.From, slash, initData, vm.NewGasBudget(1_000_000), common.U2560)
		if err != nil {
			// The revert data, not the error, carries the reason: a bare
			// ErrExecutionReverted would also be produced by a plain require(false).
			reason, _ := abi.UnpackRevert(ret)
			return reason, err
		}
		return "", nil
	}

	// A plain transaction from an ordinary account.
	reason, err := attempt(common.HexToAddress("0x00000000000000000000000000000000000000aa"), common.HexToAddress("0x000000000000000000000000000000000000dead"), 0)
	if err == nil {
		t.Fatal("an ordinary caller initialised the contract")
	}
	if want := "the message sender must be the block producer"; reason != want {
		t.Fatalf("ordinary caller: revert reason %q, want %q", reason, want)
	}

	// The block producer, but inside a priced transaction. onlyCoinbase alone would
	// let a producer hide the call among user transactions.
	reason, err = attempt(params.SystemAddress, params.SystemAddress, 1)
	if err == nil {
		t.Fatal("the producer initialised the contract inside a priced transaction")
	}
	if want := "gasprice is not zero"; reason != want {
		t.Fatalf("producer with a gas price: revert reason %q, want %q", reason, want)
	}

	// The legitimate context still initialises, which is what proves neither
	// rejection consumed the one-shot write.
	if err := TryDeployPGPSystemContracts(cfg, deployTestHeader(deployTestHeight), chain, statedb, vm.Config{}); err != nil {
		t.Fatalf("deploy after the rejected attempts: %v", err)
	}
}

// callReadOnly runs a read-only call against the state at the end of height.
func callReadOnly(t *testing.T, cfg *params.ChainConfig, chain ChainContext, statedb vm.StateDB, height uint64, to common.Address, data []byte) ([]byte, error) {
	t.Helper()

	header := deployTestHeader(height)
	author := params.SystemAddress
	ctx := NewEVMBlockContext(header, chain, &author) // deployTestChain has no engine
	if ctx.BaseFee == nil {
		ctx.BaseFee = new(big.Int)
	}
	evm := vm.NewEVM(ctx, statedb, cfg, vm.Config{})
	defer evm.Release()
	ret, _, err := evm.Call(params.SystemAddress, to, data, vm.NewGasBudget(30_000_000), common.U2560)
	return ret, err
}
