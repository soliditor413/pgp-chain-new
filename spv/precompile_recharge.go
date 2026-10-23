package spv

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// rechargeDataPrecompile is the real implementation of the 0x3F1
// getMainChainRechargeData precompile.
//
// It lives in spv, not in core/vm, because the answer comes from the ELA
// recharge store and core/vm must not import spv (spv imports core/vm to
// register precompiles, so the reverse edge would be a cycle). core/vm exposes
// RegisterPGPLegacyPrecompile for exactly this, and falls back to a
// gas-charging, always-failing stub until it is called.
//
// Reference: pgp-chain/core/vm/contracts.go:1484-1568. Two differences, both
// deliberate:
//
//   - pgp first called spv.TrySetRechargeDataFromSpvService(hash), which fetched
//     the transaction from whatever peer answered and cached it in
//     spvTransactiondb. That made the answer depend on the node's sync progress,
//     and it reached stateRoot. Here the record must already have been indexed
//     from a block this node synced itself; a miss is an error.
//   - the ABI encoding is done by vm.EncodePGPRechargeDatas, which is covered by
//     its own golden tests, instead of rebuilding the argument list inline on
//     every call.
type rechargeDataPrecompile struct{}

func (c *rechargeDataPrecompile) Name() string { return "getMainChainRechargeData" }

// RequiredGas matches pgp-chain: its RequiredGas is a literal 0, so the whole
// cost of the call is the plain call overhead. Charging anything here would
// change the gasUsed of every block that redeems a deposit.
func (c *rechargeDataPrecompile) RequiredGas([]byte) uint64 {
	return params.PGPGetMainChainRechargeDataGas
}

func (c *rechargeDataPrecompile) Run(input []byte) ([]byte, error) {
	return c.run(input)
}

// RunWithBlockContext is the path RunPrecompiledContract actually takes. The
// encoding is identical; the block number is only captured so that a failure can
// be attributed to the block that diverged, which is the single most useful
// piece of information when a state root mismatch shows up thousands of blocks
// later.
func (c *rechargeDataPrecompile) RunWithBlockContext(ctx vm.BlockContext, input []byte) ([]byte, error) {
	out, err := c.run(input)
	if err != nil {
		block := "-"
		if ctx.BlockNumber != nil {
			block = ctx.BlockNumber.String()
		}
		log.Error("pgp: 0x3f1 getMainChainRechargeData could not answer; "+
			"the calling block cannot be executed faithfully",
			"block", block, "elaHash", rechargeHashFromInput(input), "err", err)
	}
	return out, err
}

var _ vm.BlockContextPrecompiledContract = (*rechargeDataPrecompile)(nil)

// run returns the encoded recharge records, or an error.
//
// An error is the only correct outcome for a miss. Returning the zero value
// (count = 0) instead would tell ELAMinter "this ELA tx paid for nothing", which
// mints nothing and *succeeds* — a wrong state root with no error anywhere. With
// an error the call reverts, the refund of the legacy gas path does not happen
// (core/state_transition.go only refunds when vmerr == nil), gasUsed stops being
// zero, and block validation rejects the block on the exact height that needs
// the missing data.
func (c *rechargeDataPrecompile) run(input []byte) ([]byte, error) {
	elaHash := rechargeHashFromInput(input)

	datas, _, err := GetRechargeDataByTxhash(elaHash)
	if err != nil {
		return nil, err
	}

	encoded := make([]vm.PGPRechargeData, 0, len(datas))
	for i, d := range datas {
		if d == nil {
			return nil, fmt.Errorf("%w: record %d of %s", ErrRechargeUnavailable, i, elaHash)
		}
		encoded = append(encoded, vm.PGPRechargeData{
			TargetAddress: d.TargetAddress,
			TargetAmount:  d.TargetAmount,
			Fee:           d.Fee,
			TargetData:    d.TargetData,
		})
	}
	return vm.EncodePGPRechargeDatas(encoded)
}

// rechargeHashFromInput reads the elaHash argument.
//
// The caller passes a bytes32, so the normal case is a 32 byte input that is
// already the hash. pgp-chain read it with getData(input, 0, 32), i.e.
// RightPadBytes(input[:32], 32) — a *right* pad, which only differs from the ABI
// convention for inputs shorter than 32 bytes, where it yields a different hash
// than left padding would. It is reproduced byte for byte rather than "fixed",
// because a caller relying on it would otherwise get a different lookup key.
func rechargeHashFromInput(input []byte) string {
	raw := input
	if len(raw) > 32 {
		raw = raw[:32]
	}
	return common.BytesToHash(common.RightPadBytes(raw, 32)).String()
}

// registerRechargePrecompile installs the 0x3F1 implementation into core/vm.
//
// It is called from SpvDbInit, i.e. at the moment the recharge store becomes
// available, rather than from an init(), so that the precompile and the data it
// reads appear together. Registering it earlier would be harmless — the store
// lookup fails with ErrRechargeStoreNotInitialised — but this keeps the ordering
// obvious.
func registerRechargePrecompile() {
	addr := common.BytesToAddress(params.GetMainChainRechargeData.Bytes())
	if err := vm.RegisterPGPLegacyPrecompile(addr, &rechargeDataPrecompile{}); err != nil {
		log.Error("pgp: could not register the 0x3f1 getMainChainRechargeData precompile; "+
			"blocks that redeem an ELA deposit will fail", "address", addr.Hex(), "err", err)
		return
	}
	log.Info("pgp: registered 0x3f1 getMainChainRechargeData", "address", addr.Hex())
}
