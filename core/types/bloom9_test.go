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

// TestMergeBloomMatchesCreateBloomWithTxList pins the one invariant that ties the
// block producer to the block validator together.
//
// The producer fills header.Bloom by merging the per-receipt blooms that
// core.MakeReceipt already computed (MergeBloom, called from
// core/types/block.go:327). The validator does not trust that value: it
// recomputes the bloom from the receipts and the transactions
// (CreateBloomWithTxList, core/block_validator.go:176). Those two have to agree
// bit for bit, or the produced block is rejected by every peer — which is what
// "invalid bloom (remote: 000…000 local: …0400…0080…)" was.
//
// The disagreement was a "no logs, nothing to merge" fast path in MergeBloom.
// It is only valid when a receipt's bloom comes from its logs alone. On the
// legacy pgp rules a receipt's bloom is logs ∪ sender ∪ recipient
// (CreateBloomWithTx), so a receipt with no logs still has a non-empty bloom and
// skipping it dropped the from/to bits. Because the bug lives in the fast path,
// a fixture whose receipts all carry logs — like the rSmall/rLarge benchmark
// fixtures above — can never exercise it. That is why every subtest below states
// explicitly whether its receipts have logs.
//
// The assertions compare Bloom values with ==, i.e. byte for byte. Do not
// weaken them to Bloom.Test(addr): a bloom can be missing bits and still answer
// Test() true for a given address, so Test() passes on a partially wrong bloom.
func TestMergeBloomMatchesCreateBloomWithTxList(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	logAddress := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	logTopic := common.HexToHash("0x00000000000000000000000000000000000000000000000000000000000000cc")

	// sign returns an unprotected (Frontier) transaction, which is what the legacy
	// history carries and what CreateBloomWithTxList recovers the sender with.
	sign := func(t *testing.T, tx *Transaction) *Transaction {
		t.Helper()
		signed, err := SignTx(tx, FrontierSigner{}, key)
		if err != nil {
			t.Fatalf("failed to sign the transaction: %v", err)
		}
		return signed
	}
	// receipt mirrors what core.MakeReceipt produces on the legacy rules
	// (core/state_processor.go:390-392): the bloom is set from the logs AND the
	// transaction, and the processors that run afterwards must not touch it.
	receipt := func(tx *Transaction, logs []*Log) *Receipt {
		r := &Receipt{Type: tx.Type(), Status: ReceiptStatusSuccessful, TxHash: tx.Hash(), Logs: logs}
		r.Bloom = CreateBloomWithTx(r, tx)
		return r
	}
	plainTransfer := func(nonce uint64) *Transaction {
		return sign(t, NewTransaction(nonce, recipient, big.NewInt(1), 21000, big.NewInt(1), nil))
	}
	loggingCall := func(nonce uint64) *Transaction {
		return sign(t, NewTransaction(nonce, logAddress, big.NewInt(0), 100000, big.NewInt(1), []byte{0x60, 0x00, 0x60, 0x00, 0xa0}))
	}
	oneLog := []*Log{{Address: logAddress, Topics: []common.Hash{logTopic}}}

	// Both sides recover the sender from the signature, so a signing problem would
	// make them agree on the zero address and every assertion below would pass
	// vacuously. Pin the recovery first.
	probe, err := Sender(FrontierSigner{}, plainTransfer(0))
	if err != nil || probe != from {
		t.Fatalf("fixture problem: recovered %v (err %v) from the signed transaction, want %v", probe, err, from)
	}

	tests := []struct {
		name string
		// build returns the transactions of the block and the receipts the
		// producer would hold, in block order.
		build func(t *testing.T) (Transactions, Receipts)
		// wantEmpty is the value MergeBloom has to return. It is spelled out per
		// case rather than derived, so a change in the merge cannot silently
		// redefine what "correct" means.
		wantEmpty bool
		// wantLogless is how many receipts of the block carry no logs. It is
		// asserted, not documented: a case that claims to exercise the old fast
		// path has to actually contain a log-less receipt, otherwise it would
		// pass before the fix too and prove nothing.
		wantLogless int
	}{
		{
			// The devnet incident: a block holding one logging call and one plain
			// transfer. Only the first receipt has logs, so only the first one
			// used to contribute — the header bloom came out missing the
			// transfer's from/to bits.
			name: "mixed: one receipt with logs, one without",
			build: func(t *testing.T) (Transactions, Receipts) {
				withLogs, without := loggingCall(0), plainTransfer(1)
				return Transactions{withLogs, without}, Receipts{receipt(withLogs, oneLog), receipt(without, nil)}
			},
			wantLogless: 1,
		},
		{
			// Block 15 of the devnet run: nothing in the block emits a log, so the
			// old fast path skipped every receipt and header.Bloom came out all
			// zero while the validator recomputed three bits.
			name: "no receipt has logs",
			build: func(t *testing.T) (Transactions, Receipts) {
				a, b := plainTransfer(0), plainTransfer(1)
				return Transactions{a, b}, Receipts{receipt(a, nil), receipt(b, nil)}
			},
			wantLogless: 2,
		},
		{
			name: "contract creation with no logs",
			build: func(t *testing.T) (Transactions, Receipts) {
				create := sign(t, NewContractCreation(0, big.NewInt(0), 53844, big.NewInt(1), []byte{0x60, 0x00}))
				return Transactions{create}, Receipts{receipt(create, nil)}
			},
			wantLogless: 1,
		},
		{
			// Control: every receipt has logs. This one passed before the fix too,
			// because the fast path never fired. It is here so a future change
			// cannot fix the log-less case by breaking this one.
			name: "every receipt has logs",
			build: func(t *testing.T) (Transactions, Receipts) {
				a, b := loggingCall(0), loggingCall(1)
				return Transactions{a, b}, Receipts{receipt(a, oneLog), receipt(b, oneLog)}
			},
			wantLogless: 0,
		},
		{
			// An empty block: the header bloom is the zero value and block.go:310
			// never even reaches MergeBloom. Kept so the "no receipts" shape is
			// pinned alongside the rest.
			name:      "no receipts at all",
			build:     func(t *testing.T) (Transactions, Receipts) { return nil, nil },
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			txs, receipts := tt.build(t)

			// Fixture self-check first: a case that claims to hit the old fast path
			// must really contain log-less receipts whose bloom is non-empty.
			logless := 0
			for i, r := range receipts {
				if len(r.Logs) != 0 {
					continue
				}
				logless++
				if r.Bloom == EmptyBloom {
					t.Fatalf("fixture problem: receipt %d has no logs and an empty bloom, "+
						"so the case cannot exercise the merge at all", i)
				}
			}
			if logless != tt.wantLogless {
				t.Fatalf("fixture problem: %d log-less receipts, want %d", logless, tt.wantLogless)
			}

			merged := MergeBloom(receipts)
			want := CreateBloomWithTxList(receipts, txs)
			if merged != want {
				t.Fatalf("MergeBloom(producer) = %x\nCreateBloomWithTxList(validator) = %x\n"+
					"the two must agree or every peer rejects the block with \"invalid bloom\"", merged, want)
			}
			if tt.wantEmpty != (merged == EmptyBloom) {
				t.Fatalf("bloom = %x, wantEmpty = %v", merged, tt.wantEmpty)
			}
		})
	}

	// Known asymmetry, recorded rather than fixed.
	//
	// CreateBloomWithTxList can only mix in the sender and the recipient of a
	// receipt whose transaction is in the list; for a receipt it cannot resolve
	// it emits the logs and skips the from/to bits (bloom9.go:158-161). MergeBloom
	// has no such list and merges whatever the receipt carries. The two therefore
	// disagree for an "orphan" receipt — one whose transaction is missing from the
	// block body.
	//
	// That shape is unreachable through both paths that matter, because both keep
	// receipts and transactions in lockstep: the miner appends them together
	// (miner/worker.go:857-858) and block execution builds the receipt list from
	// block.Transactions() (core/state_processor.go:193). The contract this
	// subtest locks is: a caller that hands MergeBloom a receipt set must also
	// hand the same transactions to CreateBloomWithTxList. If that ever stops
	// holding, the symptom will be an "invalid bloom" that looks like this bug but
	// is not.
	t.Run("orphan receipt is a known asymmetry", func(t *testing.T) {
		logging := loggingCall(0)
		orphan := receipt(logging, oneLog)
		merged := MergeBloom(Receipts{orphan})
		validated := CreateBloomWithTxList(Receipts{orphan}, nil)

		if merged == validated {
			t.Fatal("the orphan case stopped being asymmetric; the contract documented above is stale")
		}
		// Bitwise containment, not Bloom.Test: every bit the validator derived has
		// to be in the merged bloom (the logs are on both sides), and the merged
		// bloom has to carry extra bits (the from/to pair it could not derive).
		var extraBytes int
		for i := range merged {
			if validated[i]&^merged[i] != 0 {
				t.Fatalf("byte %d: validator has bits the merge lost (0x%02x vs 0x%02x); "+
					"the logs are supposed to be on both sides", i, validated[i], merged[i])
			}
			if merged[i]&^validated[i] != 0 {
				extraBytes++
			}
		}
		if extraBytes == 0 {
			t.Fatalf("merged carries nothing beyond validated (both %x); the from/to bits are what "+
				"makes this case asymmetric", merged)
		}
	})
}
