// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package router

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// fakeEngine records which calls reached it so the tests can assert the routing.
type fakeEngine struct {
	name     string
	verify   int
	prepare  int
	seal     int
	systemTx int
	// isSystemTx is what IsSystemTransaction reports, so a test can tell the two
	// engines apart instead of seeing "false" from both.
	isSystemTx bool
	// gasReserved is what EstimateGasReservedForSystemTxs reports, for the same
	// reason: 0 from both engines would not prove the routing.
	gasReserved uint64
	gasCalls    int

	// Proposer-side surface. Every method reports a distinct value and counts
	// its calls, so a test can tell which engine answered and how many times.
	// numbers records the header number each call was handed, which is also how
	// the "routes on height+1" tests observe the difference between the block a
	// call is about and the header it carries.
	signRecently    bool
	signErr         error
	signCalls       int
	lastBlockInTurn bool
	turnCalls       int
	blockInterval   uint64
	intervalErr     error
	intervalCalls   int
	proposalStart   uint64
	proposalEnd     uint64
	proposalErr     error
	proposalCalls   int
	numbers         []uint64
}

func (f *fakeEngine) Author(*types.Header) (common.Address, error) { return common.Address{}, nil }
func (f *fakeEngine) VerifyHeader(consensus.ChainHeaderReader, *types.Header) error {
	f.verify++
	return nil
}
func (f *fakeEngine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	for range headers {
		f.verify++
		results <- nil
	}
	return abort, results
}
func (f *fakeEngine) VerifyUncles(consensus.ChainReader, *types.Block) error { return nil }
func (f *fakeEngine) VerifyRequests(*types.Header, [][]byte) error           { return nil }
func (f *fakeEngine) NextInTurnValidator(consensus.ChainHeaderReader, *types.Header) (common.Address, error) {
	return common.Address{}, nil
}
func (f *fakeEngine) Prepare(consensus.ChainHeaderReader, *types.Header) error {
	f.prepare++
	return nil
}
func (f *fakeEngine) Finalize(consensus.ChainHeaderReader, *types.Header, vm.StateDB, *[]*types.Transaction,
	[]*types.Header, []*types.Withdrawal, *[]*types.Receipt, *[]*types.Transaction, *uint64, *tracing.Hooks) error {
	return nil
}
func (f *fakeEngine) Seal(consensus.ChainHeaderReader, *types.Block, chan<- *types.Block, <-chan struct{}) error {
	f.seal++
	return nil
}
func (f *fakeEngine) SealHash(h *types.Header) common.Hash { return common.Hash{} }
func (f *fakeEngine) CalcDifficulty(consensus.ChainHeaderReader, uint64, *types.Header) *big.Int {
	return big.NewInt(0)
}
func (f *fakeEngine) Delay(consensus.ChainReader, *types.Header, *time.Duration) *time.Duration {
	return nil
}
func (f *fakeEngine) Close() error { return nil }

// The router only needs consensus.Engine, but it stores a consensus.PoSA for the
// forked era, so the fake has to satisfy PoSA as well.
func (f *fakeEngine) IsSystemTransaction(*types.Transaction, *types.Header) (bool, error) {
	f.systemTx++
	return f.isSystemTx, nil
}
func (f *fakeEngine) EstimateGasReservedForSystemTxs(consensus.ChainHeaderReader, *types.Header) uint64 {
	f.gasCalls++
	return f.gasReserved
}
func (f *fakeEngine) IsSystemContract(*common.Address) bool                    { return false }
func (f *fakeEngine) EnoughDistance(consensus.ChainReader, *types.Header) bool { return true }
func (f *fakeEngine) IsLocalBlock(*types.Header) bool                          { return false }
func (f *fakeEngine) GetJustifiedNumberAndHash(consensus.ChainHeaderReader, []*types.Header) (uint64, common.Hash, error) {
	return 0, common.Hash{}, nil
}
func (f *fakeEngine) GetFinalizedHeader(consensus.ChainHeaderReader, *types.Header) *types.Header {
	return nil
}
func (f *fakeEngine) CheckFinalityAndNotify(consensus.ChainHeaderReader, common.Hash, func(*types.Header)) {
}
func (f *fakeEngine) VerifyVote(consensus.ChainHeaderReader, *types.VoteEnvelope) error { return nil }
func (f *fakeEngine) IsActiveValidatorAt(consensus.ChainHeaderReader, *types.Header, func(*types.BLSPublicKey) bool) bool {
	return false
}

