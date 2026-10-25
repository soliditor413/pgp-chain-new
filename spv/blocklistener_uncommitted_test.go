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

package spv

import (
	"errors"
	"strings"
	"testing"

	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA.SPV/interface/store"
	"github.com/elastos/Elastos.ELA.SPV/util"

	elacommon "github.com/elastos/Elastos.ELA/common"
)

// uncommittedWarnPrefix starts the message NotifyBlock writes when the SPV hands
// it a block it never committed. The test matches the prefix because the message
// carries the whole diagnosis and is free to grow.
const uncommittedWarnPrefix = "SPV reported an ELA block it never committed"

// fakeHeaderStore answers GetBest, the only method NotifyBlock uses. Everything
// else falls through to the nil embedded interface and panics, which is the loud
// failure we want if the code under test starts asking it for more.
type fakeHeaderStore struct {
	store.HeaderStore
	best *util.Header
}

func (s fakeHeaderStore) GetBest() (*util.Header, error) {
	if s.best == nil {
		return nil, errors.New("fake header store has no best header")
	}
	return s.best, nil
}

// fakeSPVService gives the listener an SPV head without starting a service.
type fakeSPVService struct {
	spv.SPVService
	headerStore store.HeaderStore
}

func (s fakeSPVService) HeaderStore() store.HeaderStore { return s.headerStore }

// GetBlockListener has to be non-nil: GetSpvHeight reads it as "is the service
// running" and would report 0 instead of the head height without it.
func (s fakeSPVService) GetBlockListener() spv.BlockListener { return &BlockListener{} }

// installFakeSPV points the package level service at a head with the given hash
// and height for the duration of the test.
func installFakeSPV(t *testing.T, best *util.Header) {
	t.Helper()

	saved := SpvService
	SpvService = &Service{SPVService: fakeSPVService{headerStore: fakeHeaderStore{best: best}}}
	t.Cleanup(func() { SpvService = saved })
}

// uncommittedBlock builds the shape the sdk hands to NotifyBlock after a failed
// commit: the header is populated but Height was never assigned.
func uncommittedBlock(hash elacommon.Uint256) *util.Block {
	return &util.Block{
		Header: util.Header{
			BlockHeader: fakeHeader{hash: hash},
			Height:      0,
		},
	}
}

// fakeElaHeader is the SPV head used for the duplicate comparison.
func fakeElaHeader(height uint32, hash elacommon.Uint256) *util.Header {
	return &util.Header{
		BlockHeader: fakeHeader{hash: hash},
		Height:      height,
	}
}

// findWithPrefix returns how many records start with prefix, plus the attributes
// of the first one. A single lock because msgs and attrs are parallel slices, and
// the recorder's own find/count only match whole messages.
func findWithPrefix(h *recordingHandler, prefix string) (int, map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var (
		n     int
		first map[string]any
	)
	for i, m := range h.msgs {
		if !strings.HasPrefix(m, prefix) {
			continue
		}
		n++
		if first == nil {
			first = h.attrs[i]
		}
	}
	return n, first
}

// TestNotifyBlockIgnoresADuplicateOfTheTip is the benign half of the height-zero
// guard.
//
// The sdk notifies for a block whose commit failed because it was already the
// tip, and that is not worth reporting: the data is in the header store already.
// Without the guard the block is indexed as height 0, which is what put
// "elaHeight=0" in the progress line and what made an already applied ELA height
// appear to change its hash in the field.
func TestNotifyBlockIgnoresADuplicateOfTheTip(t *testing.T) {
	previousStore := GetRechargeStore()
	rs := newTestStore()
	SetRechargeStore(rs)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	hash := elacommon.Uint256{7}
	installFakeSPV(t, fakeElaHeader(0, hash))
	logs := captureLogs(t)

	listener := new(BlockListener)
	listener.NotifyBlock(uncommittedBlock(hash), false)

	if got := logs.count("ELA main chain sync progress"); got != 0 {
		t.Errorf("progress lines = %d, want 0: a height-0 block is not sync progress", got)
	}
	if got, _ := findWithPrefix(logs, uncommittedWarnPrefix); got != 0 {
		t.Errorf("rejection warnings = %d, want 0: a duplicate of the tip is not a rejection", got)
	}
	if _, found, err := rs.coverage(); err != nil {
		t.Fatalf("reading coverage: %v", err)
	} else if found {
		t.Error("the duplicate was indexed into the recharge store")
	}
	if got := listener.BlockHeight(); got != 0 {
		t.Errorf("BlockHeight = %d, want 0: the live head must not move for a rejected block", got)
	}
}

