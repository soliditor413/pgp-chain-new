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

// Seal has two waits, not one. The second one -- the view deadline -- is covered
// by TestSealExitsWhenTheMinerStops and TestFailedRoundDoesNotStallTheNode. The
// first one is the pre-delay: Seal sleeps until the block's own timestamp before
// collecting votes, so a block is never born in the future. That wait is bounded
// by nothing but the timestamp gap, and on a parent that is already ahead of the
// producer's clock it is the longest wait in this engine (up to
// allowedFutureBlockTime + blockPeriod = 18s). See R12 in
// docs/design/pbft-port-design.md.
//
// Both waits carry `case <-stop`. A test that only ever exercises the second one
// cannot see the first one lose its stop arm, which is why this file exists
// separately from the tests in seal_test.go.
//
// This is a contract test, not a shutdown-latency test. Today the miner cannot
// actually deliver stop to an in-flight Seal -- worker.close closes it from the
// same task loop that is blocked inside the call -- and that is the debt R12
// records. The contract still has to hold, for two reasons:
//
//   - the fix for that debt is to close stop from somewhere other than the task
//     loop; the engine side has to be ready for it, otherwise the fix has to
//     touch this function as well;
//   - losing the arm is not only slower, it is wrong: with the arm gone Seal
//     sleeps through the slot and then seals the block anyway, returning nil
//     instead of ErrRoundAborted even though its caller asked it to stop
//     (measured). On the parent-ahead case this arm exists for -- up to
//     allowedFutureBlockTime of lead -- header.Time lands up to 18s ahead, past
//     the 15s window consensus/pgp allows, so the block that gets sealed is one
//     ErrFutureBlock rejects on first sight.

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// TestSealHonoursStopDuringPreDelay pins the interruptibility of the pre-delay.
//
// The window is made explicit rather than incidental: the header time is pushed
// past the producer's own clock, so Seal has to sleep before it may look for
// votes, and the test then measures how long it takes to return after the miner
// closes stop.
//
// Why the header time is moved instead of the parent's: Seal derives the wait
// from header.Time alone (seal.go, `delay := time.Unix(header.Time)...`), and
// Prepare derives header.Time from parent.Time (`parent.Time + e.period`). A
// parent stamped 15s in the future and a header stamped 18s in the future are
// therefore the same input to the code under test -- but a future-stamped parent
// is exactly what a "never start a round on a future parent" gate at Prepare
// would refuse, and that gate is under discussion upstream. Writing the test
// against header.Time keeps it valid either way: it exercises Seal's own wait,
// not Prepare's admission policy.
//
// The bound is 1s on purpose. Deleting the stop arm does not make Seal hang
// forever -- it falls through to the sleep and returns once the slot arrives --
// so the assertion has to be on latency, and the reference implementation
// returns in single-digit milliseconds. The 4s slot keeps the mutant's lower
// bound at >=2s even after the remaining-wait precondition below, i.e. strictly
// above the 1s limit in both directions. Measured: reference 0.01s, mutant 3.2s.
func TestSealHonoursStopDuringPreDelay(t *testing.T) {
	const (
		// How far past the producer's clock the block's timestamp is put.
		slotWait = 4 * time.Second
		// The limit on how long Seal may take to return after stop is closed.
		limit = time.Second
	)

	accounts, producerKeys := newProducerSet(t, 1)
	legacy := &params.LegacyConfig{VerifySeal: true, Producers: producerKeys}

	// A parent two periods old: Prepare then re-anchors the round on "now", so
	// the header it hands back carries a past timestamp and the view deadline is
	// anchored sanely. The slot this test is about is introduced below, on the
	// header, so that setup and the behaviour under test do not share a knob.
	genesis := &types.Header{
		Number:     big.NewInt(0),
		Time:       uint64(time.Now().Add(-2 * blockPeriod * time.Second).Unix()),
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
	if !engine.IsOnduty() {
		t.Fatalf("the sealing key is not on duty; the producer schedule changed")
	}

	// Seal measures the wait against the dispatcher's clock, not time.Now, so the
	// timestamp is derived from the same clock. Unix() truncates towards the past,
	// which can only make the wait shorter by less than a second; the
	// remaining-wait precondition below absorbs that.
	header.Time = uint64(engine.dispatcher.GetNowTime().Add(slotWait).Unix())

	results := make(chan *types.Block, 1)
	stop := make(chan struct{})

	sealed := make(chan error, 1)
	go func() {
		sealed <- engine.Seal(chain, types.NewBlockWithHeader(header), results, stop)
	}()

	// The proposal is registered before the pre-delay, so this is evidence that
	// Seal is inside the wait -- not merely that it has been called. Without it
	// the timing assertion below could be satisfied by a Seal that was still in
	// its set-up.
	if proposal := waitForProposal(engine); proposal == nil {
		// Two different regressions hide behind "no proposal", and they need
		// different messages: a Seal that skipped the wait returns nil almost
		// immediately, and attaching the confirm clears the processing proposal
		// on its way out, so the poll above can miss it entirely.
		select {
		case err := <-sealed:
			t.Fatalf("Seal returned (%v) before the block's timestamp and cleared the round: the pre-delay was skipped, so this test would be measuring the wrong wait", err)
		default:
			t.Fatal("the round never registered a proposal")
		}
	}
	// ...and it must still be there. A Seal that skipped the pre-delay would have
	// consumed the confirm the one-producer set produces synchronously and
	// returned nil by now.
	select {
	case err := <-sealed:
		t.Fatalf("Seal returned (%v) before the block's timestamp: the pre-delay was skipped, so this test would be measuring the wrong wait", err)
	default:
	}

	// The precondition that makes the 1s limit discriminating: whatever is left
	// of the slot is what an implementation without the stop arm has to sleep
	// through, and it must be comfortably above the limit.
	remaining := time.Unix(int64(header.Time), 0).Sub(engine.dispatcher.GetNowTime())
	if remaining < 2*limit {
		t.Fatalf("only %v of the slot is left; the limit of %v would not distinguish an interrupted wait from a slept-through one", remaining, limit)
	}

	start := time.Now()
	close(stop)

	select {
	case err := <-sealed:
		elapsed := time.Since(start)
		// Latency first: it is the claim this test exists for, and an
		// implementation that sleeps through the slot also tends to return
		// something other than ErrRoundAborted (the confirm was already queued),
		// which would otherwise be reported as the lesser finding.
		if elapsed >= limit {
			t.Fatalf("Seal took %v to return after the miner closed stop, with %v of the slot still to run (limit %v): the pre-delay was slept through instead of interrupted, so shutdown waits out the whole timestamp gap (it returned %v)", elapsed, remaining, limit, err)
		}
		if !errors.Is(err, ErrRoundAborted) {
			t.Fatalf("Seal interrupted during the pre-delay returned %v, want ErrRoundAborted", err)
		}
		if len(results) != 0 {
			t.Fatal("Seal handed a block to the miner despite being interrupted: a future-timestamped block was sealed")
		}
	case <-time.After(signTolerance + 5*time.Second):
		t.Fatal("Seal did not return after the miner closed stop during the pre-delay: shutdown hangs")
	}

	// An interrupted round must leave the node usable, like every other abort.
	retry := &types.Header{
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
	if err := engine.Prepare(chain, retry); err != nil {
		t.Fatalf("Prepare after a pre-delay-interrupted round: %v", err)
	}
}
