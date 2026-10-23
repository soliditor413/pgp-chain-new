package spv

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestELAMinterABIDecode is a smoke test for the embedded ELAMinter ABI: a
// corrupted ELAMinterABIMetaData makes the package init() panic, and the three
// method-id checks plus the two pack helpers are exercised end to end.
//
// There is deliberately no completion test here any more: the "is this ELA tx
// already completed?" query left this package (see the comment block at the end
// of elaminter.go).
func TestELAMinterABIDecode(t *testing.T) {
	const (
		elaHash = "0xc4a3d5dc09808adb4e01fa805f722c3c34f41ca57496c299d17b5e0477b2b056"
		input   = "3e44a583c4a3d5dc09808adb4e01fa805f722c3c34f41ca57496c299d17b5e0477b2b05600000000000000000000000000000000000000000000000000000000000000400000000000000000000000000000000000000000000000000000000000000020c4a3d5dc09808adb4e01fa805f722c3c34f41ca57496c299d17b5e0477b2b056"
	)
	data := common.Hex2Bytes(input)
	to := ELAMinterAddress

	if !IsRechargeTx(data, &to) {
		t.Fatal("IsRechargeTx = false for a Recharge call to the ELAMinter")
	}
	if IsWithdrawTx(data, &to) {
		t.Fatal("IsWithdrawTx = true for a Recharge call")
	}
	if IsRefundWithdrawTx(data, &to) {
		t.Fatal("IsRefundWithdrawTx = true for a Recharge call")
	}

	// Pack must reproduce the reference calldata byte for byte.
	smallCrossTx := common.Hex2Bytes("c4a3d5dc09808adb4e01fa805f722c3c34f41ca57496c299d17b5e0477b2b056")
	packed := GetRechargeData(elaHash, smallCrossTx)
	if !bytes.Equal(packed, common.Hex2Bytes(input)) {
		t.Fatalf("GetRechargeData did not reproduce the reference calldata:\n got %x\nwant %s", packed, input)
	}

	// A nil recipient must not match any ELAMinter method (pgp dereferenced it).
	if IsRechargeTx(data, nil) || IsWithdrawTx(data, nil) || IsRefundWithdrawTx(data, nil) {
		t.Fatal("nil recipient matched an ELAMinter method")
	}
}
