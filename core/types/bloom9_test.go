// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package types

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestBloom(t *testing.T) {
	positive := []string{
		"testtest",
		"test",
		"hallo",
		"other",
	}
	negative := []string{
		"tes",
		"lo",
	}

	var bloom Bloom
	for _, data := range positive {
		bloom.Add([]byte(data))
	}

	for _, data := range positive {
		if !bloom.Test([]byte(data)) {
			t.Error("expected", data, "to test true")
		}
	}
	for _, data := range negative {
		if bloom.Test([]byte(data)) {
			t.Error("did not expect", data, "to test true")
		}
	}
}

// TestBloomExtensively does some more thorough tests
func TestBloomExtensively(t *testing.T) {
	var exp = common.HexToHash("c8d3ca65cdb4874300a9e39475508f23ed6da09fdbc487f89a2dcf50b09eb263")
	var b Bloom
	// Add 100 "random" things
	for i := 0; i < 100; i++ {
		data := fmt.Sprintf("xxxxxxxxxx data %d yyyyyyyyyyyyyy", i)
		b.Add([]byte(data))
		//b.Add(new(big.Int).SetBytes([]byte(data)))
	}
	got := crypto.Keccak256Hash(b.Bytes())
	if got != exp {
		t.Errorf("Got %x, exp %x", got, exp)
	}
	var b2 Bloom
	b2.SetBytes(b.Bytes())
	got2 := crypto.Keccak256Hash(b2.Bytes())
	if got != got2 {
		t.Errorf("Got %x, exp %x", got, got2)
	}
}

func BenchmarkBloom9(b *testing.B) {
	test := []byte("testestestest")
	for b.Loop() {
		Bloom9(test)
	}
}

func BenchmarkBloom9Lookup(b *testing.B) {
	toTest := []byte("testtest")
	bloom := new(Bloom)
	for b.Loop() {
		bloom.Test(toTest)
	}
}

func BenchmarkCreateBloom(b *testing.B) {
	var txs = Transactions{
		NewContractCreation(1, big.NewInt(1), 1, big.NewInt(1), nil),
		NewTransaction(2, common.HexToAddress("0x2"), big.NewInt(2), 2, big.NewInt(2), nil),
	}
	var rSmall = Receipts{
		&Receipt{
			Status:            ReceiptStatusFailed,
			CumulativeGasUsed: 1,
			Logs: []*Log{
				{Address: common.BytesToAddress([]byte{0x11})},
				{Address: common.BytesToAddress([]byte{0x01, 0x11})},
			},
			TxHash:          txs[0].Hash(),
			ContractAddress: common.BytesToAddress([]byte{0x01, 0x11, 0x11}),
			GasUsed:         1,
		},
		&Receipt{
			PostState:         common.Hash{2}.Bytes(),
			CumulativeGasUsed: 3,
			Logs: []*Log{
				{Address: common.BytesToAddress([]byte{0x22})},
				{Address: common.BytesToAddress([]byte{0x02, 0x22})},
			},
			TxHash:          txs[1].Hash(),
			ContractAddress: common.BytesToAddress([]byte{0x02, 0x22, 0x22}),
			GasUsed:         2,
		},
	}

	var rLarge = make(Receipts, 200)
	// Fill it with 200 receipts x 2 logs
	for i := 0; i < 200; i += 2 {
		copy(rLarge[i:], rSmall)
	}
	b.Run("small-createbloom", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, receipt := range rSmall {
				receipt.Bloom = CreateBloom(receipt)
			}
		}
		b.StopTimer()

		bl := MergeBloom(rSmall)
		var exp = common.HexToHash("c384c56ece49458a427c67b90fefe979ebf7104795be65dc398b280f24104949")
		got := crypto.Keccak256Hash(bl.Bytes())
		if got != exp {
			b.Errorf("Got %x, exp %x", got, exp)
		}
	})
	b.Run("large-createbloom", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, receipt := range rLarge {
				receipt.Bloom = CreateBloom(receipt)
			}
		}
		b.StopTimer()

		bl := MergeBloom(rLarge)
		var exp = common.HexToHash("c384c56ece49458a427c67b90fefe979ebf7104795be65dc398b280f24104949")
		got := crypto.Keccak256Hash(bl.Bytes())
		if got != exp {
			b.Errorf("Got %x, exp %x", got, exp)
		}
	})
	b.Run("small-mergebloom", func(b *testing.B) {
		for _, receipt := range rSmall {
			receipt.Bloom = CreateBloom(receipt)
		}
		b.ReportAllocs()

		var bl Bloom
		for b.Loop() {
			bl = MergeBloom(rSmall)
		}

		var exp = common.HexToHash("c384c56ece49458a427c67b90fefe979ebf7104795be65dc398b280f24104949")
		got := crypto.Keccak256Hash(bl.Bytes())
		if got != exp {
			b.Errorf("Got %x, exp %x", got, exp)
		}
	})
	b.Run("large-mergebloom", func(b *testing.B) {
		for _, receipt := range rLarge {
			receipt.Bloom = CreateBloom(receipt)
		}
		b.ReportAllocs()

		var bl Bloom
		for b.Loop() {
			bl = MergeBloom(rLarge)
		}

		var exp = common.HexToHash("c384c56ece49458a427c67b90fefe979ebf7104795be65dc398b280f24104949")
		got := crypto.Keccak256Hash(bl.Bytes())
		if got != exp {
			b.Errorf("Got %x, exp %x", got, exp)
		}
	})
}

