// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// This file is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package core

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// u64ptr exists because params.newUint64 is unexported and every time-based
// fork needs a pointer.
func u64ptr(v uint64) *uint64 { return &v }

// forkHeightGenesis builds the shape a private chain or a pinned mainnet would
// use: only Legacy.ForkHeight is set, every BSC block fork is left nil so it can
// be derived, and the pre-fork execution rules are pinned at genesis.
func forkHeightGenesis(forkHeight uint64) *Genesis {
	return &Genesis{
		Config: &params.ChainConfig{
			ChainID:             big.NewInt(860621),
			HomesteadBlock:      big.NewInt(1),
			EIP150Block:         big.NewInt(2),
			EIP155Block:         big.NewInt(3),
			EIP158Block:         big.NewInt(3),
			ByzantiumBlock:      big.NewInt(3),
			ConstantinopleBlock: big.NewInt(3),
			PetersburgBlock:     big.NewInt(3),
			IstanbulBlock:       big.NewInt(3),
			BerlinBlock:         big.NewInt(3),
			LondonBlock:         big.NewInt(3),
			KeplerTime:          u64ptr(0),
			FeynmanTime:         u64ptr(0),
			FeynmanFixTime:      u64ptr(0),
			Parlia:              &params.ParliaConfig{},
			Legacy: &params.LegacyConfig{
				ForkHeight:                 forkHeight,
				SystemContractDeployHeight: forkHeight - 2,
				AllowForeignGenesis:        true,
				ParliaGenesisValidators:    []string{"0x0000000000000000000000000000000000000001"},
				ParliaGenesisVotingPowers:  []uint64{100},
				ParliaGenesisCabinets:      1,
			},
		},
		GasLimit:   30000000,
		Difficulty: big.NewInt(1),
		Timestamp:  1700000000,
		Alloc:      GenesisAlloc{},
	}
}

// TestForkHeightIsDerivedOnInit is the regression for a fresh database taking a
// different path than a restart: the derivation used to live only in
// chainConfigOrDefault, which the empty-database branch never reaches, so a
// genesis that carried just Legacy.ForkHeight failed to initialise with
//
//	unsupported fork ordering: hertzfixBlock not enabled, but keplerTime enabled
//	at timestamp 0
//
// Leaving the BSC block forks nil is the documented way to pin a fork height
// (params/pgp_config.go), so it has to work on both paths.
func TestForkHeightIsDerivedOnInit(t *testing.T) {
	const fork = uint64(64)

	db := rawdb.NewMemoryDatabase()
	cfg, ghash, _, err := SetupGenesisBlock(db, triedb.NewDatabase(db, nil), forkHeightGenesis(fork))
	if err != nil {
		t.Fatalf("a genesis carrying only Legacy.ForkHeight must initialise: %v", err)
	}
	if ghash == (common.Hash{}) {
		t.Fatal("genesis hash is zero")
	}

	// Every derived fork belongs at the fork height itself: that block is the
	// first one Parlia produces, so it is also the first one whose rules must be
	// the post-switch rules.
	derived := map[string]*big.Int{
		"lubanBlock":      cfg.LubanBlock,
		"ramanujanBlock":  cfg.RamanujanBlock,
		"nielsBlock":      cfg.NielsBlock,
		"mirrorSyncBlock": cfg.MirrorSyncBlock,
		"brunoBlock":      cfg.BrunoBlock,
		"eulerBlock":      cfg.EulerBlock,
		"gibbsBlock":      cfg.GibbsBlock,
		"nanoBlock":       cfg.NanoBlock,
		"moranBlock":      cfg.MoranBlock,
		"planckBlock":     cfg.PlanckBlock,
		"platoBlock":      cfg.PlatoBlock,
		"hertzBlock":      cfg.HertzBlock,
		"hertzfixBlock":   cfg.HertzfixBlock,
	}
	for name, got := range derived {
		if got == nil {
			t.Errorf("%s is still nil: a nil BSC block fork is read as 'not configured', not as 'derive it'", name)
			continue
		}
		if got.Uint64() != fork {
			t.Errorf("%s = %v, want %d (the fork height)", name, got, fork)
		}
	}

	// And the node has to read the same numbers back at startup, otherwise the
	// chain a restart follows is not the chain init wrote.
	loaded, _, err := LoadChainConfig(db, nil)
	if err != nil {
		t.Fatalf("LoadChainConfig: %v", err)
	}
	if loaded.LubanBlock == nil || loaded.LubanBlock.Uint64() != fork {
		t.Fatalf("startup config lost the derived lubanBlock: %v", loaded.LubanBlock)
	}
	if loaded.HertzBlock == nil || loaded.HertzBlock.Uint64() != fork {
		t.Fatalf("startup config lost the derived hertzBlock: %v", loaded.HertzBlock)
	}
}

// TestForkHeightDerivationLeavesSentinelAlone guards the other half: mainnet
// ships MaxInt64 as "explicitly not scheduled", and the derivation must not
// overwrite that with a fork height just because ForkHeight got pinned.
func TestForkHeightDerivationLeavesSentinelAlone(t *testing.T) {
	cfg := forkHeightGenesis(64).Config
	cfg.LubanBlock = big.NewInt(math.MaxInt64)
	cfg.HertzBlock = big.NewInt(30) // operator-pinned, must survive

	cfg.ApplyPGPForkHeight()

	if cfg.LubanBlock.Int64() != math.MaxInt64 {
		t.Fatalf("lubanBlock sentinel was overwritten: %v", cfg.LubanBlock)
	}
	if cfg.HertzBlock.Uint64() != 30 {
		t.Fatalf("an explicitly pinned hertzBlock was overwritten: %v", cfg.HertzBlock)
	}
	if cfg.PlatoBlock == nil || cfg.PlatoBlock.Uint64() != 64 {
		t.Fatalf("platoBlock should have been derived: %v", cfg.PlatoBlock)
	}
}
