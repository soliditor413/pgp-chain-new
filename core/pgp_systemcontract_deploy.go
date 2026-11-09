// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package core

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/systemcontracts/pgp"
	"github.com/ethereum/go-ethereum/core/systemcontracts/pgpinit"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// pgpInitGasLimit is the budget for each system-contract initialiser.
//
// It matches the budget ProcessBeaconBlockRoot and ProcessParentBlockHash use
// for their system calls. The initialisers are ordinary one-shot setters; the
// largest of them (StakeHub's) writes a handful of slots.
const pgpInitGasLimit uint64 = 30_000_000

// logEnumerator is the part of the concrete statedb this file needs and the
// vm.StateDB interface does not have.
//
// It is a local interface rather than an import of core/state so that the
// deployment can be driven in tests by any StateDB implementation, and so this
// file does not depend on the storage layer's concrete type at all.
type logEnumerator interface {
	Logs() []*types.Log
}

// TryDeployPGPSystemContracts installs and initialises the pgp-chain system
// contracts at Legacy.SystemContractDeployHeight, and does nothing on any other
// block.
//
// # Why this is not in core/systemcontracts
//
// The system-call tracer markers (onSystemCallStart/OnSystemCallEnd) are
// unexported here in core, and core/systemcontracts cannot import core (core
// imports it). A deployment that cannot mark itself as a system call would show
// up in debug_trace as an ordinary transaction from the system address.
//
// # Why it builds its own EVM
//
// The initialisers gate on onlyCoinbase (msg.sender == block.coinbase) and
// onlyZeroGasPrice (tx.gasprice == 0), so the call has to come from
// SystemAddress with a context whose Coinbase is also SystemAddress.
// NewEVMBlockContext honours an explicit author argument, so passing
// &SystemAddress gives exactly that -- and because this EVM is local to this
// function, the caller's EVM and its Context are never touched. There is
// nothing to save and nothing to restore, which is the point: a "remember to
// put Coinbase back" bug would make block.coinbase SystemAddress for every
// later transaction in the block, and only the producer's own blocks would then
// be rejected.
//
// # Ordering
//
// Codes and nonces for all 17 accounts go in first, then the 12 initialisers
// run. An initialiser that reads another system contract therefore always sees
// installed code, and the order inside each phase is the image's address order,
// which is fixed by the manifest.
//
// Errors are returned, never panicked: a bad block must not take the node down,
// and one malicious peer able to produce a bad block would otherwise be able to
// panic every node on the network. The caller decides what the error means --
// invalid block on the import path, no block from the miner, RPC error on the
// read-only paths.
func TryDeployPGPSystemContracts(config *params.ChainConfig, header *types.Header, chain ChainContext, statedb vm.StateDB, vmCfg vm.Config) error {
	if config == nil || config.Legacy == nil || header == nil || header.Number == nil {
		return nil
	}
	deployHeight := config.Legacy.SystemContractDeployHeight
	if deployHeight == 0 || header.Number.Uint64() != deployHeight {
		return nil
	}
	if statedb == nil {
		return fmt.Errorf("pgp system contracts: no state at deploy height %d", deployHeight)
	}
	// EIP-150 is not optional here. Without the 63/64 cap the initialisers
	// forward their entire remaining budget into nested calls, the cost exceeds
	// what is left, and the block fails with a bare "out of gas" that says
	// nothing about the configuration. A deploy height before EIP-150 is a
	// configuration error, so it is named as one.
	if !config.IsEIP150(header.Number) {
		return fmt.Errorf("pgp system contracts: deploy height %d is before EIP-150 (block %v); the "+
			"initialisers forward all remaining gas and would run out", deployHeight, config.EIP150Block)
	}

	// Baseline for the side-effect assertion below. A StateDB that cannot
	// enumerate logs skips the check rather than failing the block.
	var logEnum logEnumerator
	logsBefore := 0
	if le, ok := statedb.(logEnumerator); ok {
		logEnum = le
		logsBefore = len(le.Logs())
	}

	author := params.SystemAddress
	blockCtx := NewEVMBlockContext(header, chain, &author)
	if blockCtx.Coinbase != params.SystemAddress {
		return fmt.Errorf("pgp system contracts: block context coinbase is %s, want the system address",
			blockCtx.Coinbase)
	}
	evm := vm.NewEVM(blockCtx, statedb, config, vmCfg)
	defer evm.Release()

	// Mark the whole deployment as a system call so tracers do not present it
	// as user activity, exactly as the other block-begin system calls do.
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}

	entries := pgp.Entries()

	// Phase A: install code and nonce for every account. SetNonce(addr, 1) is
	// what the existing system-contract upgrades do for the addresses they
	// deploy; the contract accounts on this chain are not fresh EOAs and a
	// nonce of 0 would make some tooling treat them as such.
	for _, entry := range entries {
		statedb.SetCode(entry.Address, entry.Code, tracing.CodeChangeSystemContractUpgrade)
		statedb.SetNonce(entry.Address, 1, tracing.NonceChangeNewContract)
	}

	// Phase B: run the one-shot initialisers.
	validatorAddr := common.HexToAddress(systemcontracts.ValidatorContract)
	for _, entry := range entries {
		if !entry.DeployInit {
			continue
		}
		data := append([]byte(nil), entry.InitSelector[:]...)
		if entry.Address == validatorAddr {
			// The validator contract's initialiser takes the set as arguments;
			// the 4-byte selector alone makes the ABI decoder revert.
			full, err := pgpValidatorInitCalldata(config.Legacy, entry.InitSelector)
			if err != nil {
				return fmt.Errorf("pgp system contracts: encoding %s init args: %w", entry.Name, err)
			}
			data = full
		}

		msg := &Message{
			From:      params.SystemAddress,
			GasLimit:  pgpInitGasLimit,
			GasPrice:  uint256.NewInt(0),
			GasFeeCap: uint256.NewInt(0),
			GasTipCap: uint256.NewInt(0),
			To:        &entry.Address,
			Data:      data,
		}
		evm.SetTxContext(NewEVMTxContext(msg))
		statedb.AddAddressToAccessList(entry.Address)
		if _, _, err := evm.Call(msg.From, entry.Address, data, vm.NewGasBudget(pgpInitGasLimit), common.U2560); err != nil {
			return fmt.Errorf("pgp system contracts: %s init at %s failed: %w", entry.Name, entry.Address, err)
		}
		log.Info("pgp system contract initialised", "name", entry.Name, "addr", entry.Address, "block", deployHeight)
	}

	// F-01: the contracts that inherit System.sol carry bscChainID() as a
	// compile-time constant, and SlashIndicator feeds it to the 0x68 precompile
	// as the domain of the double-sign evidence. When the running chain's id
	// differs, ecrecover still "succeeds" -- it just recovers some other address
	// -- so the evidence check degrades into a permanent "validator not
	// migrated" revert and slashing never works. Nothing else in the node can
	// observe that: the precompile exists, init succeeds, every unit test stays
	// green, and it only surfaces the day someone tries to slash.
	//
	// Block D is the last observable moment. After it the bytecode is on chain
	// and the only remedy is a hard fork, so refuse to produce a block that
	// would ship a chain where slashing is dead. devnet and mainnet both run
	// 860621, so this should never fire; it is here so that a regenerated
	// genesis with a different --chain-id cannot slip through silently.
	if err := assertBSCChainID(config, evm, validatorAddr, deployHeight); err != nil {
		return err
	}

	// One merge and one Finalise for the whole deployment, not per call: the
	// twelve initialisers belong to one block-begin step, and Finalise per call
	// would publish twelve intermediate states.
	if statedb.AccessEvents() != nil {
		statedb.AccessEvents().Merge(evm.AccessEvents)
	}
	statedb.Finalise(true)

	// The initialisers DO emit (about 21 logs: OZ Initialized/role events,
	// TimelockChange, QuorumNumeratorUpdated, EIP712DomainChanged). That is
	// consensus-safe -- logs are not part of the state tree -- but they are
	// block-begin logs with no transaction behind them. What must hold is that
	// none of them can ever be attributed to a real transaction, i.e. every one
	// carries the zero tx hash. eth_getLogs filters by tx hash, so a zero hash
	// keeps them out of every transaction's log list.
	//
	// The pending-log path of eth_getLogs needs a devnet smoke test; this
	// assertion only covers attribution.
	if logEnum != nil {
		all := logEnum.Logs()
		for i := logsBefore; i < len(all); i++ {
			if lg := all[i]; lg != nil && lg.TxHash != (common.Hash{}) {
				return fmt.Errorf("pgp system contracts: a log emitted during deployment at block %d carries tx "+
					"hash %s; block-begin logs must not be attributable to a transaction", deployHeight, lg.TxHash)
			}
		}
	}
	return nil
}

