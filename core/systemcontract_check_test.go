// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package core

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// TestCheckSystemContractsAtHead covers every branch of the startup self-check,
// including the one that matters: a database whose head is past the deploy
// height but has no validator contract must be refused, not opened.
//
// Without this test the negative path could only be reached by hand-crafting a
// database that a node refuses to open -- which is exactly the kind of check
// that rots silently.
func TestCheckSystemContractsAtHead(t *testing.T) {
	deployed := func(t *testing.T) *state.StateDB {
		t.Helper()
		statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		if err != nil {
			t.Fatalf("state.New: %v", err)
		}
		statedb.SetCode(common.HexToAddress(systemcontracts.ValidatorContract), []byte{0x60, 0x80}, 0)
		return statedb
	}
	empty := func(t *testing.T) *state.StateDB {
		t.Helper()
		statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		if err != nil {
			t.Fatalf("state.New: %v", err)
		}
		return statedb
	}

	for _, tc := range []struct {
		name      string
		legacy    *params.LegacyConfig
		head      uint64
		deployed  bool
		wantError string // empty means accepted
	}{
		{"no legacy config", nil, 100, false, ""},
		{"injection disabled", &params.LegacyConfig{SystemContractDeployHeight: 0}, 100, false, ""},
		{"head before the deploy height", &params.LegacyConfig{SystemContractDeployHeight: 100}, 99, false, ""},
		{"head at the deploy height with code", &params.LegacyConfig{SystemContractDeployHeight: 100}, 100, true, ""},
		{"head past the deploy height with code", &params.LegacyConfig{SystemContractDeployHeight: 100}, 250, true, ""},
		{"head at the deploy height without code", &params.LegacyConfig{SystemContractDeployHeight: 100}, 100, false, "deploy height"},
		{"head past the deploy height without code", &params.LegacyConfig{SystemContractDeployHeight: 100}, 150, false, "deploy height"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var statedb *state.StateDB
			if tc.deployed {
				statedb = deployed(t)
			} else {
				statedb = empty(t)
			}
			err := checkSystemContractsAtHead(tc.legacy, tc.head, statedb)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("head %d: unexpected error %v", tc.head, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("head %d without validator code was accepted; the node would run a database that "+
					"diverges from the network", tc.head)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error %q does not mention %q", err, tc.wantError)
			}
		})
	}

	// A nil state must be an error, not a silent pass: "could not check" is not
	// "checked and fine".
	if err := checkSystemContractsAtHead(&params.LegacyConfig{SystemContractDeployHeight: 100}, 100, nil); err == nil {
		t.Fatal("a nil state was accepted; an unverifiable database must not start")
	}
}
