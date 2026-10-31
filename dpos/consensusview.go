// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/consensusview.go.

package dpos

import (
	"bytes"
	"math"
	"sort"
	"sync"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
)

type ViewListener interface {
	OnViewChanged(isOnDuty bool, force bool)
}

const (
	ConsensusReady = iota
	ConsensusRunning
)

// ConsensusView is the round/view state machine.
//
// A "view" is one attempt at one height. The view offset rotates the proposer
// through the producer list; signTolerance is how long a view may fail to
// produce a block before the next one starts. Nothing is persisted: the view is
// re-derived on restart from the parent block's timestamp
// (SetChangViewTime), which is why a restarted node rejoins at the right view
// without an explicit record.
//
// # Locking
//
// mu guards the five mutable fields listed under it below. They have three
// writers in three different goroutines -- the view loop (TryChangeView ->
// ChangeView), the miner (Prepare's startView, which calls ResetView and
// SetChangViewTime) and the chain head (FinishedProposal's tail, which calls
// SetReady, UpdateDutyIndex and ChangeView) -- and readers in Seal, which
// computes its deadline from GetChangeViewTime. The fields are plain words and
// time.Time values, so the failure mode is not a graceful error: a torn
// viewChangeTime produces a deadline in the past or far future, and a torn
// viewOffset assigns the round to the wrong producer. Neither is caught by the
// Go runtime; only -race sees it.
//
// Three consequences, all of which are easy to undo by tidying up:
//
//   - Nothing inside this file may call an exported accessor while holding mu.
//     sync.RWMutex is not reentrant, so ChangeView -> ResetView -> SetChangViewTime
//     would deadlock on its own goroutine at the second lock, and taking the read
//     lock from inside dumpInfoLocked while holding the write lock would do the
//     same. That is why every entry point below has a Locked twin and the
//     composites call only the twins.
//   - Nothing reached from inside mu may call back into the Dispatcher. The one
//     callback that does leave this file, listener.OnViewChanged, is therefore
//     invoked after the unlock, not inside it.
//   - The lock order is mu -> viewMu -> producers.mtx, declared in full on
//     Dispatcher. viewMu -> mu is forbidden, and it holds today only because the
//     far end of the callback chain is non-blocking (miner/worker.go's
//     requestNewWork uses a select with a default); if that ever becomes a
//     blocking send, this edge appears and the ban starts to matter.
type ConsensusView struct {
	// mu guards exactly the five fields below. publicKey and signTolerance are
	// set at construction and never written again (which is why they are not
	// "missing a lock"); producers owns its own mutex and must be reached
	// through its accessors, not protected from here; listener and now are set
	// once by the constructor.
	mu sync.RWMutex

	consensusStatus uint32
	// anchor is the whole-second instant the view grid is measured from, and it
	// is the ONLY stored input of viewOffset -- which is not a field any more,
	// see currentOffsetLocked. Two nodes agree on the view they are in exactly
	// when they agree on anchor, which is why every writer of this field takes
	// an argument the whole set can compute identically (a block header's
	// timestamp, or a peer's window start). It is uint64 seconds and not a
	// time.Time on purpose: a value with sub-second precision cannot be shared,
	// and the type is the cheapest place to say so.
	anchor         uint64
	publicKey      []byte
	signTolerance  time.Duration
	viewStartTime  time.Time
	viewChangeTime time.Time
	isDposOnDuty   bool
	producers      *Producers

	listener ViewListener
	// now is the clock currentOffsetLocked measures against. Injected rather
	// than taken from the time package so a test can drive the derived offset
	// without sleeping; nil means time.Now.
	now func() time.Time
}

// The Locked functions below assume mu is already held by the caller. They are
// not exported and must not grow a lock of their own: that is the exact mistake
// that turns ChangeView into a self-deadlock, and it is invisible to -race.

func (v *ConsensusView) setRunningLocked() {
	v.consensusStatus = ConsensusRunning
}

func (v *ConsensusView) setReadyLocked() {
	v.consensusStatus = ConsensusReady
}

func (v *ConsensusView) isRunningLocked() bool {
	return v.consensusStatus == ConsensusRunning
}

func (v *ConsensusView) isReadyLocked() bool {
	return v.consensusStatus == ConsensusReady
}

