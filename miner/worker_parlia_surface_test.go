// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package miner

import (
	"testing"

	"github.com/ethereum/go-ethereum/consensus/router"
)

// TestRouterSatisfiesParliaWorkerEngine keeps the F-1 fix from being undone by a
// signature change somewhere else.
//
// worker holds its engine as consensus.Engine and reaches Parlia through type
// assertions on parliaWorkerEngine. Nothing in a normal build checks that a
// router satisfies that interface: the assertion just evaluates to false at run
// time and every Parlia branch in worker.go goes dead. On the gas-reservation
// path that is not a lost optimization but a chain halt at the first fork block
// ("gas consumption of system txs exceed the gas limit"), and on the proposer
// paths it is wasted block builds. Neither shows up on an idle devnet, which is
// why the guard is a compile-time assertion rather than a chain run.
//
// If this fails to compile, consensus/router.Engine is missing a method that
// worker.go asserts on. Add the forwarder there rather than deleting the method
// from parliaWorkerEngine.
func TestRouterSatisfiesParliaWorkerEngine(t *testing.T) {
	var _ parliaWorkerEngine = (*router.Engine)(nil)
}
