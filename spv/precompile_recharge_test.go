package spv

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// rechargePrecompileInput builds the calldata the ELA contracts use: the 32 byte
// ELA tx hash, nothing else.
func rechargePrecompileInput(elaHash string) []byte {
	return common.HexToHash(elaHash).Bytes()
}

// TestRechargeHashFromInput pins the argument parsing.
//
// pgp-chain read the argument with getData(input, 0, 32), whose padding is on the
// RIGHT. For a well formed 32 byte input that is indistinguishable from the ABI
// convention, which is why the oddity is invisible in practice — but it changes
// the lookup key for any other length, so the behaviour is pinned rather than
// "tidied up".
func TestRechargeHashFromInput(t *testing.T) {
	if got := rechargeHashFromInput(rechargePrecompileInput(elaHash30115)); got != "0x"+elaHash30115 {
		t.Fatalf("32 byte input = %s, want 0x%s", got, elaHash30115)
	}

	// A longer input is truncated to the first 32 bytes.
	long := append(rechargePrecompileInput(elaHash30115), 0xff, 0xff)
	if got := rechargeHashFromInput(long); got != "0x"+elaHash30115 {
		t.Fatalf("over-long input = %s, want the first 32 bytes", got)
	}

	// A short input is padded on the right, not on the left.
	short := []byte{0x01, 0x02}
	want := "0x0102000000000000000000000000000000000000000000000000000000000000"
	if got := rechargeHashFromInput(short); got != want {
		t.Fatalf("short input = %s, want right padding %s", got, want)
	}

	// No input at all is the zero hash, not a panic.
	if got := rechargeHashFromInput(nil); got != (common.Hash{}).String() {
		t.Fatalf("empty input = %s, want the zero hash", got)
	}
}

// installedStoreForRecharge30115 puts the store in the state a running node is in
// when it executes a deposit block: the record has been indexed by the listener
// and a view is pinned for the block.
func installedStoreForRecharge30115(t *testing.T) {
	t.Helper()
	prev := GetRechargeStore()
	prevView := CurrentRechargeView()
	s := newTestStore()
	SetRechargeStore(s)
	if err := ApplyElaBlock(s, elaBlockAt(elaHeight30115, realElaTx30115(t))); err != nil {
		t.Fatalf("ApplyElaBlock: %v", err)
	}
	if err := InstallHeightView(elaHeight30115); err != nil {
		t.Fatalf("InstallHeightView: %v", err)
	}
	t.Cleanup(func() {
		UninstallHeightView()
		SetRechargeStore(prev)
		SetStateView(prevView)
	})
}

// TestRechargePrecompileEncodesTheDeposit is the end-to-end guard on 0x3f1: the
// real record from ELA height 2,076,538 must come back as the flat ABI layout
// ELAMinter decodes, with the values the block was executed with.
func TestRechargePrecompileEncodesTheDeposit(t *testing.T) {
	installedStoreForRecharge30115(t)

	out, err := (&rechargeDataPrecompile{}).Run(rechargePrecompileInput(elaHash30115))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 32 bytes for the count word, then four words for the single element: the
	// address, the amount, the fee and the offset of the empty bytes payload.
	if len(out) != 192 {
		t.Fatalf("return data is %d bytes, want 192", len(out))
	}
	if got := new(big.Int).SetBytes(out[0:32]); got.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("count word = %s, want 1", got)
	}

	// Decode with the same flat argument list the encoder builds, so the test
	// fails if either side drifts.
	uint256Type, _ := abi.NewType("uint256", "", nil)
	addressType, _ := abi.NewType("address", "", nil)
	bytesType, _ := abi.NewType("bytes", "", nil)
	args := abi.Arguments{
		{Name: "count", Type: uint256Type},
		{Name: "targetAddress", Type: addressType},
		{Name: "targetAmount", Type: uint256Type},
		{Name: "fee", Type: uint256Type},
		{Name: "targetData", Type: bytesType},
	}
	values, err := args.Unpack(out)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(values) != 5 {
		t.Fatalf("decoded %d values, want 5", len(values))
	}
	if count := values[0].(*big.Int); count.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("count = %s, want 1", count)
	}
	if addr := values[1].(common.Address); addr != common.HexToAddress(rechargeTarget30115) {
		t.Fatalf("targetAddress = %s, want %s", addr.Hex(), rechargeTarget30115)
	}
	if amount := values[2].(*big.Int); amount.Cmp(big.NewInt(10150000)) != 0 {
		t.Fatalf("targetAmount = %s, want 10150000", amount)
	}
	if fee := values[3].(*big.Int); fee.Cmp(big.NewInt(150000)) != 0 {
		t.Fatalf("fee = %s, want 150000", fee)
	}
	if data := values[4].([]byte); len(data) != 0 {
		t.Fatalf("targetData = %x, want empty", data)
	}
}

// TestRechargePrecompileMissingRecordIsAnError is the rule that keeps a wrong
// answer from becoming a silent success. Returning an empty list here would tell
// ELAMinter "this deposit paid for nothing": it would mint nothing, the call
// would succeed, gas would still be refunded to zero and the block would be
// accepted with a wrong state root.
func TestRechargePrecompileMissingRecordIsAnError(t *testing.T) {
	installedStoreForRecharge30115(t)

	unknown := "00" + elaHash30115[2:]
	out, err := (&rechargeDataPrecompile{}).Run(rechargePrecompileInput(unknown))
	if err == nil {
		t.Fatalf("Run returned %d bytes and no error for an unknown ELA tx", len(out))
	}
	if out != nil {
		t.Fatalf("Run returned %d bytes alongside the error, want nil", len(out))
	}
}

// TestRechargePrecompileWithoutAViewIsAnError covers the replay case: a block
// executed without InstallHeightView must fail, not answer from the previous
// block's ELA height.
func TestRechargePrecompileWithoutAViewIsAnError(t *testing.T) {
	installedStoreForRecharge30115(t)
	UninstallHeightView()

	if _, err := (&rechargeDataPrecompile{}).Run(rechargePrecompileInput(elaHash30115)); err == nil {
		t.Fatal("Run answered with no view installed")
	}
}

// TestRechargePrecompileWithoutAStoreIsAnError covers a node where SpvDbInit has
// not run.
func TestRechargePrecompileWithoutAStoreIsAnError(t *testing.T) {
	prev := GetRechargeStore()
	prevView := CurrentRechargeView()
	SetRechargeStore(nil)
	UninstallHeightView()
	t.Cleanup(func() {
		SetRechargeStore(prev)
		SetStateView(prevView)
	})

	if _, err := (&rechargeDataPrecompile{}).Run(rechargePrecompileInput(elaHash30115)); err == nil {
		t.Fatal("Run answered with no recharge store")
	}
}

// TestRechargePrecompileGas pins pgp's RequiredGas literal.
func TestRechargePrecompileGas(t *testing.T) {
	p := &rechargeDataPrecompile{}
	if got := p.RequiredGas(nil); got != 0 {
		t.Fatalf("RequiredGas = %d, want 0 (pgp-chain returns a literal 0)", got)
	}
	if got := p.Name(); got != "getMainChainRechargeData" {
		t.Fatalf("Name = %q", got)
	}
}