// TestCreateBloomWithTxList pins the pgp-chain legacy bloom: the logs of a
// receipt plus the sender and the recipient of its transaction
// (pgp-chain/core/types/bloom9.go:104-134).
//
// The plain CreateBloom stays logs-only, which is what every other chain needs;
// the legacy variant is selected by the caller through
// DeriveReceiptContext.LegacyBloom / core.BlockValidator.
func TestCreateBloomWithTxList(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	// Unprotected (Frontier) and EIP-155 signed transactions must both resolve
	// their sender: the legacy chain has no chainId protection requirement.
	tx1, err := SignTx(NewTransaction(0, to, big.NewInt(1), 21000, big.NewInt(1), nil), FrontierSigner{}, key)
	if err != nil {
		t.Fatalf("failed to sign the frontier transaction: %v", err)
	}
	tx2, err := SignTx(NewTransaction(1, to, big.NewInt(1), 21000, big.NewInt(1), nil), NewEIP155Signer(big.NewInt(860621)), key)
	if err != nil {
		t.Fatalf("failed to sign the EIP-155 transaction: %v", err)
	}
	txs := Transactions{tx1, tx2}
	receipts := make(Receipts, len(txs))
	for i, tx := range txs {
		receipts[i] = &Receipt{TxHash: tx.Hash(), Status: ReceiptStatusSuccessful}
	}

	legacy := CreateBloomWithTxList(receipts, txs)
	if !legacy.Test(from.Bytes()) {
		t.Fatal("legacy bloom must contain the sender")
	}
	if !legacy.Test(to.Bytes()) {
		t.Fatal("legacy bloom must contain the recipient")
	}
	// No logs, no log addresses.
	if CreateBloom(receipts[0]) != EmptyBloom {
		t.Fatal("the standard bloom of a log-less receipt must stay empty")
	}
	// A receipt whose transaction is missing from the list contributes nothing
	// (pgp-chain skips it the same way).
	orphan := CreateBloomWithTxList(Receipts{{TxHash: common.HexToHash("0xdead")}}, txs)
	if orphan != EmptyBloom {
		t.Fatal("a receipt without a matching transaction must not change the bloom")
	}

	// Contract creation: only the sender is mixed in.
	create, err := SignTx(NewContractCreation(2, big.NewInt(0), 100000, big.NewInt(1), []byte{0x00}), FrontierSigner{}, key)
	if err != nil {
		t.Fatalf("failed to sign the creation: %v", err)
	}
	bloom := CreateBloomWithTxList(Receipts{{TxHash: create.Hash()}}, Transactions{create})
	if !bloom.Test(from.Bytes()) {
		t.Fatal("legacy bloom must contain the sender of a creation")
	}
	if bloom.Test(to.Bytes()) {
		t.Fatal("a creation has no recipient and must not mix one in")
	}
}