// parliaProposer: the four methods the router forwards to the PoSA engine for
// miner.worker. They are deliberately not implemented on a separate type - the
// router asserts a type switch on e.posa, and the fake has to pass it.
func (f *fakeEngine) SignRecently(_ consensus.ChainReader, parent *types.Header) (bool, error) {
	f.signCalls++
	f.numbers = append(f.numbers, parent.Number.Uint64())
	return f.signRecently, f.signErr
}
func (f *fakeEngine) IsLastBlockInTurn(_ consensus.ChainReader, header *types.Header) bool {
	f.turnCalls++
	f.numbers = append(f.numbers, header.Number.Uint64())
	return f.lastBlockInTurn
}
func (f *fakeEngine) BlockInterval(_ consensus.ChainHeaderReader, header *types.Header) (uint64, error) {
	f.intervalCalls++
	f.numbers = append(f.numbers, header.Number.Uint64())
	return f.blockInterval, f.intervalErr
}
func (f *fakeEngine) NextProposalBlock(_ consensus.ChainHeaderReader, header *types.Header, _ common.Address) (uint64, uint64, error) {
	f.proposalCalls++
	f.numbers = append(f.numbers, header.Number.Uint64())
	return f.proposalStart, f.proposalEnd, f.proposalErr
}

// noProposer is a PoSA engine that lacks the three parliaProposer methods, so
// the router has to fall back on SignRecently/IsLastBlockInTurn/BlockInterval.
// The embedded interface is never invoked; NextProposalBlock is overridden
// because consensus.PoSA already declares it and the router calls it directly.
type noProposer struct {
	consensus.PoSA
	proposalCalls int
}

func (n *noProposer) NextProposalBlock(consensus.ChainHeaderReader, *types.Header, common.Address) (uint64, uint64, error) {
	n.proposalCalls++
	return 11, 22, nil
}

func headerAt(number uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number)}
}

func blockAt(number uint64) *types.Block {
	return types.NewBlockWithHeader(headerAt(number))
}

// TestSealDelegatesToLegacyBeforeFork pins the post-port behaviour: a pre-fork
// Seal is a delegation, not a refusal. Refusing here is what kept the chain from
// ever producing block 1, and the reason it was safe to refuse (the legacy engine
// being read-only) no longer holds now that consensus/pbft produces.
func TestSealDelegatesToLegacyBeforeFork(t *testing.T) {
	legacy := &fakeEngine{name: "legacy"}
	posa := &fakeEngine{name: "posa"}
	r := New(legacy, posa, 10)

	results := make(chan *types.Block, 1)
	if err := r.Seal(nil, blockAt(9), results, nil); err != nil {
		t.Fatalf("pre-fork Seal must delegate, got %v", err)
	}
	if legacy.seal != 1 || posa.seal != 0 {
		t.Fatalf("pre-fork Seal did not reach the legacy engine: legacy=%d posa=%d", legacy.seal, posa.seal)
	}

	if err := r.Seal(nil, blockAt(10), results, nil); err != nil {
		t.Fatalf("forked Seal must delegate: %v", err)
	}
	if posa.seal != 1 || legacy.seal != 1 {
		t.Fatalf("forked Seal did not reach the posa engine: legacy=%d posa=%d", legacy.seal, posa.seal)
	}
}