// TestNotifyBlockReportsARejectedElaBlock is the half that matters in the field.
//
// A block the SPV refused leaves the ELA chain exactly where it was, and before
// this guard the only trace was a "sync progress elaHeight=0" line that read like
// progress. This warning is the deliverable: it names the block, the head and the
// cumulative count.
func TestNotifyBlockReportsARejectedElaBlock(t *testing.T) {
	previousStore := GetRechargeStore()
	rs := newTestStore()
	SetRechargeStore(rs)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	// The head is a different block at a different hash, so this is not a
	// duplicate: the commit failed for a reason.
	installFakeSPV(t, fakeElaHeader(42, elacommon.Uint256{9}))
	logs := captureLogs(t)

	listener := new(BlockListener)
	listener.NotifyBlock(uncommittedBlock(elacommon.Uint256{7}), false)

	if got := logs.count("ELA main chain sync progress"); got != 0 {
		t.Errorf("progress lines = %d, want 0: a rejected block is not progress", got)
	}
	got, attrs := findWithPrefix(logs, uncommittedWarnPrefix)
	if got != 1 {
		t.Fatalf("rejection warnings = %d, want 1", got)
	}
	wantUint(t, attrs, "elaHeight", 0)
	wantUint(t, attrs, "rejected", 1)
	wantUint(t, attrs, "spvHeight", 42)

	if _, found, err := rs.coverage(); err != nil {
		t.Fatalf("reading coverage: %v", err)
	} else if found {
		t.Error("the rejected block was indexed into the recharge store")
	}
	if got := listener.BlockHeight(); got != 0 {
		t.Errorf("BlockHeight = %d, want 0", got)
	}
}

// TestNotifyBlockStillIndexesNormalBlocks is the control: the guard has to be
// invisible to a block that was actually committed.
func TestNotifyBlockStillIndexesNormalBlocks(t *testing.T) {
	previousStore := GetRechargeStore()
	rs := newTestStore()
	SetRechargeStore(rs)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	installFakeSPV(t, fakeElaHeader(elaProgressInterval, elacommon.Uint256{1}))
	logs := captureLogs(t)

	listener := new(BlockListener)
	listener.NotifyBlock(elaBlockAt(elaProgressInterval), false)

	attrs := logs.find("ELA main chain sync progress")
	if attrs == nil {
		t.Fatal("a normal block no longer produces the progress line")
	}
	wantUint(t, attrs, "elaHeight", uint64(elaProgressInterval))
	if got, _ := findWithPrefix(logs, uncommittedWarnPrefix); got != 0 {
		t.Errorf("rejection warnings = %d, want 0", got)
	}

	height, found, err := rs.coverage()
	if err != nil {
		t.Fatalf("reading coverage: %v", err)
	}
	if !found || height != elaProgressInterval {
		t.Fatalf("coverage = (%d, %v), want (%d, true)", height, found, elaProgressInterval)
	}
}

// TestNotifyBlockRateLimitsRejectedBlocks bounds the warning.
//
// A node stuck on a block it cannot commit keeps retrying it, so the rejected
// path can fire indefinitely. The line has to stay visible without burying every
// other line, hence first-and-then-every-thousand.
func TestNotifyBlockRateLimitsRejectedBlocks(t *testing.T) {
	previousStore := GetRechargeStore()
	SetRechargeStore(newTestStore())
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	installFakeSPV(t, fakeElaHeader(42, elacommon.Uint256{9}))
	logs := captureLogs(t)

	listener := new(BlockListener)
	for i := 0; i < 1001; i++ {
		listener.NotifyBlock(uncommittedBlock(elacommon.Uint256{7}), false)
	}

	// Two lines: the first rejection, and the one at the thousandth.
	if got, _ := findWithPrefix(logs, uncommittedWarnPrefix); got != 2 {
		t.Fatalf("rejection warnings = %d over 1001 rejections, want 2 (first, then every %d)",
			got, elaUncommittedLogEvery)
	}
	if got := listener.uncommittedElaBlocks.Load(); got != 1001 {
		t.Fatalf("uncommittedElaBlocks = %d, want 1001: the number in the log is cumulative", got)
	}
	if got := logs.count("ELA main chain sync progress"); got != 0 {
		t.Fatalf("progress lines = %d, want 0: rejected blocks are never progress", got)
	}
}
