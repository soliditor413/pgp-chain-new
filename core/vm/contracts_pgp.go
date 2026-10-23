// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The pgp-chain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the pgp-chain library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// ErrPGPLegacyPrecompileUnavailable is returned by an ELA precompile that has no
// implementation yet.
//
// It is not an invented failure mode: pgp-chain's own implementations return an
// error whenever the ELA SPV database cannot answer
// (pgp-chain/core/vm/contracts.go:699-711 errGettingArbitersFailed,
// :1418-1426 getMainChainBlockByHeight, :1490-1501 getMainChainRechargeData).
// Failing here therefore reproduces exactly what a pgp node with an empty SPV
// database does. It is still better than the situation before this file existed,
// where the twelve addresses were ordinary empty accounts: that charged 2600
// (cold access) instead of the precompile's own gas, and returned empty data that
// a caller could decode as success.
var ErrPGPLegacyPrecompileUnavailable = errors.New("pgp: legacy ELA precompile has no implementation")

// pgpLegacyStandardPrecompiles is the Ethereum part of the pgp-chain legacy set.
//
// It is the same nine entries as pgp-chain's PrecompiledContractsShangHai
// (pgp-chain/core/vm/contracts.go:149-171), written out instead of aliasing
// PrecompiledContractsBerlin so the two maps stay free to drift. bigModExp is the
// eip2565 variant, exactly as in Berlin.
var pgpLegacyStandardPrecompiles = PrecompiledContracts{
	common.BytesToAddress([]byte{0x1}): &ecrecover{},
	common.BytesToAddress([]byte{0x2}): &sha256hash{},
	common.BytesToAddress([]byte{0x3}): &ripemd160hash{},
	common.BytesToAddress([]byte{0x4}): &dataCopy{},
	common.BytesToAddress([]byte{0x5}): &bigModExp{eip2565: true},
	common.BytesToAddress([]byte{0x6}): &bn256AddIstanbul{},
	common.BytesToAddress([]byte{0x7}): &bn256ScalarMulIstanbul{},
	common.BytesToAddress([]byte{0x8}): &bn256PairingIstanbul{},
	common.BytesToAddress([]byte{0x9}): &blake2F{},
}

// pgpLegacyPrecompileSpec describes one ELA precompile: where it lives, what to
// call it in logs, how much gas it charges before running, and the line of
// pgp-chain it was copied from.
type pgpLegacyPrecompileSpec struct {
	address common.Address
	name    string
	gas     uint64
	source  string
}

// pgpLegacyPrecompileSpecs mirrors pgp-chain's PrecompiledContractsShangHai in
// declaration order. The gas column is the value its RequiredGas returns; see
// params/pgp_precompiles.go for the mapping back to pgp's constant names.
var pgpLegacyPrecompileSpecs = []pgpLegacyPrecompileSpec{
	{common.BytesToAddress(params.ArbiterAddress.Bytes()), "arbiters", params.PGPArbitersGas, "pgp-chain/core/vm/contracts.go:693"},
	{common.BytesToAddress(params.P256VerifyAddress.Bytes()), "p256Verify", params.PGPP256VerifyGas, "pgp-chain/core/vm/contracts.go:722"},
	{common.BytesToAddress(params.SignatureVerifyByPbk.Bytes()), "pbkVerifySignature", params.PGPPbkVerifySignatureGas, "pgp-chain/core/vm/contracts.go:748"},
	{common.BytesToAddress(params.PledgeBillVerify.Bytes()), "pledgeBillVerify", params.PGPPledgeBillVerifyGas, "pgp-chain/core/vm/contracts.go:771"},
	{common.BytesToAddress(params.PledgeBillTokenID.Bytes()), "pledgeBillTokenID", params.PGPPledgeBillTokenIDGas, "pgp-chain/core/vm/contracts.go:891"},
	{common.BytesToAddress(params.PledgeBillTokenDetail.Bytes()), "pledgeBillTokenDetail", params.PGPPledgeBillTokenDetailGas, "pgp-chain/core/vm/contracts.go:909"},
	{common.BytesToAddress(params.PledgeBillTokenVersion.Bytes()), "pledgeBillPayloadVersion", params.PGPPledgeBillPayloadVersionGas, "pgp-chain/core/vm/contracts.go:971"},
	{common.BytesToAddress(params.GetMainChainBlockByHeight.Bytes()), "getMainChainBlockByHeight", params.PGPGetMainChainBlockGas, "pgp-chain/core/vm/contracts.go:1412"},
	{common.BytesToAddress(params.GetMainChainLatestHeight.Bytes()), "getMainChainLatestHeight", params.PGPGetMainChainLatestHeightGas, "pgp-chain/core/vm/contracts.go:1457"},
	{common.BytesToAddress(params.GetMainChainRechargeData.Bytes()), "getMainChainRechargeData", params.PGPGetMainChainRechargeDataGas, "pgp-chain/core/vm/contracts.go:1484"},
	{common.BytesToAddress(params.GetWithdrawData.Bytes()), "getWithdrawData", params.PGPGetWithdrawDataGas, "pgp-chain/core/vm/contracts.go:1582"},
	{common.BytesToAddress(params.VerifySmallCrossTx.Bytes()), "verifySmallCrossTx", params.PGPVerifySmallCrossTxGas, "pgp-chain/core/vm/contracts.go:1622"},
}

