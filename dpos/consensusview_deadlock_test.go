package dpos

import (
	"testing"
	"time"
)

// TestChangeViewDoesNotDeadlock is the only guard for a failure that -race
// cannot see. ChangeView holds the view write lock across three levels --
// ResetView forwards to SetChangViewTime, resetViewOffset writes the offset, and
// DumpInfo walks Producers -- and sync.RWMutex is not reentrant, so the moment
// one of those inner functions takes the lock itself, the calling goroutine
// blocks on a mutex it already holds. That is not a data race: the
// synchronisation is perfectly correct, which is exactly why the race detector
// stays silent and a CI job would stay green. The only detector is a timer.
//
// The deadline is in-test and deliberately short. A suite-level -timeout would
// turn this into "the package hung, run it again tomorrow" and the reason would
// be lost: the two reports from #13 were a 1m30s panic and a 25s panic for the
// same defect, which is enough to show that the timeout value decides what the
// failure says rather than whether it is one.
func TestChangeViewDoesNotDeadlock(t *testing.T) {
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
	v := NewConsensusView(3*time.Second, producers[0], p, nil)
	v.SetChangViewTime(uint64(base.Unix()))
	v.SetRunning()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// force=true walks both of ChangeView's nesting chains: the offset>0
		// branch (ResetView + DumpInfo) and the force branch (resetViewOffset +
		// ResetView again).
		v.ChangeView(base.Add(10*time.Second), true, uint64(base.Unix()))
		// The exported forms, taken from outside any lock, must still work: they
		// are the API the miner and the view loop call.
		v.DumpInfo()
		v.ResetView(uint64(base.Unix()))
		v.SetChangViewTime(uint64(base.Unix()))
		v.UpdateDutyIndex(7)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ChangeView did not return within 2s. A mutex was re-entered on the same goroutine: " +
			"the viewMu write lock is held by the goroutine running ChangeView, and something inside it " +
			"(ResetView / SetChangViewTime / resetViewOffset / DumpInfo) tried to take viewMu again. " +
			"sync.RWMutex is not reentrant, so this is a self-deadlock, not a data race -- -race will " +
			"report nothing and the process simply stops here.")
	}
}
