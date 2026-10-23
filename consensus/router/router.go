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
// IMPORTANT: the router deliberately implements consensus.Engine and nothing
// else. eth/backend.go type asserts the engine to consensus.PoSA and then
// requires *parlia.Parlia, so a PoSA implementation here would abort node
// startup with "Engine is not Parlia type". Keeping the router Engine-only also
// makes every PoSA-only feature (vote pool, payment lane system txs, finality)
// stay off for the legacy chain, which is what it needs.
package router

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

var (
	// errNoPoSA is returned for forked blocks when no Parlia engine was wired in.
	errNoPoSA = errors.New("router: posa engine not configured for forked blocks")

	// errNoLegacy is returned for pre-fork blocks when no legacy engine was wired in.
	errNoLegacy = errors.New("router: legacy engine not configured for pre-fork blocks")
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

// Prepare implements consensus.Engine.
func (e *Engine) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	engine, err := e.engineFor(header)
	if err != nil {
		return err
	}
	return engine.Prepare(chain, header)
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
	return errors.New("router: sealing not supported before fork")
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
func (e *Engine) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	engine, err := e.engineFor(header)
	if err != nil {
		return nil
	}
	return engine.Delay(chain, header, leftOver)
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
