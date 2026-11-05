// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Package pgp carries the runtime bytecode of the 17 system contracts the
// protocol installs when the chain switches to Parlia.
//
// Why an image exists at all: the pgp-chain genesis is already live, so its
// alloc cannot be edited to place these contracts. They are written by protocol
// code at Legacy.SystemContractDeployHeight instead -- SetCode + SetNonce(...,1)
// per account -- and that code needs the bytecode as a Go asset rather than from
// a contract repository at runtime.
//
// The bytes are not the ones in bsc-genesis-contract's released tag: eight
// contracts carry uncommitted changes on top of 041881a (the fork's own work).
// manifest.json records the commit, the dirty-diff hash and the per-contract
// solc version, and README.md explains how to regenerate. Do not hand-edit
// image/*: scripts/pgp-image-gen.py is the only sanctioned producer, and the
// checks below are what make a hand edit fail loudly instead of silently
// shipping different consensus code.
//
// 0x2007 PaymentLane is deliberately absent: bsc-genesis-contract has neither a
// source nor an artifact for it. See manifest.json's sourceRepo and README.md.
package pgp

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

//go:embed image/* manifest.json
var assets embed.FS

// Entry is one system-contract account: what to install, where, and how it is
// initialised.
//
// CodeHash is the keccak of Code -- the same value EVM GetCodeHash returns, so a
// deployment can compare statedb.GetCodeHash(addr) against it for idempotence.
// DeployInit says the protocol must call InitSelector on this account right
// after SetCode; StakeCredit is the one false entry whose initializer belongs to
// StakeHub, per validator, later.
type Entry struct {
	Address       common.Address
	Name          string
	Source        string
	Solc          string
	Code          []byte
	CodeLen       int
	CodeHash      common.Hash
	InitSignature string
	InitSelector  [4]byte
	DeployInit    bool
}

// manifestEntry mirrors one object of manifest.json's entries array. Fields stay
// strings/ints on the way in and are validated into Entry below.
type manifestEntry struct {
	Address       string `json:"address"`
	Name          string `json:"name"`
	Source        string `json:"source"`
	Solc          string `json:"solc"`
	CodeLen       int    `json:"codeLen"`
	CodeHash      string `json:"codeHash"`
	InitSignature string `json:"initSignature"`
	InitSelector  string `json:"initSelector"`
	DeployInit    bool   `json:"deployInit"`
}

type manifestDoc struct {
	Version       int             `json:"version"`
	SourceRepo    string          `json:"sourceRepo"`
	SourceCommit  string          `json:"sourceCommit"`
	SourceDirty   bool            `json:"sourceDirty"`
	DirtyDiffHash string          `json:"dirtyDiffSha256"`
	Foundry       foundrySettings `json:"foundry"`
	Entries       []manifestEntry `json:"entries"`
	ImageHash     string          `json:"imageHash"`
}

type foundrySettings struct {
	Optimizer     bool   `json:"optimizer"`
	OptimizerRuns int    `json:"optimizerRuns"`
	BytecodeHash  string `json:"bytecodeHash"`
}

var (
	entries   []Entry
	byAddress = make(map[common.Address][]byte, 17)
	imageHash common.Hash
)

