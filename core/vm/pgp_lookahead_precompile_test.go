package vm

// Regression guards for F-11b.
//
// SlashIndicator and TokenRecoverPortal reach two precompiles that upstream only
// ships from Feynman onwards: 0x68 (verifyDoubleSignEvidence) and 0x69
// (secp256k1SignatureRecover). This chain keeps Feynman off, so before the
// overlay both staticcalls landed on an empty account -- which SUCCEEDS with
// empty returndata. The contracts then read a zero signer: double-sign slashing
// reverts forever with "validator not migrated", and the recovery portal always
// answers InvalidProof. Nothing else in the node can observe that.
//
// activePrecompiledContracts therefore extends the post-fork set with both, and
// ActivePrecompiles has to extend the EIP-2929 warm set the same way.

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

func TestLookaheadPrecompilesGate(t *testing.T) {
	// lookaheadApplies only says "this chain may need the overlay"; whether the
	// entries are actually added is decided per entry, because fork rules are
	// cumulative and everything from Feynman up already carries both.
	for _, tc := range []struct {
		name  string
		rules params.Rules
		want  bool
	}{
		{"legacy", params.Rules{IsInBSC: true, IsPGPLegacy: true}, false},
		{"berlin", params.Rules{IsInBSC: true, IsBerlin: true}, true},
		{"luban", params.Rules{IsInBSC: true, IsLuban: true}, true},
		{"plato", params.Rules{IsInBSC: true, IsPlato: true}, true},
		{"hertz", params.Rules{IsInBSC: true, IsHertz: true}, true},
		{"feynman", params.Rules{IsInBSC: true, IsFeynman: true}, true},
		{"cancun", params.Rules{IsInBSC: true, IsCancun: true}, true},
		{"osaka", params.Rules{IsInBSC: true, IsOsaka: true}, true},
		{"pasteur", params.Rules{IsInBSC: true, IsPasteur: true}, true},
		{"non-bsc", params.Rules{IsBerlin: true}, false},
	} {
		if got := lookaheadApplies(tc.rules); got != tc.want {
			t.Errorf("%s: lookaheadApplies = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLookaheadPrecompilesAreDispatchable(t *testing.T) {
	addr68 := common.BytesToAddress([]byte{0x68})
	addr69 := common.BytesToAddress([]byte{0x69})

	// Berlin is where this chain lands post-fork today: Luban is pinned at
	// MaxInt64, so everything above falls through to the Berlin case.
	rules := params.Rules{IsInBSC: true, IsBerlin: true}
	set := activePrecompiledContracts(rules)

	if _, ok := set[addr68].(*verifyDoubleSignEvidence); !ok {
		t.Fatalf("0x68 is %T, want *verifyDoubleSignEvidence", set[addr68])
	}
	if _, ok := set[addr69].(*secp256k1SignatureRecover); !ok {
		t.Fatalf("0x69 is %T, want *secp256k1SignatureRecover", set[addr69])
	}

	// The overlay must not have written into the shared package-level map: a
	// non-BSC Berlin chain selects the very same variable.
	if _, ok := PrecompiledContractsBerlin[addr68]; ok {
		t.Fatal("the overlay mutated PrecompiledContractsBerlin; non-BSC chains would inherit 0x68")
	}
	if _, ok := activePrecompiledContracts(params.Rules{IsBerlin: true})[addr68]; ok {
		t.Fatal("a non-BSC chain got 0x68")
	}
	// Legacy stays exactly what pgp-chain ran.
	if _, ok := activePrecompiledContracts(params.Rules{IsInBSC: true, IsPGPLegacy: true})[addr68]; ok {
		t.Fatal("0x68 leaked into the legacy set; historical replay would change")
	}
}

// TestLookaheadWarmSetMatchesDispatch is the guard the code comment refers to:
// the EIP-2929 warm set and the dispatch map must list the same addresses, or
// one of them is either runnable-but-cold or warmed-but-unrunnable.
func TestLookaheadWarmSetMatchesDispatch(t *testing.T) {
	for _, rules := range []params.Rules{
		{IsInBSC: true, IsPGPLegacy: true},
		{IsInBSC: true, IsBerlin: true},
		{IsInBSC: true, IsLuban: true},
		{IsInBSC: true, IsHertz: true},
		{IsInBSC: true, IsFeynman: true},
		{IsBerlin: true},
	} {
		dispatch := activePrecompiledContracts(rules)
		warm := ActivePrecompiles(rules)
		if len(dispatch) != len(warm) {
			t.Fatalf("%v: dispatch has %d entries, warm set has %d", rules, len(dispatch), len(warm))
		}
		for _, addr := range warm {
			if _, ok := dispatch[addr]; !ok {
				t.Fatalf("%v: %s is warmed but not dispatchable", rules, addr.Hex())
			}
		}
	}
}
