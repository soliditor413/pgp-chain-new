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

package ethconfig

// CreateConsensusEngine is the seam the whole PBFT wiring hangs off, and the only
// public signature this work changed (two extra parameters, one extra return
// value). These tests pin the two properties the rest of eth/backend.go relies on:
// a legacy chain yields a producing engine the caller can actually reach, and a
// non-legacy chain yields none.

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pbft"
	"github.com/ethereum/go-ethereum/consensus/router"
	"github.com/ethereum/go-ethereum/params"
)

// TestCreateConsensusEngineLegacyChain covers the branch eth/backend.go takes on
// the pgp-chain mainnet: Legacy is set, so the height router owns the legacy
// history and the producing engine has to come back as the second return value.
func TestCreateConsensusEngineLegacyChain(t *testing.T) {
	chainConfig := &params.ChainConfig{
		ChainID: big.NewInt(860621),
		Legacy: &params.LegacyConfig{
			ForkHeight: math.MaxUint64,
			VerifySeal: true,
		},
	}

	engine, pbftEngine, err := CreateConsensusEngine(chainConfig, nil, nil, common.Hash{}, &pbft.Config{})
	if err != nil {
		t.Fatalf("CreateConsensusEngine failed for a legacy chain: %v", err)
	}
	if engine == nil {
		t.Fatal("no consensus engine for a legacy chain")
	}
	if pbftEngine == nil {
		t.Fatal("the producing engine is nil for a legacy chain: the node would silently only verify")
	}
	if _, ok := engine.(*router.Engine); !ok {
		t.Fatalf("the legacy engine is %T, want the height router", engine)
	}
	// The router deliberately implements consensus.Engine only. If it ever grows
	// PoSA, eth/backend.go asserts the engine to *parlia.Parlia and aborts startup
	// with "Engine is not Parlia type".
	if _, ok := engine.(consensus.PoSA); ok {
		t.Fatal("the height router implements consensus.PoSA: eth/backend.go would type-assert it to *parlia.Parlia and abort startup")
	}
	// No pbft configuration means a pure verifier, which is what makes the
	// rolling upgrade reversible.
	if pbftEngine.IsProductionEnabled() {
		t.Fatal("production is enabled without --pbft.enable")
	}
}

// TestCreateConsensusEngineLegacyChainThreadsTheConfig checks that the pbft config
// actually reaches the engine rather than being dropped on the way through.
func TestCreateConsensusEngineLegacyChainThreadsTheConfig(t *testing.T) {
	chainConfig := &params.ChainConfig{
		ChainID: big.NewInt(860621),
		Legacy:  &params.LegacyConfig{ForkHeight: math.MaxUint64, VerifySeal: true},
	}

	_, pbftEngine, err := CreateConsensusEngine(chainConfig, nil, nil, common.Hash{}, &pbft.Config{Enable: true})
	if err != nil {
		t.Fatalf("CreateConsensusEngine failed: %v", err)
	}
	if pbftEngine == nil {
		t.Fatal("the producing engine is nil for a legacy chain")
	}
	if !pbftEngine.IsProductionEnabled() {
		t.Fatal("--pbft.enable did not reach the engine: Config.Pbft would be silently ignored")
	}
}

// TestCreateConsensusEngineNonLegacyChain is the other half: nothing but a legacy
// chain may hand out a producing engine, so a BSC node cannot accidentally start
// sealing PBFT blocks.
func TestCreateConsensusEngineNonLegacyChain(t *testing.T) {
	chainConfig := &params.ChainConfig{
		ChainID:                 big.NewInt(56),
		TerminalTotalDifficulty: big.NewInt(0),
	}

	engine, pbftEngine, err := CreateConsensusEngine(chainConfig, nil, nil, common.Hash{}, &pbft.Config{Enable: true})
	if err != nil {
		t.Fatalf("CreateConsensusEngine failed: %v", err)
	}
	if engine == nil {
		t.Fatal("no consensus engine for a non-legacy chain")
	}
	if pbftEngine != nil {
		t.Fatalf("a non-legacy chain returned a PBFT producing engine (%T): it must only ever come from the legacy branch", pbftEngine)
	}
}