// clockNow is the time currentOffsetLocked measures against. It reads an
// immutable field, so it is safe on either side of mu.
func (v *ConsensusView) clockNow() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

// currentOffsetLocked derives the view offset from the anchor and the clock.
//
// This is the whole of the field's definition: viewOffset = (now - anchor) /
// signTolerance. It is a function, not a counter, and the difference is the
// defect this replaces. As a counter it advanced only while the view was
// Running (view.go's loop returns early otherwise, and SetRunning is called
// from startView alone), so two nodes holding the same anchor still reported
// different offsets -- measured on six producers, 96 against 288, with the
// anchor identical. Nothing later could repair it: a counter has memory, and
// that memory was this node's own history.
//
// Two nodes now agree whenever they agree on anchor, and anchor is only ever
// written from a value the set computes identically. Nothing to reset, and
// nothing for a peer to overwrite: reanchorLocked moves the origin instead.
//
// Zero is used for anchor==0 (constructed, never anchored) and for a clock that
// reads earlier than the anchor. Neither can be a real view number, and both
// would otherwise be a large or wrapped offset that would drive
// maxViewOffset's reset broadcast before the first round.
func (v *ConsensusView) currentOffsetLocked(now time.Time) uint32 {
	if v.anchor == 0 || now.Unix() <= int64(v.anchor) {
		return 0
	}
	offset := (now.Unix() - int64(v.anchor)) / int64(v.signTolerance/time.Second)
	if offset > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(offset)
}

// reanchorLocked is the only writer of anchor, and therefore the only thing
// that can move the view grid.
//
// The argument must be a whole second that the whole set computes identically:
// a block header's timestamp, or a peer's window start in recovery. Anything
// this node chose on its own -- time.Now(), the adjusted clock, a ticker phase --
// puts this node on a grid of its own, and the failure is structural rather than
// transient, because every later offset is measured from it.
//
// now is the caller's clock, used only to place the window: the anchor is the
// origin of the grid, and the window this node is sitting in is the one now falls
// in. Writing viewStartTime = anchor directly, as this did while the offset was a
// counter, would leave a reset on an old block reporting viewOffset 5 against a
// viewChangeTime six seconds after that old block -- a deadline already in the
// past, which Seal reads as an expired round.
func (v *ConsensusView) reanchorLocked(anchor uint64, now time.Time) {
	v.anchor = anchor
	start := time.Unix(int64(anchor), 0)
	offset := v.currentOffsetLocked(now)
	if offset > 0 {
		start = start.Add(time.Duration(offset) * v.signTolerance)
	}
	v.viewStartTime = start
	v.viewChangeTime = start.Add(v.signTolerance)

	// Re-anchoring can move the rotation slot, so isDposOnDuty is recomputed here
	// rather than left for the caller.
	//
	// This is not tidying. prepare.go's startView has two arms: a Ready view goes
	// through UpdateDutyIndex and gets the flag recomputed, but a view that is
	// still Running goes through ResetView alone -- and ResetView is this function.
	// Before the offset was derived, the stale flag happened to agree, because a
	// reset put the counter at 0 and the next ChangeView would have computed the
	// same 0. Derive the offset from the anchor and the two can differ: the view
	// loop's last advance before the reset left the flag true for the slot it had
	// moved to, while the new anchor puts this node back on its own slot. Prepare's
	// gate reads the flag after startView, so the stale value is what it acts on,
	// and the failure is one node refusing its turn and every later Prepare
	// answering ErrConsensusIsRunning. Measured: seal_test's
	// TestFailedRoundDoesNotStallTheNode.
	//
	// Nesting viewMu -> producers.mtx, the declared order.
	if producer := v.producers.GetNextOnDutyProducer(offset); producer != nil {
		v.isDposOnDuty = bytes.Equal(producer, v.publicKey)
	}
}

// dumpInfoLocked builds the duty listing. It takes onDuty as an argument instead
// of looking it up, because every caller inside a critical section has already
// asked Producers for it: asking twice would be two p.mtx acquisitions for one
// answer, and the two answers could differ.
func (v *ConsensusView) dumpInfoLocked(onDuty []byte) string {
	str := "\n"
	for _, signer := range v.producers.GetProducers() {
		if bytes.Equal(onDuty, signer) {
			str = str + elacom.BytesToHexString(signer) + " onDuty \n"
		} else {
			str = str + elacom.BytesToHexString(signer) + " not onDuty \n"
		}
	}
	return str
}

