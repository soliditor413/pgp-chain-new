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
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/bitutil"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

type bytesBacked interface {
	Bytes() []byte
}

const (
	// BloomByteLength represents the number of bytes used in a header log bloom.
	BloomByteLength = 256

	// BloomBitLength represents the number of bits used in a header log bloom.
	BloomBitLength = 8 * BloomByteLength
)

var EmptyBloom = Bloom{}

// Bloom represents a 2048 bit bloom filter.
type Bloom [BloomByteLength]byte

// BytesToBloom converts a byte slice to a bloom filter.
// It panics if b is not of suitable size.
func BytesToBloom(b []byte) Bloom {
	var bloom Bloom
	bloom.SetBytes(b)
	return bloom
}

// SetBytes sets the content of b to the given bytes.
// It panics if d is not of suitable size.
func (b *Bloom) SetBytes(d []byte) {
	if len(b) < len(d) {
		panic(fmt.Sprintf("bloom bytes too big %d %d", len(b), len(d)))
	}
	copy(b[BloomByteLength-len(d):], d)
}

// Add adds d to the filter. Future calls of Test(d) will return true.
func (b *Bloom) Add(d []byte) {
	var buf [6]byte
	b.AddWithBuffer(d, &buf)
}

// add is internal version of Add, which takes a scratch buffer for reuse (needs to be at least 6 bytes)
func (b *Bloom) AddWithBuffer(d []byte, buf *[6]byte) {
	i1, v1, i2, v2, i3, v3 := bloomValues(d, buf)
	b[i1] |= v1
	b[i2] |= v2
	b[i3] |= v3
}

// Big converts b to a big integer.
// Note: Converting a bloom filter to a big.Int and then calling GetBytes
// does not return the same bytes, since big.Int will trim leading zeroes
func (b Bloom) Big() *big.Int {
	return new(big.Int).SetBytes(b[:])
}

// Bytes returns the backing byte slice of the bloom
func (b Bloom) Bytes() []byte {
	return b[:]
}

// Test checks if the given topic is present in the bloom filter
func (b Bloom) Test(topic []byte) bool {
	var buf [6]byte
	i1, v1, i2, v2, i3, v3 := bloomValues(topic, &buf)
	return v1 == v1&b[i1] &&
		v2 == v2&b[i2] &&
		v3 == v3&b[i3]
}

// MarshalText encodes b as a hex string with 0x prefix.
func (b Bloom) MarshalText() ([]byte, error) {
	return hexutil.Bytes(b[:]).MarshalText()
}

// UnmarshalText b as a hex string with 0x prefix.
func (b *Bloom) UnmarshalText(input []byte) error {
	return hexutil.UnmarshalFixedText("Bloom", input, b[:])
}

// CreateBloom creates a bloom filter out of the give Receipt (+Logs)
func CreateBloom(receipt *Receipt) Bloom {
	var (
		bin Bloom
		buf [6]byte
	)
	for _, log := range receipt.Logs {
		bin.AddWithBuffer(log.Address.Bytes(), &buf)
		for _, b := range log.Topics {
			bin.AddWithBuffer(b[:], &buf)
		}
	}
	return bin
}

// CreateBloomWithTxList creates the bloom filter the pgp-chain clients produce:
// the logs of every receipt plus the sender and the recipient of every
// transaction (pgp-chain/core/types/bloom9.go:104-134).
//
// This is consensus critical for the legacy pgp history in two places, because
// the per-receipt bloom also travels inside the receipts trie:
//   - header.Bloom
//   - header.ReceiptHash
//
// The signer matches pgp-chain: unprotected transactions are recovered with the
// Frontier signer, protected ones with EIP-155. A recipient of nil (contract
// creation) contributes only the sender.
func CreateBloomWithTxList(receipts Receipts, txs Transactions) Bloom {
	var (
		bin Bloom
		buf [6]byte

		signer        Signer = FrontierSigner{}
		signerChainID *big.Int
		haveEIP155    bool
	)
	byHash := make(map[common.Hash]*Transaction, len(txs))
	for _, tx := range txs {
		if tx != nil {
			byHash[tx.Hash()] = tx
		}
	}
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			bin.AddWithBuffer(log.Address.Bytes(), &buf)
			for _, b := range log.Topics {
				bin.AddWithBuffer(b[:], &buf)
			}
		}
		tx := byHash[receipt.TxHash]
		if tx == nil {
			continue
		}
		if tx.Protected() {
			if !haveEIP155 || signerChainID.Cmp(tx.ChainId()) != 0 {
				signerChainID = tx.ChainId()
				signer = NewEIP155Signer(signerChainID)
				haveEIP155 = true
			}
		} else {
			signer = FrontierSigner{}
		}
		// pgp-chain ignores the recovery error and hashes the zero address in
		// that case; keep the same behaviour so a malformed signature cannot
		// change the bloom relative to the reference client.
		from, _ := Sender(signer, tx)
		bin.AddWithBuffer(from.Bytes(), &buf)
		if to := tx.To(); to != nil {
			bin.AddWithBuffer(to.Bytes(), &buf)
		}
	}
	return bin
}

// CreateBloomWithTx creates the legacy bloom of a single receipt.
func CreateBloomWithTx(receipt *Receipt, tx *Transaction) Bloom {
	return CreateBloomWithTxList(Receipts{receipt}, Transactions{tx})
}

// MergeBloom merges the precomputed bloom filters in the Receipts without
// recalculating them. It assumes that each receipt’s Bloom field is already
// correctly populated.
func MergeBloom(receipts Receipts) Bloom {
	var bin Bloom
	for _, receipt := range receipts {
		if len(receipt.Logs) != 0 {
			bl := receipt.Bloom.Bytes()
			bitutil.ORBytes(bin[:], bin[:], bl)
		}
	}
	return bin
}

// Bloom9 returns the bloom filter for the given data
func Bloom9(data []byte) []byte {
	var b Bloom
	b.SetBytes(data)
	return b.Bytes()
}

// bloomValues returns the bytes (index-value pairs) to set for the given data
func bloomValues(data []byte, hashbuf *[6]byte) (uint, byte, uint, byte, uint, byte) {
	sha := hasherPool.Get().(crypto.KeccakState)
	sha.Reset()
	sha.Write(data)
	sha.Read(hashbuf[:])
	hasherPool.Put(sha)
	// The actual bits to flip
	v1 := byte(1 << (hashbuf[1] & 0x7))
	v2 := byte(1 << (hashbuf[3] & 0x7))
	v3 := byte(1 << (hashbuf[5] & 0x7))
	// The indices for the bytes to OR in
	i1 := BloomByteLength - uint((binary.BigEndian.Uint16(hashbuf[0:])&0x7ff)>>3) - 1
	i2 := BloomByteLength - uint((binary.BigEndian.Uint16(hashbuf[2:])&0x7ff)>>3) - 1
	i3 := BloomByteLength - uint((binary.BigEndian.Uint16(hashbuf[4:])&0x7ff)>>3) - 1
	return i1, v1, i2, v2, i3, v3
}

// BloomLookup is a convenience-method to check presence in the bloom filter
func BloomLookup(bin Bloom, topic bytesBacked) bool {
	return bin.Test(topic.Bytes())
}
