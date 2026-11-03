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

// Package router dispatches consensus calls to the legacy pgp-chain engine or to
// the Parlia engine depending on the block height.
//
// IMPORTANT: the router deliberately implements consensus.Engine and a single
// extra method (IsSystemTransaction, see systemTxDetector), and nothing else of
// consensus.PoSA. eth/backend.go requires *parlia.Parlia for the PoSA-only
// features it wires up, so a full PoSA implementation here would abort node
// startup with "Engine is not Parlia type". Keeping the router out of the PoSA
// interface also makes every PoSA-only feature (vote pool, payment lane system
// txs, finality) stay off for the legacy chain, which is what it needs.
//
// IsSystemTransaction is the one exception because core.Process needs it to
// classify transactions while importing a block, and it is required for every
// height: without it a follower node cannot import a block that carries system
// transactions at all. It delegates by height like everything else, so pre-fork
// blocks still report "not a system transaction".
package router

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
)

var (
	// errNoPoSA is returned for forked blocks when no Parlia engine was wired in.
	errNoPoSA = errors.New("router: posa engine not configured for forked blocks")

	// errNoLegacy is returned for pre-fork blocks when no legacy engine was wired in.
	errNoLegacy = errors.New("router: legacy engine not configured for pre-fork blocks")

	// errNoBlockAssembler is returned when the engine that owns a height does not
	// implement FinalizeAndAssemble. It is unreachable for the engines this
	// repository can wire in; see the call site.
	errNoBlockAssembler = errors.New("router: engine does not implement FinalizeAndAssemble")
)

// Engine routes every consensus call to the engine that owns the block height.
// It implements consensus.Engine only, see the package comment for the reason.
type Engine struct {
	legacy consensus.Engine
	posa   consensus.PoSA

	// forkHeight is the first block owned by Parlia. With
	// params.LegacyConfig.ForkHeight set to math.MaxUint64 (the shipped default)
	// nothing is ever forked and every block is handled by the legacy engine.
	forkHeight uint64
	hasFork    bool
}

// New creates a height router. legacy must not be nil; posa may be nil, in which
// case forked heights fail with an explicit error instead of panicking.
//
// hasFork is derived from forkHeight so callers cannot get the two out of sync;
// forkHeight == 0 means "the fork is not scheduled".
func New(legacy consensus.Engine, posa consensus.PoSA, forkHeight uint64) *Engine {
	if legacy == nil {
		panic("router: legacy engine is nil")
	}
	return &Engine{
		legacy:     legacy,
		posa:       posa,
		forkHeight: forkHeight,
		hasFork:    forkHeight != 0,
	}
}

// ForkHeight returns the first block handled by Parlia, or 0 when the fork is not
// scheduled.
func (e *Engine) ForkHeight() uint64 { return e.forkHeight }

// Parlia returns the underlying Parlia engine, or false when none is wired in or
// the PoSA engine is some other implementation.
//
// It exists so the node wiring can still reach Parlia-only features that are not
// part of consensus.Engine. Callers must handle false.
func (e *Engine) Parlia() (*parlia.Parlia, bool) {
	p, ok := e.posa.(*parlia.Parlia)
	return p, ok
}

// Authorize injects the local signing key into the PoSA engine.
//
// This is not part of consensus.Engine: eth/backend.go calls it once from
// StartMining. Upstream reached Parlia by asserting s.engine.(*parlia.Parlia),
// which no longer succeeds now that the engine is this router. Without the
// forwarding below the assertion falls through *silently*, and the failure mode
// is nasty: Parlia never learns its signer, p.val stays the zero address,
// Prepare writes a zero Coinbase, every in-turn lookup misses, backOffTime logs
// "The validator is not authorized" at DEBUG only, and the node sits there
// producing nothing at INFO level.
//
// The return type is void to match Parlia.Authorize, so a single interface
// assertion in eth/backend.go catches both engines whether or not a fork is
// scheduled.
func (e *Engine) Authorize(val common.Address, signFn parlia.SignerFn, signTxFn parlia.SignerTxFn) {
	if e.posa == nil {
		// No Parlia on this chain, so there is nothing to authorize. Not an
		// error: the legacy engine has its own key handling.
		return
	}
	p, ok := e.posa.(*parlia.Parlia)
	if !ok {
		// A PoSA engine that is not Parlia would silently swallow the key, so
		// say so. Nothing in this tree does that; the check is here so that a
		// future substitution cannot reintroduce the silent failure.
		log.Error("router: refusing to authorize, PoSA engine is not Parlia", "type", fmt.Sprintf("%T", e.posa))
		return
	}
	p.Authorize(val, signFn, signTxFn)
}

