// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package txpool

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// TestTxTypeForkGateAtSubmission is the deterministic half of the on-chain
// matrix: the devnet run can lose its window (the pool judges by head, so a case
// fired two blocks early or late reports the wrong verdict), this one cannot.
//
// The pool decides by head number/time, so the same transaction is refused
// before Cancun/Prague and admitted after -- that asymmetry is the whole point
// of the fork gate.
func TestTxTypeForkGateAtSubmission(t *testing.T) {
	const (
		fork     = uint64(100)
		cancunTs = uint64(1794613200)
	)
	u := func(v uint64) *uint64 { return &v }
	cfg := &params.ChainConfig{
		ChainID:             big.NewInt(860621),
		HomesteadBlock:      big.NewInt(1),
		EIP150Block:         big.NewInt(2),
		EIP155Block:         big.NewInt(3),
		EIP158Block:         big.NewInt(3),
		ByzantiumBlock:      big.NewInt(3),
		ConstantinopleBlock: big.NewInt(3),
		PetersburgBlock:     big.NewInt(3),
		IstanbulBlock:       big.NewInt(3),
		BerlinBlock:         big.NewInt(3),
		LondonBlock:         big.NewInt(3),
		KeplerTime:          u(0),
		FeynmanTime:         u(0),
		FeynmanFixTime:      u(0),
		CancunTime:          u(cancunTs),
		HaberTime:           u(cancunTs),
		HaberFixTime:        u(cancunTs),
		BohrTime:            u(cancunTs),
		PascalTime:          u(cancunTs),
		PragueTime:          u(cancunTs),
		// Parlia 非 nil 才会让 Rules() 认 Cancun/Prague（IsInBSC）。
		Parlia: &params.ParliaConfig{},
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: params.DefaultCancunBlobConfig,
			Prague: params.DefaultPragueBlobConfigBSC,
		},
		Legacy: &params.LegacyConfig{
			ForkHeight:                 fork,
			SystemContractDeployHeight: fork - 2,
			AllowForeignGenesis:        true,
		},
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	signer := types.LatestSigner(cfg)

	// 类型 3 不需要 sidecar 才能签名，但池层校验必须先过 fork 门禁才轮到
	// ValidateBlobTx，所以"没有 sidecar"正是过了门禁的证据。
	blobTx, err := types.SignTx(types.NewTx(&types.BlobTx{
		ChainID: uint256.MustFromBig(cfg.ChainID), Nonce: 0,
		GasTipCap: uint256.NewInt(1e9), GasFeeCap: uint256.NewInt(2e9), Gas: 21000,
		To: to, Value: uint256.NewInt(0),
		BlobFeeCap: uint256.NewInt(1), BlobHashes: []common.Hash{{0x01}},
	}), signer, key)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := types.SignSetCode(key, types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(cfg.ChainID),
		Address: common.HexToAddress("0x0000000000000000000000000000000000001000"),
		Nonce:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	setCodeTx, err := types.SignTx(types.NewTx(&types.SetCodeTx{
		ChainID: uint256.MustFromBig(cfg.ChainID), Nonce: 0,
		GasTipCap: uint256.NewInt(1e9), GasFeeCap: uint256.NewInt(2e9), Gas: 120000,
		To: crypto.PubkeyToAddress(key.PublicKey), Value: uint256.NewInt(0),
		AuthList: []types.SetCodeAuthorization{auth},
	}), signer, key)
	if err != nil {
		t.Fatal(err)
	}

	opts := &ValidationOptions{
		Config:       cfg,
		Accept:       0xFF,
		MaxSize:      1 << 20,
		MaxBlobCount: 6,
		MinTip:       big.NewInt(0),
	}

	head := func(num, ts uint64) *types.Header {
		return &types.Header{Number: new(big.Int).SetUint64(num), Time: ts,
			Difficulty: big.NewInt(1), GasLimit: 30000000}
	}

	tests := []struct {
		name    string
		tx      *types.Transaction
		head    *types.Header
		wantSub string
	}{
		{"blob before cancun", blobTx, head(fork+10, cancunTs-60), "type 3 rejected, pool not yet in Cancun"},
		{"blob at cancun", blobTx, head(fork+200, cancunTs), "missing sidecar in blob transaction"},
		{"setcode before prague", setCodeTx, head(fork+10, cancunTs-60), "type 4 rejected, pool not yet in Prague"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransaction(tc.tx, tc.head, signer, opts)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}

	// 过了 Prague 之后，setcode 不再被 fork 门禁拦下。
	t.Run("setcode at prague", func(t *testing.T) {
		if err := ValidateTransaction(setCodeTx, head(fork+200, cancunTs), signer, opts); err != nil {
			t.Fatalf("setcode should be admitted once Prague is active, got %v", err)
		}
	})
}
