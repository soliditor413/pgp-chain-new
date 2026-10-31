// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The pgp-chain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the pgp-chain library. If not, see <http://www.gnu.org/licenses/>.

package pbft

import (
	"errors"
	"math/big"
	"testing"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/params"
)

// TestConfirmWithoutItsBlockDoesNotStallTheHeight is the 23:04 stall as an
// executable definition.
//
// The chain produced h4 through h21 and then stopped for four minutes. Every node
// held a confirm for h22, none of them had that block, and both gates keyed on the
// confirm fired on every attempt:
//
//	WARN PBFT: height already has a confirm, refusing to propose height=22
//	WARN OnProposalReceived: rejecting, height already finalised with another block
//
// The only code that removes a height confirm prunes a window six blocks behind the
// height being imported, and nothing was being imported, so the state was not slow
// -- it was permanent. This test builds exactly that state, starts the view loop
// (which carries the watchdog), and requires the height to become proposable again.
//
// Nothing here inserts the block into the chain: that omission IS the defect.
func TestConfirmWithoutItsBlockDoesNotStallTheHeight(t *testing.T) {
	const producerCount = 4

	accounts, producerKeys := newProducerSet(t, producerCount)
	legacy := &params.LegacyConfig{VerifySeal: true, Producers: producerKeys}

	genesis := &types.Header{
		Number:     big.NewInt(0),
		Time:       uint64(time.Now().Add(-blockPeriod * time.Second).Unix()),
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}
	chain := &sealTestChain{genesis: genesis}

	engine := New(legacy, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.MarkRecovered()
	engine.AttachChain(chain)
	defer engine.Close()

	newHeader := func() *types.Header {
		return &types.Header{
			ParentHash:  genesis.Hash(),
			UncleHash:   types.CalcUncleHash(nil),
			Coinbase:    common.HexToAddress("0x00000000000000000000000000000000000000aa"),
			Root:        common.HexToHash("0x01"),
			TxHash:      types.EmptyTxsHash,
			ReceiptHash: types.EmptyReceiptsHash,
			Number:      big.NewInt(1),
			GasLimit:    30_000_000,
			Difficulty:  new(big.Int).Set(diffInTurn),
		}
	}

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})
	defer close(stop)

	header := newHeader()
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare refused to open the first round: %v", err)
	}

	// Get the confirm for h1 without ever handing the block to the chain.
	voteErr := make(chan error, 1)
	go func() {
		proposal := waitForProposal(engine)
		if proposal == nil {
			voteErr <- errTestNoProposal
			return
		}
		proposalHash := proposal.Hash()
		// Self + (majority-1) peers decides the round; n=4 gives a majority of 2.
		for i := 1; i < engine.dispatcher.GetConsensusView().GetMajorityCount(); i++ {
			vote, err := dpos.StartVote(&proposalHash, true, accounts[i])
			if err != nil {
				voteErr <- err
				return
			}
			if _, finished, err := engine.dispatcher.ProcessVote(vote); err != nil {
				voteErr <- err
				return
			} else if finished {
				voteErr <- nil
				return
			}
		}
		voteErr <- errTestNotFinal
	}()

	if err := engine.Seal(chain, types.NewBlockWithHeader(header), results, stop); err != nil {
		t.Fatalf("Seal of the on-duty round failed: %v", err)
	}
	select {
	case err := <-voteErr:
		if err != nil {
			t.Fatalf("the peer votes did not complete the round: %v", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the peer votes never completed the round")
	}
	select {
	case <-results:
		// Deliberately not inserted. This is the lost block body: the confirm went
		// out on the DPoS channel and the block never reached the chain.
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the round never handed a block to the miner")
	}

	// ---- the state every node was in ----------------------------------------
	if _, ok := engine.blockPool.GetConfirmByHeight(1); !ok {
		t.Fatal("fixture did not leave a confirm for h1 in the pool")
	}
	if head := engine.headNumber(); head != 0 {
		t.Fatalf("fixture advanced the head to %d; the point is that it does not", head)
	}

	// The deadlock, stated: with the confirm held and the block missing, the round
	// cannot even be opened, let alone produced.
	shortStop := make(chan struct{})
	close(shortStop)
	err := engine.Seal(chain, types.NewBlockWithHeader(newHeader()), make(chan *types.Block, 1), shortStop)
	if !errors.Is(err, ErrAlreadyConfirmedBlock) {
		t.Fatalf("expected the vote lock to refuse the height, got %v", err)
	}

	// ---- the watchdog has to break it --------------------------------------
	engine.ensureViewLoop()

	deadline := time.Now().Add(confirmOrphanGrace + 8*time.Second)
	for time.Now().Before(deadline) {
		if _, held := engine.blockPool.GetConfirmByHeight(1); !held {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if c, held := engine.blockPool.GetConfirmByHeight(1); held {
		t.Fatalf("the confirm for h1 is still held after %v with no block in the pool: "+
			"the height can never be proposed or accepted again", confirmOrphanGrace)
	} else if c != nil {
		t.Fatalf("GetConfirmByHeight returned a stale confirm half-removed: %v", c)
	}

	// And both gates keyed on the confirm have released. Two of them existed, and
	// only these two:
	//
	//   - the pool gate, which is what the watchdog owns -- checked above by the
	//     entry being gone;
	//   - the competitor gate, which is what rejects a peer's proposal at that
	//     height. It fires on "a DIFFERENT block is confirmed at this height", so
	//     with the confirm dropped it must answer false.
	//
	// Seal itself is deliberately not called again. Its other gate --
	// `NumberU64() <= GetFinishedHeight()` -- is finality, and this round did reach
	// it, so h1 is correctly and permanently unproposable on this node. Relaxing
	// that one would be a double proposal at a final height; it is the opposite of
	// the bug this test is about.
	sealed := header.Hash()
	if engine.hasConfirmedCompetitor(&payload.DPOSProposal{
		BlockHash: elacom.Uint256(sealed),
	}) {
		t.Fatal("the competitor gate still rejects proposals at a height the watchdog released")
	}
}
