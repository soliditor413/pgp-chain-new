package spv

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/elastos/Elastos.ELA/core/transaction"
	"github.com/elastos/Elastos.ELA/core/types/outputpayload"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

// elaRechargeTx30115Hex is the mainnet ELA transaction that pgp block 30115
// redeems through ELAMinter.Recharge. Fetched from https://api.elastos.io/ela
// with getrawtransaction, verbose=false, at ELA height 2,322,154.
//
// It is the ONLY ELA deposit in pgp mainnet history up to block 30114 — the
// scan of every transaction in 1..30114 found zero sends to 0x…64 and zero
// ELAMinter logs — so it is the single sample the legacy recharge path has to
// be reproduced against.
const elaRechargeTx30115Hex = "09080101000a3138363833383936323501bf112ddf562045d3865ce09ffec5eee73077da556331cabfff8248b968399b4e01000000000002b037db964a231458d2d6ffd5ea18944c4f90e63d547c5d3b9874df66a4ead0a370e09a0000000000000000004b794779b4160ad4893f10c609a89f1bc9d638f85a03002a307861323066354132326546343233623065356332446237394135343735443935313264393839393731809698000000000000b037db964a231458d2d6ffd5ea18944c4f90e63d547c5d3b9874df66a4ead0a39e199c060000000000000000216c642f745a46abf0f55bf8867c30f1c54e710a60000000000000014140daaafc257c5e11d8e3140a16a6799ba6b79b4fede5a204c25335380c18277cbb230128be0d7a37a72178fbd61fb38821be57f713631eceff479dc11ef00fe662232102eb9dcf8de623996d4f8e4ee30a511aef7105b84d5338edb62a481cf68c0eafc9ac"

// elaHash30115 is the bytes32 that pgp block 30115 passes to
// ELAMinter.Recharge(bytes32 elaHash, bytes). Taken verbatim from the
// transaction input (/tmp/tx_input_30115.txt), not from any ELA source.
const elaHash30115 = "1b7e2877238323b58f5240bcf5e6797df3aae5684142c075572e145ee3d25898"

// rpcTxid30115 is how the same transaction is addressed over the ELA JSON-RPC
// (getrawtransaction's "txid" parameter, and the "txid" field it returns).
// It is the byte-reverse of elaHash30115.
const rpcTxid30115 = "9858d2e35e142e5775c0424168e5aaf37d79e6f5bc40528fb523832377287e1b"

// TestElaHashOrderMatchesStoreKey is the regression guard for the single most
// dangerous silent failure in the recharge path.
//
// There are two hex representations of an Elastos transaction hash in play:
//
//   - tx.Hash().String(), which hex-encodes the internal byte array as-is
//     (Elastos.ELA common/uint256.go:37 -> BytesToHexString -> hex.EncodeToString,
//     no reversal). This is what spv/recharge_sync.go keys the store by, and
//     what pgp-chain keys its leveldb by (spv_module.go:413 puts
//     txHash+"Fee" where txHash = tx.Hash().String()).
//   - the ELA JSON-RPC "txid", which reverses the bytes. servers/interfaces.go:143
//     parses it with common.FromReversedString, and common/common.go:67 reverses.
//
// If these two ever diverge, every 0x3F1 lookup misses, ELAMinter aborts, the
// receipt gets status 0, and the only symptom is a stateRoot mismatch thousands
// of blocks later. There is no error message anywhere along that path.
//
// So: the EVM-side elaHash MUST equal tx.Hash().String() exactly, and the RPC
// txid MUST be its byte-reverse. Both halves are asserted below.
func TestElaHashOrderMatchesStoreKey(t *testing.T) {
	raw, err := hex.DecodeString(elaRechargeTx30115Hex)
	if err != nil {
		t.Fatalf("bad fixture hex: %v", err)
	}
	r := bytes.NewReader(raw)
	tx, err := transaction.GetTransactionByBytes(r)
	if err != nil {
		t.Fatalf("reading the ELA tx header: %v", err)
	}
	if err := tx.Deserialize(r); err != nil {
		t.Fatalf("deserialising the ELA deposit tx: %v", err)
	}

	got := tx.Hash().String()
	if got != elaHash30115 {
		t.Fatalf("tx.Hash().String() = %s, want %s (the value pgp block 30115 passes as elaHash).\n"+
			"Store keys and 0x3F1 lookups keyed on these two spellings would never match, and the "+
			"failure is silent: ELAMinter aborts, status becomes 0, and only stateRoot notices.",
			got, elaHash30115)
	}

	if rev := reverseHex(got); rev != rpcTxid30115 {
		t.Fatalf("reverse(tx.Hash().String()) = %s, want the ELA RPC txid %s.\n"+
			"An ELA JSON-RPC data source (the bootstrap path) has to reverse the hash before "+
			"querying, otherwise getrawtransaction answers 'cannot find transaction'.", rev, rpcTxid30115)
	}
}

