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

// The M1 acceptance test: a block produced by this engine must be accepted by
// consensus/pgp, the engine that verifies mainnet history.
//
// That is the whole safety argument for the port. Nothing here checks that the
// round "looks right" -- it drives one real round with real P-256 keys and then
// hands the sealed header to the read-only verifier, which knows nothing about
// producing and is the same code a non-upgraded node runs.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"testing"
	"time"

	elaaccount "github.com/elastos/Elastos.ELA/account"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	daccount "github.com/elastos/Elastos.ELA/dpos/account"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/params"
)

// sealTestChain is a minimal consensus.ChainHeaderReader that knows one genesis
// header.
//
// Header verification only ever asks for the parent and for whatever canonical
// block already exists at the height being checked; the second answer is nil
// here, which is the "this block is new" case. byHash exists so a multi-height
// test can hand the engine and the verifier the parents they ask for without
// turning this stub into a block store.
type sealTestChain struct {
	genesis *types.Header
	byHash  map[common.Hash]*types.Header
}

func (c *sealTestChain) Config() *params.ChainConfig { return params.PGPChainConfig }
func (c *sealTestChain) CurrentHeader() *types.Header {
	return c.genesis
}
func (c *sealTestChain) GenesisHeader() *types.Header            { return c.genesis }
func (c *sealTestChain) GetHighestVerifiedHeader() *types.Header { return c.genesis }
func (c *sealTestChain) GetVerifiedBlockByHash(common.Hash) *types.Header {
	return c.genesis
}
func (c *sealTestChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if number == 0 && hash == c.genesis.Hash() {
		return c.genesis
	}
	if header, ok := c.byHash[hash]; ok && header.Number.Uint64() == number {
		return header
	}
	return nil
}

// GetHeaderByNumber deliberately answers nil for every height but genesis, even
// for heights whose parent is in byHash. verifySeal treats a non-nil answer here
// as "this block is already canonical" and starts comparing view offsets, so a
// stub that returned the header under test would silently skip the check.
func (c *sealTestChain) GetHeaderByNumber(number uint64) *types.Header {
	if number == 0 {
		return c.genesis
	}
	return nil
}
func (c *sealTestChain) GetHeaderByHash(hash common.Hash) *types.Header {
	if hash == c.genesis.Hash() {
		return c.genesis
	}
	if header, ok := c.byHash[hash]; ok {
		return header
	}
	return nil
}
func (c *sealTestChain) GetTd(common.Hash, uint64) *big.Int { return big.NewInt(1) }

// newProducerSet generates n arbiter keys and returns them sorted by compressed
// public key, together with the hex form the configuration takes.
//
// The sort is not cosmetic. pool/producer ordering is by sorted public key, and
// the duty index at height 0 with a working height of 1 is
//
//	(0 + 1 - 1) % n = 0
//
// so the proposer of block 1 is whichever key sorts first. Returning the sorted
// slice lets the test hand the sealing account to the engine without re-deriving
// the schedule.
func newProducerSet(t *testing.T, n int) ([]daccount.Account, []string) {
	t.Helper()

	accounts := make([]daccount.Account, 0, n)
	for i := 0; i < n; i++ {
		base, err := elaaccount.NewAccount()
		if err != nil {
			t.Fatalf("cannot generate arbiter %d: %v", i, err)
		}
		// dpos.NewAccountFrom, not daccount.New: the upstream wrapper's signing
		// method panics on this Go version. See dpos/signer.go.
		account, err := dpos.NewAccountFrom(base)
		if err != nil {
			t.Fatalf("cannot wrap arbiter %d: %v", i, err)
		}
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool {
		return bytes.Compare(accounts[i].PublicKeyBytes(), accounts[j].PublicKeyBytes()) < 0
	})

	keys := make([]string, len(accounts))
	for i, account := range accounts {
		keys[i] = hex.EncodeToString(account.PublicKeyBytes())
	}
	return accounts, keys
}

