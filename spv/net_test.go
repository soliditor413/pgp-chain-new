// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package spv

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeActiveNet pins the accepted spellings and, more importantly, the
// rejection of everything else.
//
// The switch this replaced ended in `default:` = mainnet, so any unrecognised
// name - a typo, a new network, and in particular "chapel" (a BSC network, not
// an ELA one) - produced a node that followed the ELA main chain while looking
// completely healthy. A node on the wrong ELA chain does not fail; it disagrees
// with the network about the arbiter set, so legacy blocks are rejected for
// reasons that only make sense on the other chain.
func TestNormalizeActiveNet(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", NetMainnet, false},
		{"mainnet", NetMainnet, false},
		{"MAIN", NetMainnet, false},
		{" m ", NetMainnet, false},
		{"testnet", NetTestnet, false},
		{"Test", NetTestnet, false},
		{"t", NetTestnet, false},
		{"regnet", NetRegnet, false},
		{"reg", NetRegnet, false},
		{"r", NetRegnet, false},
		// pgp-chain mapped its --goerli flag to the ELA regression network, and
		// this fork carried a misspelled arm. Both still resolve, to regnet.
		{"g", NetRegnet, false},
		{"goerli", NetRegnet, false},
		{"goreli", NetRegnet, false},
		// The one that matters: a BSC network name is not an ELA network name.
		{"chapel", "", true},
		{"moonnet", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeActiveNet(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeActiveNet(%q) = %q, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeActiveNet(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeActiveNet(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEnsureNetworkDir covers the datadir half of network selection.
//
// The failure this prevents is not "wrong network" but "two networks in one
// header store": the SPV store is keyed by hash and records no chain identity, so
// reusing a directory across networks corrupts it in a way that looks like a
// consensus problem.
func TestEnsureNetworkDir(t *testing.T) {
	t.Run("fresh directory is stamped", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spv")
		if err := EnsureNetworkDir(dir, NetTestnet); err != nil {
			t.Fatalf("EnsureNetworkDir: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, networkMarkerFile))
		if err != nil {
			t.Fatalf("marker not written: %v", err)
		}
		if got := strings.TrimSpace(string(raw)); got != NetTestnet {
			t.Fatalf("marker = %q, want %q", got, NetTestnet)
		}
	})

	t.Run("same network again is fine", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spv")
		for i := 0; i < 2; i++ {
			if err := EnsureNetworkDir(dir, NetTestnet); err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
		}
	})

	t.Run("switching networks is refused", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spv")
		if err := EnsureNetworkDir(dir, NetMainnet); err != nil {
			t.Fatalf("stamp mainnet: %v", err)
		}
		err := EnsureNetworkDir(dir, NetTestnet)
		if !errors.Is(err, ErrNetworkMismatch) {
			t.Fatalf("error = %v, want ErrNetworkMismatch", err)
		}
		// The refusal has to be actionable: an operator needs to know which
		// directory, which network it holds, and what to do about it.
		for _, want := range []string{dir, NetMainnet, NetTestnet} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("an unusable marker is refused, not overwritten", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spv")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, networkMarkerFile), []byte("moonnet\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := EnsureNetworkDir(dir, NetMainnet); !errors.Is(err, ErrNetworkMismatch) {
			t.Fatalf("error = %v, want ErrNetworkMismatch", err)
		}
	})

	t.Run("empty directory is rejected", func(t *testing.T) {
		if err := EnsureNetworkDir("", NetMainnet); err == nil {
			t.Fatal("expected an error for an empty data directory")
		}
	})
}