// TestSealForkedWithoutPoSA checks the forked era still fails loudly.
func TestSealForkedWithoutPoSA(t *testing.T) {
	r := New(&fakeEngine{}, nil, 10)
	if err := r.Seal(nil, blockAt(10), make(chan *types.Block, 1), nil); err == nil {
		t.Fatal("expected an error for a forked Seal without a posa engine")
	}
}

// TestDelayNeverReturnsNil guards the miner against a nil *time.Duration: it
// dereferences the value at miner/worker.go:1548-1550 without a check, so a nil
// answer panics on the next transaction arrival. Delay's own comment carries the
// full chain.
func TestDelayNeverReturnsNil(t *testing.T) {
	r := New(&fakeEngine{}, nil, 10)

	if d := r.Delay(nil, headerAt(9), nil); d == nil {
		t.Fatal("Delay returned nil for a pre-fork header")
	}
	if d := r.Delay(nil, headerAt(10), nil); d == nil {
		t.Fatal("Delay returned nil for a header whose engine is missing")
	}
	if d := r.Delay(nil, nil, nil); d == nil {
		t.Fatal("Delay returned nil for a nil header")
	}
}

// TestRoutingByHeight checks that the fork height alone decides which engine runs.
func TestRoutingByHeight(t *testing.T) {
	legacy := &fakeEngine{name: "legacy"}
	posa := &fakeEngine{name: "posa"}
	r := New(legacy, posa, 10)

	for _, number := range []uint64{0, 1, 9} {
		if err := r.VerifyHeader(nil, headerAt(number)); err != nil {
			t.Fatalf("block %d: %v", number, err)
		}
		if err := r.Prepare(nil, headerAt(number)); err != nil {
			t.Fatalf("block %d: %v", number, err)
		}
	}
	if legacy.verify != 3 || legacy.prepare != 3 {
		t.Fatalf("pre-fork blocks did not reach the legacy engine: %+v", legacy)
	}
	if posa.verify != 0 || posa.prepare != 0 {
		t.Fatalf("pre-fork blocks leaked into the posa engine: %+v", posa)
	}

	if err := r.VerifyHeader(nil, headerAt(10)); err != nil {
		t.Fatalf("fork block: %v", err)
	}
	if err := r.VerifyHeader(nil, headerAt(11)); err != nil {
		t.Fatalf("post fork block: %v", err)
	}
	if posa.verify != 2 {
		t.Fatalf("post-fork blocks did not reach the posa engine: %+v", posa)
	}
	if legacy.verify != 3 {
		t.Fatalf("post-fork blocks leaked into the legacy engine: %+v", legacy)
	}
}

// TestForkedWithoutPoSA checks that a forked height fails loudly instead of
// panicking when no Parlia engine was configured.
func TestForkedWithoutPoSA(t *testing.T) {
	r := New(&fakeEngine{}, nil, 10)

	err := r.VerifyHeader(nil, headerAt(10))
	if err == nil {
		t.Fatal("expected an error for a forked block without a posa engine")
	}
	if err := r.VerifyHeader(nil, headerAt(9)); err != nil {
		t.Fatalf("pre-fork block must still work: %v", err)
	}
}

// TestMaxUint64ForkHeightIsNeverForked pins the default PGPChainConfig value:
// math.MaxUint64 means "the fork is not scheduled", so everything is legacy.
func TestMaxUint64ForkHeightIsNeverForked(t *testing.T) {
	legacy := &fakeEngine{}
	posa := &fakeEngine{}
	r := New(legacy, posa, ^uint64(0))

	for _, number := range []uint64{0, 1, 1_000_000_000_000} {
		if err := r.VerifyHeader(nil, headerAt(number)); err != nil {
			t.Fatalf("block %d: %v", number, err)
		}
	}
	if legacy.verify != 3 || posa.verify != 0 {
		t.Fatalf("unexpected routing: legacy=%d posa=%d", legacy.verify, posa.verify)
	}
}

