package core

import (
	"bytes"
	"sync"

	"github.com/ethereum/go-ethereum/core/types"
)

type ReceiptProcessor interface {
	Apply(receipt *types.Receipt)
}

var (
	_ ReceiptProcessor = (*ReceiptBloomGenerator)(nil)
	_ ReceiptProcessor = (*AsyncReceiptBloomGenerator)(nil)
)

func NewReceiptBloomGenerator() *ReceiptBloomGenerator {
	return &ReceiptBloomGenerator{}
}

type ReceiptBloomGenerator struct {
}

// Apply fills in the bloom of a receipt that does not have one yet.
//
// It must not overwrite a bloom that core.MakeReceipt has already computed. On
// the pgp-chain legacy rules that bloom is logs ∪ sender ∪ recipient
// (core/state_processor.go:390-392, CreateBloomWithTx), and CreateBloom here is
// logs-only, so overwriting it would drop the from/to bits from both the header
// bloom and the receipts trie. Note that MakeReceipt runs the processors AFTER
// setting the bloom, so the value is always present by the time Apply is called.
// The asynchronous variant below has carried exactly this guard since it was
// written (receipt_processor.go:61-65); this one was the only violator, and it is
// reachable from core/chain_makers.go:128 and miner/bid_simulator.go:1398.
func (p *ReceiptBloomGenerator) Apply(receipt *types.Receipt) {
	if receipt != nil && bytes.Equal(receipt.Bloom[:], types.EmptyBloom[:]) {
		receipt.Bloom = types.CreateBloom(receipt)
	}
}

func NewAsyncReceiptBloomGenerator(txNums int) *AsyncReceiptBloomGenerator {
	generator := &AsyncReceiptBloomGenerator{
		receipts: make(chan *types.Receipt, txNums),
	}
	generator.startWorker()
	return generator
}

type AsyncReceiptBloomGenerator struct {
	receipts chan *types.Receipt
	wg       sync.WaitGroup
	isClosed bool
}

func (p *AsyncReceiptBloomGenerator) startWorker() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for receipt := range p.receipts {
			if receipt != nil && bytes.Equal(receipt.Bloom[:], types.EmptyBloom[:]) {
				receipt.Bloom = types.CreateBloom(receipt)
			}
		}
	}()
}

func (p *AsyncReceiptBloomGenerator) Apply(receipt *types.Receipt) {
	if !p.isClosed {
		p.receipts <- receipt
	}
}

func (p *AsyncReceiptBloomGenerator) Close() {
	close(p.receipts)
	p.isClosed = true
	p.wg.Wait()
}