// isForked reports whether the given height belongs to the Parlia era.
func (e *Engine) isForked(num *big.Int) bool {
	if !e.hasFork || num == nil {
		return false
	}
	return num.Uint64() >= e.forkHeight
}

func (e *Engine) legacyEngine() (consensus.Engine, error) {
	if e.legacy == nil {
		return nil, errNoLegacy
	}
	return e.legacy, nil
}

func (e *Engine) posaEngine() (consensus.Engine, error) {
	if e.posa == nil {
		return nil, errNoPoSA
	}
	return e.posa, nil
}

// Author implements consensus.Engine.
func (e *Engine) Author(header *types.Header) (common.Address, error) {
	if header == nil || header.Number == nil {
		return common.Address{}, errors.New("router: header without number")
	}
	if e.isForked(header.Number) {
		engine, err := e.posaEngine()
		if err != nil {
			return common.Address{}, err
		}
		return engine.Author(header)
	}
	engine, err := e.legacyEngine()
	if err != nil {
		return common.Address{}, err
	}
	return engine.Author(header)
}

// VerifyHeader implements consensus.Engine.
func (e *Engine) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	if header == nil || header.Number == nil {
		return errors.New("router: header without number")
	}
	if e.isForked(header.Number) {
		engine, err := e.posaEngine()
		if err != nil {
			return err
		}
		return engine.VerifyHeader(chain, header)
	}
	engine, err := e.legacyEngine()
	if err != nil {
		return err
	}
	return engine.VerifyHeader(chain, header)
}

