// Copyright 2014 The go-ethereum Authors
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

package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// ExecutionResult includes all output after executing given evm
// message no matter the execution itself is successful or not.
type ExecutionResult struct {
	UsedGas    uint64 // Total used gas, refunded gas is deducted
	MaxUsedGas uint64 // Maximum gas consumed during execution, excluding gas refunds.
	Err        error  // Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData []byte // Returned data from evm(function result or data supplied with revert opcode)
}

// Unwrap returns the internal evm error which allows us for further
// analysis outside.
func (result *ExecutionResult) Unwrap() error {
	return result.Err
}

// Failed returns the indicator whether the execution is successful or not
func (result *ExecutionResult) Failed() bool { return result.Err != nil }

// Return is a helper function to help caller distinguish between revert reason
// and function return. Return returns the data after execution if no error occurs.
func (result *ExecutionResult) Return() []byte {
	if result.Err != nil {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// Revert returns the concrete revert reason if the execution is aborted by `REVERT`
// opcode. Note the reason can be nil if no data supplied with revert opcode.
func (result *ExecutionResult) Revert() []byte {
	if result.Err != vm.ErrExecutionReverted {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, accessList types.AccessList, authList []types.SetCodeAuthorization, isContractCreation, isHomestead, isEIP2028, isEIP3860, isAmsterdam bool) (vm.GasCosts, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if isContractCreation && isHomestead {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	dataLen := uint64(len(data))
	// Bump the required gas by the amount of transactional data
	if dataLen > 0 {
		// Zero and non-zero bytes are priced differently
		z := uint64(bytes.Count(data, []byte{0}))
		nz := dataLen - z

		// Make sure we don't exceed uint64 for all data combinations
		nonZeroGas := params.TxDataNonZeroGasFrontier
		if isEIP2028 {
			nonZeroGas = params.TxDataNonZeroGasEIP2028
		}
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += nz * nonZeroGas

		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += z * params.TxDataZeroGas

		if isContractCreation && isEIP3860 {
			lenWords := toWordSize(dataLen)
			if (math.MaxUint64-gas)/params.InitCodeWordGas < lenWords {
				return vm.GasCosts{}, ErrGasUintOverflow
			}
			gas += lenWords * params.InitCodeWordGas
		}
	}
	if accessList != nil {
		addresses := uint64(len(accessList))
		storageKeys := uint64(accessList.StorageKeys())
		if (math.MaxUint64-gas)/params.TxAccessListAddressGas < addresses {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += addresses * params.TxAccessListAddressGas
		if (math.MaxUint64-gas)/params.TxAccessListStorageKeyGas < storageKeys {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += storageKeys * params.TxAccessListStorageKeyGas

		// EIP-7981: access list data is charged in addition to the base charge.
		if isAmsterdam {
			const (
				addressCost    = common.AddressLength * params.TxCostFloorPerToken7976 * params.TxTokenPerNonZeroByte
				storageKeyCost = common.HashLength * params.TxCostFloorPerToken7976 * params.TxTokenPerNonZeroByte
			)
			if (math.MaxUint64-gas)/addressCost < addresses {
				return vm.GasCosts{}, ErrGasUintOverflow
			}
			gas += addresses * addressCost
			if (math.MaxUint64-gas)/storageKeyCost < storageKeys {
				return vm.GasCosts{}, ErrGasUintOverflow
			}
			gas += storageKeys * storageKeyCost
		}
	}
	if authList != nil {
		gas += uint64(len(authList)) * params.CallNewAccountGas
	}
	return vm.GasCosts{RegularGas: gas}, nil
}

// FloorDataGas computes the minimum gas required for a transaction based on its data tokens (EIP-7623).
func FloorDataGas(rules params.Rules, data []byte, accessList types.AccessList) (uint64, error) {
	var (
		tokens    uint64
		tokenCost uint64
	)
	if rules.IsAmsterdam {
		// EIP-7976 changes how calldata is priced.
		// From 10/40 to 64/64 for zero/non-zero bytes.
		tokenCost = params.TxCostFloorPerToken7976
		dataLen := uint64(len(data))
		if math.MaxUint64/params.TxTokenPerNonZeroByte < dataLen {
			return 0, ErrGasUintOverflow
		}
		tokens = dataLen * params.TxTokenPerNonZeroByte

		// EIP-7981 adds additional tokens for every entry in the accesslist
		const addressTokenCost = uint64(common.AddressLength) * params.TxTokenPerNonZeroByte
		addresses := uint64(len(accessList))
		if (math.MaxUint64-tokens)/addressTokenCost < addresses {
			return 0, ErrGasUintOverflow
		}
		tokens += addresses * addressTokenCost

		const storageKeyTokenCost = uint64(common.HashLength) * params.TxTokenPerNonZeroByte
		storageKeys := uint64(accessList.StorageKeys())
		if (math.MaxUint64-tokens)/storageKeyTokenCost < storageKeys {
			return 0, ErrGasUintOverflow
		}
		tokens += storageKeys * storageKeyTokenCost
	} else {
		var (
			z  = uint64(bytes.Count(data, []byte{0}))
			nz = uint64(len(data)) - z
		)
		// Pre-Amsterdam
		if math.MaxUint64/params.TxTokenPerNonZeroByte < nz {
			return 0, ErrGasUintOverflow
		}
		tokens = nz * params.TxTokenPerNonZeroByte
		if math.MaxUint64-tokens < z {
			return 0, ErrGasUintOverflow
		}
		tokens += z
		tokenCost = params.TxCostFloorPerToken
	}

	// Check for overflow
	if (math.MaxUint64-params.TxGas)/tokenCost < tokens {
		return 0, ErrGasUintOverflow
	}
	// Minimum gas required for a transaction based on its data tokens (EIP-7623).
	return params.TxGas + tokens*tokenCost, nil
}

// toWordSize returns the ceiled word size required for init code payment calculation.
func toWordSize(size uint64) uint64 {
	if size > math.MaxUint64-31 {
		return math.MaxUint64/32 + 1
	}

	return (size + 31) / 32
}

// A Message contains the data derived from a single transaction that is relevant to state
// processing.
type Message struct {
	To                    *common.Address
	From                  common.Address
	Nonce                 uint64
	Value                 *uint256.Int
	GasLimit              uint64
	GasPrice              *uint256.Int
	GasFeeCap             *uint256.Int
	GasTipCap             *uint256.Int
	Data                  []byte
	AccessList            types.AccessList
	BlobGasFeeCap         *uint256.Int
	BlobHashes            []common.Hash
	SetCodeAuthorizations []types.SetCodeAuthorization

	// When SkipNonceChecks is true, the message nonce is not checked against the
	// account nonce in state.
	//
	// This field will be set to true for operations like RPC eth_call
	// or the state prefetching.
	SkipNonceChecks bool

	// When set, the message is not treated as a transaction, and certain
	// transaction-specific checks are skipped:
	//
	// - From is not verified to be an EOA
	// - GasLimit is not checked against the protocol defined tx gaslimit
	SkipTransactionChecks bool
}

// TransactionToMessage converts a transaction into a Message.
func TransactionToMessage(tx *types.Transaction, s types.Signer, baseFee *big.Int) (*Message, error) {
	from, err := types.Sender(s, tx)
	if err != nil {
		return nil, err
	}
	gasPrice, overflow := uint256.FromBig(tx.GasPrice())
	if overflow {
		return nil, fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
			from.Hex(), tx.GasPrice().BitLen())
	}
	txGasFeeCap := tx.GasFeeCap()
	gasFeeCap, overflow := uint256.FromBig(txGasFeeCap)
	if overflow {
		return nil, fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
			from.Hex(), tx.GasFeeCap().BitLen())
	}
	txGasTipCap := tx.GasTipCap()
	gasTipCap, overflow := uint256.FromBig(txGasTipCap)
	if overflow {
		return nil, fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
			from.Hex(), tx.GasTipCap().BitLen())
	}
	value, overflow := uint256.FromBig(tx.Value())
	if overflow {
		return nil, fmt.Errorf("value exceeds 256 bits: address %v", from.Hex())
	}
	blobGasFeeCap, overflow := uint256.FromBig(tx.BlobGasFeeCap())
	if overflow {
		return nil, fmt.Errorf("blobGasFeeCap exceeds 256 bits: address %v", from.Hex())
	}

	msg := &Message{
		From:                  from,
		Nonce:                 tx.Nonce(),
		GasLimit:              tx.Gas(),
		GasPrice:              gasPrice,
		GasFeeCap:             gasFeeCap,
		GasTipCap:             gasTipCap,
		To:                    tx.To(),
		Value:                 value,
		Data:                  tx.Data(),
		AccessList:            tx.AccessList(),
		SetCodeAuthorizations: tx.SetCodeAuthorizations(),
		SkipNonceChecks:       false,
		SkipTransactionChecks: false,
		BlobHashes:            tx.BlobHashes(),
		BlobGasFeeCap:         blobGasFeeCap,
	}
	// If baseFee provided, set gasPrice to effectiveGasPrice.
	if baseFee != nil {
		effectiveGasPrice := new(big.Int).Add(baseFee, txGasTipCap)
		if effectiveGasPrice.Cmp(txGasFeeCap) > 0 {
			effectiveGasPrice = txGasFeeCap
		}
		// EffectiveGasPrice is already capped by txGasFeeCap, therefore
		// the overflow check is not required.
		msg.GasPrice = uint256.MustFromBig(effectiveGasPrice)
	}
	return msg, nil
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(evm *vm.EVM, msg *Message, gp *GasPool) (*ExecutionResult, error) {
	// Do not panic if the gas pool is nil. This is allowed when executing
	// a single message via RPC invocation.
	if gp == nil {
		gp = NewGasPool(msg.GasLimit)
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	return newStateTransition(evm, msg, gp).execute()
}

// stateTransition represents a state transition.
//
// == The State Transitioning Model
//
// A state transition is a change made when a transaction is applied to the current world
// state. The state transitioning model does all the necessary work to work out a valid new
// state root.
//
//  1. Nonce handling
//  2. Pre pay gas
//  3. Create a new state object if the recipient is nil
//  4. Value transfer
//
// == If contract creation ==
//
//	4a. Attempt to run transaction data
//	4b. If valid, use result as code for the new state object
//
// == end ==
//
//  5. Run Script section
//  6. Derive new state root
type stateTransition struct {
	gp            *GasPool
	msg           *Message
	initialBudget vm.GasBudget
	gasRemaining  vm.GasBudget
	state         vm.StateDB
	evm           *vm.EVM

	// passBalance is the temporary balance borrowed by a legacy recharge /
	// refundWithdraw transaction, nil for every other transaction. passSnapshot
	// is the state checkpoint taken right before the borrow; the settlement at
	// the end of execute() either returns the borrow or rewinds the whole
	// transaction to that checkpoint (pgp-chain semantics, see
	// settlePassBalance).
	passBalance  *uint256.Int
	passSnapshot int

	// vmErr records the EVM level error of the message, so the pass balance
	// settlement can rewind the transaction exactly like pgp-chain does
	// (pgp-chain/core/state_transition.go:263-267).
	vmErr error
}

// newStateTransition initialises and returns a new state transition object.
func newStateTransition(evm *vm.EVM, msg *Message, gp *GasPool) *stateTransition {
	return &stateTransition{
		gp:    gp,
		evm:   evm,
		msg:   msg,
		state: evm.StateDB,
	}
}

// to returns the recipient of the message.
func (st *stateTransition) to() common.Address {
	if st.msg == nil || st.msg.To == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To
}

func (st *stateTransition) buyGas() error {
	mgval := new(uint256.Int).SetUint64(st.msg.GasLimit)
	_, overflow := mgval.MulOverflow(mgval, st.msg.GasPrice)
	if overflow {
		return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
	}
	balanceCheck := new(uint256.Int).Set(mgval)
	if st.msg.GasFeeCap != nil {
		balanceCheck.SetUint64(st.msg.GasLimit)
		if _, overflow := balanceCheck.MulOverflow(balanceCheck, st.msg.GasFeeCap); overflow {
			return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}
	}
	if st.msg.Value != nil {
		if _, overflow := balanceCheck.AddOverflow(balanceCheck, st.msg.Value); overflow {
			return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}
	}

	if st.evm.ChainConfig().IsCancun(st.evm.Context.BlockNumber, st.evm.Context.Time) {
		if blobGas := st.blobGasUsed(); blobGas > 0 {
			// Check that the user has enough funds to cover blobGasUsed * tx.BlobGasFeeCap
			blobBalanceCheck := new(uint256.Int).SetUint64(blobGas)
			if _, overflow := blobBalanceCheck.MulOverflow(blobBalanceCheck, st.msg.BlobGasFeeCap); overflow {
				return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
			if _, overflow := balanceCheck.AddOverflow(balanceCheck, blobBalanceCheck); overflow {
				return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
			// Pay for blobGasUsed * actual blob fee
			blobBaseFee, overflow := uint256.FromBig(st.evm.Context.BlobBaseFee)
			if overflow {
				return fmt.Errorf("invalid blobBaseFee: %v", st.evm.Context.BlobBaseFee)
			}
			blobFee := new(uint256.Int).SetUint64(blobGas)

			// In practice, overflow checking is unnecessary, as blobBaseFee cannot exceed
			// BlobGasFeeCap. However, in eth_call it is still possible for users to specify
			// an excessively large blob base fee and bypass the blob base fee validation.
			_, overflow = blobFee.MulOverflow(blobFee, blobBaseFee)
			if overflow {
				return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
			_, overflow = mgval.AddOverflow(mgval, blobFee)
			if overflow {
				return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
		}
	}
	if have, want := st.state.GetBalance(st.msg.From), balanceCheck; have.Cmp(want) < 0 {
		return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
	}
	if err := st.gp.SubGas(st.msg.GasLimit); err != nil {
		return err
	}

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil {
		st.evm.Config.Tracer.OnGasChange(0, st.msg.GasLimit, tracing.GasChangeTxInitialBalance)
	}
	st.gasRemaining = vm.NewGasBudget(st.msg.GasLimit)
	st.initialBudget = st.gasRemaining.Copy()

	st.state.SubBalance(st.msg.From, mgval, tracing.BalanceDecreaseGasBuy)
	return nil
}

func (st *stateTransition) preCheck() error {
	// Only check transactions that are not fake
	msg := st.msg
	if !msg.SkipNonceChecks {
		// Make sure this transaction's nonce is correct.
		stNonce := st.state.GetNonce(msg.From)
		if msgNonce := msg.Nonce; stNonce < msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce+1 < stNonce {
			return fmt.Errorf("%w: address %v, nonce: %d", ErrNonceMax,
				msg.From.Hex(), stNonce)
		}
	}
	isOsaka := st.evm.ChainConfig().IsOsaka(st.evm.Context.BlockNumber, st.evm.Context.Time)
	isAmsterdam := st.evm.ChainConfig().IsAmsterdam(st.evm.Context.BlockNumber, st.evm.Context.Time)
	// pgp-chain has no EIP-3607 check at all: its preCheck only validates the
	// nonce and buys gas (pgp-chain/core/state_transition.go:221-232), so a
	// legacy transaction may be sent from an account that has code. This tree
	// applies the check unconditionally, so it has to be switched off for the
	// legacy range explicitly.
	isLegacy := st.evm.ChainConfig().IsPGPLegacy(st.evm.Context.BlockNumber)
	if !msg.SkipTransactionChecks {
		// Verify tx gas limit does not exceed EIP-7825 cap.
		if isOsaka && !isAmsterdam && msg.GasLimit > params.MaxTxGas {
			return fmt.Errorf("%w (cap: %d, tx: %d)", ErrGasLimitTooHigh, params.MaxTxGas, msg.GasLimit)
		}
		// Make sure the sender is an EOA
		if !isLegacy {
			code := st.state.GetCode(msg.From)
			_, delegated := types.ParseDelegation(code)
			if len(code) > 0 && !delegated {
				return fmt.Errorf("%w: address %v, len(code): %d", ErrSenderNoEOA, msg.From.Hex(), len(code))
			}
		}
	}
	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	if st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
		skipCheck := st.evm.Config.NoBaseFee && msg.GasFeeCap.BitLen() == 0 && msg.GasTipCap.BitLen() == 0
		if !skipCheck {
			if msg.GasFeeCap.Cmp(msg.GasTipCap) < 0 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
					msg.From.Hex(), msg.GasTipCap, msg.GasFeeCap)
			}
			// This will panic if baseFee is nil, but basefee presence is verified
			// as part of header validation.
			if msg.GasFeeCap.CmpBig(st.evm.Context.BaseFee) < 0 {
				return fmt.Errorf("%w: address %v, maxFeePerGas: %s, baseFee: %s", ErrFeeCapTooLow,
					msg.From.Hex(), msg.GasFeeCap, st.evm.Context.BaseFee)
			}
		}
	}
	// Check the blob version validity
	if msg.BlobHashes != nil {
		// The to field of a blob tx type is mandatory, and a `BlobTx` transaction internally
		// has it as a non-nillable value, so any msg derived from blob transaction has it non-nil.
		// However, messages created through RPC (eth_call) don't have this restriction.
		if msg.To == nil {
			return ErrBlobTxCreate
		}
		if len(msg.BlobHashes) == 0 {
			return ErrMissingBlobHashes
		}
		if isOsaka && len(msg.BlobHashes) > params.BlobTxMaxBlobs {
			return ErrTooManyBlobs
		}
		for i, hash := range msg.BlobHashes {
			if !kzg4844.IsValidVersionedHash(hash[:]) {
				return fmt.Errorf("blob %d has invalid hash version", i)
			}
		}
	}
	// Check that the user is paying at least the current blob fee
	if st.evm.ChainConfig().IsCancun(st.evm.Context.BlockNumber, st.evm.Context.Time) {
		if st.blobGasUsed() > 0 {
			// Skip the checks if gas fields are zero and blobBaseFee was explicitly disabled (eth_call)
			skipCheck := st.evm.Config.NoBaseFee && msg.BlobGasFeeCap.BitLen() == 0
			if !skipCheck {
				// This will panic if blobBaseFee is nil, but blobBaseFee presence
				// is verified as part of header validation.
				if msg.BlobGasFeeCap.CmpBig(st.evm.Context.BlobBaseFee) < 0 {
					return fmt.Errorf("%w: address %v blobGasFeeCap: %v, blobBaseFee: %v", ErrBlobFeeCapTooLow,
						msg.From.Hex(), msg.BlobGasFeeCap, st.evm.Context.BlobBaseFee)
				}
			}
		}
	}
	// Check that EIP-7702 authorization list signatures are well formed.
	if msg.SetCodeAuthorizations != nil {
		if msg.To == nil {
			return fmt.Errorf("%w (sender %v)", ErrSetCodeTxCreate, msg.From)
		}
		if len(msg.SetCodeAuthorizations) == 0 {
			return fmt.Errorf("%w (sender %v)", ErrEmptyAuthList, msg.From)
		}
	}
	return st.buyGas()
}

// execute will transition the state by applying the current message and
// returning the evm execution result with following fields.
//
//   - used gas: total gas used (including gas being refunded)
//   - returndata: the returned data from evm
//   - concrete execution error: various EVM errors which abort the execution, e.g.
//     ErrOutOfGas, ErrExecutionReverted
//
// However if any consensus issue encountered, return the error directly with
// nil evm execution result.
func (st *stateTransition) execute() (result *ExecutionResult, err error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. caller has enough balance to cover transaction fee(gaslimit * gasprice)
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	var (
		msg              = st.msg
		rules            = st.evm.ChainConfig().Rules(st.evm.Context.BlockNumber, st.evm.Context.Random != nil, st.evm.Context.Time)
		contractCreation = msg.To == nil
		floorDataGas     uint64
		// legacyGasRefund marks the two ELA gateway calls (Recharge and
		// refundWithdraw) that pgp-chain executes on the house: the pass balance
		// is lent to the sender, the gas is refunded in full and the fee goes
		// back to the sender (pgp-chain/core/state_transition.go:256-282,
		// :325-334, :437-447).
		legacyGasRefund = rules.IsPGPLegacy &&
			(params.IsPGPRechargeTx(msg.Data, msg.To) || params.IsPGPRefundWithdrawTx(msg.Data, msg.To))
	)
	if legacyGasRefund {
		// pgp-chain refuses a Recharge whose ELA side is already settled
		// (pgp-chain/core/state_transition.go:256-261). The answer is a slot in
		// ELAMinter's own storage, so it is read from the state being executed
		// rather than from anything outside the block - see
		// legacyRechargeAlreadyCompleted.
		if legacyRechargeAlreadyCompleted(st.state, msg.Data, msg.To) {
			return nil, errLegacyTxAlreadyCompleted
		}
		// The borrow has to happen before preCheck/buyGas, otherwise the sender
		// cannot pay for the gas of the gateway call (it has nothing to spend).
		st.passSnapshot = st.borrowPassBalance()
		defer func() { st.settlePassBalance(&result, &err) }()
	}

	// Check clauses 1-3, buy gas if everything is correct
	if err := st.preCheck(); err != nil {
		return nil, err
	}
	if st.evm.ChainConfig().IsNano(st.evm.Context.BlockNumber) {
		for _, blackListAddr := range types.NanoBlackList {
			if blackListAddr == msg.From {
				return nil, errors.New("block blacklist account")
			}
			if msg.To != nil && *msg.To == blackListAddr {
				return nil, errors.New("block blacklist account")
			}
		}
	}
	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	//
	// pgp-chain's IntrinsicGas has no EIP-3860 word gas at all
	// (pgp-chain/core/state_transition.go:125-163), so the legacy range must not
	// be charged for it even though it is Shanghai-active. The init code *size*
	// limit further down is a different rule and does apply.
	cost, err := IntrinsicGas(msg.Data, msg.AccessList, msg.SetCodeAuthorizations, contractCreation,
		rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai && !rules.IsPGPLegacy, rules.IsAmsterdam)
	if err != nil {
		return nil, err
	}
	prior, sufficient := st.gasRemaining.Charge(cost)
	if !sufficient {
		return nil, fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gasRemaining.RegularGas, cost.RegularGas)
	}
	if t := st.evm.Config.Tracer; t != nil && t.OnGasChange != nil {
		t.OnGasChange(prior, st.gasRemaining.RegularGas, tracing.GasChangeTxIntrinsicGas)
	}
	// Gas limit suffices for the floor data cost (EIP-7623)
	if rules.IsPrague {
		floorDataGas, err = FloorDataGas(rules, msg.Data, msg.AccessList)
		if err != nil {
			return nil, err
		}
		if msg.GasLimit < floorDataGas {
			return nil, fmt.Errorf("%w: have %d, want %d", ErrFloorDataGas, msg.GasLimit, floorDataGas)
		}
	}

	if rules.IsEIP4762 {
		st.evm.AccessEvents.AddTxOrigin(msg.From)

		if targetAddr := msg.To; targetAddr != nil {
			st.evm.AccessEvents.AddTxDestination(*targetAddr, msg.Value.Sign() != 0, !st.state.Exist(*targetAddr))
		}
	}

	// Check clause 6
	value := msg.Value
	if value == nil {
		value = new(uint256.Int)
	}
	if !value.IsZero() && !st.evm.Context.CanTransfer(st.state, msg.From, value) {
		return nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From.Hex())
	}

	// Check whether the init code size has been exceeded.
	if contractCreation {
		if err := vm.CheckMaxInitCodeSize(&rules, uint64(len(msg.Data))); err != nil {
			return nil, err
		}
	}

	// Execute the preparatory steps for state transition which includes:
	// - prepare accessList(post-berlin)
	// - reset transient storage(eip 1153)
	st.state.Prepare(rules, msg.From, st.evm.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)

	var (
		ret   []byte
		vmerr error // vm errors do not effect consensus and are therefore not assigned to err
	)
	if contractCreation {
		ret, _, st.gasRemaining, vmerr = st.evm.Create(msg.From, msg.Data, st.gasRemaining, value)
	} else {
		// Increment the nonce for the next transaction.
		st.state.SetNonce(msg.From, st.state.GetNonce(msg.From)+1, tracing.NonceChangeEoACall)

		// Apply EIP-7702 authorizations.
		if msg.SetCodeAuthorizations != nil {
			for _, auth := range msg.SetCodeAuthorizations {
				// Note errors are ignored, we simply skip invalid authorizations here.
				st.applyAuthorization(&auth)
			}
		}

		// Perform convenience warming of sender's delegation target. Although the
		// sender is already warmed in Prepare(..), it's possible a delegation to
		// the account was deployed during this transaction. To handle correctly,
		// simply wait until the final state of delegations is determined before
		// performing the resolution and warming.
		if addr, ok := types.ParseDelegation(st.state.GetCode(*msg.To)); ok {
			st.state.AddAddressToAccessList(addr)
		}

		// Execute the transaction's call.
		ret, st.gasRemaining, vmerr = st.evm.Call(msg.From, st.to(), msg.Data, st.gasRemaining, value)
	}

	// Record the gas used excluding gas refunds. This value represents the actual
	// gas allowance required to complete execution.
	peakGasUsed := st.gasUsed()
	st.vmErr = vmerr

	if legacyGasRefund && vmerr == nil {
		// pgp-chain's refundCostGas: a successful recharge / refundWithdraw
		// hands the entire gas back, so the receipt reports GasUsed 0 and the
		// block gas counter does not move (pgp-chain/core/state_transition.go:437-447).
		st.gasRemaining.Refund(vm.NewGasBudget(st.gasUsed()))
	} else {
		// Compute refund counter, capped to a refund quotient.
		st.gasRemaining.Refund(st.calcRefund())
	}

	if rules.IsPrague {
		// After EIP-7623: Data-heavy transactions pay the floor gas.
		if used := st.gasUsed(); used < floorDataGas {
			prior, _ := st.gasRemaining.Charge(vm.GasCosts{RegularGas: floorDataGas - used})
			if t := st.evm.Config.Tracer; t != nil && t.OnGasChange != nil {
				t.OnGasChange(prior, st.gasRemaining.RegularGas, tracing.GasChangeTxDataFloor)
			}
		}
		if peakGasUsed < floorDataGas {
			peakGasUsed = floorDataGas
		}
	}
	// Return gas to the user
	st.returnGas()

	// Return gas to the gas pool
	if rules.IsAmsterdam {
		// Refund is excluded for returning
		err = st.gp.ReturnGas(st.initialBudget.RegularGas-peakGasUsed, st.gasUsed())
	} else {
		// Refund is included for returning
		err = st.gp.ReturnGas(st.gasRemaining.RegularGas, st.gasUsed())
	}
	if err != nil {
		return nil, err
	}
	effectiveTip := msg.GasPrice
	if rules.IsLondon {
		// A legacy header carries no baseFee at all, in which case
		// core.NewEVMBlockContext substitutes zero and the tip is the full gas
		// price — the same arithmetic pgp-chain does with its
		// minerFee = gasUsed * gasPrice. The nil check keeps synthetic contexts
		// (RPC simulation, tests) from panicking.
		baseFee := new(uint256.Int)
		if st.evm.Context.BaseFee != nil {
			var overflow bool
			baseFee, overflow = uint256.FromBig(st.evm.Context.BaseFee)
			if overflow {
				return nil, fmt.Errorf("invalid baseFee: %v", st.evm.Context.BaseFee)
			}
		}
		effectiveTip = new(uint256.Int).Sub(msg.GasPrice, baseFee)
	}

	fee := new(uint256.Int).SetUint64(st.gasUsed())
	fee.Mul(fee, effectiveTip)
	// consensus engine is parlia
	if rules.IsPGPLegacy {
		st.payLegacyFee(fee, legacyGasRefund)
	} else if st.evm.ChainConfig().IsInBSC() {
		st.state.AddBalance(consensus.SystemAddress, fee, tracing.BalanceIncreaseRewardTransactionFee)
		// add extra blob fee reward
		if rules.IsCancun {
			blobFee := new(big.Int).SetUint64(st.blobGasUsed())
			blobFee.Mul(blobFee, st.evm.Context.BlobBaseFee)
			blobFeeU256, _ := uint256.FromBig(blobFee)
			st.state.AddBalance(consensus.SystemAddress, blobFeeU256, tracing.BalanceIncreaseRewardTransactionFee)
		}
	} else {
		st.state.AddBalance(st.evm.Context.Coinbase, fee, tracing.BalanceIncreaseRewardTransactionFee)

		// add the coinbase to the witness iff the fee is greater than 0
		if rules.IsEIP4762 && fee.Sign() != 0 {
			st.evm.AccessEvents.AddAccount(st.evm.Context.Coinbase, true, math.MaxUint64)
		}
	}
	if rules.IsAmsterdam {
		for _, log := range st.evm.StateDB.LogsForBurnAccounts() {
			st.evm.StateDB.AddLog(log)
		}
	}
	return &ExecutionResult{
		UsedGas:    st.gasUsed(),
		MaxUsedGas: peakGasUsed,
		Err:        vmerr,
		ReturnData: ret,
	}, nil
}

// validateAuthorization validates an EIP-7702 authorization against the state.
func (st *stateTransition) validateAuthorization(auth *types.SetCodeAuthorization) (authority common.Address, err error) {
	// Verify chain ID is null or equal to current chain ID.
	if !auth.ChainID.IsZero() && auth.ChainID.CmpBig(st.evm.ChainConfig().ChainID) != 0 {
		return authority, ErrAuthorizationWrongChainID
	}
	// Limit nonce to 2^64-1 per EIP-2681.
	if auth.Nonce+1 < auth.Nonce {
		return authority, ErrAuthorizationNonceOverflow
	}
	// Validate signature values and recover authority.
	authority, err = auth.Authority()
	if err != nil {
		return authority, fmt.Errorf("%w: %v", ErrAuthorizationInvalidSignature, err)
	}
	if slices.Contains(types.NanoBlackList, authority) {
		return authority, errors.New("block blacklist account")
	}
	// Check the authority account
	//  1) doesn't have code or has existing delegation
	//  2) matches the auth's nonce
	//
	// Note it is added to the access list even if the authorization is invalid.
	st.state.AddAddressToAccessList(authority)
	code := st.state.GetCode(authority)
	if _, ok := types.ParseDelegation(code); len(code) != 0 && !ok {
		return authority, ErrAuthorizationDestinationHasCode
	}
	if have := st.state.GetNonce(authority); have != auth.Nonce {
		return authority, ErrAuthorizationNonceMismatch
	}
	return authority, nil
}

// applyAuthorization applies an EIP-7702 code delegation to the state.
func (st *stateTransition) applyAuthorization(auth *types.SetCodeAuthorization) error {
	authority, err := st.validateAuthorization(auth)
	if err != nil {
		return err
	}

	// If the account already exists in state, refund the new account cost
	// charged in the intrinsic calculation.
	if st.state.Exist(authority) {
		st.state.AddRefund(params.CallNewAccountGas - params.TxAuthTupleGas)
	}

	// Update nonce and account code.
	st.state.SetNonce(authority, auth.Nonce+1, tracing.NonceChangeAuthorization)
	if auth.Address == (common.Address{}) {
		// Delegation to zero address means clear.
		st.state.SetCode(authority, nil, tracing.CodeChangeAuthorizationClear)
		return nil
	}

	// Otherwise install delegation to auth.Address.
	st.state.SetCode(authority, types.AddressToDelegation(auth.Address), tracing.CodeChangeAuthorization)

	return nil
}

// payLegacyFee distributes the transaction fee with the pgp-chain legacy rules.
//
// pgp-chain settles the fee inside the transaction rather than in Finalize
// (pgp-chain/core/state_transition.go:331-352):
//
//   - recharge / refundWithdraw: the whole fee is returned to the sender, the
//     call is free;
//   - otherwise GasFeeRecipientRatio percent (65 on mainnet, integer division
//     rounding down) goes to the PG fee sink and the remainder to the block
//     producer named in the header.
//
// The producer is Context.Coinbase, which core.NewEVMBlockContext fills from
// Engine.Author — consensus/pgp.Author returns the header coinbase, so this is
// the block's producer exactly as pgp-chain sees it.
func (st *stateTransition) payLegacyFee(fee *uint256.Int, gasRefunded bool) {
	if gasRefunded {
		st.state.AddBalance(st.msg.From, fee, tracing.BalanceIncreaseRewardTransactionFee)
		return
	}
	sink, ratio := st.evm.ChainConfig().GasFeeSplit()
	if ratio > 0 && sink != (common.Address{}) {
		pgFee := new(uint256.Int).Mul(fee, uint256.NewInt(ratio))
		pgFee.Div(pgFee, uint256.NewInt(100))
		st.state.AddBalance(sink, pgFee, tracing.BalanceIncreaseRewardTransactionFee)
		fee = new(uint256.Int).Sub(fee, pgFee)
	}
	st.state.AddBalance(st.evm.Context.Coinbase, fee, tracing.BalanceIncreaseRewardTransactionFee)
}

// legacyRechargeAlreadyCompleted reports whether the ELA Minter contract at
// params.ELAMINTER has already settled the ELA transaction named by a Recharge
// call, in which case the transaction has to be rejected with
// errLegacyTxAlreadyCompleted (pgp-chain/core/state_transition.go:256-261).
//
// The answer is a single storage slot, so it is read straight from the state
// being executed. pgp-chain instead ABI-packed a completed(bytes32) call and
// issued it to itself with nil call options (pgp-chain/spv/elaminter.go:113-134);
// nil call options mean "latest state of the node", which is not something a
// replay can reproduce. Reading the slot from state removes the whole problem:
// the state at block N is the state at block N, whenever it is replayed, and no
// ELA SPV database is involved.
//
// Only Recharge is checked. pgp-chain's IsCompletedByTxInput matches the Recharge
// method id and answers "not completed" for every other method, refundWithdraw
// included (pgp-chain/spv/elaminter.go:86-96). That filter is not cosmetic:
// refundWithdraw(bytes32) writes the *same* mapping (completed is slot 1 of
// ELAMinter, see params.CompletedSlot), so a withdraw would look "already
// completed" to a storage-only check the moment the withdraw itself landed, and
// every such transaction would be rejected - transactions pgp-chain accepted and
// that are therefore already in the chain.
//
// params.IsPGPRechargeTx and params.LegacyRechargeElaHash both filter on the
// method id. The redundancy is deliberate: the gate states the consensus rule,
// the extractor keeps its own check because it reproduces pgp-chain's filtering
// on its own.
func legacyRechargeAlreadyCompleted(state vm.StateDB, input []byte, to *common.Address) bool {
	if !params.IsPGPRechargeTx(input, to) {
		return false
	}
	elaHash, ok := params.LegacyRechargeElaHash(input)
	if !ok {
		return false
	}
	slot := params.CompletedSlot(common.HexToHash(elaHash))
	return state.GetState(params.ELAMINTER, slot) == params.CompletedSetValue
}

// borrowPassBalance lends the configured pass balance to the sender of a legacy
// recharge / refundWithdraw transaction, recording the checkpoint needed to
// settle it afterwards (pgp-chain/core/state_transition.go:256-282).
func (st *stateTransition) borrowPassBalance() int {
	amount := uint256.NewInt(st.evm.ChainConfig().PassBalance())
	if amount.Sign() == 0 {
		return -1
	}
	snapshot := st.state.Snapshot()
	st.passBalance = amount
	st.state.AddBalance(st.msg.From, amount, tracing.BalanceIncreaseRewardTransactionFee)
	return snapshot
}

// settlePassBalance takes the borrowed pass balance back, following pgp-chain's
// three way branch exactly:
//
//  1. the transaction failed (EVM error or consensus error) → the whole
//     transaction is rewound to the checkpoint taken before the borrow;
//  2. the sender ended up with less than the borrowed amount, i.e. the call
//     spent from the pass balance → the transaction is rewound and rejected with
//     ErrGasLimitReached;
//  3. otherwise the borrow is simply taken back.
//
// Case 2 is a consensus rule, not a local policy: pgp-chain returns the error
// from TransitionDb, which makes the block invalid.
func (st *stateTransition) settlePassBalance(result **ExecutionResult, err *error) {
	if st.passBalance == nil {
		return
	}
	if st.vmErr != nil || *err != nil {
		st.state.RevertToSnapshot(st.passSnapshot)
		return
	}
	if have := st.state.GetBalance(st.msg.From); have.Cmp(st.passBalance) < 0 {
		if *result != nil {
			(*result).UsedGas = 0
			(*result).MaxUsedGas = 0
			(*result).Err = nil
			(*result).ReturnData = nil
		}
		if *err == nil {
			*err = ErrGasLimitReached
		}
		st.state.RevertToSnapshot(st.passSnapshot)
		return
	}
	st.state.SubBalance(st.msg.From, st.passBalance, tracing.BalanceChangeUnspecified)
}

// calcRefund computes refund counter, capped to a refund quotient.
func (st *stateTransition) calcRefund() vm.GasBudget {
	var refund uint64
	// pgp-chain caps refunds at gasUsed/2 unconditionally: it defines
	// RefundQuotientEIP3529 but never uses it
	// (pgp-chain/core/state_transition.go:420-435), so EIP-3529 does not apply
	// to the legacy range even though it is London-active by block number.
	if st.evm.ChainConfig().IsPGPLegacy(st.evm.Context.BlockNumber) ||
		!st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Before EIP-3529: refunds were capped to gasUsed / 2
		refund = st.gasUsed() / params.RefundQuotient
	} else {
		// After EIP-3529: refunds are capped to gasUsed / 5
		refund = st.gasUsed() / params.RefundQuotientEIP3529
	}
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && refund > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, st.gasRemaining.RegularGas+refund, tracing.GasChangeTxRefunds)
	}
	return vm.NewGasBudget(refund)
}

// returnGas returns ETH for remaining gas,
// exchanged at the original rate.
func (st *stateTransition) returnGas() {
	remaining := uint256.NewInt(st.gasRemaining.RegularGas)
	remaining.Mul(remaining, st.msg.GasPrice)
	st.state.AddBalance(st.msg.From, remaining, tracing.BalanceIncreaseGasReturn)

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && st.gasRemaining.RegularGas > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, 0, tracing.GasChangeTxLeftOverReturned)
	}
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *stateTransition) gasUsed() uint64 {
	return st.gasRemaining.Used(st.initialBudget)
}

// blobGasUsed returns the amount of blob gas used by the message.
func (st *stateTransition) blobGasUsed() uint64 {
	return uint64(len(st.msg.BlobHashes) * params.BlobTxBlobGasPerBlob)
}
