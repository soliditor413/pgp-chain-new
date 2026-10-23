// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package router

import (
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
	name    string
	verify  int
	prepare int
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
	return false, nil
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
func (f *fakeEngine) NextProposalBlock(consensus.ChainHeaderReader, *types.Header, common.Address) (uint64, uint64, error) {
	return 0, 0, nil
}

func headerAt(number uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number)}
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