// producerIsOnDutyLocked mirrors ProducerIsOnDuty for callers that already hold
// the lock. It nests viewMu -> producers.mtx, which is the allowed direction.
func (v *ConsensusView) producerIsOnDutyLocked(account []byte) bool {
	producer := v.producers.GetNextOnDutyProducer(v.currentOffsetLocked(v.clockNow()))
	return bytes.Equal(producer, account)
}

// updateDutyIndexLocked recomputes both the rotation index and the on-duty flag.
//
// The offset it passes is derived from the clock, so this function's answer is a
// function of (height, anchor, now) and nothing local: two nodes that agree on
// the anchor and the head height agree on who is on duty, whatever their
// histories.
func (v *ConsensusView) updateDutyIndexLocked(height uint64) {
	v.producers.UpdateDutyIndex(height)

	currentProducer := v.producers.GetNextOnDutyProducer(v.currentOffsetLocked(v.clockNow()))
	v.isDposOnDuty = bytes.Equal(currentProducer, v.publicKey)
}

// SetRunning and SetReady are the two places the chain head publishes consensus
// progress; they take the write lock because the status is read by Seal and by
// Prepare on other goroutines.
func (v *ConsensusView) SetRunning() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.setRunningLocked()
}

func (v *ConsensusView) SetReady() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.setReadyLocked()
}

func (v *ConsensusView) IsRunning() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.isRunningLocked()
}

func (v *ConsensusView) IsReady() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.isReadyLocked()
}

// TryChangeView advances the view if the deadline has passed.
//
// The read of the two fields and the call to ChangeView are two separate
// critical sections, so the decision can be based on a view that has already
// moved when ChangeView runs; the result is a redundant or skipped offset, never
// a torn read. Closing that window is another ticket, because it changes how
// often the view advances, and the frequency is a consensus parameter.
func (v *ConsensusView) TryChangeView(now time.Time) {
	v.mu.RLock()
	running := v.isRunningLocked()
	changeTime := v.viewChangeTime
	v.mu.RUnlock()

	if !running || !now.After(changeTime) {
		return
	}
	Info("[TryChangeView] succeed", "now", now.String(), "changeTime", changeTime.String())
	parentTime := float64(changeTime.Unix()) - v.signTolerance.Seconds()
	v.ChangeView(now, false, uint64(parentTime))
}

func (v *ConsensusView) GetProducers() [][]byte {
	return v.producers.GetProducers()
}

// The four getters below used to read the Producers fields directly, which no
// view lock can cover: they are the other struct's state, and the only lock that
// can serialise them is p.mtx. See ticket item 8.
func (v *ConsensusView) GetTotalArbiterCount() int {
	return v.producers.GetTotalProducers()
}

func (v *ConsensusView) GetSpvHeight() uint64 {
	return v.producers.GetSpvHeight()
}

func (v *ConsensusView) GetTotalProducersCount() int {
	return v.producers.GetTotalProducers()
}

func (v *ConsensusView) GetDutyIndex() uint32 {
	return v.producers.GetDutyIndex()
}

// UpdateProducers, ChangeCurrentProducers and UpdateNextProducers must NOT take
// mu. They touch only Producers, which has its own lock, and the view lock adds
// nothing but a way to get the order wrong: Producers is not a leaf (the view
// reaches it from inside mu), so taking mu here would create the reverse edge
// producers.mtx -> viewMu, and an ABBA cycle against ChangeView.
func (v *ConsensusView) UpdateProducers(producers [][]byte, totalCount int, spvHeight uint64) {
	sort.Slice(producers, func(i, j int) bool {
		return bytes.Compare(producers[i], producers[j]) < 0
	})
	v.producers.UpdateProducers(producers, totalCount, spvHeight)
}

func (v *ConsensusView) ChangeCurrentProducers(changeHeight uint64, spvHeight uint64) {
	v.producers.ChangeCurrentProducers(changeHeight, spvHeight)
}

