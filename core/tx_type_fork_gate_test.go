// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// The transaction-type gate has two layers that answer at different times: the
// txpool rejects at submission (by head number/time), the block executor rejects
// at import (by the signer the header's fork state selects). A devnet can only
// observe the first — the pool refuses the transaction, so there is nothing to
// put in a block — which is why the second layer is covered here.
//
// The chain shape mirrors the devnet used for the on-chain matrix: a legacy
// segment up to ForkHeight, Parlia afterwards, and Cancun/Prague at a timestamp
// that is only reached well after the fork.
func txTypeTestConfig(fork uint64, cancunTs uint64) *params.ChainConfig {
	u := func(v uint64) *uint64 { return &v }
	return &params.ChainConfig{
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
		Parlia:              &params.ParliaConfig{},
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: params.DefaultCancunBlobConfig,
			Prague: params.DefaultPragueBlobConfigBSC,
		},
		Legacy: &params.LegacyConfig{
			ForkHeight:                 fork,
			SystemContractDeployHeight: fork - 2,
			AllowForeignGenesis:        true,
			ParliaGenesisValidators:    []string{"0x0000000000000000000000000000000000000001"},
			ParliaGenesisVotingPowers:  []uint64{100},
			ParliaGenesisCabinets:      1,
		},
	}
}

func signedTxOfType(t *testing.T, cfg *params.ChainConfig, ty int) *types.Transaction {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	to := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	signer := types.LatestSigner(cfg)

	var inner types.TxData
	switch ty {
	case 0:
		inner = &types.LegacyTx{Nonce: 0, GasPrice: big.NewInt(1e9), Gas: 21000, To: &to, Value: big.NewInt(1)}
	case 1:
		inner = &types.AccessListTx{ChainID: cfg.ChainID, Nonce: 0, GasPrice: big.NewInt(1e9),
			Gas: 30000, To: &to, Value: big.NewInt(1),
			AccessList: types.AccessList{{Address: to, StorageKeys: []common.Hash{{0x01}}}}}
	case 2:
		inner = &types.DynamicFeeTx{ChainID: cfg.ChainID, Nonce: 0,
			GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(2e9), Gas: 21000, To: &to, Value: big.NewInt(1)}
	case 3:
		inner = &types.BlobTx{
			ChainID: uint256.MustFromBig(cfg.ChainID), Nonce: 0,
			GasTipCap: uint256.NewInt(1e9), GasFeeCap: uint256.NewInt(2e9), Gas: 21000,
			To: to, Value: uint256.NewInt(0),
			BlobFeeCap: uint256.NewInt(1), BlobHashes: []common.Hash{{0x01}},
		}
	case 4:
		auth := types.SetCodeAuthorization{
			ChainID: *uint256.MustFromBig(cfg.ChainID),
			Address: common.HexToAddress("0x0000000000000000000000000000000000001000"),
			Nonce:   1,
		}
		signed, err := types.SignSetCode(key, auth)
		if err != nil {
			t.Fatalf("sign authorization: %v", err)
		}
		inner = &types.SetCodeTx{
			ChainID: uint256.MustFromBig(cfg.ChainID), Nonce: 0,
			GasTipCap: uint256.NewInt(1e9), GasFeeCap: uint256.NewInt(2e9), Gas: 120000,
			To: crypto.PubkeyToAddress(key.PublicKey), Value: uint256.NewInt(0),
			AuthList: []types.SetCodeAuthorization{signed},
		}
	default:
		t.Fatalf("unsupported type %d", ty)
	}

	tx, err := types.SignTx(types.NewTx(inner), signer, key)
	if err != nil {
		t.Fatalf("sign tx type %d: %v", ty, err)
	}
	return tx
}

// TestTxTypeIsRejectedAtImportBeforeItsFork is the block-level half of the gate:
// state_processor resolves the signer from the header's (number, time), and a
// signer that does not support the type makes the whole transaction unappliable.
func TestTxTypeIsRejectedAtImportBeforeItsFork(t *testing.T) {
	const (
		fork     = uint64(100)
		cancunTs = uint64(1794613200)
	)
	cfg := txTypeTestConfig(fork, cancunTs)

	states := []struct {
		name string
		num  uint64
		ts   uint64
	}{
		{"legacy", fork - 1, cancunTs - 500},
		{"post-fork-pre-cancun", fork + 10, cancunTs - 60},
		{"cancun", fork + 200, cancunTs},
	}

	for _, ty := range []int{0, 1, 2, 3, 4} {
		tx := signedTxOfType(t, cfg, ty)
		for _, st := range states {
			t.Run(st.name+"/type"+string(rune('0'+ty)), func(t *testing.T) {
				signer := types.MakeSigner(cfg, new(big.Int).SetUint64(st.num), st.ts)
				_, err := TransactionToMessage(tx, signer, big.NewInt(0))

				// 只有 3/4 受时间型分叉约束；0/1/2 在 Berlin/London（block 3）之后一直可用。
				wantReject := (ty == 3 || ty == 4) && st.ts < cancunTs
				if wantReject {
					if !errors.Is(err, types.ErrTxTypeNotSupported) {
						t.Fatalf("type %d at %s should be rejected with ErrTxTypeNotSupported, got %v", ty, st.name, err)
					}
					if !strings.Contains(err.Error(), "transaction type not supported") {
						t.Fatalf("unexpected error text: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("type %d at %s should be accepted, got %v", ty, st.name, err)
				}
			})
		}
	}
}

// TestTxTypeSenderDependsOnForkNotOnSignature pins the asymmetry the devnet
// matrix relies on: the signature is always valid (LatestSigner knows every
// type), and only the signer derived from the header's fork state refuses it.
func TestTxTypeSenderDependsOnForkNotOnSignature(t *testing.T) {
	const (
		fork     = uint64(100)
		cancunTs = uint64(1794613200)
	)
	cfg := txTypeTestConfig(fork, cancunTs)

	before := types.MakeSigner(cfg, big.NewInt(int64(fork+10)), cancunTs-60)
	after := types.MakeSigner(cfg, big.NewInt(int64(fork+200)), cancunTs)

	for _, ty := range []int{3, 4} {
		tx := signedTxOfType(t, cfg, ty)
		if _, err := types.Sender(types.LatestSigner(cfg), tx); err != nil {
			t.Fatalf("type %d: signature should be valid under LatestSigner: %v", ty, err)
		}
		if _, err := types.Sender(before, tx); !errors.Is(err, types.ErrTxTypeNotSupported) {
			t.Fatalf("type %d: pre-fork signer should refuse, got %v", ty, err)
		}
		if _, err := types.Sender(after, tx); err != nil {
			t.Fatalf("type %d: post-Cancun signer should accept, got %v", ty, err)
		}
	}
}
