// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package core

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// TestPGPGenesisHash pins the pgp-chain mainnet genesis block. If it fails, the
// node can no longer sync the live network: every header check, every fork id
// handshake and the eth/64 status exchange depend on this hash.
func TestPGPGenesisHash(t *testing.T) {
	g := DefaultPGPGenesisBlock()

	// Layout guards, so a future edit fails here instead of on mainnet.
	if g.Timestamp != 1762218000 {
		t.Fatalf("timestamp mismatch: have %d, want 1762218000", g.Timestamp)
	}
	if g.GasLimit != 0x2068F7700 {
		t.Fatalf("gas limit mismatch: have %d, want %d", g.GasLimit, uint64(0x2068F7700))
	}
	if g.Difficulty.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("difficulty mismatch: have %v, want 1", g.Difficulty)
	}
	if len(g.ExtraData) != 32+12*common.AddressLength+65 {
		t.Fatalf("extraData length mismatch: have %d, want 337", len(g.ExtraData))
	}
	if bal := g.Alloc[common.HexToAddress("0x5d48e17ff0e45bf08fd47cc922612d6407140aeb")].Balance; bal == nil || bal.String() != "200000000000000000000000000" {
		t.Fatalf("initial balance mismatch: have %v, want 200000000000000000000000000", bal)
	}
	for _, addr := range []common.Address{params.ELAMINTER, params.ELATOKEN} {
		if len(g.Alloc[addr].Code) == 0 {
			t.Fatalf("system contract %x has no code in the genesis alloc", addr)
		}
	}
	if have, want := g.ToBlock().Hash(), params.PGPGenesisHash; have != want {
		t.Fatalf("genesis hash mismatch: have %x, want %x", have, want)
	}
}

// TestLoadChainConfigRejectsForeignDatadir makes sure a node started with an
// explicit pgp-chain genesis refuses to open a data directory that was
// initialised for another chain. Without the check the built-in config of the
// stored genesis wins over the flag and the node silently follows the wrong
// chain.
func TestLoadChainConfigRejectsForeignDatadir(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)
	DefaultBSCGenesisBlock().MustCommit(db, tdb)

	_, _, err := LoadChainConfig(db, DefaultPGPGenesisBlock())
	if _, ok := err.(*GenesisMismatchError); !ok {
		t.Fatalf("want *GenesisMismatchError, have %v", err)
	}
	// The same data directory must still be usable without --mainnet, so that
	// read-only commands keep working on BSC datadirs.
	if cfg, ghash, err := LoadChainConfig(db, nil); err != nil || ghash != params.BSCGenesisHash || cfg.ChainID.Uint64() != 56 {
		t.Fatalf("bsc datadir without preset: cfg=%v hash=%x err=%v", cfg, ghash, err)
	}
}

// TestSetupGenesisRejectsPGPConfigOnForeignDatadir covers the pgp-chain specific
// safety net in SetupGenesisBlockWithOverride: a chain ID of 860621 combined
// with a non-pgp genesis hash must fail loudly instead of quietly syncing a
// chain the database was never initialised for.
func TestSetupGenesisRejectsPGPConfigOnForeignDatadir(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)

	// Some other chain carrying the pgp-chain config, e.g. a datadir that was
	// initialised with a different alloc.
	foreign := DefaultPGPGenesisBlock()
	foreign.Alloc[common.HexToAddress("0x0000000000000000000000000000000000000001")] = types.Account{
		Balance: big.NewInt(1),
	}
	foreign.MustCommit(db, tdb)

	_, _, _, err := SetupGenesisBlock(db, tdb, nil)
	if err == nil {
		t.Fatal("expected an error for a pgp-chain config on a foreign datadir")
	}
	if !strings.Contains(err.Error(), "different chain") {
		t.Fatalf("want the explicit wrong-datadir error, have %v", err)
	}
}

// TestDynamicArbiterHeightCannotMoveTheGenesisHash is the guard for the way
// --spv.arbiter.height is applied: SetEthConfig writes the flag value into
// cfg.Genesis.Config.Legacy, and cfg.Genesis.Config is params.PGPChainConfig
// itself, i.e. the very pointer the genesis block was built from.
//
// That is only safe because Genesis.ToBlock() does not read Config, and this
// test pins it. If a future change ever folds part of the chain config into the
// genesis header, every node started with a non-default flag value would fork
// itself off the network at block zero, and the symptom would look like a bad
// bootnode rather than a bad flag.
func TestDynamicArbiterHeightCannotMoveTheGenesisHash(t *testing.T) {
	g := DefaultPGPGenesisBlock()
	before := g.ToBlock().Hash()

	cfg := *params.PGPChainConfig
	legacy := *cfg.Legacy
	legacy.DynamicArbiterHeight = 1 // as far from the default as possible
	cfg.Legacy = &legacy
	g.Config = &cfg

	if after := g.ToBlock().Hash(); after != before {
		t.Fatalf("changing DynamicArbiterHeight moved the genesis hash: %x -> %x", before, after)
	}
	if before != params.PGPGenesisHash {
		t.Fatalf("genesis hash mismatch: have %x, want %x", before, params.PGPGenesisHash)
	}
	if got := cfg.DynamicArbiterHeight(); got != 1 {
		t.Fatalf("override did not take: DynamicArbiterHeight() = %d, want 1", got)
	}
	if got := params.PGPChainConfig.DynamicArbiterHeight(); got != params.DefaultDynamicArbiterHeight {
		t.Fatalf("the global config was modified: have %d, want %d", got, params.DefaultDynamicArbiterHeight)
	}
}