func (v *ConsensusView) UpdateNextProducers(producers []peer.PID, totalCount int) {
	v.producers.UpdateNextProducers(producers, totalCount)
}

func (v *ConsensusView) ProducerIndex(signer []byte) int {
	if len(signer) <= 0 {
		return -1
	}
	return v.producers.ProducerIndex(signer)
}

// SetWorkingHeight delegates to Producers.UpdateWorkingHeight, which takes p.mtx.
// The view lock plays no part here: workingHeight is Producers' field, and p.mtx
// is the only lock that can order it against UpdateDutyIndex' read.
func (v *ConsensusView) SetWorkingHeight(workingHeight uint64) {
	v.producers.UpdateWorkingHeight(workingHeight)
}

func (v *ConsensusView) GetWorkingHeight() uint64 {
	return v.producers.GetWorkingHeight()
}

func (v *ConsensusView) getCurrentNeedConnectArbiters() []peer.PID {
	return v.producers.getCurrentNeedConnectArbiters()
}

func (v *ConsensusView) GetNextNeedConnectArbiters() []peer.PID {
	return v.producers.GetNextNeedConnectArbiters()
}

// IsSameProducers compares curProducers with the set this node will adopt next.
//
// Same family as IsCurrentProducers below, and the same rules apply: do not delete
// it for having no caller here, and do not "fix" the in-place sort by copying.
// Upstream calls it directly from pgp-chain consensus/pbft/pbft.go:318 and
// network.go:426, and through the Pbft.IsSameProducers forwarder at pbft.go:346
// from network.go:1136, so M3 revives it either way.
//
// It is the more dangerous of the two, and that is the part worth reading twice.
// IsCurrentProducers sorts p.producers, the set in force, so the reordering only
// changes the order of one query's answer. This one sorts p.nextProducers -- the
// set ChangeCurrentProducers is going to adopt -- so the reordering stays behind in
// the slice that becomes the producer set, and dutyIndex is an index into that
// slice. Sorting here is not answering a question, it is editing the state the
// next epoch runs on.
func (v *ConsensusView) IsSameProducers(curProducers [][]byte) bool {
	v.producers.mtx.Lock()
	defer v.producers.mtx.Unlock()
	nextProducers := v.producers.nextProducers
	if len(curProducers) != len(nextProducers) {
		return false
	}
	sort.Slice(nextProducers, func(i, j int) bool {
		return bytes.Compare(nextProducers[i][:], nextProducers[j][:]) < 0
	})

	sort.Slice(curProducers, func(i, j int) bool {
		return bytes.Compare(curProducers[i], curProducers[j]) < 0
	})

	for index, v := range curProducers {
		if !bytes.Equal(v, nextProducers[index][:]) {
			return false
		}
	}
	return true
}

// IsCurrentProducers compares the current set with curProducers. Do not delete it
// for having no caller here: upstream calls it from six sites on the producer-set
// change path (pgp-chain consensus/pbft/network.go:222/:253/:463/:483/:1132 and
// pgp-chain's eth/backend.go:476, all through the Pbft.IsCurrentProducers
// forwarder), so M3 revives it.
//
// This family has two members, and only one of them is harmless:
// IsSameProducers above sorts nextProducers, the set that will take effect, so
// its reordering is left behind in the slice ChangeCurrentProducers adopts. This
// one sorts p.producers, the set in force, so the reordering is not cosmetic
// either -- dutyIndex is an index into that slice, which makes the order decide
// who is on duty next, and it is only correct while every node produces the same
// order -- but it does not survive into the next epoch. Fixing one and not the
// other leaves half the family in place.
//
// That is why this is left exactly as it is: an in-place sort under p.mtx is the
// ported behaviour, and "tidying" it into a copy would change which producer a
// node selects.
func (v *ConsensusView) IsCurrentProducers(curProducers [][]byte) bool {
	v.producers.mtx.Lock()
	defer v.producers.mtx.Unlock()
	producers := v.producers.producers
	if len(producers) <= 0 {
		return false
	}
	if len(curProducers) != len(producers) {
		return false
	}
	sort.Slice(producers, func(i, j int) bool {
		return bytes.Compare(producers[i][:], producers[j][:]) < 0
	})

	sort.Slice(curProducers, func(i, j int) bool {
		return bytes.Compare(curProducers[i], curProducers[j]) < 0
	})

	for index, v := range curProducers {
		if !bytes.Equal(v, producers[index][:]) {
			return false
		}
	}
	return true
}