// pgpValidatorInitCalldata builds
//
//	selector || abi.encode(bytes payload, uint256 cabinets)
//
// for BSCValidatorSet.init(bytes,uint256). The selector comes from the image
// entry, and is compared against the derived one so a manifest that disagrees
// with the ABI signature fails here instead of sending undecodable calldata.
func pgpValidatorInitCalldata(legacy *params.LegacyConfig, selector [4]byte) ([]byte, error) {
	if got, want := selector[:], pgpinit.ValidatorInitSelector; len(want) == 4 && !bytesEqual(got, want) {
		return nil, fmt.Errorf("init selector 0x%x does not match init(bytes,uint256) 0x%x", got, want)
	}
	payload, cabinets, err := pgpinit.ValidatorInitArgs(legacy)
	if err != nil {
		return nil, err
	}
	bytesTy, err := abi.NewType("bytes", "", nil)
	if err != nil {
		return nil, err
	}
	uintTy, err := abi.NewType("uint256", "", nil)
	if err != nil {
		return nil, err
	}
	args := abi.Arguments{{Type: bytesTy}, {Type: uintTy}}
	encoded, err := args.Pack(payload, new(big.Int).SetUint64(cabinets))
	if err != nil {
		return nil, err
	}
	return append(append([]byte(nil), selector[:]...), encoded...), nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// bscChainIDSelector is keccak256("bscChainID()")[:4]. The getter comes from
// System.sol's `uint32 public constant bscChainID`, so it is answered by every
// contract that inherits it (the validator set among them) and by none of the
// SystemV2 ones.
var bscChainIDSelector = [4]byte{0x49, 0x32, 0x79, 0xb1}

// assertBSCChainID compares the chain id compiled into the system contracts with
// the one this node runs. See the call site for why a mismatch has to stop block
// D rather than be logged.
func assertBSCChainID(config *params.ChainConfig, evm *vm.EVM, validatorAddr common.Address, deployHeight uint64) error {
	if config == nil || config.ChainID == nil {
		return fmt.Errorf("pgp system contracts: chain config carries no chain id")
	}
	ret, _, err := evm.StaticCall(params.SystemAddress, validatorAddr, bscChainIDSelector[:], vm.NewGasBudget(1_000_000))
	if err != nil {
		return fmt.Errorf("pgp system contracts: reading bscChainID() from %s: %w", validatorAddr, err)
	}
	if len(ret) != 32 {
		return fmt.Errorf("pgp system contracts: bscChainID() returned %d bytes, want 32", len(ret))
	}
	onchain := new(big.Int).SetBytes(ret)
	if onchain.Cmp(config.ChainID) != 0 {
		return fmt.Errorf("pgp system contracts: the deployed system contracts carry bscChainID=%s but this chain "+
			"runs %s; double-sign evidence would be verified against the wrong domain and slashing would never "+
			"work (F-01). Fix the genesis chain id before block %d",
			onchain, config.ChainID, deployHeight)
	}
	return nil
}