// VerifyHeaders implements consensus.Engine.
//
// A batch that straddles the fork height cannot be delegated to a single engine,
// so it is verified header by header with the same abort/result channel contract
// the other engines use. Homogeneous batches are delegated as-is.
func (e *Engine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	if e.homogeneous(headers) {
		if len(headers) > 0 && e.isForked(headers[0].Number) {
			if engine, err := e.posaEngine(); err == nil {
				return engine.VerifyHeaders(chain, headers)
			}
		} else if engine, err := e.legacyEngine(); err == nil {
			return engine.VerifyHeaders(chain, headers)
		}
	}
	// Mixed batch, or the owning engine is missing: verify one by one so the
	// per-header routing (and the explicit error) still applies.
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	go func() {
		for _, header := range headers {
			err := e.VerifyHeader(chain, header)
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// homogeneous reports whether all headers belong to the same era.
func (e *Engine) homogeneous(headers []*types.Header) bool {
	if len(headers) == 0 {
		return true
	}
	if headers[0] == nil || headers[0].Number == nil {
		return false
	}
	forked := e.isForked(headers[0].Number)
	for _, header := range headers[1:] {
		if header == nil || header.Number == nil || e.isForked(header.Number) != forked {
			return false
		}
	}
	return true
}

// VerifyUncles implements consensus.Engine.
func (e *Engine) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if block == nil {
		return errors.New("router: nil block")
	}
	if e.isForked(block.Number()) {
		engine, err := e.posaEngine()
		if err != nil {
			return err
		}
		return engine.VerifyUncles(chain, block)
	}
	engine, err := e.legacyEngine()
	if err != nil {
		return err
	}
	return engine.VerifyUncles(chain, block)
}

// VerifyRequests implements consensus.Engine.
func (e *Engine) VerifyRequests(header *types.Header, requests [][]byte) error {
	engine, err := e.engineFor(header)
	if err != nil {
		return err
	}
	return engine.VerifyRequests(header, requests)
}

// NextInTurnValidator implements consensus.Engine.
func (e *Engine) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	engine, err := e.engineFor(header)
	if err != nil {
		return common.Address{}, err
	}
	return engine.NextInTurnValidator(chain, header)
}

// systemTxDetector is the one-method slice of consensus.PoSA that
// core.StateProcessor.Process needs in order to tell a system transaction apart
// from a user transaction while importing a block.
//
// It is declared here rather than used as consensus.PoSA because this package
// deliberately does not implement consensus.PoSA (see the package comment): a
// full implementation would switch on vote pool, payment lane and finality for
// the legacy chain as well. The narrow interface is the whole intersection.
type systemTxDetector interface {
	IsSystemTransaction(tx *types.Transaction, header *types.Header) (bool, error)
}

// IsSystemTransaction reports whether tx is a consensus-generated system
// transaction in this block.
//
// Why this method exists at all: core.Process used to do
//
//	posa, isPoSA := engine.(consensus.PoSA)
//	if isPoSA { if ok, _ := posa.IsSystemTransaction(tx, header); ok { ... skip ... } }
//
// With the router in place that assertion fails, isPoSA is false, and system
// transactions are then applied as ordinary ones. Their gas field is
// math.MaxUint64/2, so the very first of them empties the block gas pool and
// the import dies with
//
//	could not apply tx 0 [...]: gas limit reached
//
// The sealer never notices: it assembles the block through Finalize and writes
// it without re-executing, so Process is not on its path. The symptom therefore
// only appears on a second node -- or on the first node after a restart that
// needs to reprocess a block. core.Process now probes systemTxDetector, which
// this method satisfies.
//
// Pre-fork heights have no system transactions: the legacy chain produced none,
// and answering true would make Process skip a real transaction. A legacy engine
// that does understand them (it would have to implement the same one method) is
// still consulted, so the behaviour is unchanged for such an engine.
func (e *Engine) IsSystemTransaction(tx *types.Transaction, header *types.Header) (bool, error) {
	if header == nil || header.Number == nil {
		return false, errors.New("router: header without number")
	}
	if e.isForked(header.Number) {
		if e.posa == nil {
			return false, errNoPoSA
		}
		return e.posa.IsSystemTransaction(tx, header)
	}
	if d, ok := e.legacy.(systemTxDetector); ok {
		return d.IsSystemTransaction(tx, header)
	}
	return false, nil
}

// systemTxGasReserver is the one-method slice of parlia.Parlia that
// miner.worker.makeEnv needs in order to size the block gas pool before it packs
// user transactions.
//
// A private interface for the same reason systemTxDetector is one: this package
// implements consensus.Engine and nothing more, and the narrow interface is the
// whole intersection the caller needs.
type systemTxGasReserver interface {
	EstimateGasReservedForSystemTxs(chain consensus.ChainHeaderReader, header *types.Header) uint64
}

// EstimateGasReservedForSystemTxs reports how much of the block gas limit Parlia
// needs for the system transactions it appends in FinalizeAndAssemble.
//
// Why the router has to forward this at all: worker.makeEnv sizes the user-tx gas
// pool at GasLimit-gasReserved, and it used to obtain that number with a
// concrete assertion, w.engine.(*parlia.Parlia). On a chain that switches
// consensus at ForkHeight the engine is this router, so the assertion failed on
// every block, gasReserved stayed 0, and user transactions were allowed to fill
// the whole block. The system transactions that the fork block and every epoch
// block must carry then no longer fit and FinalizeAndAssemble fails with
//
//	gas consumption of system txs exceed the gas limit
//
// which stops the chain on exactly the blocks that matter most. It does not show
// up on an idle devnet, because with no user transactions nothing competes for
// the gas - which is also why this needs its own test rather than a chain run.
//
// Pre-fork heights answer 0: the legacy chain produces no system transactions. A
// legacy engine that does reserve gas (it would have to implement the same one
// method) is still consulted, mirroring IsSystemTransaction.
func (e *Engine) EstimateGasReservedForSystemTxs(chain consensus.ChainHeaderReader, header *types.Header) uint64 {
	if header == nil {
		return 0
	}
	// isForked treats a header without a number as pre-fork, so a malformed
	// header falls through to the legacy engine rather than being misclassified
	// as Parlia's.
	if e.isForked(header.Number) {
		if r, ok := e.posa.(systemTxGasReserver); ok {
			return r.EstimateGasReservedForSystemTxs(chain, header)
		}
		return 0
	}
	if r, ok := e.legacy.(systemTxGasReserver); ok {
		return r.EstimateGasReservedForSystemTxs(chain, header)
	}
	return 0
}

// parliaProposer is the proposer-timing slice of *parlia.Parlia that
// miner.worker asks for beyond consensus.Engine. Together with
// systemTxGasReserver it is the whole surface the worker needs; worker declares
// the union as parliaWorkerEngine and this engine satisfies it through the
// forwarders below.
//
// NextProposalBlock is not in here: consensus.PoSA already declares it, so the
// router only has to gate it by height. These three are the ones PoSA does not
// have; they are also the three the legacy engine could never answer.
//
// Why an interface rather than an assertion on *parlia.Parlia: it keeps this
// package from naming the concrete engine when it only needs behaviour,
// consistent with systemTxDetector and systemTxGasReserver above, and it makes
// the forwarding reachable from a unit test - a test double can satisfy an
// interface but can never satisfy a concrete-type assertion, so asserting on
// *parlia.Parlia would leave only the failure paths testable.
type parliaProposer interface {
	SignRecently(chain consensus.ChainReader, parent *types.Header) (bool, error)
	IsLastBlockInTurn(chain consensus.ChainReader, header *types.Header) bool
	BlockInterval(chain consensus.ChainHeaderReader, header *types.Header) (uint64, error)
}

// The four forwarders below share one routing rule, and it is the easy thing to
// get wrong:
//
//	SignRecently and NextProposalBlock judge the block being produced, i.e.
//	height+1, so they route on height+1. IsLastBlockInTurn and BlockInterval
//	describe the header they are handed, so they route on that header's height.
//
// Routing SignRecently on height instead of height+1 looks right and is not: at
// the fork the worker asks about parent ForkHeight-1, which routes to the
// legacy engine, which reports "not signed recently", so every validator builds
// the fork block. It is only the fork block, but it is the one block where a
// wasted build is most expensive.

// pendingBlockNumber is the height of the block a miner would build on top of
// parent, or nil when parent carries no number. Kept as a function so the +1
// cannot be forgotten in one of its two callers.
func pendingBlockNumber(parent *types.Header) *big.Int {
	if parent == nil || parent.Number == nil {
		return nil
	}
	return new(big.Int).Add(parent.Number, common.Big1)
}

// SignRecently reports whether this validator signed too recently to propose on
// top of parent. Routed on the height of the block that would be produced.
func (e *Engine) SignRecently(chain consensus.ChainReader, parent *types.Header) (bool, error) {
	if e.isForked(pendingBlockNumber(parent)) {
		p, ok := e.posa.(parliaProposer)
		if !ok {
			return false, errNoPoSA
		}
		return p.SignRecently(chain, parent)
	}
	// The legacy engine has no notion of "signed recently": PBFT does not rotate
	// producers, and its Seal re-checks whatever it does have.
	return false, nil
}

// IsLastBlockInTurn reports whether header is the last block of this
// validator's turn, which the worker uses to cap its local build budget.
// Routed on header's own height.
func (e *Engine) IsLastBlockInTurn(chain consensus.ChainReader, header *types.Header) bool {
	if header == nil || header.Number == nil || !e.isForked(header.Number) {
		return false
	}
	p, ok := e.posa.(parliaProposer)
	if !ok {
		return false
	}
	return p.IsLastBlockInTurn(chain, header)
}

// BlockInterval returns the block interval in milliseconds. Routed on header's
// own height. The error for a pre-fork header is deliberate: every caller
// treats it as "skip the timing optimization", and there is no meaningful
// interval to report for the legacy chain.
func (e *Engine) BlockInterval(chain consensus.ChainHeaderReader, header *types.Header) (uint64, error) {
	if header == nil || header.Number == nil || !e.isForked(header.Number) {
		return 0, errNoPoSA
	}
	p, ok := e.posa.(parliaProposer)
	if !ok {
		return 0, errNoPoSA
	}
	return p.BlockInterval(chain, header)
}

// NextProposalBlock returns the [start, end] block range of this validator's
// next proposal window. Routed on the height of the block that would be
// produced, matching SignRecently, and for the same fork-block reason. No
// interface assertion is needed - consensus.PoSA already declares this.
func (e *Engine) NextProposalBlock(chain consensus.ChainHeaderReader, header *types.Header, proposer common.Address) (uint64, uint64, error) {
	if !e.isForked(pendingBlockNumber(header)) || e.posa == nil {
		return 0, 0, errNoPoSA
	}
	return e.posa.NextProposalBlock(chain, header, proposer)
}

// Prepare implements consensus.Engine.
func (e *Engine) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	engine, err := e.engineFor(header)
	if err != nil {
		return err
	}
	return engine.Prepare(chain, header)
}