// calculateOffsetTime only reads signTolerance, which is immutable after
// construction, so it needs no lock and is safe to call from either side of mu.
func (v *ConsensusView) calculateOffsetTime(startTime time.Time,
	now time.Time) (uint32, time.Duration) {
	duration := now.Sub(startTime)
	offset := duration / v.signTolerance
	offsetTime := duration % v.signTolerance

	return uint32(offset), offsetTime
}

// UpdateDutyIndex is one composite state transition: it moves the duty index and
// recomputes whether this node is on duty, and the two must not be observed
// apart. Hence one lock for both, and the Locked twin for callers that already
// hold it (RecoverFromStatus).
func (v *ConsensusView) UpdateDutyIndex(height uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.updateDutyIndexLocked(height)
}

// ChangeView moves the view forward, and it is the deepest composite in this
// file: a forced reset re-anchors the grid, a timeout advance moves the window,
// and DumpInfo walks Producers. All of it happens under one write lock because
// halfway through, the view is internally inconsistent, and a reader that saw
// that pair would compute a deadline against the wrong start. The logging and
// the listener call are moved outside the lock: logging can block on I/O, and
// the listener runs into the miner.
//
// The reported offset is derived after the move, never accumulated. That is the
// one behavioural difference from the ported version and it is the point of the
// change: an accumulator only advances while the view is Running, so a node that
// was Ready for a while reported a smaller offset than one that was Running, with
// the same anchor and the same clock. Nothing could reconcile the two, because
// each node's number embedded its own history.
func (v *ConsensusView) ChangeView(now time.Time, force bool, parentTime uint64) {
	var (
		offset, effective     uint32
		offsetTime            time.Duration
		startTime, changeTime time.Time
		dutyIndex, viewOffset uint32
		onDuty                bool
		dump                  string
		changed               bool
	)

	v.mu.Lock()
	if force {
		// The only operation that can put two nodes on different grids back on
		// one: move the origin. parentTime is the caller's shared value, and it
		// is the reason this branch takes an argument at all.
		v.reanchorLocked(parentTime, now)
		effective = 1
		changed = true
	} else {
		offset, offsetTime = v.calculateOffsetTime(v.viewStartTime, now)
		if offset > 0 {
			// now is past the window the view is sitting on: move the window to
			// the grid point now falls in. The grid itself does not move, so the
			// derived offset below lands exactly on viewStartTime's window.
			v.viewStartTime = now.Add(-offsetTime)
			v.viewChangeTime = v.viewStartTime.Add(v.signTolerance)
			effective = offset
			changed = true
		}
	}

	if changed {
		viewOffset = v.currentOffsetLocked(now)
		startTime = v.viewStartTime
		changeTime = v.viewChangeTime
		dutyIndex = v.producers.GetDutyIndex()
		onDutyProducer := v.producers.GetNextOnDutyProducer(viewOffset)
		v.isDposOnDuty = bytes.Equal(onDutyProducer, v.publicKey)
		onDuty = v.isDposOnDuty
		dump = v.dumpInfoLocked(onDutyProducer)
	}
	v.mu.Unlock()

	if !changed {
		return
	}
	Info("\n\n\n--------------------Change View---------------------")
	Info("viewStartTime:", startTime, "changeViewTime", changeTime, "nowTime:", now, "offset:", effective, "offsetTime:", offsetTime, "force:", force,
		"viewOffset", viewOffset, "dutyIndex", dutyIndex)
	Info(dump)
	Info("\n\n\n")
	// Outside the lock, and dependent on a property it cannot see: the listener
	// reaches miner.RequestNewWork, which is non-blocking (miner/worker.go's
	// requestNewWork, select with default). This is the reason viewMu -> mu does
	// not exist today; see the note on Dispatcher.
	if v.listener != nil {
		v.listener.OnViewChanged(onDuty, force)
	}
}

