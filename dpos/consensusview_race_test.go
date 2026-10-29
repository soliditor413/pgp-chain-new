//go:build race

package dpos

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file is race-detector only, and the build tag is the point rather than a
// convenience: the defect it exists for is a plain-field race, and plain-field
// races have no runtime check. Measured on this machine (an-quan-shi, reproduced
// by kou-dou-ma): four goroutines writing the same three plain fields 200000
// times each, built WITHOUT -race, exit 0 in ten out of ten runs -- silent. The
// same construction under -race reports immediately. So a green run of this file
// without -race would mean nothing, and the tag makes that impossible to misread.
//
// What is asserted here is only what the test forces: how many goroutines entered
// the entry points and how many were in flight at once. Both are guaranteed by
// the cyclic barrier below, so they cannot flake. The event we actually care
// about -- two goroutines touching one field with no lock between them -- is
// judged by -race, i.e. by the process exit status, not by counting anything.
// Counting occurrences of a race is a scheduling question and would flake in both
// directions: it can be zero under a legal schedule, and it can be large under a
// broken one.

// barrier is a cyclic barrier: every round, each of the size goroutines waits
// until all of them have arrived, then they are released together. It is what
// makes "at least two callers were in flight at once" a property of the
// construction instead of an observation about scheduling.
type barrier struct {
	mu      sync.Mutex
	size    int
	waiting int
	release chan struct{}
}

func newBarrier(size int) *barrier {
	return &barrier{size: size, release: make(chan struct{})}
}

func (b *barrier) wait() {
	b.mu.Lock()
	b.waiting++
	if b.waiting == b.size {
		close(b.release)
		b.release = make(chan struct{})
		b.waiting = 0
		b.mu.Unlock()
		return
	}
	ch := b.release
	b.mu.Unlock()
	<-ch
}

// counter bookkeeping. entered counts distinct goroutines that reached an entry
// point at least once; inFlight/peak describe how many callers were between
// enter and leave simultaneously.
type tracker struct {
	seen     []int32
	entered  int32
	inFlight int32
	peak     int32
}

func newTracker(workers int) *tracker {
	return &tracker{seen: make([]int32, workers)}
}

func (t *tracker) enter(id int) {
	if atomic.CompareAndSwapInt32(&t.seen[id], 0, 1) {
		atomic.AddInt32(&t.entered, 1)
	}
	n := atomic.AddInt32(&t.inFlight, 1)
	for {
		old := atomic.LoadInt32(&t.peak)
		if n <= old || atomic.CompareAndSwapInt32(&t.peak, old, n) {
			return
		}
	}
}

func (t *tracker) leave() {
	atomic.AddInt32(&t.inFlight, -1)
}

func (t *tracker) assert(tb testing.TB, wantEntered int32) {
	tb.Helper()
	if got := atomic.LoadInt32(&t.entered); got != wantEntered {
		tb.Fatalf("only %d of %d goroutines reached an entry point; the run would prove nothing", got, wantEntered)
	}
	if got := atomic.LoadInt32(&t.peak); got < 2 {
		tb.Fatalf("peak in-flight callers was %d, so no two calls ever overlapped; the run would prove nothing", got)
	} else {
		tb.Logf("goroutines that entered = %d, peak in-flight callers = %d", t.entered, got)
	}
}

// drive runs f in workers goroutines for rounds rounds. Two barriers, each used
// once per round per goroutine: the first marks the start of a round, the second
// sits between enter and leave, so no goroutine can leave before every other one
// has entered. That is what makes "at least two callers overlapped" true by
// construction rather than by luck -- with a single barrier before enter, the
// overlap still happens in practice but nothing forces it, and an assertion that
// can flake is worse than no assertion.
func drive(workers, rounds int, enter func(id int), leave func(), f func(id int)) {
	start := newBarrier(workers)
	allIn := newBarrier(workers)
	var wg sync.WaitGroup
	for id := 0; id < workers; id++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				start.wait()
				enter(id)
				allIn.wait()
				f(id)
				leave()
			}
		}(id)
	}
	wg.Wait()
}