// FinalizeAndAssemble implements the blockAssembler interface core.AssembleBlock
// looks for.
//
// It exists so that core.AssembleBlock delegates instead of running its own
// fallback, and so that the assembly rule lives in exactly one place per engine.
// It also used to carry a copy of that fallback for engines without the
// interface; that copy is gone, because both engines this repository can wire in
// — *pgp.Engine and *parlia.Parlia — implement the method.
//
// The import/production distinction this method exists for is a Parlia-only one,
// and the comment says so because a reader would otherwise assume it applies
// everywhere. On Parlia, Finalize is the import path, so every system
// transaction it emits is checked against the transactions already in the block
// (systemTxImporting), while FinalizeAndAssemble is the production path and signs
// them itself (systemTxMining, consensus/parlia/parlia.go:1616-1623 and
// :2259-2272). There the two are not interchangeable: a locally produced block
// without the production variant fails at the first system call with "supposed to
// get a actual transaction, but get none", which is what a block containing the
// BSCValidatorSet.init() call does.
//
// None of that reaches the legacy segment: pgp.Finalize ignores systemTxs
// entirely, consensus/pbft forwards to it unchanged (consensus/pbft/engine.go,
// Finalize), and a legacy block carries no system transaction at all — the fork
// that emits them is unreachable below LegacyConfig.ForkHeight. So on the legacy
// path the two methods are the same rule, and the delegation below applies the
// same state rule, it is not a mode switch.
func (e *Engine) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB,
	body *types.Body, receipts []*types.Receipt, tracer *tracing.Hooks) (*types.Block, []*types.Receipt, error) {
	engine, err := e.engineFor(header)
	if err != nil {
		return nil, nil, err
	}
	assembler, ok := engine.(blockAssembler)
	if !ok {
		// Unreachable on any node this repository can build: *pgp.Engine and
		// *parlia.Parlia both implement the method, and
		// consensus/router/block_assembler_assert_test.go pins the legacy ones at
		// compile time. The error is here so the failure is loud if a third engine
		// is ever wired in without one, instead of silently falling back to
		// Finalize. It used to fall back, and that was wrong for a reason that is
		// not "duplication": this type is a dispatcher, it picks an engine by
		// height, and an engine it picked that cannot assemble a block is a wiring
		// error — it has to be reported, not papered over.
		//
		// Do not read that as an argument for deleting core.AssembleBlock's own
		// fallback (core/state_processor.go:567-571). That is a different layer's
		// contract, not a third copy: blockAssembler is an *optional* interface
		// there (core/state_processor.go:553-555), "assemble a block with any
		// engine" is part of what AssembleBlock promises, and the fallback has
		// live consumers — the ethash faker that most of core's block-building
		// tests use reaches it through GenerateChain. The copy deleted here was a
		// copy of the assembly *rule*; that fallback is the assembler's
		// *semantics*. They only look alike.
		return nil, nil, fmt.Errorf("%w: %T", errNoBlockAssembler, engine)
	}
	return assembler.FinalizeAndAssemble(chain, header, state, body, receipts, tracer)
}

