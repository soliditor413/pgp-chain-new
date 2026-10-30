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

package dpos

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/elastos/Elastos.ELA/utils/elalog"

	glog "github.com/ethereum/go-ethereum/log"
)

// The adapter has to satisfy the interface the Elastos packages are typed
// against; a signature drift there is a compile error at the UseLogger call
// sites, which is exactly when it should be caught.
var _ elalog.Logger = p2pLogAdapter{}

// captureHandler records what reaches go-ethereum's root logger.
type captureHandler struct {
	mu      sync.Mutex
	levels  []slog.Level
	records []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.levels = append(h.levels, r.Level)
	h.records = append(h.records, r.Message)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// TestP2PLogAdapterReachesTheRootLogger is the guard for the failure it is easy
// to ship: dpos/p2p calls DisableLog() from its init, so "the adapter exists" and
// "the adapter is installed" are different facts, and only the second one makes
// any message visible.
//
// This asserts on the adapter rather than through dpos/p2p's own logger because
// that logger is unexported: the only public way to emit is to trigger a real
// connection, which is the bench's job, not a unit test's.
func TestP2PLogAdapterReachesTheRootLogger(t *testing.T) {
	h := &captureHandler{}
	prev := glog.Root()
	glog.SetDefault(glog.NewLogger(h))
	defer glog.SetDefault(prev)

	var logger elalog.Logger = p2pLogAdapter{}
	logger.Debugf("connect %s failed", "peer")
	logger.Infof("connected %s", "peer")
	logger.Warnf("refused %s", "peer")
	logger.Errorf("died: %v", "reason")
	// Reaching this line at all is part of the assertion: elalog's Fatal does not
	// terminate, so the adapter must not terminate either. If this ever maps to
	// glog.Crit, the test binary exits here instead of failing.
	logger.Fatalf("cannot %s", "listen")

	if len(h.records) != 5 {
		t.Fatalf("root logger saw %d records, want 5: %v", len(h.records), h.records)
	}
	want := []string{
		"[dpos-p2p] connect peer failed",
		"[dpos-p2p] connected peer",
		"[dpos-p2p] refused peer",
		"[dpos-p2p] died: reason",
		"[dpos-p2p] FATAL cannot listen",
	}
	for i, w := range want {
		if h.records[i] != w {
			t.Errorf("record %d = %q, want %q", i, h.records[i], w)
		}
		if !strings.HasPrefix(h.records[i], p2pLogPrefix) {
			t.Errorf("record %d %q lost the subsystem prefix", i, h.records[i])
		}
	}
	// The level has to survive the trip: promoting everything to Info would make
	// the per-attempt debug chatter ruin a healthy run, and demoting everything
	// would re-hide the two lines that explain a peer that never connected.
	for i := 1; i < len(h.levels); i++ {
		if h.levels[i] < h.levels[i-1] {
			t.Errorf("levels are not non-decreasing at %d: %v", i, h.levels)
			break
		}
	}
	if h.levels[0] == h.levels[3] {
		t.Errorf("debug and error collapsed to the same level: %v", h.levels)
	}
}

// TestInitP2PLogIsCallable covers the wiring, not the output: the assertion is
// that the two UseLogger calls accept this adapter. hub is the one worth
// checking, because p2p.UseLogger does not reach it and the hub is where the
// magic comparison lives.
func TestInitP2PLogIsCallable(t *testing.T) {
	prev := glog.Root()
	glog.SetDefault(glog.NewLogger(glog.DiscardHandler()))
	defer glog.SetDefault(prev)

	InitP2PLog()
}

// TestP2PLogAdapterIgnoresUpstreamFiltering pins the "one verbosity setting"
// rule from log.go: elalog's own level must not vary, because geth's root logger
// is the only thing allowed to decide what is printed.
func TestP2PLogAdapterIgnoresUpstreamFiltering(t *testing.T) {
	var logger elalog.Logger = p2pLogAdapter{}
	if got := logger.Level(); got != elalog.LevelDebug {
		t.Errorf("Level() = %v, want LevelDebug (geth applies the filter)", got)
	}
	logger.SetLevel(elalog.LevelOff)
	if got := logger.Level(); got != elalog.LevelDebug {
		t.Errorf("after SetLevel(Off) Level() = %v; upstream filtering must stay inert", got)
	}
}
