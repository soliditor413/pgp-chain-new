// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package ethapi

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// The RPC fee defaults used to panic on a pgp-legacy block. The chain never
// activated EIP-1559, so its headers carry no BaseFee at all (core/evm.go
// substitutes 0 for the EVM context for the same reason). setFeeDefaults used
// to decide solely on IsLondon, and a devnet genesis sets londonBlock=0 so that
// modern transaction types are accepted -- which makes IsLondon answer true for
// legacy-era blocks too. The 1559 branch then multiplied a nil base fee:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	  math/big.(*Int).mul
//	  internal/ethapi/transaction_args.go:273 setLondonFeeDefaults
//
// The tests below pin both sides of the fix: a header without a base fee falls
// back to the legacy gas price, and a header with one still gets 1559 defaults.

// legacyFeeBackend is the backendMock shape these paths read, with a
// devnet-like config: londonBlock 0 (modern tx types accepted) plus a Legacy
// fork at 642, so blocks 1..641 are pgp-legacy.
func legacyFeeBackend(head *types.Header) *backendMock {
	return &backendMock{
		current: head,
		config: &params.ChainConfig{
			ChainID:             big.NewInt(42),
			HomesteadBlock:      big.NewInt(0),
			DAOForkBlock:        nil,
			DAOForkSupport:      true,
			EIP150Block:         big.NewInt(0),
			EIP155Block:         big.NewInt(0),
			EIP158Block:         big.NewInt(0),
			ByzantiumBlock:      big.NewInt(0),
			ConstantinopleBlock: big.NewInt(0),
			PetersburgBlock:     big.NewInt(0),
			IstanbulBlock:       big.NewInt(0),
			MuirGlacierBlock:    big.NewInt(0),
			BerlinBlock:         big.NewInt(0),
			LondonBlock:         big.NewInt(0),
			Legacy:              &params.LegacyConfig{ForkHeight: 642},
		},
	}
}

// legacyHead is a header in the pgp-legacy era: height 2, and -- the point of
// the whole file -- no BaseFee field.
func legacyHead(baseFee *big.Int) *types.Header {
	return &types.Header{
		Number:   big.NewInt(2),
		GasLimit: 30_000_000,
		Time:     555,
		BaseFee:  baseFee,
	}
}

// legacyArgs is the eth_sendTransaction shape from the report: only from/to/
// value are given, no fee fields at all. Gas and Nonce are filled in so
// setDefaults does not need chain state to estimate them; that keeps the test on
// the fee path, which is where the panic was.
func legacyArgs(from, to common.Address) *TransactionArgs {
	gas := hexutil.Uint64(21000)
	nonce := hexutil.Uint64(0)
	return &TransactionArgs{
		From:  &from,
		To:    &to,
		Value: (*hexutil.Big)(big.NewInt(0)),
		Gas:   &gas,
		Nonce: &nonce,
	}
}

// TestSetDefaultsLegacyHeaderWithoutBaseFee is the crash: before the fix this
// test panicked inside setLondonFeeDefaults. It must not, and it must fall back
// to the legacy gas price -- a zero or missing price would be just as wrong for
// the caller as the panic.
func TestSetDefaultsLegacyHeaderWithoutBaseFee(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

	head := legacyHead(nil)
	b := legacyFeeBackend(head)
	args := legacyArgs(from, to)

	// The panic this guards against happened inside this call.
	if err := args.setDefaults(context.Background(), b, sidecarConfig{}); err != nil {
		t.Fatalf("setDefaults on a header without a base fee returned %v, want the legacy gas-price default", err)
	}

	if args.GasPrice == nil {
		t.Fatalf("GasPrice is nil after setDefaults on a legacy header: the tx has no fee at all")
	}
	if got, want := args.GasPrice.ToInt(), big.NewInt(42); got.Cmp(want) != 0 {
		t.Fatalf("GasPrice = %v, want the backend's suggested tip %v", got, want)
	}
	if args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil {
		t.Fatalf("1559 fee fields were filled (maxFee=%v, maxPriority=%v) on a header with no base fee",
			args.MaxFeePerGas, args.MaxPriorityFeePerGas)
	}
	if args.ChainID == nil || args.ChainID.ToInt().Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("ChainID = %v, want the chain's 42 (setDefaults must still complete)", args.ChainID)
	}
}

// TestSetDefaultsHeaderWithBaseFeeStaysLondon is the control: the same config
// and the same args, with a base fee on the header. The 1559 defaults must
// still be derived -- the fix must narrow the branch, not disable it.
func TestSetDefaultsHeaderWithBaseFeeStaysLondon(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

	baseFee := big.NewInt(1_000_000_000)
	head := legacyHead(baseFee)
	b := legacyFeeBackend(head)
	args := legacyArgs(from, to)

	if err := args.setDefaults(context.Background(), b, sidecarConfig{}); err != nil {
		t.Fatalf("setDefaults with a base fee in the head returned %v, want nil", err)
	}

	if args.GasPrice != nil {
		t.Fatalf("GasPrice = %v was set on a London-active header; the legacy default leaked into the 1559 path",
			args.GasPrice)
	}
	if args.MaxPriorityFeePerGas == nil {
		t.Fatalf("MaxPriorityFeePerGas is nil on a London-active header")
	}
	if args.MaxFeePerGas == nil {
		t.Fatalf("MaxFeePerGas is nil on a London-active header")
	}
	// 2*baseFee + tip, the same formula the 1559 branch uses.
	want := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), big.NewInt(42))
	if got := args.MaxFeePerGas.ToInt(); got.Cmp(want) != 0 {
		t.Fatalf("MaxFeePerGas = %v, want %v", got, want)
	}
}

// TestLegacyHeaderProducesARecoverableLegacyTx pins the second symptom the user
// reported: a transaction that the node signs but cannot recover the sender of
// ("invalid transaction v, r, s values"). A legacy header must produce a legacy
// tx -- not a 1559 tx with fields that were never set -- and the sender must
// come back out of it.
//
// ToTransaction is asked for DynamicFeeTxType on purpose: that is what the
// send-transaction path passes when 1559 is available, and the legacy gas price
// set above is what must override it.
func TestLegacyHeaderProducesARecoverableLegacyTx(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

	head := legacyHead(nil)
	b := legacyFeeBackend(head)
	args := legacyArgs(from, to)

	if err := args.setDefaults(context.Background(), b, sidecarConfig{}); err != nil {
		t.Fatalf("setDefaults returned %v", err)
	}

	tx := args.ToTransaction(types.DynamicFeeTxType)
	if got := tx.Type(); got != types.LegacyTxType {
		t.Fatalf("ToTransaction(DynamicFeeTxType) on a legacy header produced tx type %d, want %d (legacy): a "+
			"dynamic-fee tx needs a base fee this chain does not have", got, types.LegacyTxType)
	}

	signed, err := types.SignTx(tx, types.LatestSigner(b.ChainConfig()), key)
	if err != nil {
		t.Fatalf("signing the legacy tx failed: %v", err)
	}

	// MakeSigner is what the transaction pool and the block validator use to
	// recover the sender, so it is what the assertion has to go through.
	sender, err := types.Sender(types.MakeSigner(b.ChainConfig(), head.Number, head.Time), signed)
	if err != nil {
		t.Fatalf("recovering the sender failed (%v): the node would reject its own transaction with "+
			"\"invalid transaction v, r, s values\"", err)
	}
	if sender != from {
		t.Fatalf("recovered sender = %v, want %v", sender, from)
	}
}