// PrecompiledAddressesPGPLegacy is the EIP-2929 warm list for the legacy rules:
// the nine Ethereum precompiles plus the twelve ELA ones.
//
// Being on this list is worth real gas. core/state_transition.go:672 hands
// ActivePrecompiles(rules) to state.Prepare, so an address that is missing here
// is charged 2600 for its first access instead of 100. pgp-chain warms all
// twenty-one; leaving the ELA twelve out would diverge on the very first call
// that touches them even if the implementation were correct.
var PrecompiledAddressesPGPLegacy = pgpLegacyPrecompileAddresses()

func pgpLegacyPrecompileAddresses() []common.Address {
	out := make([]common.Address, 0, len(pgpLegacyStandardPrecompiles)+len(pgpLegacyPrecompileSpecs))
	for addr := range pgpLegacyStandardPrecompiles {
		out = append(out, addr)
	}
	for _, spec := range pgpLegacyPrecompileSpecs {
		out = append(out, spec.address)
	}
	return out
}

// unsupportedPGPLegacyPrecompile stands in for an ELA precompile until the ELA
// SPV subsystem registers a real one. It charges the reference gas and then fails
// the call, and it says so loudly the first time it is hit.
type unsupportedPGPLegacyPrecompile struct {
	spec pgpLegacyPrecompileSpec

	// once keeps the diagnostic to one line per address per process: a contract
	// retrying in a loop would otherwise flood the log.
	once sync.Once
}

func (p *unsupportedPGPLegacyPrecompile) RequiredGas([]byte) uint64 { return p.spec.gas }

func (p *unsupportedPGPLegacyPrecompile) Name() string { return p.spec.name }

func (p *unsupportedPGPLegacyPrecompile) Run(input []byte) ([]byte, error) {
	return p.RunWithBlockContext(BlockContext{}, input)
}

// RunWithBlockContext is the path RunPrecompiledContract actually takes; it is
// the only place that knows which block is being executed, which is exactly what
// an operator needs in order to find the transaction that diverged.
func (p *unsupportedPGPLegacyPrecompile) RunWithBlockContext(ctx BlockContext, input []byte) ([]byte, error) {
	pgpFailedPrecompiles.Add(1)
	block := "-"
	if ctx.BlockNumber != nil {
		block = ctx.BlockNumber.String()
	}
	p.once.Do(func() {
		log.Error("pgp: legacy ELA precompile has no implementation, blocks that call it cannot be executed faithfully",
			"precompile", p.spec.name,
			"address", p.spec.address.Hex(),
			"firstBlock", block,
			"reference", p.spec.source,
			"fix", "port the ELA SPV subsystem and call vm.RegisterPGPLegacyPrecompile")
	})
	return nil, ErrPGPLegacyPrecompileUnavailable
}

// BlockContextPrecompiledContract is what makes RunPrecompiledContract dispatch to
// the block-aware path above instead of Run.
var _ BlockContextPrecompiledContract = (*unsupportedPGPLegacyPrecompile)(nil)

var (
	pgpLegacyMu sync.RWMutex
	// pgpLegacyOverrides holds the real implementations, keyed by address.
	pgpLegacyOverrides = make(map[common.Address]PrecompiledContract)
	// pgpLegacyCache is the assembled 21-entry set; nil means "rebuild".
	pgpLegacyCache PrecompiledContracts
	// pgpLegacyLogOnce guards the one-shot completeness report.
	pgpLegacyLogOnce sync.Once
	// pgpFailedPrecompiles counts calls that hit an unimplemented precompile, so
	// a verification run can assert the number is zero.
	pgpFailedPrecompiles atomic.Uint64
)

