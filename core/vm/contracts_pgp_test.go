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
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

// pgpLegacyExpectedGas pins the gas pgp-chain charges for each ELA precompile.
// The numbers are literals on purpose: they must be compared against
// pgp-chain/params/protocol_params.go:153-171, not against our own constants,
// otherwise a typo in params would be invisible.
var pgpLegacyExpectedGas = map[string]struct {
	addr common.Address
	gas  uint64
}{
	"arbiters":                  {common.BytesToAddress([]byte{0x03, 0xe8}), 1000},
	"p256Verify":                {common.BytesToAddress([]byte{0x03, 0xe9}), 1000},
	"pbkVerifySignature":        {common.BytesToAddress([]byte{0x03, 0xea}), 1000},
	"pledgeBillVerify":          {common.BytesToAddress([]byte{0x03, 0xeb}), 1000},
	"pledgeBillTokenID":         {common.BytesToAddress([]byte{0x03, 0xec}), 0},
	"pledgeBillTokenDetail":     {common.BytesToAddress([]byte{0x03, 0xed}), 0},
	"pledgeBillPayloadVersion":  {common.BytesToAddress([]byte{0x03, 0xee}), 0},
	"getMainChainBlockByHeight": {common.BytesToAddress([]byte{0x03, 0xef}), 1000},
	"getMainChainLatestHeight":  {common.BytesToAddress([]byte{0x03, 0xf0}), 0},
	"getMainChainRechargeData":  {common.BytesToAddress([]byte{0x03, 0xf1}), 0},
	"getWithdrawData":           {common.BytesToAddress([]byte{0x03, 0xf2}), 0},
	"verifySmallCrossTx":        {common.BytesToAddress([]byte{0x03, 0xf3}), 0},
}

// pgpLegacyRules returns the rules of a real pgp mainnet block, taken from
// params.PGPChainConfig rather than hand-assembled.
//
// Hand-assembling them is how the "legacy is not Shanghai" mistake happens: the
// pgp genesis timestamp is past ShanghaiTime, so params.Rules hard-wires
// IsShanghai on the legacy range (params/config.go:2249-2252). Deriving the rules
// here means the fixture cannot drift from the shipped config.
func pgpLegacyRules(t *testing.T) params.Rules {
	t.Helper()

	// Block 1923 of pgp mainnet; its timestamp is 1762224252, comfortably past
	// ShanghaiTime (1761657650).
	rules := params.PGPChainConfig.Rules(big.NewInt(1923), false, 1762224252)
	if !rules.IsPGPLegacy {
		t.Fatal("fixture is not legacy, the precompile test would prove nothing")
	}
	if !rules.IsShanghai {
		t.Fatal("legacy rules must be Shanghai-active (params/config.go:2249-2252)")
	}
	if !rules.IsBerlin {
		t.Fatal("legacy rules must be Berlin-active or there is no EIP-2929 warm set")
	}
	if rules.IsCancun {
		t.Fatal("pgp mainnet never activated Cancun, a Cancun rule here would add 0x0a")
	}
	return rules
}

// TestActivePrecompilesPGPLegacy checks that the legacy rules expose the same
// twenty-one precompiles as pgp-chain: nine Ethereum ones plus twelve ELA ones,
// and nothing else (no KZG, no BSC system contracts at 0x64/0x65 -- pgp keeps
// those two addresses for the ELAMINTER and ELATOKEN *contracts* in its genesis
// state, so treating them as precompiles would shadow them).
func TestActivePrecompilesPGPLegacy(t *testing.T) {
	rules := pgpLegacyRules(t)

	addresses := ActivePrecompiles(rules)
	if len(addresses) != 21 {
		t.Fatalf("legacy warm set has %d addresses, want 21", len(addresses))
	}
	seen := make(map[common.Address]bool, len(addresses))
	for _, addr := range addresses {
		if seen[addr] {
			t.Fatalf("duplicate address %s in the legacy warm set", addr.Hex())
		}
		seen[addr] = true
	}
	for _, b := range []byte{0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8, 0x9} {
		if !seen[common.BytesToAddress([]byte{b})] {
			t.Fatalf("Ethereum precompile 0x%x missing from the legacy warm set", b)
		}
	}
	for name, want := range pgpLegacyExpectedGas {
		if !seen[want.addr] {
			t.Fatalf("ELA precompile %s (%s) missing from the legacy warm set", name, want.addr.Hex())
		}
	}
	for _, forbidden := range []common.Address{
		common.BytesToAddress([]byte{0x0a}), // KZG point evaluation, Cancun only
		common.BytesToAddress([]byte{0x64}), // BSC tmHeaderValidate, and pgp's ELAMINTER
		common.BytesToAddress([]byte{0x65}), // BSC iavlMerkleProofValidate, and pgp's ELATOKEN
	} {
		if seen[forbidden] {
			t.Fatalf("%s must not be a precompile under the pgp legacy rules", forbidden.Hex())
		}
	}

	// The dispatch map must agree with the warm list, key for key: a mismatch
	// means an address is either warmed but not runnable, or runnable but cold.
	contracts := ActivePrecompiledContracts(rules)
	if len(contracts) != len(addresses) {
		t.Fatalf("dispatch map has %d entries but the warm set has %d", len(contracts), len(addresses))
	}
	for _, addr := range addresses {
		if _, ok := contracts[addr]; !ok {
			t.Fatalf("%s is warmed but has no implementation", addr.Hex())
		}
	}
}

