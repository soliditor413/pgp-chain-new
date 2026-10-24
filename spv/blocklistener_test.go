package spv

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/elastos/Elastos.ELA.SPV/util"

	"github.com/ethereum/go-ethereum/log"
)

// recordingHandler captures what the listener actually emitted. Asserting on the
// message text is the point: the progress line is the only signal that separates
// "the ELA chain is advancing" from "the ELA chain is advancing and matching
// nothing", and a plain counter can climb while the line never prints.
type recordingHandler struct {
	mu    sync.Mutex
	msgs  []string
	attrs []map[string]any
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	h.attrs = append(h.attrs, attrs)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// find returns the attributes of the last record whose message equals msg, or
// nil when no such line was written.
func (h *recordingHandler) find(msg string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i := len(h.msgs) - 1; i >= 0; i-- {
		if h.msgs[i] == msg {
			return h.attrs[i]
		}
	}
	return nil
}

// count returns how many records carried msg.
func (h *recordingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	var n int
	for _, m := range h.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// captureLogs redirects the root logger into a recorder for the duration of the
// test and restores the previous one afterwards.
func captureLogs(t *testing.T) *recordingHandler {
	t.Helper()

	previous := log.Root()
	h := new(recordingHandler)
	log.SetDefault(log.NewLogger(h))
	t.Cleanup(func() { log.SetDefault(previous) })
	return h
}

// wantUint compares a logged attribute numerically. The record stores whatever
// width the call site passed (uint32 height, uint64 counter), and slog keeps it,
// so comparing the raw interface would fail on width rather than on value.
func wantUint(t *testing.T, attrs map[string]any, key string, want uint64) {
	t.Helper()

	got, ok := attrs[key]
	if !ok {
		t.Fatalf("attribute %q missing from %v", key, attrs)
	}
	gotUint, err := strconv.ParseUint(fmt.Sprint(got), 10, 64)
	if err != nil {
		t.Fatalf("attribute %q = %v (%T), not a number", key, got, got)
	}
	if gotUint != want {
		t.Errorf("%s = %d, want %d", key, gotUint, want)
	}
}

// TestNotifyBlockReportsProgressWithoutAStore is the regression test for the
// diagnostic hole this line was moved out of: SpvDbInit returns early when the
// leveldb open fails, and NewService then starts syncing anyway. With the
// progress line nested inside the store check, that node would sync 2.3M blocks
// in total silence — indistinguishable from an empty bloom filter, which is the
// exact failure this diagnostic exists to catch.
func TestNotifyBlockReportsProgressWithoutAStore(t *testing.T) {
	previousStore := GetRechargeStore()
	SetRechargeStore(nil)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	logs := captureLogs(t)

	listener := new(BlockListener)
	listener.NotifyBlock(elaBlockAt(elaProgressInterval, emptyTx{}), false)

	attrs := logs.find("ELA main chain sync progress")
	if attrs == nil {
		t.Fatalf("no progress line was written without a recharge store; got %d records", len(logs.msgs))
	}
	wantUint(t, attrs, "elaHeight", uint64(elaProgressInterval))
	wantUint(t, attrs, "txs", 1)
	if got, ok := attrs["indexing"]; !ok || got != false {
		t.Errorf("indexing = %v, want false (no store installed)", got)
	}
}

// TestNotifyBlockReportsProgressWhenIndexingFails pins the other half: a block
// that cannot be indexed must still produce the progress line. Otherwise a store
// that is present but rejecting every block hides the sync rate behind one error
// per block and no counter.
func TestNotifyBlockReportsProgressWhenIndexingFails(t *testing.T) {
	previousStore := GetRechargeStore()
	SetRechargeStore(newTestStore())
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	logs := captureLogs(t)

	listener := new(BlockListener)
	// emptyTx fails to decode, so ApplyElaBlock returns an error.
	listener.NotifyBlock(elaBlockAt(elaProgressInterval, emptyTx{}), false)

	attrs := logs.find("ELA main chain sync progress")
	if attrs == nil {
		t.Fatal("no progress line was written although indexing failed")
	}
	if got, ok := attrs["indexing"]; !ok || got != true {
		t.Errorf("indexing = %v, want true (store is installed)", got)
	}
}

// TestNotifyBlockReportsProgressOnTheIntervalOnly keeps the log volume bounded:
// a full sync of the ELA main chain is ~2.3M blocks, so the line must not fire
// on every one of them.
func TestNotifyBlockReportsProgressOnTheIntervalOnly(t *testing.T) {
	previousStore := GetRechargeStore()
	SetRechargeStore(nil)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	logs := captureLogs(t)

	listener := new(BlockListener)
	for _, height := range []uint32{1, 2, elaProgressInterval - 1, elaProgressInterval, elaProgressInterval + 1} {
		listener.NotifyBlock(elaBlockAt(height, emptyTx{}), false)
	}

	if got := logs.count("ELA main chain sync progress"); got != 1 {
		t.Fatalf("progress lines = %d, want 1 (only height %d)", got, elaProgressInterval)
	}
	if got := listener.txsSeen.Load(); got != 5 {
		t.Fatalf("txsSeen = %d, want 5 (every block is counted, only some are reported)", got)
	}
}

// TestNotifyBlockRunsTheCallbackOnlyAboveTheArbiterHeight pins the two early
// returns that the mutex refactor had to preserve: a block below
// dynamicArbiterHeight is recorded as the live head but does not reach the
// registered callback, and a duplicate height does not reach it either. Both
// were bare `return`s before the critical section was introduced, and falling
// through to the callback instead would fire the legacy block handler on every
// ELA block below the arbiter schedule.
func TestNotifyBlockRunsTheCallbackOnlyAboveTheArbiterHeight(t *testing.T) {
	previousStore := GetRechargeStore()
	SetRechargeStore(nil)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	// onBlockHandled reaches into the package-level SPV service once the
	// arbiter height is reached, so both globals are parked for the duration.
	previousService, previousInfo := SpvService, nextTurnDposInfo
	SpvService, nextTurnDposInfo = nil, nil
	t.Cleanup(func() { SpvService, nextTurnDposInfo = previousService, previousInfo })

	captureLogs(t)

	var handled []uint32
	listener := &BlockListener{dynamicArbiterHeight: 100}
	listener.RegisterFunc(func(block interface{}) error {
		if b, ok := block.(*util.Block); ok {
			handled = append(handled, b.Height)
		}
		return nil
	})

	listener.NotifyBlock(elaBlockAt(99), true)
	if len(handled) != 0 {
		t.Fatalf("callback ran below dynamicArbiterHeight 100 at height 99: %v", handled)
	}
	if got := listener.BlockHeight(); got != 99 {
		t.Fatalf("BlockHeight = %d, want 99 (the block is still recorded)", got)
	}

	listener.NotifyBlock(elaBlockAt(100), true)
	if len(handled) != 1 || handled[0] != 100 {
		t.Fatalf("callback calls = %v, want [100]", handled)
	}

	listener.NotifyBlock(elaBlockAt(100), true)
	if len(handled) != 1 {
		t.Fatalf("callback re-ran for a duplicate height: %v", handled)
	}
}

// TestNotifyBlockSerialisesHeightAndAuxBlock is the concurrency guard for the
// listener's live-head state.
//
// blockNumber and param used to be written by NotifyBlock with no
// synchronisation while BlockHeight read blockNumber from the block execution
// path, so two notifications could interleave: one advances blockNumber to H,
// a second advances it to H+1 and stores its aux block, the first then resumes
// and overwrites the aux block with the H one. The listener is then pointing at
// a height it no longer holds the block for, which is what onBlockHandled hands
// to the arbiters.
//
// Run with -race this fails on the unsynchronised field access itself; the
// final invariant check catches the torn pair even without the detector.
func TestNotifyBlockSerialisesHeightAndAuxBlock(t *testing.T) {
	previousStore := GetRechargeStore()
	SetRechargeStore(nil)
	t.Cleanup(func() { SetRechargeStore(previousStore) })

	// The arbiter gate stays shut so onBlockHandled, which touches the
	// package-level SPV service, never runs: this test is about the mutex.
	listener := &BlockListener{dynamicArbiterHeight: math.MaxUint64}

	const writers, blocks = 8, 200

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 2; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = listener.BlockHeight()
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 1; n <= blocks; n++ {
				listener.NotifyBlock(elaBlockAt(uint32(n)), true)
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	listener.mu.Lock()
	height, paramHeight, paramBlock := listener.blockNumber, listener.param.height, listener.param.block
	listener.mu.Unlock()

	if height != blocks {
		t.Fatalf("blockNumber = %d, want %d", height, blocks)
	}
	if paramHeight != height {
		t.Fatalf("param.height = %d but blockNumber = %d: the height and the aux block disagree", paramHeight, height)
	}
	if paramBlock == nil {
		t.Fatal("param.block is nil although the listener advanced to a live head")
	}
	if paramBlock.Height != height {
		t.Fatalf("param.block.Height = %d but blockNumber = %d: the aux block belongs to another height",
			paramBlock.Height, height)
	}
}

// TestStoreAuxBlockIsSafeOnItsOwn keeps the exported entry point honest. It is
// part of the spv.BlockListener interface, so it must take the lock itself
// rather than assume it is only ever called from inside NotifyBlock.
func TestStoreAuxBlockIsSafeOnItsOwn(t *testing.T) {
	listener := new(BlockListener)
	listener.StoreAuxBlock(elaBlockAt(42))

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.param.height != 42 || listener.param.block == nil || listener.param.block.Height != 42 {
		t.Fatalf("param = (%v, %v), want the block at height 42", listener.param.height, listener.param.block)
	}
}