// RegisterPGPLegacyPrecompile installs the real implementation of one of the
// twelve ELA precompiles.
//
// Registration is dynamic on purpose: the implementations need the ELA SPV
// database, which lives in a package that must not be imported by core/vm, and
// they become available later than this package's init. Until an address is
// registered the stub above stays in place, charging the reference gas and
// failing the call.
//
// The address must be one of params.PGPLegacyPrecompileAddresses; anything else
// is rejected so a typo cannot register a precompile the legacy rules never
// enable.
func RegisterPGPLegacyPrecompile(addr common.Address, p PrecompiledContract) error {
	if p == nil {
		return errors.New("pgp: nil legacy precompile implementation")
	}
	known := false
	for _, spec := range pgpLegacyPrecompileSpecs {
		if spec.address == addr {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("pgp: %s is not one of the legacy ELA precompile addresses", addr.Hex())
	}
	pgpLegacyMu.Lock()
	pgpLegacyOverrides[addr] = p
	pgpLegacyCache = nil
	pgpLegacyMu.Unlock()
	// Force the next lookup to rebuild, then report the new completeness state.
	pgpLegacyPrecompiledContracts()
	return nil
}

// MissingPGPLegacyPrecompiles lists the ELA precompiles that still run on the
// stub, as "name (0xaddr)" strings, in declaration order. An empty result means
// the legacy precompile set is complete.
func MissingPGPLegacyPrecompiles() []string {
	pgpLegacyMu.RLock()
	defer pgpLegacyMu.RUnlock()

	var missing []string
	for _, spec := range pgpLegacyPrecompileSpecs {
		if _, ok := pgpLegacyOverrides[spec.address]; !ok {
			missing = append(missing, fmt.Sprintf("%s (%s)", spec.name, spec.address.Hex()))
		}
	}
	return missing
}

// PGPLegacyPrecompileFailures returns how many calls have hit an unimplemented
// ELA precompile since start-up.
func PGPLegacyPrecompileFailures() uint64 { return pgpFailedPrecompiles.Load() }

// pgpLegacyPrecompiledContracts assembles the legacy set: nine Ethereum
// precompiles, then twelve ELA entries (stub or registered implementation).
//
// The returned map is shared for the lifetime of the cache window and must be
// treated as read-only. Its consumers already do: ActivePrecompiledContracts
// wraps it in maps.Clone, and NewEVM (evm.go:187) only ever reads it or replaces
// evm.precompiles wholesale via SetPrecompiles. RegisterPGPLegacyPrecompile drops
// the cache so the next call rebuilds instead of mutating a live map, which would
// race with any goroutine already holding it.
func pgpLegacyPrecompiledContracts() PrecompiledContracts {
	pgpLegacyMu.RLock()
	cached := pgpLegacyCache
	pgpLegacyMu.RUnlock()
	if cached != nil {
		return cached
	}
	set := pgpLegacyBuildSet()

	pgpLegacyLogOnce.Do(func() { logPGPLegacyPrecompileStatus() })
	return set
}

func pgpLegacyBuildSet() PrecompiledContracts {
	pgpLegacyMu.Lock()
	defer pgpLegacyMu.Unlock()
	if pgpLegacyCache != nil {
		return pgpLegacyCache
	}
	set := make(PrecompiledContracts, len(pgpLegacyStandardPrecompiles)+len(pgpLegacyPrecompileSpecs))
	maps.Copy(set, pgpLegacyStandardPrecompiles)
	for _, spec := range pgpLegacyPrecompileSpecs {
		if impl, ok := pgpLegacyOverrides[spec.address]; ok {
			set[spec.address] = impl
			continue
		}
		set[spec.address] = &unsupportedPGPLegacyPrecompile{spec: spec}
	}
	pgpLegacyCache = set
	return set
}

// logPGPLegacyPrecompileStatus records which ELA precompiles are real and which
// are stubs, once per process, the first time the legacy set is assembled.
func logPGPLegacyPrecompileStatus() {
	missing := MissingPGPLegacyPrecompiles()
	if len(missing) == 0 {
		log.Info("pgp: legacy ELA precompile set complete", "count", len(pgpLegacyPrecompileSpecs))
		return
	}
	log.Warn("pgp: legacy ELA precompile set incomplete, calls to these addresses fail until the ELA SPV subsystem registers them",
		"missing", len(missing), "total", len(pgpLegacyPrecompileSpecs), "precompiles", missing)
}