// TestVerifyHeadersMixedBatch checks that a batch straddling the fork is split
// per header instead of being delegated to one engine.
func TestVerifyHeadersMixedBatch(t *testing.T) {
	legacy := &fakeEngine{}
	posa := &fakeEngine{}
	r := New(legacy, posa, 10)

	headers := []*types.Header{headerAt(8), headerAt(9), headerAt(10), headerAt(11)}
	abort, results := r.VerifyHeaders(nil, headers)
	for range headers {
		if err := <-results; err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	close(abort)

	if legacy.verify != 2 || posa.verify != 2 {
		t.Fatalf("mixed batch was not split: legacy=%d posa=%d", legacy.verify, posa.verify)
	}
}

// TestNewRequiresLegacyEngine guards against a silent misconfiguration.
func TestNewRequiresLegacyEngine(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New must panic when the legacy engine is nil")
		}
	}()
	New(nil, nil, 0)
}

// TestRouterSatisfiesSystemTxDetector pins the interface contract that block
// import depends on.
//
// This is the regression test for the bug that made a follower node unable to
// import any block carrying system transactions. core.Process used to assert
// consensus.PoSA; the router is not one, the assertion failed, system
// transactions were applied as ordinary ones, and the first of them (gas =
// math.MaxUint64/2) drained the block gas pool:
//
//	could not apply tx 0 [...]: gas limit reached
//
// A sealer never hits this because it assembles through Finalize and writes the
// block without re-executing. Only a second node -- or a restart that reprocesses
// a block -- sees it.
func TestRouterSatisfiesSystemTxDetector(t *testing.T) {
	posa := &fakeEngine{name: "posa"}
	var engine consensus.Engine = New(&fakeEngine{name: "legacy"}, posa, 10)

	if _, ok := engine.(systemTxDetector); !ok {
		t.Fatal("router must satisfy systemTxDetector, or core.Process classifies " +
			"system transactions as user transactions and every import fails")
	}
	// The other half of the contract, and the reason the detector is a private
	// one-method interface instead of consensus.PoSA: implementing PoSA would
	// switch on vote pool, payment lane and finality for the legacy era.
	if _, ok := engine.(consensus.PoSA); ok {
		t.Fatal("router must NOT satisfy consensus.PoSA; that turns on PoSA-only " +
			"features for pre-fork blocks")
	}

	// But it must hand its PoSA half to anything that asks properly. Every site
	// that asserted engine.(consensus.PoSA) directly - the vote pool, the vote
	// manager, justified/finalized numbers, fast-finality reorg protection -
	// guards on that assertion and degrades silently when it fails, which on a
	// forked chain is all of them (F-21).
	got, ok := consensus.PoSAOf(engine)
	if !ok {
		t.Fatal("consensus.PoSAOf must unwrap the router to its PoSA engine")
	}
	if got != consensus.PoSA(posa) {
		t.Fatalf("consensus.PoSAOf returned %v, want the posa engine", got)
	}

	// A legacy-only router has no PoSA half, and asking must say so rather than
	// panic or return a zero interface.
	legacyOnly := New(&fakeEngine{name: "legacy"}, nil, 10)
	if p, ok := consensus.PoSAOf(legacyOnly); ok || p != nil {
		t.Fatalf("consensus.PoSAOf on a legacy-only router = (%v, %v), want (nil, false)", p, ok)
	}
}

// TestIsSystemTransactionRoutesByHeight checks that the detector follows the
// same height rule as everything else in the router.
func TestIsSystemTransactionRoutesByHeight(t *testing.T) {
	legacy := &fakeEngine{name: "legacy", isSystemTx: false}
	posa := &fakeEngine{name: "posa", isSystemTx: true}
	r := New(legacy, posa, 10)

	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)

	if got, err := r.IsSystemTransaction(tx, headerAt(10)); err != nil || !got {
		t.Fatalf("forked block: got (%v, %v), want (true, nil)", got, err)
	}
	if posa.systemTx != 1 {
		t.Fatalf("forked block did not reach the posa engine (calls=%d)", posa.systemTx)
	}
	if legacy.systemTx != 0 {
		t.Fatalf("forked block reached the legacy engine (calls=%d)", legacy.systemTx)
	}

	if got, err := r.IsSystemTransaction(tx, headerAt(9)); err != nil || got {
		t.Fatalf("pre-fork block: got (%v, %v), want (false, nil)", got, err)
	}
	if legacy.systemTx != 1 {
		t.Fatalf("pre-fork block did not reach the legacy engine (calls=%d)", legacy.systemTx)
	}
}