// DumpInfo builds the whole listing under one read lock and prints it after the
// unlock. Taking a lock per producer instead would give the caller a mixture of
// snapshots that never existed at one instant, and printing inside the lock
// would hold viewMu across a write to the log sink.
func (v *ConsensusView) DumpInfo() {
	v.mu.RLock()
	str := v.dumpInfoLocked(v.producers.GetNextOnDutyProducer(v.currentOffsetLocked(v.clockNow())))
	v.mu.RUnlock()
	Info(str)
}

func (v *ConsensusView) GetViewInterval() time.Duration {
	return v.signTolerance
}

func (v *ConsensusView) GetViewStartTime() time.Time {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.viewStartTime
}

func (v *ConsensusView) GetChangeViewTime() time.Time {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.viewChangeTime
}

// GetViewOffset returns the view this node is in, derived from the anchor and the
// clock at the moment of the call.
//
// Derived rather than read from a field, which is what makes two nodes agree:
// there is no per-node history left to disagree about. The cost is that the
// answer moves without anyone calling a setter, so a caller that reads it twice
// can see two values -- which is correct, and is why the value is never used as
// an identity. See currentOffsetLocked.
func (v *ConsensusView) GetViewOffset() uint32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.currentOffsetLocked(v.clockNow())
}

// GetAnchor returns the whole-second instant the view grid is measured from.
//
// This is the value operators should compare across nodes: two nodes reporting
// the same anchor are in the same view regardless of what their counters used to
// say, and two reporting different anchors cannot agree on anything.
func (v *ConsensusView) GetAnchor() uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.anchor
}

// GetClock is the instant GetViewOffset derives from, exposed so a status report
// can show the derivation instead of just its result.
//
// It exists because of a real support case: a node reported viewoffset 0 while the
// last reset in the log carried an anchor nearly half an hour old, which should
// have been 285. vo on its own cannot distinguish "the derivation is wrong" from
// "the anchor is not what the log said" from "the clock this node uses is not the
// one you think". anchor, vo and now together can, in one line.
func (v *ConsensusView) GetClock() time.Time {
	return v.clockNow()
}

func (v *ConsensusView) GetConsensusStatus() uint32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.consensusStatus
}

// GetViewState returns the three fields a recovering peer needs as one snapshot.
// Asking three getters would return a status, an offset and a start time that
// never belonged to the same view.
//
// The receiver now treats viewStartTime as an anchor and ignores the offset, so
// what this function has to guarantee is narrower than it used to be: the start
// time must be a whole second, because it is what the peer will measure from.
// That holds by construction -- reanchorLocked writes time.Unix(anchor, 0) and
// ChangeView's advance moves viewStartTime to another whole second -- so there is
// no longer a write ORDER to preserve here, only a write INVARIANT.
func (v *ConsensusView) GetViewState() (status uint32, viewOffset uint32, viewStartTime time.Time) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.consensusStatus, v.currentOffsetLocked(v.clockNow()), v.viewStartTime
}

// ResetView is a forwarder, not a leaf: its body is one call to
// reanchorLocked. It takes the lock on its own only because it is exported;
// ChangeView calls the Locked twin directly, because taking this lock from
// inside ChangeView's write lock would deadlock on the same goroutine.
func (v *ConsensusView) ResetView(parentTime uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reanchorLocked(parentTime, v.clockNow())
}

func (v *ConsensusView) SetChangViewTime(parentTime uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reanchorLocked(parentTime, v.clockNow())
}