// init decodes and verifies the image once, at startup.
//
// It panics on any inconsistency rather than degrading: these bytes are
// consensus data, and a process that starts with the wrong system contract code
// produces a state root that differs from every other node's. Failing at load
// time is the only outcome an operator can act on. The style follows
// core/systemcontracts/upgrade.go, which panics on a bad hex constant for the
// same reason.
//
// What is checked here is everything derivable from the image alone: hex
// validity, codeLen, keccak(code) == codeHash, the init selector recomputed from
// its signature, and address uniqueness. The check that the address SET equals
// core/systemcontracts/const.go's constants lives in image_test.go instead, on
// purpose: this package must not import its parent, because the deployment code
// in that parent will import this package, and an import in both directions is a
// cycle. The test asserts the same property with no such constraint.
func init() {
	raw, err := assets.ReadFile("manifest.json")
	if err != nil {
		panic(fmt.Errorf("pgp image: cannot read manifest.json: %w", err))
	}
	var doc manifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(fmt.Errorf("pgp image: manifest.json is not valid: %w", err))
	}
	if doc.Version != 1 {
		panic(fmt.Errorf("pgp image: manifest version %d, this package understands 1", doc.Version))
	}
	if len(doc.Entries) == 0 {
		panic(fmt.Errorf("pgp image: manifest has no entries"))
	}
	imageHash = common.HexToHash(doc.ImageHash)
	if imageHash == (common.Hash{}) {
		panic(fmt.Errorf("pgp image: manifest has no imageHash"))
	}

	entries = make([]Entry, 0, len(doc.Entries))
	for _, me := range doc.Entries {
		addr := common.HexToAddress(me.Address)
		if addr == (common.Address{}) {
			panic(fmt.Errorf("pgp image: entry %q has no address", me.Name))
		}
		if _, dup := byAddress[addr]; dup {
			panic(fmt.Errorf("pgp image: address %s appears twice", addr))
		}

		hexCode, err := assets.ReadFile("image/" + me.Name)
		if err != nil {
			panic(fmt.Errorf("pgp image: entry %s: cannot read image/%s: %w", me.Name, me.Name, err))
		}
		code, err := hex.DecodeString(strings.TrimSpace(string(hexCode)))
		if err != nil {
			panic(fmt.Errorf("pgp image: entry %s: image is not valid hex: %w", me.Name, err))
		}
		if len(code) != me.CodeLen {
			panic(fmt.Errorf("pgp image: entry %s: image decodes to %d bytes, manifest says %d",
				me.Name, len(code), me.CodeLen))
		}
		hash := crypto.Keccak256Hash(code)
		if want := common.HexToHash(me.CodeHash); hash != want {
			panic(fmt.Errorf("pgp image: entry %s: keccak(code) = %s, manifest says %s",
				me.Name, hash, want))
		}

		var selector [4]byte
		if me.InitSignature != "" {
			sigHash := crypto.Keccak256([]byte(me.InitSignature))
			copy(selector[:], sigHash[:4])
			declared, err := hex.DecodeString(strings.TrimPrefix(me.InitSelector, "0x"))
			if err != nil || len(declared) != 4 {
				panic(fmt.Errorf("pgp image: entry %s: init selector %q is not 4 bytes", me.Name, me.InitSelector))
			}
			if string(declared) != string(selector[:]) {
				panic(fmt.Errorf("pgp image: entry %s: keccak(%s)[:4] = 0x%x, manifest says %s",
					me.Name, me.InitSignature, selector, me.InitSelector))
			}
		}

		entries = append(entries, Entry{
			Address:       addr,
			Name:          me.Name,
			Source:        me.Source,
			Solc:          me.Solc,
			Code:          code,
			CodeLen:       len(code),
			CodeHash:      hash,
			InitSignature: me.InitSignature,
			InitSelector:  selector,
			DeployInit:    me.DeployInit,
		})
		byAddress[addr] = code
	}

	// Ascending address order, so callers never have to sort and the order is a
	// property of the image rather than of whoever iterates it.
	sort.Slice(entries, func(i, j int) bool {
		return bytesLess(entries[i].Address, entries[j].Address)
	})
}

func bytesLess(a, b common.Address) bool {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Entries returns a copy of the image's entries, ordered by address ascending.
//
// A copy, not the slice: the deployment path holds these across a state
// transition, and a caller that appends or reorders must not be able to change
// what the next caller sees.
func Entries() []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = e
		out[i].Code = append([]byte(nil), e.Code...)
	}
	return out
}

// Code returns the runtime bytecode for addr, or nil and false if that address
// is not part of the image.
//
// The returned slice is a copy for the same reason as Entries.
func Code(addr common.Address) ([]byte, bool) {
	code, ok := byAddress[addr]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), code...), true
}

// ImageHash identifies the image: keccak256 of the canonical JSON of the entries
// array. It is what a deployment log should carry, so "which code is on this
// chain" can be answered by one hash rather than by 17.
func ImageHash() common.Hash { return imageHash }