// TestIsSystemTransactionHonoursLegacyDetector checks the fallback: a legacy
// engine that does understand system transactions keeps working.
func TestIsSystemTransactionHonoursLegacyDetector(t *testing.T) {
	legacy := &fakeEngine{name: "legacy", isSystemTx: true}
	r := New(legacy, &fakeEngine{name: "posa"}, 10)

	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)
	got, err := r.IsSystemTransaction(tx, headerAt(1))
	if err != nil || !got {
		t.Fatalf("got (%v, %v), want (true, nil): the legacy engine's verdict must be passed through", got, err)
	}
}

// TestIsSystemTransactionErrors covers the two malformed calls.
func TestIsSystemTransactionErrors(t *testing.T) {
	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)

	// No header at all.
	if _, err := New(&fakeEngine{}, &fakeEngine{}, 10).IsSystemTransaction(tx, nil); err == nil {
		t.Fatal("expected an error for a nil header")
	}
	// Forked height but no PoSA engine wired in: must be an explicit error, not
	// a silent "false", because false means "treat as a user transaction".
	if _, err := New(&fakeEngine{}, nil, 10).IsSystemTransaction(tx, headerAt(10)); !errors.Is(err, errNoPoSA) {
		t.Fatalf("got %v, want errNoPoSA", err)
	}
}

// posaOnly exposes nothing but consensus.PoSA, hiding any extra method the
// wrapped engine has. It stands in for a PoSA implementation that does not
// reserve system-tx gas, which is what the router has to degrade to rather than
// panic on.
type posaOnly struct{ consensus.PoSA }

// TestRouterSatisfiesSystemTxGasReserver is the structural half of the F-1 fix.
// worker.makeEnv asserts this interface; if the router stops providing it the
// assertion fails silently and gasReserved goes back to 0 on every block of a
// fork chain, which is the bug this whole section exists for.
func TestRouterSatisfiesSystemTxGasReserver(t *testing.T) {
	var engine consensus.Engine = New(&fakeEngine{}, &fakeEngine{}, 10)
	if _, ok := engine.(systemTxGasReserver); !ok {
		t.Fatal("router must satisfy systemTxGasReserver, or worker.makeEnv reserves " +
			"no gas for system transactions on a chain with a consensus fork")
	}
}

// TestEstimateGasReservedForSystemTxsRoutesByHeight pins that the reservation
// follows the same height rule as every other router method, and that the two
// engines are distinguishable - a test where both answer 0 would pass even if
// the routing were broken.
func TestEstimateGasReservedForSystemTxsRoutesByHeight(t *testing.T) {
	legacy := &fakeEngine{name: "legacy", gasReserved: 1_000}
	posa := &fakeEngine{name: "posa", gasReserved: 7_000_000}
	r := New(legacy, posa, 10)

	if got, want := r.EstimateGasReservedForSystemTxs(nil, headerAt(10)), uint64(7_000_000); got != want {
		t.Fatalf("forked block: got %d, want %d", got, want)
	}
	if posa.gasCalls != 1 || legacy.gasCalls != 0 {
		t.Fatalf("forked block reached the wrong engine (posa=%d legacy=%d)", posa.gasCalls, legacy.gasCalls)
	}
	if got, want := r.EstimateGasReservedForSystemTxs(nil, headerAt(9)), uint64(1_000); got != want {
		t.Fatalf("pre-fork block: got %d, want %d", got, want)
	}
	if legacy.gasCalls != 1 || posa.gasCalls != 1 {
		t.Fatalf("pre-fork block reached the wrong engine (posa=%d legacy=%d)", posa.gasCalls, legacy.gasCalls)
	}
}