// RecoverFromStatus applies a recovered view state under one acquisition of the
// view lock. It exists because RecoverFromConsensusStatus needs to set four
// fields and then recompute the duty flag from them; going through the exported
// setters would release the lock in between, and another goroutine could read
// the recovered offset with the pre-recovery start time.
//
// One acquisition of v.mu, not one atomic transaction. The five view fields are
// stored atomically with respect to anyone who takes the view lock; what is not
// atomic is the value one of them was derived from. isDposOnDuty is derived from
// Producers state, and reaching that state costs more than one p.mtx
// acquisition -- two when finishedHeight is 0, four when it is positive, because
// updateDutyIndexLocked sits inside that branch. In execution order:
// UpdateWorkingHeight is #1; producerIsOnDutyLocked is #2, inside
// GetNextOnDutyProducer; updateDutyIndexLocked then takes #3 inside
// Producers.UpdateDutyIndex and #4 inside the GetNextOnDutyProducer that follows
// it. All of them take the write lock, including the ones that only read --
// GetNextOnDutyProducer is not an RLock and does not pretend to be one.
//
// ChangeCurrentProducers takes p.mtx and does not take this lock -- the comment
// on it says why it must not -- so it can land in the gaps between those
// acquisitions: one gap before the branch, two more inside it. The last one is
// the one worth naming: between #3 and #4 the duty index has already been
// rewritten and isDposOnDuty has not been recomputed from it yet, so a
// producer-set change landing there leaves this node deriving its
// block-production flag from a new duty index against a different producer
// slice. Every step holds a lock; the combination never existed. Locks stop
// tearing, not composition.
//
// Naming the last gap is not the same as saying the others are equally bad. In
// the first gap the value #2 writes is overwritten at #3 and #4 whenever
// finishedHeight is positive, and is the approximation documented on the two
// assignments in the body when it is 0; in the second gap #3 recomputes from
// whatever working height and roster are in force. Only the last gap's result
// reaches the return.
//
// It is also invisible, which is why it needs a paragraph rather than a fix.
// UpdateDutyIndex and GetNextOnDutyProducer each reduce their index modulo the
// roster length in front of them, and each returns early when that length is 0
// (the modulo is producers.go:200 and :268; the guards are :197 and :265). An
// index computed against the old length and then reduced against the new one is
// always in range -- nothing for a bounds check to catch, and an index that
// neither roster's own arithmetic produces. All four acquisitions take p.mtx,
// so -race has nothing to report either.
//
// None of this is reachable in this tree today, and that changes what the
// paragraph is for. ChangeCurrentProducers has no caller here, production or
// test: only this forwarder and Producers' own definition exist. Upstream calls
// it from pgp-chain consensus/pbft/network.go:505, inside changeNextTurnProduces,
// which has no counterpart here. UpdateNextProducers is called from
// pgp-chain consensus/pbft/pbft.go:304 and :312, and the event that drives it is
// still a TODO in this tree -- spv/blocklistener.go:379 and :496 both say
// pgp-chain notified dpos.ETNextProducers there. The gap is structural, not
// live; it becomes live when those wires are joined, and the reason to describe
// it here is that whoever joins them will be reading this function.
//
// Before R20 the same gaps were data races; now they are legal interleavings
// with names. That is better, it is not atomic, and calling it atomic would hide
// the remaining gap from the next reader. Closing it needs one Producers entry
// point that does all of them under a single acquisition, which is a separate
// change rather than a note here.
func (v *ConsensusView) RecoverFromStatus(status uint32, viewOffset uint32, viewStartTime time.Time, workingHeight uint64, finishedHeight uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.consensusStatus = status
	// The peer's viewStartTime becomes this node's anchor; the peer's viewOffset
	// is deliberately ignored.
	//
	// That asymmetry is the point of the derivation. A peer's counter is that
	// peer's history -- how long ITS view happened to be Running -- so importing
	// it transfers a number that already embeds a local accident. Its window
	// start is different: it is the same kind of value this node would have
	// computed for itself, and RecoverAbnormal has already rounded it to a grid
	// point that every node computing the same median gets. So the start time is
	// a shared candidate and the counter is not, and taking the start time while
	// dropping the counter is what makes recovery converge instead of copying the
	// divergence.
	//
	// _ = viewOffset keeps the parameter in the signature: it is the wire shape
	// (dmsg.ConsensusStatus) and callers should not have to care that this side
	// no longer reads it.
	_ = viewOffset
	v.reanchorLocked(uint64(viewStartTime.Unix()), v.clockNow())
	// UpdateWorkingHeight, not the locked twin: workingHeight belongs to
	// Producers, and viewMu orders nothing about it. This nests viewMu ->
	// producers.mtx, which is the allowed direction.
	v.producers.UpdateWorkingHeight(workingHeight)
	// isDposOnDuty is assigned twice, and the second one is not redundant.
	// GetNextOnDutyProducer reads the dutyIndex field rather than deriving an
	// index, so this first assignment uses whatever dutyIndex the node carried
	// into the recovery. The second assignment is inside updateDutyIndexLocked --
	// the only call the finishedHeight branch makes -- and it writes
	// isDposOnDuty only after UpdateDutyIndex has recomputed the index from the
	// chain's finished height, so it is the one that sees a recovered index. The
	// two answers can differ,
	// and the second is the final one. Removing the finishedHeight branch as
	// "duplicate work" would silently leave isDposOnDuty derived from a stale
	// dutyIndex -- and IsOnduty is a block-production gate (Seal asks it), so the
	// symptom would be a node that stops producing, not a wrong number in a log.
	//
	// What the finishedHeight == 0 case costs is much smaller, which is why it is
	// not a reason to change the condition: the duty index is not restored, and
	// every non-test consumer of it in this tree is a log line -- the End-Recover
	// summary and ChangeView's view summary. Block production goes through
	// isDposOnDuty, which the producerIsOnDutyLocked assignment right after this
	// comment writes unconditionally; the finishedHeight condition only decides
	// whether the value is then recomputed.
	v.isDposOnDuty = v.producerIsOnDutyLocked(v.publicKey)
	if finishedHeight > 0 {
		v.updateDutyIndexLocked(finishedHeight)
	}
}

