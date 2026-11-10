// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// ELA network stamping for the SPV data directory.
//
// The network selection itself lives in spv_module.go (NormalizeActiveNet). This
// file answers the other half of the question: once a node knows which ELA chain
// it follows, how does it avoid mixing two chains' data in one directory.
package spv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// networkMarkerFile records which ELA network owns a SPV data directory.
const networkMarkerFile = "ELA_NETWORK"

// ErrNetworkMismatch reports a SPV data directory that belongs to another ELA
// network.
var ErrNetworkMismatch = errors.New("spv: data directory belongs to a different ELA network")

// EnsureNetworkDir stamps dir with the ELA network and refuses to let the same
// directory be reused for a different one.
//
// The hazard: the SPV store keeps ELA headers keyed by hash and records nothing
// about which chain they came from. Two ELA networks share neither magic nor
// genesis, so pointing a testnet node at a mainnet directory does not fail
// cleanly - it produces a header store that is a mixture of two chains, and the
// symptoms (forks, reorg loops, arbiter sets that make no sense) point at
// consensus rather than at the datadir.
//
// The marker is written on first use and never rewritten. An existing mainnet
// directory predates this check and has no marker, so it is adopted by whichever
// network starts it first; every later switch is an error instead of a silent
// mixture. That is the one migration this can do without knowing history.
func EnsureNetworkDir(dir, net string) error {
	if dir == "" {
		return errors.New("spv: empty data directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("spv: create data directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, networkMarkerFile)
	recorded, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(path, []byte(net+"\n"), 0o600); err != nil {
			return fmt.Errorf("spv: write network marker %s: %w", path, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("spv: read network marker %s: %w", path, err)
	}
	have, err := NormalizeActiveNet(strings.TrimSpace(string(recorded)))
	if err != nil || have != net {
		return fmt.Errorf("%w: %s says %q, this node is %q; use a different --datadir, or delete the SPV directory to resync",
			ErrNetworkMismatch, path, strings.TrimSpace(string(recorded)), net)
	}
	return nil
}