// TestEstimateGasReservedForSystemTxsNoFork covers the shipped default: with
// forkHeight 0 nothing is ever forked, so every height is the legacy engine's.
func TestEstimateGasReservedForSystemTxsNoFork(t *testing.T) {
	legacy := &fakeEngine{gasReserved: 42}
	posa := &fakeEngine{gasReserved: 999}
	r := New(legacy, posa, 0)

	for _, n := range []uint64{0, 1, 10, 1 << 40} {
		if got := r.EstimateGasReservedForSystemTxs(nil, headerAt(n)); got != 42 {
			t.Fatalf("height %d: got %d, want 42 (legacy, no fork scheduled)", n, got)
		}
	}
	if posa.gasCalls != 0 {
		t.Fatalf("a chain without a fork consulted the posa engine (%d calls)", posa.gasCalls)
	}
}

// TestEstimateGasReservedForSystemTxsDegradesToZero covers a PoSA engine that
// does not implement the method: the answer is 0, the same answer the old
// concrete assertion produced, instead of a panic.
func TestEstimateGasReservedForSystemTxsDegradesToZero(t *testing.T) {
	r := New(&fakeEngine{gasReserved: 1_000}, posaOnly{&fakeEngine{gasReserved: 5_000}}, 10)

	if got := r.EstimateGasReservedForSystemTxs(nil, headerAt(10)); got != 0 {
		t.Fatalf("got %d, want 0: a PoSA engine without the method must not reserve", got)
	}
	// Pre-fork still consults legacy, which does implement it.
	if got := r.EstimateGasReservedForSystemTxs(nil, headerAt(9)); got != 1_000 {
		t.Fatalf("pre-fork: got %d, want 1000", got)
	}
}

// TestEstimateGasReservedForSystemTxsEdgeCases covers nil engines and a header
// without a number.
func TestEstimateGasReservedForSystemTxsEdgeCases(t *testing.T) {
	r := New(&fakeEngine{gasReserved: 7}, nil, 10)

	if got := r.EstimateGasReservedForSystemTxs(nil, headerAt(10)); got != 0 {
		t.Fatalf("forked height without a posa engine: got %d, want 0", got)
	}
	if got := r.EstimateGasReservedForSystemTxs(nil, nil); got != 0 {
		t.Fatalf("nil header: got %d, want 0", got)
	}
	// A header without a number is treated as pre-fork by isForked, so the
	// legacy engine answers. Anything else would misclassify a malformed header
	// as Parlia's and reserve gas the legacy chain never spends.
	if got := r.EstimateGasReservedForSystemTxs(nil, &types.Header{}); got != 7 {
		t.Fatalf("header without a number: got %d, want 7 (legacy)", got)
	}
}

// TestRouterSatisfiesParliaProposer is the structural half of the proposer
// forwarders. worker holds the router as its consensus.Engine and asserts
// parliaWorkerEngine on it; if any of these three methods disappears the
// assertion fails, and the worker silently stops asking whether it may propose.
func TestRouterSatisfiesParliaProposer(t *testing.T) {
	var engine consensus.Engine = New(&fakeEngine{}, &fakeEngine{}, 10)
	if _, ok := engine.(parliaProposer); !ok {
		t.Fatal("router must satisfy parliaProposer, or miner.worker stops consulting " +
			"Parlia for SignRecently/IsLastBlockInTurn/BlockInterval")
	}
}

