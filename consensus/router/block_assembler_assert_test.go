// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package router_test

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pbft"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/consensus/router"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// blockAssembler is core.AssembleBlock's unexported optional interface, spelled
// out here so the assertion below is about the shape core actually probes for.
type blockAssembler interface {
	FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB,
		body *types.Body, receipts []*types.Receipt, tracer *tracing.Hooks) (*types.Block, []*types.Receipt, error)
}

// The legacy engines have to satisfy blockAssembler, and this has to be a
// compile-time assertion rather than a test body: the failure mode it guards
// against is silent. Both engines used to lack the method, and nothing broke —
// core.AssembleBlock quietly fell back to Finalize, and on a router-wired node
// the router reproduced that fallback inline instead.
//
// Its reach is exactly this package's test binary. go build ./... does not
// compile _test.go files, so it does not fire there: the guarantee is "go test
// ./consensus/router stops compiling", not "the tree stops building". That is a
// registered limitation rather than an oversight, and the reason for accepting
// it is that moving the assertion into non-test code would make package router
// import consensus/pgp and consensus/pbft, which it does not import today
// (New takes a consensus.Engine); two production imports cost more than the gap.
//
// What actually protects a running node is errNoBlockAssembler: an engine
// without the method now makes router.FinalizeAndAssemble return an error at the
// first block it assembles, loudly, instead of silently falling back. The
// assertion only moves that discovery forward to the test run.
//
// (*router.Engine) is included because it is what core.AssembleBlock actually
// receives on a live node: if the router ever stopped implementing the
// interface, core would stop delegating and start executing its own fallback
// again, which is the behaviour the router's deleted copy used to reproduce.
var (
	_ blockAssembler = (*pgp.Engine)(nil)
	_ blockAssembler = (*pbft.Engine)(nil)
	_ blockAssembler = (*router.Engine)(nil)
)

// noAssemblerEngine is a consensus.Engine that pointedly does not implement
// blockAssembler. Embedding the interface supplies every method consensus.Engine
// declares (none of them is FinalizeAndAssemble), and Finalize is overridden with
// a do-nothing body so that a reinstated fallback would run to completion instead
// of panicking on a nil embedded engine — a panic would also fail the test, but
// with a message that says nothing about the fallback coming back.
type noAssemblerEngine struct {
	consensus.Engine
}

func (noAssemblerEngine) Finalize(consensus.ChainHeaderReader, *types.Header, vm.StateDB, *[]*types.Transaction,
	[]*types.Header, []*types.Withdrawal, *[]*types.Receipt, *[]*types.Transaction, *uint64, *tracing.Hooks) error {
	return nil
}

// TestFinalizeAndAssembleWithoutAssemblerFailsLoudly pins the removal of the
// router's fallback.
//
// The router used to reproduce core.AssembleBlock's fallback inline when the
// engine had no FinalizeAndAssemble. That made one consensus rule exist twice,
// tied together only by a comment. Deleting it is only half the fix: a later
// reader who finds the error path "unnecessarily strict" can add the fallback
// back, and every test in the tree would still pass — the engines that reach
// this code all implement the interface, so the branch would be dead and
// untested. A comment asking people not to add it back would not fail the build;
// this does.
func TestFinalizeAndAssembleWithoutAssemblerFailsLoudly(t *testing.T) {
	// forkHeight 0 means "no fork scheduled", so every height is legacy and the
	// router resolves to the embedded engine without consulting the chain.
	eng := router.New(noAssemblerEngine{}, nil, 0)

	header := &types.Header{Number: big.NewInt(1), GasLimit: 30_000_000}
	// A config is supplied so that a reinstated fallback would run to completion and
	// reach the fatal below, instead of panicking on a nil chain config — a panic
	// fails the test too, but says nothing about the fallback coming back.
	chain := &bloomTestChain{config: &params.ChainConfig{}, header: header}
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("cannot open an empty state: %v", err)
	}
	_, _, err = eng.FinalizeAndAssemble(chain, header, statedb, &types.Body{}, nil, nil)
	if err == nil {
		t.Fatal("FinalizeAndAssemble succeeded for an engine without the method: the router fell back " +
			"to Finalize again, which is a second copy of core.AssembleBlock's fallback " +
			"(core/state_processor.go:567-571) and the shape of the defect this work removed")
	}
	if !strings.Contains(err.Error(), "does not implement FinalizeAndAssemble") {
		t.Fatalf("error = %v, want it to name the method the engine is missing", err)
	}
}
