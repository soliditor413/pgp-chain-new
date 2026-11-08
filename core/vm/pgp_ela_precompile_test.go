package vm

// Regression guards for F-11a.
//
// 0x64 and 0x65 hold ELAMINTER (the ELA bridge gateway) and ELATOKEN (bridged
// ELA) in this chain's genesis. A precompile always wins over account code in
// EVM dispatch, so any BSC precompile set that claims those two addresses makes
// the bridge unroutable and freezes every bridged balance -- 1,145,643.82 ELA at
// the time of writing, and contract storage cannot be moved elsewhere.
//
// The two BSC tendermint precompiles therefore live at 0x264/0x265.

import (
	"bytes"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

func TestELAAddressesAreNeverPrecompiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules params.Rules
	}{
		{"legacy", params.Rules{IsInBSC: true, IsPGPLegacy: true}},
		{"berlin", params.Rules{IsInBSC: true, IsBerlin: true}},
		{"luban", params.Rules{IsInBSC: true, IsLuban: true}},
		{"plato", params.Rules{IsInBSC: true, IsPlato: true}},
		{"hertz", params.Rules{IsInBSC: true, IsHertz: true}},
		{"feynman", params.Rules{IsInBSC: true, IsFeynman: true}},
		{"cancun", params.Rules{IsInBSC: true, IsCancun: true}},
		{"prague", params.Rules{IsInBSC: true, IsPrague: true}},
		{"osaka", params.Rules{IsInBSC: true, IsOsaka: true}},
		{"pasteur", params.Rules{IsInBSC: true, IsPasteur: true}},
		// A non-BSC chain must not grow the relocated precompiles either.
		{"standard-osaka", params.Rules{IsOsaka: true}},
	} {
		set := activePrecompiledContracts(tc.rules)
		for _, addr := range []common.Address{
			common.BytesToAddress([]byte{0x64}),
			common.BytesToAddress([]byte{0x65}),
		} {
			if _, ok := set[addr]; ok {
				t.Fatalf("%s: %s is a precompile; ELAMINTER/ELATOKEN would be shadowed", tc.name, addr.Hex())
			}
		}
	}
}

// TestRelocatedTendermintPrecompilesExist makes the previous test mean something:
// deleting the two entries outright would also satisfy it.
func TestRelocatedTendermintPrecompilesExist(t *testing.T) {
	addr264 := common.BytesToAddress([]byte{0x2, 0x64})
	addr265 := common.BytesToAddress([]byte{0x2, 0x65})

	if _, ok := PrecompiledContractsLuban[addr264].(*tmHeaderValidate); !ok {
		t.Fatalf("0x264 must be tmHeaderValidate in the Luban set, got %T", PrecompiledContractsLuban[addr264])
	}
	if _, ok := PrecompiledContractsLuban[addr265]; !ok {
		t.Fatal("0x265 is missing from the Luban set")
	}
	// And the standard (non-BSC) sets must not inherit them.
	for name, set := range map[string]PrecompiledContracts{
		"istanbul": PrecompiledContractsIstanbul,
		"cancun":   PrecompiledContractsCancun,
		"osaka":    PrecompiledContractsOsaka,
	} {
		if _, ok := set[addr264]; ok {
			t.Fatalf("%s: the standard set inherited BSC's 0x264 precompile", name)
		}
		if _, ok := set[addr265]; ok {
			t.Fatalf("%s: the standard set inherited BSC's 0x265 precompile", name)
		}
	}
	// The legacy set is replay of a live chain: it must be exactly what pgp-chain
	// ran, so it may contain neither the old nor the new addresses. A single
	// extra entry here would change historical execution.
	legacySet := pgpLegacyPrecompiledContracts()
	for _, addr := range []common.Address{
		common.BytesToAddress([]byte{0x64}),
		common.BytesToAddress([]byte{0x65}),
		addr264,
		addr265,
	} {
		if _, ok := legacySet[addr]; ok {
			t.Fatalf("%s leaked into the legacy set; historical replay would change", addr.Hex())
		}
	}
}

// TestNoPrecompileTableKeysELAAddresses checks the literals themselves. The
// table above enumerates the fork sets that exist today; this one keeps the next
// upstream set from quietly reintroducing the collision, which is exactly how
// the bug would come back -- a new fork map copied from upstream.
func TestNoPrecompileTableKeysELAAddresses(t *testing.T) {
	src, err := os.ReadFile("contracts.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"BytesToAddress([]byte{0x64})",
		"BytesToAddress([]byte{0x65})",
	} {
		if bytes.Contains(src, []byte(bad)) {
			t.Fatalf("contracts.go still keys a precompile as %s; that address holds an ELA contract. "+
				"Relocate it to 0x264/0x265 like the other sets.", bad)
		}
	}
}
