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

// The vote INGRESS, as opposed to the vote arithmetic.
//
// TestSealProducesABlockTheReadOnlyVerifierAccepts feeds votes straight into
// dispatcher.ProcessVote, which is what a vote ends up at -- but not how it gets
// there. The hop in between is Engine.OnVoteAccepted, and that is where a whole
// quorum was being lost: it looked the vote's proposal hash up in a pool keyed by
// block seal hashes, never matched, and parked every vote in pendingVotes. The
// arithmetic tests stayed green and the four-node devnet could not finalise a
// height, because on a single node there is no peer to send the vote that gets
// dropped.

import (
	"math/big"
	"testing"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/params"
)

// TestOnVoteAcceptedCountsAPeerVote is the regression test for the dropped
// quorum. Four producers, i.e. the devnet's shape: legacyMajorityCount rounds
// 4*2/3 down to 2, so the proposer's own vote is one short and the round can only
// finish if the peer's vote survives the ingress. Two producers will not do -- a set
// of two needs one vote, which the proposer itself casts, so the round seals
// without any peer and the test would pass over a broken ingress.
func TestOnVoteAcceptedCountsAPeerVote(t *testing.T) {
	accounts, producerKeys := newProducerSet(t, 4)
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

	header := &types.Header{
		ParentHash:  genesis.Hash(),
		UncleHash:   types.CalcUncleHash(nil),
		Coinbase:    common.HexToAddress("0x00000000000000000000000000000000000000aa"),
		Root:        common.HexToHash("0x01"),
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Number:      big.NewInt(1),
		GasLimit:    30_000_000,
	}
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare refused to open the round: %v", err)
	}

	// The one peer that supplies the quorum, going through the same function the
	// network delivers to.
	voteErr := make(chan error, 1)
	go func() {
		proposal := waitForProposal(engine)
		if proposal == nil {
			voteErr <- errTestNoProposal
			return
		}
		proposalHash := proposal.Hash()
		vote, err := dpos.StartVote(&proposalHash, true, accounts[1])
		if err != nil {
			voteErr <- err
			return
		}
		var id dpeer.PID
		copy(id[:], accounts[1].PublicKeyBytes())
		engine.OnVoteAccepted(id, vote)
		voteErr <- nil
	}()

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})
	defer close(stop)

	sealErr := make(chan error, 1)
	go func() { sealErr <- engine.Seal(chain, types.NewBlockWithHeader(header), results, stop) }()

	select {
	case err := <-voteErr:
		if err != nil {
			select {
			case se := <-sealErr:
				t.Fatalf("the peer vote was not delivered: %v (Seal had already ended: %v)", err, se)
			default:
			}
			t.Fatalf("the peer vote was not delivered: %v", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		select {
		case se := <-sealErr:
			t.Fatalf("the peer vote never went out, and Seal had already ended: %v", se)
		default:
		}
		t.Fatal("the peer vote never went out")
	}

	var sealed *types.Block
	select {
	case sealed = <-results:
	case err := <-sealErr:
		t.Fatalf("Seal ended without a block: %v (a vote for the proposal being sealed was parked instead of counted)", err)
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("Seal never produced a block: the peer vote was not counted")
	}
	if sealed.NumberU64() != 1 {
		t.Fatalf("sealed block %d, want 1", sealed.NumberU64())
	}
	if err := pgp.New(legacy).VerifyHeader(chain, sealed.Header()); err != nil {
		t.Fatalf("the sealed block does not carry a valid confirm: %v", err)
	}
}

// TestOnVoteAcceptedParksAVoteForAnotherProposal is the other half of the same
// branch: a vote must not be counted against a proposal we are not processing, or
// a vote for a competing proposal would advance a round it does not belong to.
func TestOnVoteAcceptedParksAVoteForAnotherProposal(t *testing.T) {
	accounts, producerKeys := newProducerSet(t, 4)
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

	// No round in flight: nothing is being processed, so nothing may be counted.
	engine.dispatcher.GetConsensusView().SetRunning()

	hash := elacom.Uint256{0x42}
	vote, err := dpos.StartVote(&hash, true, accounts[1])
	if err != nil {
		t.Fatalf("cannot sign a vote: %v", err)
	}
	var id dpeer.PID
	copy(id[:], accounts[1].PublicKeyBytes())
	engine.OnVoteAccepted(id, vote)

	if processing := engine.dispatcher.GetProcessingProposal(); processing != nil {
		t.Fatalf("a vote created a processing proposal: %v", processing.Hash().String())
	}
}
