package spv

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA/core/types/payload"
)

// These tests cover producerStateMu, the lock over nextTurnDposInfo and
// consensusMode.
//
// The pair is written by the SPV notification path, which Elastos.ELA.SPV runs
// on one goroutine per peer, and read by the consensus mode switch. Neither
// value can tear observably — one is a pointer, the other a byte — so unlike
// BlockListener's (blockNumber, param) pair there is nothing for a plain test to
// catch by inspecting a final value. The deterministic guard is therefore
// "does a reader actually block while a writer holds the lock", driven through
// testHookProducerStateWriteLocked.

// nextTurnInfoFor builds a producer set with the given working height.
func nextTurnInfoFor(workingHeight uint32) *NextTurnDPOSInfo {
	return &NextTurnDPOSInfo{
		&payload.NextTurnDPOSInfo{
			WorkingHeight:  workingHeight,
			CRPublicKeys:   [][]byte{{0x01, 0x02}},
			DPOSPublicKeys: [][]byte{{0x03, 0x04}},
		},
	}
}

// parkProducerStateWriter installs the write hook so that the first writer to
// take the lock parks until the returned release channel is closed. It returns
// the channel that fires once the writer is parked.
func parkProducerStateWriter(t *testing.T) (parked <-chan struct{}, release chan<- struct{}) {
	t.Helper()

	entered := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once

	previous := testHookProducerStateWriteLocked
	testHookProducerStateWriteLocked = func() {
		once.Do(func() {
			close(entered)
			<-released
		})
	}
	t.Cleanup(func() { testHookProducerStateWriteLocked = previous })
	return entered, released
}

// awaitParked waits for the writer to reach the hook.
//
// The timeout is not a correctness window: a writer that takes the lock reaches
// the hook immediately, and one that does not never reaches it at all. Waiting
// on the channel alone would turn that second case into a 10 minute test timeout
// with no diagnosis, so it is reported as a failure instead.
func awaitParked(t *testing.T, parked <-chan struct{}) {
	t.Helper()

	select {
	case <-parked:
	case <-time.After(2 * time.Second):
		t.Fatal("no writer reached testHookProducerStateWriteLocked: the write path does not take producerStateMu")
	}
}

// saveProducerState parks the producer state for the duration of a test and
// restores it afterwards.
func saveProducerState(t *testing.T) {
	t.Helper()

	previousService := SpvService
	previousInfo, previousMode := nextTurnInfo(), currentConsensusMode()
	SpvService = nil
	t.Cleanup(func() {
		SpvService = previousService
		setNextTurnInfo(previousInfo)
		setConsensusMode(previousMode)
	})
}

// TestProducerStateReadersBlockOnAWriter is the deterministic guard for the
// read side of the lock.
//
// A writer is parked inside the write lock, then every reader entry point is
// called from its own goroutine. If a reader honours producerStateMu it cannot
// finish while the writer is parked; if someone drops the RLock it returns
// immediately. The verdict does not depend on timing: with the lock the reader
// blocks indefinitely, so waiting out a short window is always enough.
//
// DumpNextDposInfo is in the list but proves less than the others: its nil
// branch publishes an empty set, so what blocks it here is the write lock, not a
// read lock of its own. It stays because the non-nil path is worth covering, but
// when it is the only entry point reported the read side may still be unlocked.
func TestProducerStateReadersBlockOnAWriter(t *testing.T) {
	saveProducerState(t)
	captureLogs(t)

	parked, release := parkProducerStateWriter(t)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		setNextTurnInfo(nextTurnInfoFor(7))
	}()
	awaitParked(t, parked)

	readers := map[string]func(){
		"GetWorkingHeight":       func() { _ = GetWorkingHeight() },
		"GetTotalProducersCount": func() { _ = GetTotalProducersCount() },
		"MainChainIsPowMode":     func() { _ = MainChainIsPowMode() },
		"GetNextTurnPeers":       func() { _ = GetNextTurnPeers() },
		"DumpNextDposInfo":       func() { _ = DumpNextDposInfo() },
		"SpvIsWorkingHeight":     func() { _ = SpvIsWorkingHeight() },
		"isSameNexturnArbiters":  func() { _ = isSameNexturnArbiters(7, nil, nil) },
	}

	// Every reader is joined before the test returns. They block until release,
	// and a leaked one would read the package-level SpvService while the next
	// test's saveProducerState writes it — a race in the test, not in the code.
	var wg sync.WaitGroup
	for name, read := range readers {
		done := make(chan struct{})
		wg.Add(1)
		go func(read func()) {
			defer wg.Done()
			defer close(done)
			read()
		}(read)

		select {
		case <-done:
			t.Errorf("%s completed while a writer held producerStateMu: it does not take the read lock", name)
		case <-time.After(50 * time.Millisecond):
		}
	}

	close(release)
	<-writerDone
	wg.Wait()

	if got := GetWorkingHeight(); got != 7 {
		t.Fatalf("GetWorkingHeight = %d after the writer published 7", got)
	}
}