// TestPGPLegacyPrecompileGas checks the up-front charge of every ELA precompile.
// pgp-chain charges the whole cost in RequiredGas (Run gets no gas), so getting
// these numbers right is necessary for the gasUsed of every calling block.
func TestPGPLegacyPrecompileGas(t *testing.T) {
	contracts := ActivePrecompiledContracts(pgpLegacyRules(t))
	for name, want := range pgpLegacyExpectedGas {
		p, ok := contracts[want.addr]
		if !ok {
			t.Fatalf("%s (%s) is not a precompile under the legacy rules", name, want.addr.Hex())
		}
		if got := p.RequiredGas(nil); got != want.gas {
			t.Errorf("%s (%s) charges %d gas, want %d", name, want.addr.Hex(), got, want.gas)
		}
		if got := p.Name(); got != name {
			t.Errorf("%s reports name %q, want %q", want.addr.Hex(), got, name)
		}
	}
}

// TestPGPLegacyPrecompileUnimplemented proves that an unregistered ELA precompile
// fails the call instead of behaving like an empty account, and that the failure
// is counted.
func TestPGPLegacyPrecompileUnimplemented(t *testing.T) {
	restore := unregisterAllPGPLegacyPrecompiles(t)
	defer restore()

	arbiters := common.BytesToAddress(params.ArbiterAddress.Bytes())
	p := ActivePrecompiledContracts(pgpLegacyRules(t))[arbiters]
	if p == nil {
		t.Fatal("arbiters precompile is not in the legacy set")
	}
	before := PGPLegacyPrecompileFailures()
	if _, err := p.Run(nil); !errors.Is(err, ErrPGPLegacyPrecompileUnavailable) {
		t.Fatalf("unimplemented precompile returned %v, want ErrPGPLegacyPrecompileUnavailable", err)
	}
	if got := PGPLegacyPrecompileFailures(); got != before+1 {
		t.Fatalf("failure counter went from %d to %d, want +1", before, got)
	}
	// The block-aware path is the one the interpreter takes; it must fail too.
	bp, ok := p.(BlockContextPrecompiledContract)
	if !ok {
		t.Fatal("stub does not implement BlockContextPrecompiledContract, so the block number never reaches the log")
	}
	if _, err := bp.RunWithBlockContext(BlockContext{BlockNumber: big.NewInt(1923)}, nil); !errors.Is(err, ErrPGPLegacyPrecompileUnavailable) {
		t.Fatalf("block-aware path returned %v, want ErrPGPLegacyPrecompileUnavailable", err)
	}

	missing := MissingPGPLegacyPrecompiles()
	if len(missing) != len(pgpLegacyExpectedGas) {
		t.Fatalf("MissingPGPLegacyPrecompiles reports %d missing, want %d", len(missing), len(pgpLegacyExpectedGas))
	}
}

// TestRegisterPGPLegacyPrecompile checks the plug-in path the ELA SPV port will
// use, and that a wrong address cannot be registered.
func TestRegisterPGPLegacyPrecompile(t *testing.T) {
	restore := unregisterAllPGPLegacyPrecompiles(t)
	defer restore()

	target := common.BytesToAddress(params.GetMainChainBlockByHeight.Bytes())
	impl := &fakePGPLegacyPrecompile{gas: 1000, name: "getMainChainBlockByHeight", output: []byte{0xde, 0xad}}

	// Wrong address: rejected, and the set is untouched.
	if err := RegisterPGPLegacyPrecompile(common.BytesToAddress([]byte{0x0a}), impl); err == nil {
		t.Fatal("registering a non-legacy address must fail")
	}
	if err := RegisterPGPLegacyPrecompile(target, nil); err == nil {
		t.Fatal("registering a nil implementation must fail")
	}
	before := len(MissingPGPLegacyPrecompiles())

	if err := RegisterPGPLegacyPrecompile(target, impl); err != nil {
		t.Fatalf("registering %s failed: %v", target.Hex(), err)
	}
	after := MissingPGPLegacyPrecompiles()
	if len(after) != before-1 {
		t.Fatalf("missing count went from %d to %d, want %d", before, len(after), before-1)
	}
	for _, entry := range after {
		if entry == "getMainChainBlockByHeight ("+target.Hex()+")" {
			t.Fatal("registered precompile is still reported as missing")
		}
	}

	got := ActivePrecompiledContracts(pgpLegacyRules(t))[target]
	if got != impl {
		t.Fatalf("dispatch returned %v, want the registered implementation", got)
	}
	out, err := got.Run(nil)
	if err != nil || string(out) != string(impl.output) {
		t.Fatalf("registered implementation returned (%x, %v), want (%x, nil)", out, err, impl.output)
	}
}