// TestSignRecentlyRoutesOnPendingHeight pins the +1. SignRecently is asked about
// the parent, but the question it answers is about the block that would be built
// on top of it, so a parent one below the fork belongs to Parlia. Routing on the
// parent instead would let every validator build the fork block.
func TestSignRecentlyRoutesOnPendingHeight(t *testing.T) {
	legacy := &fakeEngine{}
	posa := &fakeEngine{signRecently: true}
	r := New(legacy, posa, 10)

	got, err := r.SignRecently(nil, headerAt(9))
	if err != nil || !got {
		t.Fatalf("parent 9 (pending block 10): got (%v, %v), want (true, nil)", got, err)
	}
	if posa.signCalls != 1 {
		t.Fatalf("pending block 10 did not reach the posa engine (calls=%d)", posa.signCalls)
	}
	if got := posa.numbers[0]; got != 9 {
		t.Fatalf("posa engine was handed header %d, want 9: the router must forward the parent untouched", got)
	}

	// Parent 8 means pending block 9, which is still legacy. PBFT does not
	// rotate producers, so the answer is "go ahead and build".
	got, err = r.SignRecently(nil, headerAt(8))
	if err != nil || got {
		t.Fatalf("parent 8 (pending block 9): got (%v, %v), want (false, nil)", got, err)
	}
	if posa.signCalls != 1 {
		t.Fatalf("pre-fork parent reached the posa engine (calls=%d)", posa.signCalls)
	}
}

// TestSignRecentlyPropagatesError checks the error travels with the answer, since
// the worker treats a non-nil error as "skip this round" rather than "propose".
func TestSignRecentlyPropagatesError(t *testing.T) {
	boom := errors.New("snapshot unavailable")
	posa := &fakeEngine{signRecently: true, signErr: boom}
	r := New(&fakeEngine{}, posa, 10)

	got, err := r.SignRecently(nil, headerAt(10))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the posa engine's error", err)
	}
	if !got {
		t.Fatal("the boolean must be passed through together with the error")
	}
}

// TestIsLastBlockInTurnRoutesOnOwnHeight is the counterpart of the +1 above:
// this one describes the header it is given, so no shift.
func TestIsLastBlockInTurnRoutesOnOwnHeight(t *testing.T) {
	posa := &fakeEngine{lastBlockInTurn: true}
	r := New(&fakeEngine{}, posa, 10)

	if !r.IsLastBlockInTurn(nil, headerAt(10)) {
		t.Fatal("forked header must be answered by the posa engine")
	}
	if posa.turnCalls != 1 {
		t.Fatalf("posa engine calls = %d, want 1", posa.turnCalls)
	}
	if r.IsLastBlockInTurn(nil, headerAt(9)) {
		t.Fatal("pre-fork header must answer false")
	}
	if posa.turnCalls != 1 {
		t.Fatalf("pre-fork header reached the posa engine (calls=%d)", posa.turnCalls)
	}
	// A header without a number cannot be placed, so it must not be treated as
	// Parlia's.
	if r.IsLastBlockInTurn(nil, &types.Header{}) {
		t.Fatal("header without a number must answer false")
	}
	if r.IsLastBlockInTurn(nil, nil) {
		t.Fatal("nil header must answer false")
	}
}

// TestBlockIntervalRoutesOnOwnHeight covers the value and the pre-fork error.
func TestBlockIntervalRoutesOnOwnHeight(t *testing.T) {
	posa := &fakeEngine{blockInterval: 3000}
	r := New(&fakeEngine{}, posa, 10)

	if got, err := r.BlockInterval(nil, headerAt(10)); err != nil || got != 3000 {
		t.Fatalf("forked header: got (%d, %v), want (3000, nil)", got, err)
	}
	if _, err := r.BlockInterval(nil, headerAt(9)); !errors.Is(err, errNoPoSA) {
		t.Fatalf("pre-fork header: got %v, want errNoPoSA", err)
	}
	if posa.intervalCalls != 1 {
		t.Fatalf("posa engine calls = %d, want 1", posa.intervalCalls)
	}
	if _, err := r.BlockInterval(nil, nil); !errors.Is(err, errNoPoSA) {
		t.Fatalf("nil header: got %v, want errNoPoSA", err)
	}
	if _, err := r.BlockInterval(nil, &types.Header{}); !errors.Is(err, errNoPoSA) {
		t.Fatalf("header without a number: got %v, want errNoPoSA", err)
	}
}

