// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/consensusview.go.

package dpos

import (
	"bytes"
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
	// through its accessors, not protected from here; listener is set once by
	// the constructor.
	mu sync.RWMutex

	consensusStatus uint32
	viewOffset      uint32
	publicKey       []byte
	signTolerance   time.Duration
	viewStartTime   time.Time
	viewChangeTime  time.Time
	isDposOnDuty    bool
	producers       *Producers

	listener ViewListener
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

func (v *ConsensusView) resetViewOffsetLocked() {
	v.viewOffset = 0
}

func (v *ConsensusView) setChangViewTimeLocked(parentTime uint64) {
	headerTime := time.Unix(int64(parentTime), 0)
	v.viewChangeTime = headerTime.Add(v.signTolerance)
	v.viewStartTime = headerTime
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
	producer := v.producers.GetNextOnDutyProducer(v.viewOffset)
	return bytes.Equal(producer, account)
}

func (v *ConsensusView) updateDutyIndexLocked(height uint64) {
	v.producers.UpdateDutyIndex(height)

	currentProducer := v.producers.GetNextOnDutyProducer(v.viewOffset)
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
// file: ResetView forwards to SetChangViewTime, resetViewOffset writes the
// offset, and DumpInfo walks Producers. All of it happens under one write lock
// because halfway through, the view is internally inconsistent (new start time,
// old offset), and a reader that saw that pair would compute a deadline against
// the wrong start. The logging and the listener call are moved outside the lock:
// logging can block on I/O, and the listener runs into the miner.
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
	offset, offsetTime = v.calculateOffsetTime(v.viewStartTime, now)
	if offset > 0 {
		v.viewStartTime = now.Add(-offsetTime)
		// Forwarder, not leaf: SetChangViewTime writes the two times.
		v.setChangViewTimeLocked(uint64(v.viewStartTime.Unix()))
	}
	v.viewOffset += offset
	if force {
		offset = 1
		v.resetViewOffsetLocked()
		// Same forwarder, second call site.
		v.setChangViewTimeLocked(parentTime)
	}

	if offset > 0 {
		changed = true
		effective = offset
		startTime = v.viewStartTime
		changeTime = v.viewChangeTime
		viewOffset = v.viewOffset
		dutyIndex = v.producers.GetDutyIndex()
		onDutyProducer := v.producers.GetNextOnDutyProducer(v.viewOffset)
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
	str := v.dumpInfoLocked(v.producers.GetNextOnDutyProducer(v.viewOffset))
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

func (v *ConsensusView) GetViewOffset() uint32 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.viewOffset
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
// What it does not return is viewChangeTime; the receiver recomputes it from
// viewStartTime. That round trip holds only because viewStartTime is always
// stored at second granularity: ChangeView writes it with sub-second precision
// first, then calls setChangViewTimeLocked, which overwrites both times from the
// truncated Unix second. So this function's symmetry rests on one write order
// inside ChangeView, one line apart. Whoever writes viewStartTime directly and
// skips that call -- the half-way shape ChangeView itself uses immediately
// before it -- breaks the round trip for every recovering peer, and nothing will
// report it: the peer just resumes at a start time that is not the one this node
// used, with a viewChangeTime that never matched it.
func (v *ConsensusView) GetViewState() (status uint32, viewOffset uint32, viewStartTime time.Time) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.consensusStatus, v.viewOffset, v.viewStartTime
}

// ResetViewOffset is the exported form of the reset the Dispatcher uses when it
// restarts a round outside of a ChangeView.
func (v *ConsensusView) ResetViewOffset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.resetViewOffsetLocked()
}

// ResetView is a forwarder, not a leaf: its body is one call to
// SetChangViewTime. It takes the lock on its own only because it is exported;
// ChangeView calls the Locked twin directly, because taking this lock from
// inside ChangeView's write lock would deadlock on the same goroutine.
func (v *ConsensusView) ResetView(parentTime uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.setChangViewTimeLocked(parentTime)
}

func (v *ConsensusView) SetChangViewTime(parentTime uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.setChangViewTimeLocked(parentTime)
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
// Before R20 the same gaps were data races; now they are legal interleavings
// with names. That is better, it is not atomic, and calling it atomic would hide
// the remaining gap from the next reader. Closing it needs one Producers entry
// point that does all of them under a single acquisition, which is a separate
// change rather than a note here.
func (v *ConsensusView) RecoverFromStatus(status uint32, viewOffset uint32, viewStartTime time.Time, workingHeight uint64, finishedHeight uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.consensusStatus = status
	v.viewOffset = viewOffset
	v.setChangViewTimeLocked(uint64(viewStartTime.Unix()))
	// UpdateWorkingHeight, not the locked twin: workingHeight belongs to
	// Producers, and viewMu orders nothing about it. This nests viewMu ->
	// producers.mtx, which is the allowed direction.
	v.producers.UpdateWorkingHeight(workingHeight)
	// isDposOnDuty is assigned twice, and the second one is not redundant.
	// GetNextOnDutyProducer reads the dutyIndex field rather than deriving an
	// index, so this first assignment uses whatever dutyIndex the node carried
	// into the recovery; the second one, below, runs after UpdateDutyIndex has
	// recomputed it from the chain's finished height. The two answers can differ,
	// and the second is the final one. Removing the finishedHeight branch as
	// "duplicate work" would silently leave isDposOnDuty derived from a stale
	// dutyIndex -- and IsOnduty is a block-production gate (Seal asks it), so the
	// symptom would be a node that stops producing, not a wrong number in a log.
	//
	// What the finishedHeight == 0 case costs is much smaller, which is why it is
	// not a reason to change the condition: the duty index is not restored, and
	// every non-test consumer of it in this tree is a log line -- the End-Recover
	// summary and ChangeView's view summary. Block production goes through
	// isDposOnDuty, which was written above unconditionally.
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
	offset := v.viewOffset
	v.mu.RUnlock()
	producer := v.producers.GetNextOnDutyProducer(offset)
	return bytes.Equal(producer, account)
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
		viewOffset:      0,
		publicKey:       account,
		signTolerance:   tolerance,
		producers:       producers,
		listener:        viewListener,
		isDposOnDuty:    false,
	}
	return c
}
