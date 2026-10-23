package spv

import (
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// pgpElaDepositAddress is the ELA main chain address that pgp mainnet deposits
// are paid to.
//
// It is not read from anywhere at runtime — it is derived from the pgp genesis
// block hash by CalculateGenesisAddress, and this constant is the independent
// answer to "did the derivation produce the address the chain actually uses?".
// It was obtained by decoding the one real deposit in pgp mainnet history (ELA
// tx 1b7e2877…, the tx pgp block 30115 redeems) and reading the ProgramHash of
// its cross chain output.
//
// If this test fails, the SPV filter is watching an address nobody pays. The
// node then syncs ELA headers forever, stores no recharge records, and every
// deposit block dies with ErrRechargeUnavailable — with no error pointing here.
func TestCalculateGenesisAddressMatchesTheChain(t *testing.T) {
	const want = "XNQWEZ7aqNyJHvav8j8tNo2ZQypuTsWQk6"

	got, err := CalculateGenesisAddress(params.PGPGenesisHash.String())
	if err != nil {
		t.Fatalf("CalculateGenesisAddress: %v", err)
	}
	if got != want {
		t.Fatalf("deposit address = %s, want %s", got, want)
	}

	// The function accepts the hash with or without the 0x prefix, because the
	// two callers disagree: params.PGPGenesisHash prints with it, ELA tooling
	// prints without.
	bare, err := CalculateGenesisAddress(trimHexPrefix(params.PGPGenesisHash.String()))
	if err != nil {
		t.Fatalf("CalculateGenesisAddress (no prefix): %v", err)
	}
	if bare != got {
		t.Fatalf("address differs with the 0x prefix stripped: %s vs %s", bare, got)
	}
}

// TestDepositAddressIsWhereTheRechargeTxPays is the join between this file and
// the 30115 fixture: the address we derive must be the payee of the real
// deposit, otherwise one of the two is wrong and the filter matches nothing.
func TestDepositAddressIsWhereTheRechargeTxPays(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}

	want, err := CalculateGenesisAddress(params.PGPGenesisHash.String())
	if err != nil {
		t.Fatalf("CalculateGenesisAddress: %v", err)
	}

	match := 0
	for _, out := range tx.Outputs() {
		addr, err := out.ProgramHash.ToAddress()
		if err != nil {
			continue
		}
		if addr == want {
			match++
		}
	}
	if match != 1 {
		t.Fatalf("%d outputs of the 30115 deposit pay %s, want exactly 1", match, want)
	}
}

func TestCalculateGenesisAddressRejectsBadInput(t *testing.T) {
	if _, err := CalculateGenesisAddress("0xnothex"); err == nil {
		t.Fatal("accepted a non-hex genesis hash")
	}
}