// TestNonLegacyRulesKeepTheirPrecompiles makes sure the new first case cannot
// leak into the BSC fork ladders.
func TestNonLegacyRulesKeepTheirPrecompiles(t *testing.T) {
	ela := common.BytesToAddress(params.ArbiterAddress.Bytes())

	// Synthetic on purpose: there is no shipped config that is "plain Berlin and
	// nothing else", so this fixture has no PGPLegacy marker to derive from. Its
	// job is only to prove the new first case does not fire without it.
	berlin := params.Rules{IsHomestead: true, IsEIP150: true, IsEIP155: true, IsEIP158: true,
		IsByzantium: true, IsConstantinople: true, IsPetersburg: true, IsIstanbul: true, IsBerlin: true}
	if _, ok := ActivePrecompiledContracts(berlin)[ela]; ok {
		t.Fatal("ELA precompile leaked into the plain Berlin set")
	}
	if n := len(ActivePrecompiles(berlin)); n != len(PrecompiledContractsBerlin) {
		t.Fatalf("Berlin warm set has %d addresses, want %d", n, len(PrecompiledContractsBerlin))
	}
	// A config without Legacy (every BSC chain) must never see IsPGPLegacy.
	if (params.Rules{}).IsPGPLegacy {
		t.Fatal("zero-value Rules must not be legacy")
	}
}

// TestPGPLegacyInitCodeSizeLimit pins the rule that is easiest to "fix" wrongly.
//
// The legacy range IS Shanghai-active (params/config.go:2249-2252) and pgp-chain
// enforces the EIP-3860 49152-byte init code limit there
// (pgp-chain/core/state_transition.go:299-300), so CheckMaxInitCodeSize must stay
// on. What pgp-chain never had is the EIP-3860 *word gas*
// (pgp-chain/core/state_transition.go:125: IntrinsicGas takes no Shanghai flag),
// which is why core/state_transition.go:623 passes
// "rules.IsShanghai && !rules.IsPGPLegacy" instead. If someone ever turns
// IsShanghai off for the legacy range to disable the word gas, this test fails
// before a 49999-byte create can slip through.
func TestPGPLegacyInitCodeSizeLimit(t *testing.T) {
	rules := pgpLegacyRules(t)

	if err := CheckMaxInitCodeSize(&rules, params.MaxInitCodeSize); err != nil {
		t.Fatalf("init code at the limit must be accepted: %v", err)
	}
	err := CheckMaxInitCodeSize(&rules, params.MaxInitCodeSize+1)
	if !errors.Is(err, ErrMaxInitCodeSizeExceeded) {
		t.Fatalf("init code one byte over the limit returned %v, want ErrMaxInitCodeSizeExceeded", err)
	}
}

// fakePGPLegacyPrecompile is a stand-in implementation for the registration test.
type fakePGPLegacyPrecompile struct {
	gas    uint64
	name   string
	output []byte
}

func (f *fakePGPLegacyPrecompile) RequiredGas([]byte) uint64  { return f.gas }
func (f *fakePGPLegacyPrecompile) Run([]byte) ([]byte, error) { return f.output, nil }
func (f *fakePGPLegacyPrecompile) Name() string               { return f.name }

// unregisterAllPGPLegacyPrecompiles clears the registry so a test starts from the
// "nothing ported yet" state, and returns a func that restores it.
func unregisterAllPGPLegacyPrecompiles(t *testing.T) func() {
	t.Helper()

	pgpLegacyMu.Lock()
	saved := pgpLegacyOverrides
	pgpLegacyOverrides = make(map[common.Address]PrecompiledContract)
	pgpLegacyCache = nil
	pgpLegacyMu.Unlock()

	return func() {
		pgpLegacyMu.Lock()
		pgpLegacyOverrides = saved
		pgpLegacyCache = nil
		pgpLegacyMu.Unlock()
	}
}