func (v *ConsensusView) IsProducers(account []byte) bool {
	return v.producers.IsProducers(account)
}

func (v *ConsensusView) IsOnduty() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.isDposOnDuty
}

func (v *ConsensusView) ProducerIsOnDuty(account []byte) bool {
	v.mu.RLock()
	offset := v.currentOffsetLocked(v.clockNow())
	v.mu.RUnlock()
	producer := v.producers.GetNextOnDutyProducer(offset)
	return bytes.Equal(producer, account)
}

// CurrentOnDutyProducer names the producer the rotation puts on duty for the
// view this node is in: slot (dutyIndex + viewOffset) % producers, answered by
// Producers itself rather than recomputed here.
//
// It exists for the status RPC, and it is a method rather than two getters at the
// call site because the answer spans both structs. Reading dutyIndex and
// viewOffset separately and doing the modulo outside gives a slot that may belong
// to no single instant -- the offset moves on its own now, once per
// signTolerance -- and a wrong answer here is read as "the wrong node is on duty".
// The two reads below are one acquisition each, view lock then producer lock,
// which is the declared order.
//
// What it is for: on the six-producer rig two producers reported different
// viewOffsets and BOTH landed on a key holder. That is why the chain had two nodes
// claiming the round at once instead of none, and it is invisible from the offsets
// alone -- the modulo is what turns two different counts into two live claimants.
// One line per node now answers it.
func (v *ConsensusView) CurrentOnDutyProducer() []byte {
	v.mu.RLock()
	offset := v.currentOffsetLocked(v.clockNow())
	v.mu.RUnlock()
	return v.producers.GetNextOnDutyProducer(offset)
}

func (v *ConsensusView) IsMajorityAgree(count int) bool {
	return v.producers.IsMajorityAgree(count)
}

func (v *ConsensusView) IsMajorityRejected(count int) bool {
	return v.producers.IsMajorityRejected(count)
}

func (v *ConsensusView) HasArbitersMinorityCount(count int) bool {
	return v.producers.HasArbitersMinorityCount(count)
}

func (v *ConsensusView) HasProducerMajorityCount(count int) bool {
	return v.producers.HasProducerMajorityCount(count)
}

func (v *ConsensusView) GetMajorityCount() int {
	return v.producers.GetMajorityCount()
}

func (v *ConsensusView) GetCRMajorityCount() int {
	return v.producers.GetCRMajorityCount()
}

func (v *ConsensusView) GetMajorityCountByTotalSigners(totalSigner int) int {
	return v.producers.GetMajorityCountByTotalSigners(totalSigner)
}

func NewConsensusView(tolerance time.Duration, account []byte,
	producers *Producers, viewListener ViewListener) *ConsensusView {
	c := &ConsensusView{
		consensusStatus: ConsensusReady,
		viewStartTime:   time.Unix(0, 0),
		// anchor 0 means "never anchored": currentOffsetLocked answers 0 until a
		// shared value arrives, which is what a node that has not taken part in a
		// round yet should report. The first startView or reset anchors it.
		anchor:        0,
		publicKey:     account,
		signTolerance: tolerance,
		producers:     producers,
		listener:      viewListener,
		isDposOnDuty:  false,
	}
	return c
}