// waitForProposal polls until the round has registered a proposal.
//
// Polling is what makes the test honest: Seal blocks, so the peer vote has to
// come from another goroutine, and that goroutine must not vote before the
// proposal it votes for exists.
func waitForProposal(e *Engine) *payload.DPOSProposal {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if proposal := e.dispatcher.GetProcessingProposal(); proposal != nil {
			return proposal
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// TestSealProducesABlockTheReadOnlyVerifierAccepts is the M1 gate.
//
// Twelve producers, i.e. exactly the mainnet shape, so the threshold is the real
// floor(12*2/3) = 8 votes and the resulting header.Extra is the real 1199 bytes.
// The proposer contributes its own vote inside Seal; the test contributes the
// other seven as peers would, each through the same dispatcher path a vote
// arriving from the network takes.
func TestSealProducesABlockTheReadOnlyVerifierAccepts(t *testing.T) {
	const (
		producerCount = 12
		votesNeeded   = 8
	)

	accounts, producerKeys := newProducerSet(t, producerCount)
	legacy := &params.LegacyConfig{VerifySeal: true, Producers: producerKeys}

	// The parent is one period old so Prepare does not have to re-anchor the view
	// on "now", and block 1 lands at parent.Time + blockPeriod, which is a valid
	// timestamp for the verifier.
	genesis := &types.Header{
		Number:     big.NewInt(0),
		Time:       uint64(time.Now().Add(-blockPeriod * time.Second).Unix()),
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}
	chain := &sealTestChain{genesis: genesis}

	// The account is attached below rather than opened from a keystore, which is
	// the same seam the node wiring uses once it has the key in hand. Enable is
	// what makes this node an intended producer; KeyStore stays empty so New does
	// not try to open a file.
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
	if !engine.IsOnduty() {
		t.Fatalf("account 0 is not the proposer of block 1; the producer schedule changed")
	}
	if header.Nonce.Uint64() != 0 {
		t.Fatalf("prepare anchored the block to ELA height %d, want 0", header.Nonce.Uint64())
	}

	block := types.NewBlockWithHeader(header)

	// The peers that supply the remaining seven votes.
	voteErr := make(chan error, 1)
	go func() {
		proposal := waitForProposal(engine)
		if proposal == nil {
			voteErr <- errTestNoProposal
			return
		}
		proposalHash := proposal.Hash()
		for i := 1; i < votesNeeded; i++ {
			vote, err := dpos.StartVote(&proposalHash, true, accounts[i])
			if err != nil {
				voteErr <- err
				return
			}
			if _, finished, err := engine.dispatcher.ProcessVote(vote); err != nil {
				voteErr <- err
				return
			} else if finished {
				// The threshold was reached on this vote. Any later vote would be
				// counted by a round that has already produced its confirm, which
				// is not what a network does: the rest of the set hears the confirm
				// and stays quiet.
				voteErr <- nil
				return
			}
		}
		voteErr <- errTestNotFinal
	}()

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})
	defer close(stop)

	if err := engine.Seal(chain, block, results, stop); err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	select {
	case err := <-voteErr:
		if err != nil {
			t.Fatalf("the peer vote was not counted: %v", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the peer vote never completed")
	}

	var sealed *types.Block
	select {
	case sealed = <-results:
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("Seal did not hand a block to the miner")
	}

	// The gate: the read-only engine, which is what a node that never upgrades
	// runs, must accept the block this engine produced.
	if err := pgp.New(legacy).VerifyHeader(chain, sealed.Header()); err != nil {
		t.Fatalf("the verifier rejected the block the producer sealed: %v", err)
	}

	// The confirm must carry exactly the votes the threshold needed, not every
	// vote the set could have cast: 12 producers need 8, so 135 + 8 + 8*132.
	wantExtra := pgp.ConfirmMinLen + votesNeeded*pgp.VoteWireLen
	if got := len(sealed.Header().Extra); got != wantExtra {
		t.Fatalf("header.Extra is %d bytes, want %d for an 8 vote confirm", got, wantExtra)
	}
	if wantExtra != 1199 {
		t.Fatalf("the 8 vote confirm is %d bytes, the chain's own headers are 1199", wantExtra)
	}

	// And the round must have advanced finality, otherwise the next Prepare would
	// be refused for the wrong reason.
	if got := engine.GetIrreversibleHeight(); got != 1 {
		t.Fatalf("the engine considers height %d final, want 1", got)
	}
}

var (
	errTestNoProposal = testError("the round never registered a proposal")
	errTestNotFinal   = testError("the second vote did not reach the threshold")
)

type testError string

func (e testError) Error() string { return string(e) }

// TestFailedRoundDoesNotStallTheNode is the M2 regression test for the round/view
// handshake.
//
// The shape it pins down: a round that runs and produces no block must leave the
// view in ConsensusReady, because SetRunning happens only in Prepare->startView
// and the only two calls that undo it are Dispatcher.FinishedProposal (which only
// runs when this node actually sealed something) and Dispatcher.ResetConsensus. A
// Seal that returned nil on its failure paths without resetting the view would
// therefore answer ErrConsensusIsRunning on every later Prepare and leave the node
// producing nothing, forever, with no error in the log.
//
// The test drives exactly that: one round with no peer votes at all (a plain
// timeout), then a second attempt at the same height, which must both open and
// complete. It is deliberately not satisfied by checking the error alone -- the
// second round is sealed and handed to the read-only verifier, so "the node still
// works" is demonstrated rather than inferred.
func TestFailedRoundDoesNotStallTheNode(t *testing.T) {
	const (
		producerCount = 12
		votesNeeded   = 8
	)

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
		}
	}

	// --- round 1: nobody votes, so the round can only time out ----------------

	header := newHeader()
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare refused to open the first round: %v", err)
	}
	// Let the view deadline expire instead of moving it into the past, which is
	// what this did before (SetChangViewTime(now-1min)). That setter re-anchors the
	// grid now, and the view offset is derived from the anchor, so moving the
	// deadline by a minute also rotates the rotation slot by ten views -- the
	// signer stops being on duty, and Seal answers ErrSignerNotOnduty before it
	// ever reaches the timeout path this test is about. There is no setter that
	// moves the deadline without moving the anchor, so the round is now allowed to
	// time out for real: at most one signTolerance, which the guard below covers.
	// The behaviour under test is what happens AFTER the failure, not how long the
	// failure took.

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})
	defer close(stop)

	sealed := make(chan error, 1)
	go func() {
		sealed <- engine.Seal(chain, types.NewBlockWithHeader(header), results, stop)
	}()
	select {
	case err := <-sealed:
		if !errors.Is(err, ErrRoundAborted) {
			t.Fatalf("Seal of a round nobody voted for returned %v, want ErrRoundAborted", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("Seal never gave up on the abandoned round")
	}

	// --- the assertion this test exists for -----------------------------------

	// The round no longer resets the view itself -- a per-node reset puts this node
	// one view behind the set and every peer then drops its proposals (see
	// Engine.resetRound). What releases it is the rotation, and the assertion this
	// test exists for is still "the node is not stalled for good", now in two steps.
	if engine.dispatcher.GetConsensusView().IsReady() {
		t.Fatal("a failed round cleared the view itself; the release belongs to the rotation")
	}
	releaseAfterFailedRound(engine)
	if !engine.dispatcher.GetConsensusView().IsReady() {
		t.Fatal("the rotation did not release the view: every later Prepare will answer ErrConsensusIsRunning")
	}
	reAnchorToNow(chain)
	retryHeader := newHeader()
	if err := engine.Prepare(chain, retryHeader); err != nil {
		t.Fatalf("Prepare after a failed round: %v (ErrConsensusIsRunning here means the node is stalled for good)", err)
	}
	if !engine.IsOnduty() {
		t.Fatalf("account 0 is no longer the proposer after the reset; the producer schedule changed")
	}

	// --- round 2: the same height, this time with the peer votes --------------

	voteErr := make(chan error, 1)
	go func() {
		proposal := waitForProposal(engine)
		if proposal == nil {
			voteErr <- errTestNoProposal
			return
		}
		proposalHash := proposal.Hash()
		for i := 1; i < votesNeeded; i++ {
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

	retryBlock := types.NewBlockWithHeader(retryHeader)
	if err := engine.Seal(chain, retryBlock, results, stop); err != nil {
		t.Fatalf("Seal of the retry round failed: %v", err)
	}
	select {
	case err := <-voteErr:
		if err != nil {
			t.Fatalf("the peer vote was not counted in the retry round: %v", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the peer vote never completed in the retry round")
	}
	var final *types.Block
	select {
	case final = <-results:
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the retry round did not hand a block to the miner")
	}

	// And the block is a real one: the read-only verifier accepts it.
	if err := pgp.New(legacy).VerifyHeader(chain, final.Header()); err != nil {
		t.Fatalf("the verifier rejected the block produced after the failed round: %v", err)
	}
	if got := engine.GetIrreversibleHeight(); got != 1 {
		t.Fatalf("the engine considers height %d final after the retry, want 1", got)
	}
}

// TestLateConfirmAfterFinishRoundDoesNotPanic pins the round-boundary contract
// between Seal and the goroutine that processes votes.
//
// The vote path is onConfirm -> trySendConfirm, which reads the round channel and
// then sends to it. finishRound used to close that channel and put a fresh one in
// the field. The two orders that follow are the ones a real network produces all
// the time -- a vote is accepted by the dispatcher just as the round gives up --
// and under the old implementation both of them are "send on closed channel":
// a panic that takes the node down while it is doing exactly what it should.
//
// The fix is that the channels are immutable after New and rounds are separated by
// draining, so every delivery below has to be silently harmless, and the next
// round has to be unaffected by whatever was left queued.
func TestLateConfirmAfterFinishRoundDoesNotPanic(t *testing.T) {
	const (
		producerCount = 12
		votesNeeded   = 8
	)

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
		}
	}

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})
	defer close(stop)

	// --- a round that ends without a block, to have a finished round at all ---

	header := newHeader()
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare refused to open the first round: %v", err)
	}
	// Same as the first test: the deadline expires on its own now that the offset
	// is derived from the anchor and SetChangViewTime can no longer move the
	// deadline without rotating the slot. See the note there.

	sealed := make(chan error, 1)
	go func() {
		sealed <- engine.Seal(chain, types.NewBlockWithHeader(header), results, stop)
	}()
	select {
	case err := <-sealed:
		if !errors.Is(err, ErrRoundAborted) {
			t.Fatalf("Seal of a round nobody voted for returned %v, want ErrRoundAborted", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("Seal never gave up on the abandoned round")
	}

	// A confirm for some block, not necessarily this one: the point of the test is
	// the channel handshake, and BlockHash is left zero so that a regression is
	// reported as "the stale confirm aborted the next round" rather than as a nil
	// dereference.
	stale := &payload.Confirm{}

	// --- the two orders that used to panic ------------------------------------
	//
	// Read the channel the way trySendConfirm does, let the round finish, then
	// send. finishRound used to close exactly this channel, so each send below is
	// a send on a closed channel under the old implementation, which is a panic
	// and not a test failure.
	capturedConfirm := engine.confirmCh
	engine.finishRound("late confirm")
	capturedConfirm <- stale

	capturedUnconfirm := engine.unConfirmCh
	engine.finishRound("late reject")
	capturedUnconfirm <- stale

	// The same deliveries through the real entry points. One of the two channels
	// is full by now, so this also exercises the "channel busy, drop it" branch:
	// dropping is the contract, because a lost round is retried by the view change
	// while a blocking send here would stall vote processing for the whole set.
	engine.trySendConfirm(stale)
	engine.trySendUnconfirm(stale)

	// --- and the node still produces a block ---------------------------------

	releaseAfterFailedRound(engine)
	reAnchorToNow(chain)
	retryHeader := newHeader()
	if err := engine.Prepare(chain, retryHeader); err != nil {
		t.Fatalf("Prepare after late votes: %v", err)
	}

	voteErr := make(chan error, 1)
	go func() {
		proposal := waitForProposal(engine)
		if proposal == nil {
			voteErr <- errTestNoProposal
			return
		}
		proposalHash := proposal.Hash()
		for i := 1; i < votesNeeded; i++ {
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

	// Seal drains the leftovers above before it starts waiting. Without that drain
	// it would immediately consume the stale confirm, fail the seal-hash comparison
	// and answer ErrRoundAborted here.
	if err := engine.Seal(chain, types.NewBlockWithHeader(retryHeader), results, stop); err != nil {
		t.Fatalf("Seal of the round after the late votes failed: %v", err)
	}
	select {
	case err := <-voteErr:
		if err != nil {
			t.Fatalf("the peer vote was not counted after the late votes: %v", err)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the peer vote never completed after the late votes")
	}
	var final *types.Block
	select {
	case final = <-results:
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("the round after the late votes did not hand a block to the miner")
	}
	if err := pgp.New(legacy).VerifyHeader(chain, final.Header()); err != nil {
		t.Fatalf("the verifier rejected the block produced after the late votes: %v", err)
	}
}

// TestSealExitsWhenTheMinerStops pins the shutdown contract of a blocked Seal.
//
// It exists because Seal must not depend on anybody closing a channel to get out:
// the round channels are never closed (see drainRoundChannels), and Seal is their
// only reader anyway, so closure could never have released it. What releases it is
// the miner's `stop` channel, with the view deadline as the backstop. Here the
// backstop really is one window, because this round has no pre-delay to wait out
// (the header time is already in the past); a round that does have one can block
// for ~21s, which is the shutdown debt recorded as R12 and is not what this test
// measures. If either arm regressed, shutdown would hang in miner.worker.close's
// Wait -- so it is worth a test rather than a comment.
func TestSealExitsWhenTheMinerStops(t *testing.T) {
	const producerCount = 12

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

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})
	sealed := make(chan error, 1)
	go func() {
		sealed <- engine.Seal(chain, types.NewBlockWithHeader(header), results, stop)
	}()

	// Wait until the round is really in flight, so closing stop interrupts a live
	// wait rather than racing the setup.
	if proposal := waitForProposal(engine); proposal == nil {
		t.Fatal("the round never registered a proposal")
	}
	// ...and check that it is still blocked. Without this the timing assertion
	// below could be satisfied by a Seal that had already given up on its own for
	// an unrelated reason.
	select {
	case err := <-sealed:
		t.Fatalf("Seal returned (%v) before the miner stopped: the round was not actually in flight", err)
	default:
	}

	// The point of this test is the latency of the stop path, so it has to be
	// measured. A Seal that reached the view deadline instead would return the same
	// ErrRoundAborted roughly one view window later, and every other assertion here
	// would still pass -- which is exactly how this test used to be satisfiable
	// with the `case <-stop:` arm deleted.
	//
	// One second, not signTolerance/2: deleting the stop arm makes Seal wait out the
	// view deadline, which was measured at 2.5s-3.0s, i.e. inside a 3s bound. The
	// honest baseline is single-digit milliseconds (a channel close observed by one
	// select), so 1s is still ~100x headroom while being strictly below the mutant.
	start := time.Now()
	close(stop)

	select {
	case err := <-sealed:
		elapsed := time.Since(start)
		if !errors.Is(err, ErrRoundAborted) {
			t.Fatalf("Seal interrupted by the miner returned %v, want ErrRoundAborted", err)
		}
		if limit := time.Second; elapsed >= limit {
			t.Fatalf("Seal took %v to return after the miner closed stop (limit %v): it was released by the view deadline, not by stop -- shutdown would hang for a whole view window", elapsed, limit)
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("Seal did not return after the miner closed stop: shutdown would hang")
	}

	// The interrupted round must also leave the node usable, i.e. the stop path
	// resets the view like every other abort.
	retryHeader := &types.Header{
		ParentHash:  genesis.Hash(),
		UncleHash:   types.CalcUncleHash(nil),
		Coinbase:    common.HexToAddress("0x00000000000000000000000000000000000000aa"),
		Root:        common.HexToHash("0x01"),
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Number:      big.NewInt(1),
		GasLimit:    30_000_000,
	}
	releaseAfterFailedRound(engine)
	if err := engine.Prepare(chain, retryHeader); err != nil {
		t.Fatalf("Prepare after a stop-interrupted round: %v", err)
	}
}

// TestSingleProducerSealsInOneRound is the P0 regression test for the round
// handshake when the producer set is small enough that one vote is a majority.
//
// legacyMajorityCount rounds down (consensus/pgp/confirm.go), so a set of one
// needs zero votes and a set of two needs one. On both, the vote Seal casts for
// itself already reaches quorum, and the dispatcher builds the confirm and calls
// back into onConfirm synchronously, inside ProcessVote, on Seal's own goroutine.
// The round's confirm is therefore already queued before Seal reaches its select.
//
// An implementation that drains the round channels *after* the self-vote -- to
// clean up after the previous round -- throws that confirm away, times out, and
// then cannot retry the height either: the confirm is still in blockPool, so
// Seal's "this height already has a confirm" gate answers ErrAlreadyConfirmedBlock
// on every later attempt while dispatcher.finishedHeight never moved. Height 1 is
// then stuck for good. That is the devnet failure this test exists to catch, and
// it is invisible to a twelve-producer test: there the confirm arrives asynchronously,
// long after the drain.
//
// Three consecutive heights, because the state that makes the retry impossible is
// per height: a version that survives only height 1 would pass a one-height test.
//
// The parents are synthesised one block period and a second in the past instead of
// being the previously sealed header. Chaining them would put each block's
// timestamp a full period (3s) ahead of now, and Seal correctly waits out that slot
// before collecting votes -- three heights would take ~9s of real time and the
// timing assertion below would stop meaning anything. What this test is about is
// the round handshake, not timestamp arithmetic: the chained case is covered by
// TestSealProducesABlockTheReadOnlyVerifierAccepts, and the period rule itself by
// consensus/pgp's own tests.
func TestSingleProducerSealsInOneRound(t *testing.T) {
	for _, producerCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("producers=%d", producerCount), func(t *testing.T) {
			accounts, producerKeys := newProducerSet(t, producerCount)
			legacy := &params.LegacyConfig{VerifySeal: true, Producers: producerKeys}

			genesis := &types.Header{
				Number:     big.NewInt(0),
				Time:       uint64(time.Now().Add(-blockPeriod * time.Second).Unix()),
				Difficulty: new(big.Int).Set(diffInTurn),
				UncleHash:  types.CalcUncleHash(nil),
			}
			chain := &sealTestChain{genesis: genesis, byHash: make(map[common.Hash]*types.Header)}

			engine := New(legacy, &Config{Enable: true})
			engine.attachAccount(accounts[0])
			engine.MarkRecovered()
			engine.AttachChain(chain)
			defer engine.Close()

			// One vote is the whole threshold in both cases, so the confirm carries
			// one vote and not producerCount of them. 143 + 132 = 275 bytes, the
			// size the devnet produced once the drain was moved.
			const votesInConfirm = 1
			wantExtra := pgp.ConfirmMinLen + votesInConfirm*pgp.VoteWireLen
			if wantExtra != 275 {
				t.Fatalf("a one-vote confirm is %d bytes, the reference is 275", wantExtra)
			}

			newHeader := func(number uint64, parentHash common.Hash) *types.Header {
				return &types.Header{
					ParentHash:  parentHash,
					UncleHash:   types.CalcUncleHash(nil),
					Coinbase:    common.HexToAddress("0x00000000000000000000000000000000000000aa"),
					Root:        common.HexToHash("0x01"),
					TxHash:      types.EmptyTxsHash,
					ReceiptHash: types.EmptyReceiptsHash,
					Number:      new(big.Int).SetUint64(number),
					GasLimit:    30_000_000,
				}
			}

			results := make(chan *types.Block, 1)
			stop := make(chan struct{})
			defer close(stop)

			for height := uint64(1); height <= 3; height++ {
				// One period and a second old, so Prepare re-anchors the round on the
				// current time and Seal has no slot to wait for. The Root makes each
				// synthetic parent's hash distinct, as a real chain's would be.
				parent := &types.Header{
					Number:     new(big.Int).SetUint64(height - 1),
					Time:       uint64(time.Now().Unix() - blockPeriod - 1),
					Difficulty: new(big.Int).Set(diffInTurn),
					UncleHash:  types.CalcUncleHash(nil),
					Root:       common.BigToHash(new(big.Int).SetUint64(height)),
				}
				chain.byHash[parent.Hash()] = parent

				header := newHeader(height, parent.Hash())
				if err := engine.Prepare(chain, header); err != nil {
					t.Fatalf("height %d: Prepare refused the round: %v", height, err)
				}
				if !engine.IsOnduty() {
					t.Fatalf("height %d: the sealing key is not on duty, the schedule changed", height)
				}
				// The precondition for the timing assertion below: Seal must not have a
				// future timestamp to wait out, or "it returned fast" would just mean
				// "the slot had not arrived".
				if delay := time.Unix(int64(header.Time), 0).Sub(time.Now()); delay > 0 {
					t.Fatalf("height %d: the header is %v in the future, this test would be measuring a slot wait", height, delay)
				}

				start := time.Now()
				err := engine.Seal(chain, types.NewBlockWithHeader(header), results, stop)
				elapsed := time.Since(start)
				// Deliberately names no root cause. This branch has been reached by
				// several unrelated defects, and pinning it to whichever one was
				// current when it was written turns the message into a false lead:
				//
				//   - the drain sitting after the self-vote, which ate the round's
				//     own confirm (the n<=2 permanent stall, R17);
				//   - a stale isSealOver==1 left by the previous round, so onConfirm
				//     refused this round's confirm at seal.go's "seal already
				//     finished" check;
				//   - a confirm carrying another block's seal hash, which Seal drops
				//     as stale instead of attaching;
				//   - the proposal never being registered with the dispatcher, so no
				//     vote is ever counted (R16).
				//
				// The message says what is observed -- the round ended without a
				// confirm it could attach -- because guessing which of those it is
				// cost more time than reading the list.
				if err != nil {
					t.Fatalf("height %d: Seal failed after %v: %v (the round did not end with a confirm it could attach; the comment above lists what has caused this before)",
						height, elapsed.Round(time.Millisecond), err)
				}
				// The confirm is produced synchronously, so the round must finish in
				// single-digit milliseconds. One second is ~100x the baseline and well
				// under the ~3s an implementation that waits out the view deadline
				// spends; such an implementation also returns ErrRoundAborted, so this
				// bound is the second line of defence, not the only one.
				if elapsed >= time.Second {
					t.Fatalf("height %d: Seal took %v, so it did not finish on its own confirm", height, elapsed.Round(time.Millisecond))
				}

				var sealed *types.Block
				select {
				case sealed = <-results:
				case <-time.After(signTolerance):
					t.Fatalf("height %d: Seal returned nil but handed no block to the miner", height)
				}
				if got := len(sealed.Header().Extra); got != wantExtra {
					t.Fatalf("height %d: header.Extra is %d bytes, want %d for a one-vote confirm", height, got, wantExtra)
				}
				// The block still has to be a real one for the read-only verifier.
				if err := pgp.New(legacy).VerifyHeader(chain, sealed.Header()); err != nil {
					t.Fatalf("height %d: the read-only verifier rejected the block: %v", height, err)
				}
				if got := engine.GetIrreversibleHeight(); got != height {
					t.Fatalf("height %d: the engine considers height %d final after the round", height, got)
				}

				// The retry assertions, which is where the P0 shows up.
				//
				// The stuck state is a confirm in the pool with the watermark behind
				// it: Seal's blockPool gate then answers ErrAlreadyConfirmedBlock at
				// this height for the rest of the node's life while nothing is final.
				// BlockPool keeps the confirm for the height it just sealed
				// (CleanFinalConfirmedBlock only prunes past cachedCount), so the pool
				// entry alone says nothing; the assertion is on the pair.
				if _, hasConfirm := engine.blockPool.GetConfirmByHeight(height); hasConfirm {
					if got := engine.GetIrreversibleHeight(); got < height {
						t.Fatalf("height %d: the pool holds a confirm the dispatcher never registered as final (watermark %d): every later Seal at this height answers ErrAlreadyConfirmedBlock while nothing advances",
							height, got)
					}
				}
				retry := newHeader(height, parent.Hash())
				if err := engine.Prepare(chain, retry); !errors.Is(err, ErrAlreadyConfirmedBlock) {
					t.Fatalf("height %d: Prepare at an already sealed height returned %v, want ErrAlreadyConfirmedBlock", height, err)
				}
				if err := engine.Seal(chain, types.NewBlockWithHeader(retry), results, stop); !errors.Is(err, ErrAlreadyConfirmedBlock) {
					t.Fatalf("height %d: Seal at an already sealed height returned %v, want ErrAlreadyConfirmedBlock", height, err)
				}
			}
		})
	}
}

// releaseAfterFailedRound plays the part the view loop plays after a round ends
// without a block: the rotation fires OnViewChanged, and that callback is what lets
// the producer the new view points at prepare again.
//
// The round itself no longer resets the view. It used to, through
// Dispatcher.ResetConsensus, which zeroes the view offset on one node -- and a node
// that resets alone is a node whose proposals every peer drops as stale. See
// Engine.resetRound for the failure that produced. So "an aborted round leaves the
// node usable" now has two steps, and the second one is this.
func releaseAfterFailedRound(engine *Engine) {
	engine.SetIsCurrent(func() bool { return true })
	engine.OnViewChanged(true, false)
}

// reAnchorToNow moves the mock chain head to a current timestamp.
//
// Both tests below fail a round and then expect THIS account to produce the next
// one, and that expectation is no longer free. The view offset is derived from the
// anchor (dpos.ConsensusView.currentOffsetLocked), Prepare anchors on parent.Time,
// and a head that is nine seconds old is therefore -- by the grid's own arithmetic
// -- one whole view late, so the rotation has legitimately moved to the next
// producer. Put another way, the offset now advances with wall-clock time instead
// of with each ChangeView, which is the whole repair: it is why two nodes that were
// Running for different lengths of time finally agree on the view number.
//
// On the live chain this account gets the slot back in one of three ways: the
// rotation wraps around after producers*signTolerance, the view is re-anchored by a
// block import, or the head advances. These tests take the block-import shape and
// make the parent current, which is what a landed block does to the grid.
func reAnchorToNow(chain *sealTestChain) {
	chain.genesis.Time = uint64(time.Now().Unix())
}