// TestNextProposalBlockRoutesOnPendingHeight is the +1 case again: the worker
// asks this while shutting down, to decide whether its next proposal window is
// close enough to wait for. At the fork the current head is ForkHeight-1 and the
// window belongs to Parlia.
func TestNextProposalBlockRoutesOnPendingHeight(t *testing.T) {
	posa := &fakeEngine{proposalStart: 11, proposalEnd: 22}
	r := New(&fakeEngine{}, posa, 10)

	start, end, err := r.NextProposalBlock(nil, headerAt(9), common.Address{})
	if err != nil || start != 11 || end != 22 {
		t.Fatalf("head 9 (pending block 10): got (%d, %d, %v), want (11, 22, nil)", start, end, err)
	}
	if posa.proposalCalls != 1 {
		t.Fatalf("posa engine calls = %d, want 1", posa.proposalCalls)
	}
	if _, _, err := r.NextProposalBlock(nil, headerAt(8), common.Address{}); !errors.Is(err, errNoPoSA) {
		t.Fatalf("head 8 (pending block 9): got %v, want errNoPoSA", err)
	}
	if posa.proposalCalls != 1 {
		t.Fatalf("pre-fork head reached the posa engine (calls=%d)", posa.proposalCalls)
	}
}

// TestProposerForwardersDegradeWithoutParlia covers a PoSA engine that has none
// of the three parliaProposer methods. The router must not panic; it answers
// "not Parlia" and the worker keeps the pre-fork behaviour. NextProposalBlock
// still works, because consensus.PoSA declares it.
func TestProposerForwardersDegradeWithoutParlia(t *testing.T) {
	posa := &noProposer{}
	r := New(&fakeEngine{}, posa, 10)

	if got, err := r.SignRecently(nil, headerAt(10)); got || !errors.Is(err, errNoPoSA) {
		t.Fatalf("SignRecently: got (%v, %v), want (false, errNoPoSA)", got, err)
	}
	if r.IsLastBlockInTurn(nil, headerAt(10)) {
		t.Fatal("IsLastBlockInTurn must be false without parliaProposer")
	}
	if _, err := r.BlockInterval(nil, headerAt(10)); !errors.Is(err, errNoPoSA) {
		t.Fatalf("BlockInterval: got %v, want errNoPoSA", err)
	}
	if start, end, err := r.NextProposalBlock(nil, headerAt(10), common.Address{}); err != nil || start != 11 || end != 22 {
		t.Fatalf("NextProposalBlock goes through consensus.PoSA: got (%d, %d, %v)", start, end, err)
	}
	if posa.proposalCalls != 1 {
		t.Fatalf("NextProposalBlock calls = %d, want 1", posa.proposalCalls)
	}
}

// TestProposerForwardersWithoutPoSAEngine covers posa == nil: every forwarder
// must answer the "no Parlia configured" value rather than dereference nil.
func TestProposerForwardersWithoutPoSAEngine(t *testing.T) {
	r := New(&fakeEngine{}, nil, 10)

	if got, err := r.SignRecently(nil, headerAt(10)); got || !errors.Is(err, errNoPoSA) {
		t.Fatalf("SignRecently: got (%v, %v), want (false, errNoPoSA)", got, err)
	}
	if r.IsLastBlockInTurn(nil, headerAt(10)) {
		t.Fatal("IsLastBlockInTurn must be false without a posa engine")
	}
	if _, err := r.BlockInterval(nil, headerAt(10)); !errors.Is(err, errNoPoSA) {
		t.Fatalf("BlockInterval: got %v, want errNoPoSA", err)
	}
	if _, _, err := r.NextProposalBlock(nil, headerAt(10), common.Address{}); !errors.Is(err, errNoPoSA) {
		t.Fatalf("NextProposalBlock: got %v, want errNoPoSA", err)
	}
}