// TestConsensusViewFieldsAreSafeForConcurrentViewWork drives the three callers
// that R20 exists for -- the view loop, the miner goroutine and the chain head --
// against the getters Seal reads.
//
// Before R20 this test must fail under -race with the stack on one of
// consensusStatus, viewOffset, viewStartTime, viewChangeTime or isDposOnDuty. If
// it fails somewhere else, the construction reaches code this ticket does not
// cover and the case has to be narrowed, not the failure explained away.
func TestConsensusViewFieldsAreSafeForConcurrentViewWork(t *testing.T) {
	const (
		workers = 5
		rounds  = 32
	)
	pk := make([]byte, 33)
	base := time.Unix(1762218000, 0)
	producers := make([][]byte, 3)
	for i := range producers {
		producers[i] = make([]byte, 33)
		producers[i][0] = byte(i + 1)
	}
	p := &Producers{}
	if err := p.UpdateProducers(producers, len(producers), 0); err != nil {
		t.Fatalf("preparing producers: %v", err)
	}

	v := NewConsensusView(3*time.Second, pk, p, nil)
	v.SetChangViewTime(uint64(base.Unix()))
	v.SetRunning()

	tr := newTracker(workers)
	drive(workers, rounds, tr.enter, tr.leave, func(id int) {
		switch id {
		case 0: // view loop: the writer that moves the view forward
			v.ChangeView(base.Add(time.Duration(rounds)*4*time.Second), false, uint64(base.Unix()))
		case 1: // chain head: FinishedProposal's tail
			v.UpdateDutyIndex(uint64(rounds))
		case 2: // Seal: the deadline and the signer domain
			_ = v.GetChangeViewTime()
			_ = v.GetViewStartTime()
			_ = v.GetViewOffset()
		case 3: // reader pair: status and duty flags
			_ = v.IsRunning()
			_ = v.IsReady()
			_ = v.IsOnduty()
		case 4: // consensusStatus has its own writers, separate from the view loop
			v.SetReady()
			v.SetRunning()
		}
	})
	tr.assert(t, workers)
}

// TestConsensusViewProducerReadsAreSafe is ticket item 8: view.mu cannot cover
// these, because they never touch the view's own fields. They read the
// Producers struct directly while UpdateProducers rewrites it under p.mtx.
//
// The stack here is expected on producers.go (totalProducers, spvHeight,
// dutyIndex, the producers slice), not on consensusview.go -- that difference is
// the whole reason the item exists, so it is reported separately rather than
// merged into the case above.
func TestConsensusViewProducerReadsAreSafe(t *testing.T) {
	const (
		workers = 4
		rounds  = 32
	)

	producers := make([][]byte, 3)
	for i := range producers {
		producers[i] = make([]byte, 33)
		producers[i][0] = byte(i + 1)
	}
	p := &Producers{}
	if err := p.UpdateProducers(producers, len(producers), 0); err != nil {
		t.Fatalf("preparing producers: %v", err)
	}
	v := NewConsensusView(3*time.Second, make([]byte, 33), p, nil)

	tr := newTracker(workers)
	drive(workers, rounds, tr.enter, tr.leave, func(id int) {
		switch id {
		case 0: // engine attach / epoch switch: the only writing path
			v.UpdateProducers(producers, len(producers), uint64(rounds))
		case 1: // the three bare field reads
			_ = v.GetTotalProducersCount()
			_ = v.GetTotalArbiterCount()
			_ = v.GetSpvHeight()
		case 2: // GetDutyIndex: same blind spot, reached from the recovery path
			_ = v.GetDutyIndex()
		case 3: // DumpInfo: looks protected by the view lock, iterates the raw slice
			v.DumpInfo()
		}
	})
	tr.assert(t, workers)
}