// TestConsensusModeReadersBlockOnAWriter is the same guard for consensusMode,
// which has its own writer (setConsensusMode) but the same lock.
func TestConsensusModeReadersBlockOnAWriter(t *testing.T) {
	saveProducerState(t)
	captureLogs(t)

	parked, release := parkProducerStateWriter(t)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		setConsensusMode(spv.POW)
	}()
	awaitParked(t, parked)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = MainChainIsPowMode()
	}()
	select {
	case <-done:
		t.Error("MainChainIsPowMode completed while a writer held producerStateMu: it does not take the read lock")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	<-writerDone
	<-done

	if !MainChainIsPowMode() {
		t.Fatal("MainChainIsPowMode = false after the writer recorded POW")
	}
}

// TestNextTurnInfoSnapshotSurvivesRepublishing pins the contract the readers
// rely on: a snapshot taken under the read lock stays valid after the lock is
// released, because a new producer set is published as a whole new struct and
// the old one is never mutated in place. If someone starts editing the struct in
// place instead of publishing a replacement, every reader that took a snapshot
// becomes racy again.
func TestNextTurnInfoSnapshotSurvivesRepublishing(t *testing.T) {
	saveProducerState(t)
	captureLogs(t)

	setNextTurnInfo(nextTurnInfoFor(10))
	snapshot := nextTurnInfo()

	setNextTurnInfo(nextTurnInfoFor(20))

	if snapshot.WorkingHeight != 10 {
		t.Fatalf("the old snapshot now reports working height %d, want 10: the producer set was mutated in place",
			snapshot.WorkingHeight)
	}
	if got := GetWorkingHeight(); got != 20 {
		t.Fatalf("GetWorkingHeight = %d, want 20", got)
	}
}

// TestProducerStateIsRaceFreeUnderConcurrentAccess hammers every accessor from
// several goroutines while writers republish. Run with -race this is the check
// that no access escaped the lock; the assertions afterwards catch a lost write.
func TestProducerStateIsRaceFreeUnderConcurrentAccess(t *testing.T) {
	saveProducerState(t)
	captureLogs(t)

	listener := &BlockListener{dynamicArbiterHeight: math.MaxUint64}

	var readers sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = GetWorkingHeight()
				_ = GetTotalProducersCount()
				_ = MainChainIsPowMode()
				_ = GetNextTurnPeers()
				_ = SpvIsWorkingHeight()
				_ = listener.BlockHeight()
			}
		}()
	}

	for i := 0; i < 500; i++ {
		switch i % 3 {
		case 0:
			setNextTurnInfo(nextTurnInfoFor(uint32(i)))
		case 1:
			setNextTurnInfo(nil)
		case 2:
			setConsensusMode(spv.ConsensusAlgorithm(i % 2))
		}
	}

	close(stop)
	readers.Wait()

	// A single writer means the last write wins, and it is observable.
	setConsensusMode(spv.POW)
	if !MainChainIsPowMode() {
		t.Fatal("MainChainIsPowMode = false after recording POW")
	}
	setNextTurnInfo(nextTurnInfoFor(42))
	if got := GetWorkingHeight(); got != 42 {
		t.Fatalf("GetWorkingHeight = %d, want 42", got)
	}
	if got := GetTotalProducersCount(); got != 2 {
		t.Fatalf("GetTotalProducersCount = %d, want 2 (one CR plus one DPOS key)", got)
	}
}

// TestSpvIsWorkingHeightToleratesAStoppedService covers the nil guards in
// currentSpvBlockHeight. Before them this path dereferenced SpvService
// unconditionally, so a published producer set plus a stopped SPV service was a
// nil-pointer panic on the notification goroutine.
func TestSpvIsWorkingHeightToleratesAStoppedService(t *testing.T) {
	saveProducerState(t)
	captureLogs(t)

	var stopped *Service
	SpvService = stopped
	setNextTurnInfo(nextTurnInfoFor(0))

	// WorkingHeight 0 and a live height of 0: not past the working height.
	if SpvIsWorkingHeight() {
		t.Fatal("SpvIsWorkingHeight = true with a stopped SPV service reporting height 0")
	}
}

// TestSetNextTurnInfoPublishesAtomically is a small sanity check on the write
// path itself: the hook must fire exactly once per write and after the lock is
// taken, and the published value must be visible to the next reader.
func TestSetNextTurnInfoPublishesAtomically(t *testing.T) {
	saveProducerState(t)

	var calls atomic.Int32
	previous := testHookProducerStateWriteLocked
	testHookProducerStateWriteLocked = func() { calls.Add(1) }
	t.Cleanup(func() { testHookProducerStateWriteLocked = previous })

	setNextTurnInfo(nextTurnInfoFor(3))
	setConsensusMode(spv.POW)

	if got := calls.Load(); got != 2 {
		t.Fatalf("hook calls = %d, want 2 (one per writer)", got)
	}
	if got := GetWorkingHeight(); got != 3 {
		t.Fatalf("GetWorkingHeight = %d, want 3", got)
	}
	if !MainChainIsPowMode() {
		t.Fatal("MainChainIsPowMode = false after recording POW")
	}
}
