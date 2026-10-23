// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package params

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestPGPLegacyRulesShanghai pins the execution rules of the pgp-chain legacy
// history.
//
// INVARIANT: legacy blocks carry rules.IsShanghai == true, and it must never be
// turned back into false by a later change. Three consensus behaviours hang off
// it, and pgp-chain has all three switched on:
//
//   - the Shanghai instruction set, so PUSH0 (EIP-3855) and the 0x44 ->
//     0xPREVRANDAO override in the legacy jump table mean what pgp meant;
//   - EIP-3651 warm coinbase (core/state/statedb.go:1527), worth 2500 gas on the
//     first access of every transaction that touches the block producer;
//   - the EIP-3860 49152 byte initcode ceiling (core/vm/common.go:34), which
//     pgp-chain enforces in pgp-chain/core/state_transition.go:299-300.
//
// The ONE rule that must not follow IsShanghai is the EIP-3860 *word gas*:
// pgp-chain's IntrinsicGas takes no Shanghai flag at all
// (pgp-chain/core/state_transition.go:125), so core/state_transition.go:623
// gates it on "IsShanghai && !IsPGPLegacy". Turning IsShanghai off here to
// "align" the word gas would silently disable the three rules above instead -
// and the first 8064 blocks of pgp mainnet, which are the only regression oracle
// we have, would still match, because no transaction in that range touches the
// coinbase address.
//
// The pgp genesis timestamp (1762218000) is already past ShanghaiTime
// (1761657650), and pgp-chain's own IsShanghai only compares the timestamp
// (pgp-chain params/config.go:627), so the chain runs the Shanghai instruction
// set - PUSH0 included - from block 1. BSC's gates would say otherwise:
//
//   - Rules() required (isMerge || IsInBSC()), and pgp is neither;
//   - ChainConfig.IsShanghai() also requires IsLondon(num), but pgp's LondonBlock
//     is 3, so blocks 1 and 2 would still be wrong.
//
// If this test fails the node silently executes historical pgp blocks with the
// London instruction set and any PUSH0 in a deployed contract becomes
// opUndefined.
func TestPGPLegacyRulesShanghai(t *testing.T) {
	const genesisTime = uint64(1762218000)

	for _, number := range []int64{1, 2, 3, 1000, 1 << 40} {
		rules := PGPChainConfig.Rules(big.NewInt(number), false, genesisTime)
		if !rules.IsPGPLegacy {
			t.Fatalf("block %d: PGPChainConfig must report the legacy rules", number)
		}
		if !rules.IsShanghai {
			t.Fatalf("block %d: the legacy history must run with the Shanghai instruction set", number)
		}
	}
	// A hypothetical block before ShanghaiTime must not have it.
	if rules := PGPChainConfig.Rules(big.NewInt(1), false, 1761657649); rules.IsShanghai {
		t.Fatal("Shanghai must not be active before ShanghaiTime")
	}
	// London must stay governed by LondonBlock, unchanged.
	if rules := PGPChainConfig.Rules(big.NewInt(1), false, genesisTime); rules.IsLondon {
		t.Fatal("London must still start at LondonBlock (3), not at genesis")
	}
	if rules := PGPChainConfig.Rules(big.NewInt(3), false, genesisTime); !rules.IsLondon {
		t.Fatal("London must be active from LondonBlock (3)")
	}
	// Cancun and later are not configured for pgp, so they must stay off.
	if rules := PGPChainConfig.Rules(big.NewInt(1000), false, genesisTime+1<<30); rules.IsCancun || rules.IsPrague {
		t.Fatal("Cancun/Prague are not activated on pgp-chain mainnet")
	}
}

// TestPGPLegacyRulesDoNotLeak makes sure the legacy correction never reaches the
// BSC or Ethereum configs.
func TestPGPLegacyRulesDoNotLeak(t *testing.T) {
	cases := map[string]*ChainConfig{
		"bsc":      BSCChainConfig,
		"chapel":   ChapelChainConfig,
		"ethereum": MainnetChainConfig,
	}
	for name, config := range cases {
		rules := config.Rules(big.NewInt(1), false, 1762218000)
		if rules.IsPGPLegacy {
			t.Fatalf("%s must not report the pgp legacy rules", name)
		}
	}
}

// TestPGPLegacyDefaults pins the legacy parameters that pgp-chain supplies from
// command line flag defaults rather than from source constants.
func TestPGPLegacyDefaults(t *testing.T) {
	legacy := PGPChainConfig.Legacy
	if legacy == nil {
		t.Fatal("PGPChainConfig.Legacy must be configured")
	}
	// pgp-chain cmd/utils/flags.go:772-776, the value mainnet nodes run with.
	if legacy.PassBalance != 1000000000000000000 {
		t.Fatalf("PassBalance mismatch: have %d, want 1e18", legacy.PassBalance)
	}
	if legacy.GasFeeRecipientRatio != 65 {
		t.Fatalf("GasFeeRecipientRatio mismatch: have %d, want 65", legacy.GasFeeRecipientRatio)
	}
	if legacy.GasFeeRecipient != DefaultGasFeeRecipient {
		t.Fatalf("GasFeeRecipient mismatch: have %x, want %x", legacy.GasFeeRecipient, DefaultGasFeeRecipient)
	}
	// math.MaxUint64 means "the fork is not scheduled yet", so IsPGPLegacy is true
	// for every reachable block height.
	if !PGPChainConfig.IsPGPLegacy(big.NewInt(0)) || !PGPChainConfig.IsPGPLegacy(nil) {
		t.Fatal("every height of the shipped pgp config must be legacy")
	}
}

// TestPGPELAMinterPredicates pins the two ELA gateway entry points that the
// legacy state transition treats specially.
//
// The selectors are keccak256 of the canonical ABI signatures, and both appear
// verbatim in the dispatcher of the ELAMINTER bytecode that ships in the
// pgp-chain genesis state (params/pgp_contracts.go: 0x3e44a583 and 0x68655bcc),
// which is the independent confirmation that they are the right four bytes.
func TestPGPELAMinterPredicates(t *testing.T) {
	if got := hex.EncodeToString(elaMinterRechargeSelector); got != "3e44a583" {
		t.Fatalf("Recharge(bytes32,bytes) selector changed: have %s, want 3e44a583", got)
	}
	if got := hex.EncodeToString(elaMinterRefundWithdrawSelector); got != "68655bcc" {
		t.Fatalf("refundWithdraw(bytes32) selector changed: have %s, want 68655bcc", got)
	}

	minter := ELAMINTER
	other := common.HexToAddress("0x0000000000000000000000000000000000000099")
	recharge := append([]byte{0x3e, 0x44, 0xa5, 0x83}, make([]byte, 64)...)
	refund := append([]byte{0x68, 0x65, 0x5b, 0xcc}, make([]byte, 32)...)

	if !IsPGPRechargeTx(recharge, &minter) || !IsPGPRefundWithdrawTx(refund, &minter) {
		t.Fatal("the gateway calls must be recognised")
	}
	// Address, selector and nil recipient are all part of the test.
	if IsPGPRechargeTx(recharge, &other) {
		t.Fatal("a call to another contract must not be treated as a recharge")
	}
	if IsPGPRechargeTx(recharge, nil) {
		t.Fatal("a contract creation must not be treated as a recharge")
	}
	if IsPGPRechargeTx(refund, &minter) || IsPGPRefundWithdrawTx(recharge, &minter) {
		t.Fatal("the two entry points must not be confused")
	}
	if IsPGPRechargeTx([]byte{0x01, 0x02}, &minter) {
		t.Fatal("a short input must not match")
	}
}