// TestElaRechargeFields30115 pins the four RechargeData fields that 0x3F1 has to
// hand to ELAMinter for block 30115, straight from the ELA chain.
//
// The field naming is the trap here: RechargeData.targetAmount carries the
// OUTPUT VALUE (principal plus fee), not the cross-chain payload's TargetAmount.
// pgp-chain builds it that way in spv/spv_module.go:406
// ("amounts = append(amounts, output.Value.String())") and spv/recharge.go:95
// assigns that value to data.TargetAmount. An implementation that "fixes" this
// to op.TargetAmount under-mints by exactly one fee per element.
func TestElaRechargeFields30115(t *testing.T) {
	raw, err := hex.DecodeString(elaRechargeTx30115Hex)
	if err != nil {
		t.Fatalf("bad fixture hex: %v", err)
	}
	r := bytes.NewReader(raw)
	tx, err := transaction.GetTransactionByBytes(r)
	if err != nil {
		t.Fatalf("reading the ELA tx header: %v", err)
	}
	if err := tx.Deserialize(r); err != nil {
		t.Fatalf("deserialising the ELA deposit tx: %v", err)
	}

	if got, want := tx.PayloadVersion(), byte(1); got != want {
		t.Fatalf("payload version = %d, want %d (the output-carries-the-amount layout)", got, want)
	}

	var crossChain int
	var gotValue, gotTargetAmount int64
	var gotAddress string
	var gotData []byte
	for _, out := range tx.Outputs() {
		if out == nil {
			continue
		}
		op, ok := out.Payload.(*outputpayload.CrossChainOutput)
		if !ok || op == nil {
			continue
		}
		crossChain++
		gotValue = out.Value.IntValue()
		gotTargetAmount = op.TargetAmount.IntValue()
		gotAddress = op.TargetAddress
		gotData = []byte(op.TargetData)
	}
	if crossChain != 1 {
		t.Fatalf("found %d cross-chain outputs, want exactly 1", crossChain)
	}

	// The three numbers that decide the mint.
	if gotValue != 10150000 {
		t.Errorf("output.Value = %d, want 10150000 (0.1015 ELA) — this is RechargeData.targetAmount", gotValue)
	}
	if gotTargetAmount != 10000000 {
		t.Errorf("payload.TargetAmount = %d, want 10000000 (0.1 ELA) — the principal the ELA side promised", gotTargetAmount)
	}
	if fee := gotValue - gotTargetAmount; fee != 150000 {
		t.Errorf("fee = output.Value - payload.TargetAmount = %d, want 150000", fee)
	}
	if want := "0xa20f5A22eF423b0e5c2Db79A5475D9512d989971"; !strings.EqualFold(gotAddress, want) {
		t.Errorf("payload.TargetAddress = %s, want %s", gotAddress, want)
	}
	if len(gotData) != 0 {
		t.Errorf("payload.TargetData = %x, want empty — block 30115's smallRechargeData is a zero-length bytes", gotData)
	}

	// And the address really is a PGP-chain EVM address, not an ELA one.
	if !ethCommon.IsHexAddress(gotAddress) {
		t.Fatalf("%s is not a hex address", gotAddress)
	}

	_ = big.NewInt(0) // keep math/big imported for the fee arithmetic above
}

// reverseHex reverses the byte order of a 64-character hex string (with or
// without a 0x prefix) and returns it without a prefix.
func reverseHex(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	b, err := hex.DecodeString(s)
	if err != nil {
		return ""
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return hex.EncodeToString(b)
}