// blockAssembler mirrors core's unexported interface of the same shape. It is
// used only as the delegation probe above; this file has no fallback branch any
// more, so an engine that does not implement it is an error rather than a
// second implementation to keep in sync.
type blockAssembler interface {
	FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB,
		body *types.Body, receipts []*types.Receipt, tracer *tracing.Hooks) (*types.Block, []*types.Receipt, error)
}

// Finalize implements consensus.Engine.
func (e *Engine) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB,
	txs *[]*types.Transaction, uncles []*types.Header, withdrawals []*types.Withdrawal,
	receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) error {
	engine, err := e.engineFor(header)
	if err != nil {
		return err
	}
	return engine.Finalize(chain, header, state, txs, uncles, withdrawals, receipts, systemTxs, usedGas, tracer)
}

// Seal implements consensus.Engine.
//
// Pre-fork this is delegated to the legacy engine, which after the PBFT port is
// the engine that can actually produce a block. Refusing here — as this method
// did while the legacy engine was a read-only replay engine — would leave the
// chain unable to make block 1 once the producer keys are configured.
//
// The delegation is still safe for a node that has no PBFT configuration: the
// read-only engine in consensus/pgp answers Seal with ErrProductionNotSupported,
// so a follower stays a follower by construction rather than by a check here.
// See docs/design/pbft-port-design.md D4.
func (e *Engine) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	if block == nil {
		return errors.New("router: nil block")
	}
	if e.isForked(block.Number()) {
		engine, err := e.posaEngine()
		if err != nil {
			return err
		}
		return engine.Seal(chain, block, results, stop)
	}
	engine, err := e.legacyEngine()
	if err != nil {
		return err
	}
	return engine.Seal(chain, block, results, stop)
}

