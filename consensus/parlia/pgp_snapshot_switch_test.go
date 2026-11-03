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

package parlia

// Tests for the validator-set switch checkpoint landing before the fork.
//
// Background: Snapshot.apply() re-reads the validator list from a checkpoint
// header that sits minerHistoryCheckLen() blocks behind the switch height. On a
// chain that only becomes Parlia at ForkHeight, the earliest such checkpoint can
// be a pre-fork block -- the genesis header included -- whose extra field is in
// the legacy consensus' layout and therefore carries no addresses. parseValidators
// then returns "invalid validators bytes" and the node cannot prepare headers.
//
// These tests pin the arithmetic that decides whether it happens, because the
// condition is not obvious: it depends on the seat count through
// minerHistoryCheckLen(), which is 0 seats-aware and hides the bug at one seat.

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func checkpointTestHeader(number uint64) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number)}
}

// TestIsPreForkCheckpoint is the truth table for the guard itself.
func TestIsPreForkCheckpoint(t *testing.T) {
	cfg := func(fork uint64) *params.ChainConfig {
		return &params.ChainConfig{Legacy: &params.LegacyConfig{ForkHeight: fork}}
	}
	cases := []struct {
		name   string
		cfg    *params.ChainConfig
		number uint64
		want   bool
	}{
		{"genesis under a fork at 1", cfg(1), 0, true},
		{"block 2 under a fork at 1", cfg(1), 2, false},
		{"block 1099 under a fork at 1100", cfg(1100), 1099, true},
		{"block 1100 under a fork at 1100 (fork block is Parlia's)", cfg(1100), 1100, false},
		{"no legacy block at all", &params.ChainConfig{}, 0, false},
		{"fork not scheduled (MaxUint64)", cfg(math.MaxUint64), 0, false},
		{"fork at 0 means Parlia from genesis", cfg(0), 0, false},
		{"nil chain config", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPreForkCheckpoint(tc.cfg, checkpointTestHeader(tc.number)); got != tc.want {
				t.Fatalf("isPreForkCheckpoint = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMinerHistoryCheckLenDrivesTheFirstSwitch records the seat-count dependence
// that made this bug invisible on a single-validator devnet.
//
// The switch fires at heights where number % epochLength == minerHistoryCheckLen(),
// and its checkpoint is minerHistoryCheckLen() blocks earlier. So the first switch
// after a fork at height H has a checkpoint at (checkLen - H) relative to the first
// epoch boundary, and is pre-fork whenever H <= checkLen.
func TestMinerHistoryCheckLenDrivesTheFirstSwitch(t *testing.T) {
	snapWith := func(seats int, turnLength uint8) *Snapshot {
		validators := make(map[common.Address]*ValidatorInfo, seats)
		for i := 0; i < seats; i++ {
			validators[common.BytesToAddress([]byte{byte(i + 1)})] = &ValidatorInfo{Index: i + 1}
		}
		return &Snapshot{Validators: validators, TurnLength: turnLength}
	}

	cases := []struct {
		seats      int
		turnLength uint8
		checkLen   uint64
		// firstSwitch is the lowest height > 0 where number%epochLength == checkLen,
		// with epochLength left at its default of 200.
		firstSwitch uint64
	}{
		{1, 1, 0, 200},  // checkLen 0 -> the checkpoint is the epoch block itself
		{6, 1, 3, 3},    // checkLen 3 -> switch at 3, checkpoint at 0 (pre-fork!)
		{21, 1, 10, 10}, // checkLen 10 -> switch at 10, checkpoint at 0 (pre-fork!)
	}

	for _, tc := range cases {
		s := snapWith(tc.seats, tc.turnLength)
		if got := s.minerHistoryCheckLen(); got != tc.checkLen {
			t.Fatalf("%d seats: minerHistoryCheckLen = %d, want %d", tc.seats, got, tc.checkLen)
		}
		var first uint64
		for n := uint64(1); n <= defaultEpochLength; n++ {
			if n%defaultEpochLength == tc.checkLen {
				first = n
				break
			}
		}
		if first != tc.firstSwitch {
			t.Fatalf("%d seats: first switch at %d, want %d", tc.seats, first, tc.firstSwitch)
		}
		// The point of the test: at one seat the checkpoint is the epoch block
		// (post-fork, Parlia layout); at six and twenty-one it is block 0, the
		// legacy genesis, which is exactly the case isPreForkCheckpoint guards.
		checkpoint := first - tc.checkLen
		forked := &params.ChainConfig{Legacy: &params.LegacyConfig{ForkHeight: 1}}
		if got, want := isPreForkCheckpoint(forked, checkpointTestHeader(checkpoint)), checkpoint < 1; got != want {
			t.Fatalf("%d seats: pre-fork checkpoint verdict at height %d = %v, want %v",
				tc.seats, checkpoint, got, want)
		}
		if tc.seats == 1 && checkpoint != defaultEpochLength {
			t.Fatalf("one seat must checkpoint on the epoch block itself, got %d", checkpoint)
		}
		if tc.seats > 1 && checkpoint != 0 {
			t.Fatalf("%d seats must checkpoint on the genesis block, got %d", tc.seats, checkpoint)
		}
	}
}