// SealHash implements consensus.Engine.
func (e *Engine) SealHash(header *types.Header) common.Hash {
	engine, err := e.engineFor(header)
	if err != nil {
		// SealHash cannot return an error; fall back to the legacy engine so a
		// misconfigured fork does not panic, and let verification report it.
		if e.legacy != nil {
			return e.legacy.SealHash(header)
		}
		return common.Hash{}
	}
	return engine.SealHash(header)
}

// CalcDifficulty implements consensus.Engine.
func (e *Engine) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	if parent == nil || parent.Number == nil {
		return nil
	}
	if e.isForked(new(big.Int).Add(parent.Number, common.Big1)) {
		engine, err := e.posaEngine()
		if err != nil {
			return nil
		}
		return engine.CalcDifficulty(chain, time, parent)
	}
	engine, err := e.legacyEngine()
	if err != nil {
		return nil
	}
	return engine.CalcDifficulty(chain, time, parent)
}

// Delay implements consensus.Engine.
//
// A nil return is not a valid answer. The miner reads it as "something is
// wrong": it logs a warning and clears its stop timer at
// miner/worker.go:1466-1467, then keeps looping. The fatal part comes later --
// the next transaction arrival re-derives the delay at miner/worker.go:1547 and
// passes it to log.Debug as delay.String() at :1548-1550, with no nil check, and
// time.Duration.String is a value-receiver method, so a nil *time.Duration
// panics there. That panic does not depend on the log level: arguments are
// evaluated before the call. When the owning engine cannot be resolved the
// answer is therefore "no delay", which keeps the miner working on the next
// block instead of crashing on it.
func (e *Engine) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	engine, err := e.engineFor(header)
	if err != nil {
		noDelay := time.Duration(0)
		return &noDelay
	}
	if delay := engine.Delay(chain, header, leftOver); delay != nil {
		return delay
	}
	noDelay := time.Duration(0)
	return &noDelay
}

// Close implements consensus.Engine. Both engines are closed, the Parlia one
// first so it stops touching shared state before the legacy engine goes away.
func (e *Engine) Close() error {
	var errs []error
	if e.posa != nil {
		if err := e.posa.Close(); err != nil {
			errs = append(errs, fmt.Errorf("posa: %w", err))
		}
	}
	if e.legacy != nil {
		if err := e.legacy.Close(); err != nil {
			errs = append(errs, fmt.Errorf("legacy: %w", err))
		}
	}
	return errors.Join(errs...)
}

// engineFor picks the engine that owns the given header's height.
func (e *Engine) engineFor(header *types.Header) (consensus.Engine, error) {
	if header == nil || header.Number == nil {
		return nil, errors.New("router: header without number")
	}
	if e.isForked(header.Number) {
		return e.posaEngine()
	}
	return e.legacyEngine()
}
