// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/dispatcher.go.

package dpos

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/crypto"
	"github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"

	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
)

// Dispatcher is the round state machine: it accepts the current proposal,
// collects votes for it, and turns a 2/3 majority into a confirm.
//
// State is per-round and entirely in memory. finishedHeight is the only thing
// that outlives a round, and it is the irreversible height the reorg guard
// consumes.
//
// Lock order, stated as the edges that actually exist rather than as a slogan:
//
//	mu -> proposalMu   allowed, and required. ProcessVote holds mu and processVote
//	                   reaches GetProcessingProposal; createConfirm and
//	                   createUnConfirm do the same. proposalMu is a leaf.
//	proposalMu -> mu   forbidden. Nothing may acquire mu while holding
//	                   proposalMu -- which is why CleanProposals takes its two
//	                   locks in two separate critical sections instead of one.
//	mu -> viewMu       allowed, from this file's own CleanProposals branch and
//	                   nowhere else yet. viewMu is not a leaf: it nests into
//	                   producers.mtx, so the chain is mu -> viewMu -> producers.mtx.
//	viewMu -> mu       forbidden. Nothing may take mu while holding viewMu.
//	                   proposalMu stays a leaf and must not be taken under viewMu.
//
// The last two rows name a lock that does not exist yet: ConsensusView has no
// lock today, so GetViewOffset is a bare read and mu -> viewMu is not an edge
// there until R20 (#17) adds one. They are in the table early because that edge
// is created by this file's function body, not by its callers -- CleanProposals
// (true) has no caller in this tree -- and a lock order that only described
// today's call graph would have to be rewritten on the day someone wires the
// branch, by whoever is least equipped to notice. Note the direction: the
// dispatcher may take the view's lock, the view must never take the
// dispatcher's, because ConsensusView's callers include this file's critical
// sections and not the other way round.
//
// The forbidden edge is not a style rule, it is a deadlock: code that took
// proposalMu and then mu would run head-on into ProcessVote, which takes mu and
// then proposalMu.
//
// Two consequences that are easy to undo by "tidying up":
//
//   - CleanProposals' two locks must stay separate (see its own comment).
//   - Everything reached while mu is held must not take mu again, because
//     sync.RWMutex is not reentrant. That covers processVote and, through it, the
//     onConfirm/unConfirm callbacks. It is also why processVote takes the maps
//     directly instead of going through the exported accessors.
//
// mu is also taken by two entry points that must therefore never be called with
// mu already held: ProcessVote (obviously) and ProcessProposal, which takes it for
// its single map write to precociousProposals. ProcessProposal's callers are Seal
// (no locks held, consensus/pbft/seal.go) and, from M3, the p2p proposal
// handler -- both must stay lock-free at the call site. This is a precondition on
// the caller that the callee cannot check, and violating it is a self-deadlock
// rather than a data race, so -race will not find it.
//
// finishedMu guards the three finality fields (finishedHeight,
// finishedBlockSealHash, finishedProposal) and is the outermost lock in the file:
// it is never held while mu or proposalMu is taken, because FinishedProposal
// releases it before touching the view, and the accessors only read the fields.
//
// processingProposal is NOT guarded by finishedMu. It already has an owner,
// proposalMu, at its one write site (setProcessingProposal), its clearing site
// (setProcessingProsalNil) and its accessor (GetProcessingProposal); giving one
// field two locks is the defect, not the fix. Everything that touches it goes
// through proposalMu, including the two places that used to read it unsynchronised
// (FinishedProposal and createUnConfirm).
type Dispatcher struct {
	acceptVotes         map[elacom.Uint256]*payload.DPOSProposalVote
	rejectedVotes       map[elacom.Uint256]*payload.DPOSProposalVote
	pendingVotes        map[elacom.Uint256]*payload.DPOSProposalVote
	precociousProposals map[elacom.Uint256]*payload.DPOSProposal

	processingProposal *payload.DPOSProposal
	consensusView      *ConsensusView
	timeSource         dtime.MedianTimeSource

	onConfirm func(confirm *payload.Confirm) error
	unConfirm func(confirm *payload.Confirm) error

	finishedHeight        uint64
	finishedBlockSealHash elacom.Uint256
	finishedProposal      elacom.Uint256

	mu         sync.RWMutex
	proposalMu sync.RWMutex
	finishedMu sync.RWMutex

	resetViewRequests map[string]struct{} // sponsors
	resetViewMu       sync.RWMutex
}

func (d *Dispatcher) ProcessProposal(id peer.PID, proposal *payload.DPOSProposal) (err error, isSendReject bool, handled bool) {
	Info("[ProcessProposal] start ", proposal.Hash().String(), "peerID", id.String(), "Sponsor", elacom.BytesToHexString(proposal.Sponsor))
	defer Info("[ProcessProposal] end", proposal.Hash().String())
	self := bytes.Equal(id[:], proposal.Sponsor)
	Info("is self", self)
	// One read of the view offset, not two: the two reads below used to be
	// separate, so the comparison and the decision could be made against
	// different values while a view change was in flight.
	if currentOffset := d.GetConsensusView().GetViewOffset(); currentOffset != proposal.ViewOffset {
		Info("have different view offset")
		if proposal.ViewOffset > currentOffset {
			// This map is rebuilt by CleanProposals, so the write needs mu like
			// every other writer of the vote state. It matters from M3 onwards,
			// when proposals arrive on the p2p handler goroutine instead of
			// only on the miner's.
			d.mu.Lock()
			d.precociousProposals[proposal.Hash()] = proposal
			d.mu.Unlock()
		}
		return errors.New("have different view offset"), false, false
	}

	if !d.consensusView.ProducerIsOnDuty(proposal.Sponsor) {
		return errors.New("current signer is not onDuty"), false, !self
	}
	processingProposal := d.GetProcessingProposal()
	if processingProposal != nil && processingProposal.Hash().IsEqual(proposal.Hash()) {
		return errors.New("already processing this proposal:" + proposal.Hash().String()), false, true
	}

	if !d.consensusView.IsProducers(proposal.Sponsor) {
		str := fmt.Sprintf("%s proposal signer is not producer", elacom.BytesToHexString(proposal.Sponsor))
		return errors.New(str), true, true
	}
	err = CheckProposal(proposal)
	if err != nil {
		return err, true, true
	}

	d.setProcessingProposal(proposal)
	return nil, false, true
}

// GetFinishedHeight returns the highest height a confirm has finalised.
//
// It is read from the miner goroutine on every Prepare and Seal, while
// FinishedProposal writes it from the chain-head goroutine, which is why the
// field cannot simply be a uint64 on the struct.
func (d *Dispatcher) GetFinishedHeight() uint64 {
	d.finishedMu.RLock()
	defer d.finishedMu.RUnlock()
	return d.finishedHeight
}

func (d *Dispatcher) GetFinishedBlockSealHash() elacom.Uint256 {
	d.finishedMu.RLock()
	defer d.finishedMu.RUnlock()
	return d.finishedBlockSealHash
}

func (d *Dispatcher) GetFinishedProposal() elacom.Uint256 {
	d.finishedMu.RLock()
	defer d.finishedMu.RUnlock()
	return d.finishedProposal
}

func (d *Dispatcher) setProcessingProposal(p *payload.DPOSProposal) (finished bool) {
	Info("setProcessingProposal start")
	d.proposalMu.Lock()
	d.processingProposal = p
	d.proposalMu.Unlock()
	d.mu.Lock()
	defer func() {
		d.mu.Unlock()
		Info("setProcessingProposal end")
	}()
	for _, v := range d.pendingVotes {
		if v.ProposalHash.IsEqual(p.Hash()) {
			_, finished, _ := d.processVote(v)
			if finished {
				return finished
			}
		}
	}
	d.pendingVotes = make(map[elacom.Uint256]*payload.DPOSProposalVote)
	return false
}

func (d *Dispatcher) AddPendingVote(v *payload.DPOSProposalVote) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendingVotes[v.Hash()] = v
}

func (d *Dispatcher) ProcessVote(vote *payload.DPOSProposalVote) (succeed bool, finished bool, err error) {
	Info("[ProcessVote] start")
	defer Info("[ProcessVote] end")
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.processVote(vote)
}

func (d *Dispatcher) processVote(vote *payload.DPOSProposalVote) (succeed bool, finished bool, err error) {
	proposal := d.GetProcessingProposal()
	if proposal == nil {
		err = errors.New("not proposal to process vote")
		return false, false, err
	}

	if !vote.ProposalHash.IsEqual(proposal.Hash()) {
		err = errors.New("vote proposal is not processing proposal")
		return false, false, err
	}

	if d.alreadyExistVote(vote) {
		err = errors.New("alreadyExistVote")
		return false, false, err
	}

	if !d.consensusView.IsProducers(vote.Signer) {
		str := fmt.Sprintf("%s vote signer is not producer", elacom.BytesToHexString(vote.Signer))
		err = errors.New(str)
		return false, false, err
	}

	if err := CheckVote(vote); err != nil {
		return false, false, err
	}

	if vote.Accept {
		d.acceptVotes[vote.Hash()] = vote
		acceptCount := len(d.acceptVotes)
		Info("acceptVotes count:", acceptCount)
		if d.consensusView.IsMajorityAgree(acceptCount) {
			Info("Collect majority signs. Proposal confirmed.")
			confirm := d.createConfirm()
			if confirm != nil {
				err = d.onConfirm(confirm)
			}
			Info("Block confirmed.", "error", err)
			return true, true, err
		}
	} else {
		d.rejectedVotes[vote.Hash()] = vote
		Info("rejectedVotes count:", len(d.rejectedVotes))
		if d.consensusView.IsMajorityRejected(len(d.rejectedVotes)) {
			Info("Collect majority signs, reject proposal")
			if unconfirm := d.createUnConfirm(); unconfirm != nil {
				d.unConfirm(unconfirm)
			}
			return true, false, nil
		}
	}

	return true, false, nil
}

// FinishedProposal advances the irreversible height after a block is imported.
//
// It is called from consensus/pbft's chain-head handling, NOT from Finalize: in
// this client engine.Finalize runs on every imported block including side
// branches, so advancing finality there would let a side chain move the
// irreversible height. See docs/design/pbft-port-design.md D2.
//
// The "is this a fork block" comparison and the write that follows it have to be
// one critical section: split apart, two concurrent imports of heights h and h+1
// could both pass the comparison against h-1 and then write in either order,
// moving the watermark backwards. By contrast the view work at the bottom of this
// function must happen OUTSIDE the lock: SetReady/CleanProposals/ChangeView call
// back into the listener, which calls miner.RequestNewWork, and holding
// finishedMu across that would put a foreign goroutine's wake-up inside our
// critical section.
func (d *Dispatcher) FinishedProposal(height uint64, sealHash elacom.Uint256,
	headerTime uint64) {
	Info("FinishedProposal")

	// Read the proposal before taking finishedMu: proposalMu is never acquired
	// while finishedMu is held (see the lock order note on Dispatcher).
	processing := d.GetProcessingProposal()

	d.finishedMu.Lock()
	if height <= d.finishedHeight {
		d.finishedMu.Unlock()
		Warn("FinishedProposal received fork block", "height", height)
		return
	}
	d.finishedHeight = height
	d.finishedBlockSealHash = sealHash
	if processing != nil {
		d.finishedProposal = processing.Hash()
	}
	d.finishedMu.Unlock()

	d.consensusView.SetReady()
	d.CleanProposals(false)
	d.consensusView.UpdateDutyIndex(height)
	d.consensusView.ChangeView(d.timeSource.AdjustedTime(), true, headerTime)
	d.resetViewMu.Lock()
	d.resetViewRequests = make(map[string]struct{}, 0)
	d.resetViewMu.Unlock()
}

func (d *Dispatcher) ResetConsensus(height uint64) {
	Info("[resetConsensus] start", "d.consensusView.IsRunning()", d.consensusView.IsRunning())
	defer Info("[resetConsensus] end")

	if d.consensusView.IsRunning() {
		Info("[resetConsensus] reset view")
		d.consensusView.SetReady()
		d.CleanProposals(false)
		d.consensusView.ResetViewOffset()
		d.consensusView.UpdateDutyIndex(height)
		d.consensusView.ChangeView(d.timeSource.AdjustedTime(), true, uint64(d.timeSource.AdjustedTime().Unix()))
	}
	d.resetViewMu.Lock()
	d.resetViewRequests = make(map[string]struct{}, 0)
	d.resetViewMu.Unlock()
}

func (d *Dispatcher) ResetConsensusForEpochTransition(height uint64, parentTime uint64) {
	Info("[ResetConsensusForEpochTransition] start", "height", height)
	defer Info("[ResetConsensusForEpochTransition] end")

	d.consensusView.SetReady()
	d.CleanProposals(false)
	d.consensusView.ResetViewOffset()
	d.consensusView.UpdateDutyIndex(height)
	d.consensusView.ChangeView(d.timeSource.AdjustedTime(), true, parentTime)

	d.resetViewMu.Lock()
	d.resetViewRequests = make(map[string]struct{}, 0)
	d.resetViewMu.Unlock()
}

func (d *Dispatcher) OnResponseResetViewReceived(msg *emsg.ResetView) error {
	signer := msg.Sponsor
	sign := msg.Sign

	data := new(bytes.Buffer)
	if err := msg.SerializeUnsigned(
		data); err != nil {
		return err
	}

	pk, err := crypto.DecodePoint(signer)
	if err != nil {
		return err
	}

	if err := crypto.Verify(*pk, data.Bytes(), sign); err != nil {
		Errorf("invalid message signature: %v", *msg)
		return err
	}
	d.RecordViewRequest(signer)
	return nil
}

func (d *Dispatcher) GetResetViewReqCount() int {
	d.resetViewMu.Lock()
	defer d.resetViewMu.Unlock()
	return len(d.resetViewRequests)
}

func (d *Dispatcher) ResetViewRequestIsContain(sponsor []byte) bool {
	d.resetViewMu.Lock()
	defer d.resetViewMu.Unlock()
	if d.resetViewRequests == nil {
		return false
	}
	_, ok := d.resetViewRequests[elacom.BytesToHexString(sponsor)]
	return ok
}

func (d *Dispatcher) RecordViewRequest(sponsor []byte) {
	d.resetViewMu.Lock()
	defer d.resetViewMu.Unlock()
	if d.resetViewRequests == nil {
		d.resetViewRequests = make(map[string]struct{}, 0)
	}
	d.resetViewRequests[elacom.BytesToHexString(sponsor)] = struct{}{}
}

// CleanProposals discards the per-round proposal and vote state.
//
// mu guards this rebuild, and the position of the lock is the whole point: it is
// taken AFTER setProcessingProsalNil has returned, i.e. after proposalMu has been
// released. mu -> proposalMu is the one allowed edge in this file (see the lock
// order note on Dispatcher); acquiring mu while holding proposalMu would take
// them in the opposite order to ProcessVote, which holds mu and reaches
// GetProcessingProposal, and the two would deadlock. Do not merge these two
// critical sections.
//
// Before mu was taken here, this was one of the two writers of the vote maps
// that did not hold it (the other is RecoverFromConsensusStatus): AddPendingVote,
// ProcessVote and setProcessingProposal all do. A vote arriving while a round
// finished therefore raced with the rebuild, and in Go that is not a race report
// -- it is `fatal error: concurrent map read and map write`, which kills the
// process.
//
// The rebuild itself now goes through resetVoteMapsLocked, shared with the
// recovery path, so the two sites cannot drift apart again: there is one place
// that replaces the three maps and it takes a d.mu holder to call.
//
// Do not call this from the onConfirm/unConfirm callbacks: ProcessVote invokes
// them while holding mu, and sync.RWMutex is not reentrant. That holds today
// (engine.onConfirm only appends to the block pool and does a non-blocking
// channel send), and it has to keep holding.
// resetVoteMapsLocked replaces the three vote maps. The caller must hold d.mu.
//
// It exists so the two rebuild sites -- CleanProposals and
// RecoverFromConsensusStatus -- cannot drift apart: one of them held the lock and
// the other did not for as long as they were written independently, and the
// difference was invisible in both. The maps share one owner (mu) because they
// are written by ProcessVote and read by the confirm path; giving any one of them
// a second lock is the defect, not the fix.
//
// precociousProposals is deliberately not here: only CleanProposals rebuilds it,
// and the other site prunes rather than replaces.
func (d *Dispatcher) resetVoteMapsLocked() {
	d.acceptVotes = make(map[elacom.Uint256]*payload.DPOSProposalVote)
	d.rejectedVotes = make(map[elacom.Uint256]*payload.DPOSProposalVote)
	d.pendingVotes = make(map[elacom.Uint256]*payload.DPOSProposalVote)
}

func (d *Dispatcher) CleanProposals(changeView bool) {
	Info("Clean proposals")
	d.setProcessingProsalNil()

	d.mu.Lock()
	defer d.mu.Unlock()

	d.resetVoteMapsLocked()
	if !changeView {
		d.precociousProposals = make(map[elacom.Uint256]*payload.DPOSProposal)
	} else {
		// Not reachable today: every call site in this tree passes false -- the
		// three in this file above, and the race test. Upstream is not like that,
		// which is why this branch exists: the Elastos.ELA module's view-change
		// handlers pass true (dpos/manager/dposnormalhandler.go:89,
		// dposondutyhandler.go:57, proposaldispatcher.go:822) and so does
		// pgp-chain (consensus/pbft/pbft.go:1189, the !force path). This is
		// ported-but-not-wired, not abandoned: it starts running again when M3
		// connects the view-change handlers.
		//
		// When it does, the lock matters: this code runs inside mu (taken above)
		// and GetViewOffset takes viewMu, so this is the mu -> viewMu edge --
		// allowed in that direction only, as the lock order note on Dispatcher
		// says, and nothing reached from inside this critical section may take mu
		// again.
		// clear pending proposals less than current view offset
		currentOffset := d.consensusView.GetViewOffset()
		for k, v := range d.precociousProposals {
			if v.ViewOffset < currentOffset {
				delete(d.precociousProposals, k)
			}
		}
	}
}

func (d *Dispatcher) setProcessingProsalNil() {
	d.proposalMu.Lock()
	d.processingProposal = nil
	d.proposalMu.Unlock()
}

func (d *Dispatcher) alreadyExistVote(v *payload.DPOSProposalVote) bool {
	_, ok := d.acceptVotes[v.Hash()]
	if ok {
		Info("[alreadyExistVote]: ", elacom.BytesToHexString(v.Signer), "already in the AcceptVotes!")
		return true
	}

	_, ok = d.rejectedVotes[v.Hash()]
	if ok {
		Info("[alreadyExistVote]: ", elacom.BytesToHexString(v.Signer), "already in the RejectedVotes!")
		return true
	}

	return false
}

func (d *Dispatcher) AcceptProposal(proposal *payload.DPOSProposal, ac account.Account) *emsg.Vote {
	hash := proposal.Hash()

	vote, err := StartVote(&hash, true, ac)
	if err != nil {
		Error("StartVote error", "err", err)
		return nil
	}
	return &emsg.Vote{Command: emsg.CmdAcceptVote, Vote: *vote}
}

func (d *Dispatcher) RejectProposal(proposal *payload.DPOSProposal, ac account.Account) *emsg.Vote {
	if d.setProcessingProposal(proposal) {
		return nil
	}
	hash := proposal.Hash()
	vote, err := StartVote(&hash, false, ac)
	if err != nil {
		Error("StartVote error", "err", err)
		return nil
	}
	return &emsg.Vote{Command: emsg.CmdRejectVote, Vote: *vote}
}

func (d *Dispatcher) createConfirm() *payload.Confirm {
	processingProposal := d.GetProcessingProposal()
	if processingProposal == nil {
		Warn("processingProposal is nil, can't create confirm")
		return nil
	}
	confirm := &payload.Confirm{
		Proposal: *processingProposal,
		Votes:    make([]payload.DPOSProposalVote, 0),
	}
	for _, vote := range d.acceptVotes {
		confirm.Votes = append(confirm.Votes, *vote)
	}

	return confirm
}

// createUnConfirm builds the rejection message for the proposal being processed.
//
// It goes through GetProcessingProposal rather than reading the field: the field
// is guarded by proposalMu, and this runs on the vote goroutine while ProcessProposal
// may be writing it from a network goroutine. The nil check is what the direct
// dereference used to do implicitly -- by panicking.
func (d *Dispatcher) createUnConfirm() *payload.Confirm {
	processingProposal := d.GetProcessingProposal()
	if processingProposal == nil {
		Warn("processingProposal is nil, can't create unconfirm")
		return nil
	}
	confirm := &payload.Confirm{
		Proposal: *processingProposal,
		Votes:    make([]payload.DPOSProposalVote, 0),
	}
	for _, vote := range d.rejectedVotes {
		confirm.Votes = append(confirm.Votes, *vote)
	}

	return confirm
}

func (d *Dispatcher) IsProducer(account []byte) bool {
	return d.consensusView.IsProducers(account)
}

func (d *Dispatcher) ProducerIsOnDuty() bool {
	return d.consensusView.IsOnduty()
}

func (d *Dispatcher) GetProcessingProposal() *payload.DPOSProposal {
	d.proposalMu.Lock()
	defer d.proposalMu.Unlock()
	return d.processingProposal
}

func (d *Dispatcher) GetNextNeedConnectArbiters() []peer.PID {
	return d.consensusView.GetNextNeedConnectArbiters()
}

func (d *Dispatcher) GetCurrentNeedConnectArbiters() []peer.PID {
	return d.consensusView.getCurrentNeedConnectArbiters()
}

func (d *Dispatcher) OnChangeView() {
	d.consensusView.TryChangeView(d.timeSource.AdjustedTime())
}

func (d *Dispatcher) ResetView(parentTime uint64) {
	d.consensusView.ResetView(parentTime)
}

func (d *Dispatcher) GetConsensusView() *ConsensusView {
	return d.consensusView
}

func (d *Dispatcher) HelpToRecoverAbnormal(id peer.PID, height uint64, currentHeight uint64) *dmsg.ConsensusStatus {
	Info("[HelpToRecoverAbnormal]  peer id:", elacom.BytesToHexString(id[:]))

	if height > currentHeight {
		Warn("Requesting height greater than current processing height")
		return nil
	}
	if len(d.consensusView.GetProducers()) <= 0 {
		Warn("Requesting chain is pow state")
		return nil
	}
	status := &dmsg.ConsensusStatus{}
	// One snapshot, not three getters: a status, an offset and a start time read
	// separately can come from three different views, and the receiving peer
	// would resume at a turn that never existed.
	status.ConsensusStatus, status.ViewOffset, status.ViewStartTime = d.consensusView.GetViewState()

	// The vote and proposal payloads are deliberately not shipped: a recovering
	// peer only needs the view, the working height and the duty index to resume
	// at the right turn. Sending the votes would let a peer collect signatures
	// it never witnessed, which is exactly what the quorum is there to prevent.
	status.AcceptVotes = make([]payload.DPOSProposalVote, 0, len(d.acceptVotes))
	status.RejectedVotes = make([]payload.DPOSProposalVote, 0, len(d.rejectedVotes))
	status.PendingProposals = make([]payload.DPOSProposal, 0, 1)
	status.PendingVotes = make([]payload.DPOSProposalVote, 0, len(d.pendingVotes))
	status.WorkingHeight = d.consensusView.GetWorkingHeight()
	return status

}

func (d *Dispatcher) RecoverAbnormal(status *dmsg.ConsensusStatus, medianTime int64) {
	Info("RecoverAbnormal", "status", status.ConsensusStatus, "viewOffset", status.ViewOffset, "viewStartTime", status.ViewStartTime)
	if status == nil {
		return
	}
	status.ViewStartTime = dtime.Int64ToTime(medianTime)
	if medianTime != 0 {
		offset, offsetTime := d.consensusView.calculateOffsetTime(status.ViewStartTime, d.timeSource.AdjustedTime())
		status.ViewOffset += offset
		status.ViewStartTime = d.timeSource.AdjustedTime().Add(-offsetTime)
	}
	d.RecoverFromConsensusStatus(status)
}

func (d *Dispatcher) RecoverFromConsensusStatus(status *dmsg.ConsensusStatus) error {
	Info("RecoverFromConsensusStatus", "status", status.ConsensusStatus, "viewOffset", status.ViewOffset, "viewStartTime", status.ViewStartTime)
	// setProcessingProsalNil takes proposalMu and releases it before returning, so
	// it has to happen before mu is taken. Taking mu while holding proposalMu is
	// the forbidden direction (see the lock order note on Dispatcher), and it is
	// the direction ProcessVote runs in, so getting this wrong is an ABBA
	// deadlock rather than a race.
	d.setProcessingProsalNil()

	// The vote maps have exactly one owner: d.mu. CleanProposals was the rebuild
	// site that took it; this one did not, which is the same invariant broken in
	// the same shape -- a vote arriving during a recovery would have raced a map
	// rebuild and killed the process with `fatal error: concurrent map read and
	// map write`. Both sites now call resetVoteMapsLocked, so they cannot drift
	// apart again.
	//
	// No defer on purpose. The log line and DumpInfo below must stay outside the
	// lock: after R20, DumpInfo takes viewMu, so a deferred unlock would hold mu
	// across a view lock and across a write to the log sink.
	//
	// Unreachable in this tree today -- HelpToRecoverAbnormal and RecoverAbnormal
	// have no callers until the recovery messages are wired -- and reachable on
	// the day M3 connects them (upstream calls this from network.go:597 and
	// :924). Ported-but-not-wired, not dead: the same status as CleanProposals'
	// changeView branch.
	d.mu.Lock()
	d.resetVoteMapsLocked()
	d.mu.Unlock()

	// One call, one critical section. Doing this through the exported setters
	// would release the view lock between the offset and the start time, and a
	// reader in that gap sees the recovered offset against the pre-recovery
	// start time -- a deadline computed from a pair that never existed.
	d.consensusView.RecoverFromStatus(status.ConsensusStatus, status.ViewOffset,
		status.ViewStartTime, status.WorkingHeight, d.GetFinishedHeight())
	Info("-------[End RecoverFromConsensusStatus]--------", "startTime", d.consensusView.GetViewStartTime(), "WorkingHeight", status.WorkingHeight, "dutyIndex", d.GetConsensusView().GetDutyIndex(), "d.finishedHeight", d.GetFinishedHeight(), "status.ViewOffset", status.ViewOffset)
	d.consensusView.DumpInfo()
	return nil
}

func (d *Dispatcher) GetNowTime() time.Time {
	return d.timeSource.AdjustedTime()
}

func NewDispatcher(producers [][]byte, onConfirm func(confirm *payload.Confirm) error,
	unConfirm func(confirm *payload.Confirm) error, tolerance time.Duration, publicKey []byte,
	medianTime dtime.MedianTimeSource, viewListener ViewListener, dposStartHeight uint64) *Dispatcher {
	return &Dispatcher{
		acceptVotes:         make(map[elacom.Uint256]*payload.DPOSProposalVote),
		rejectedVotes:       make(map[elacom.Uint256]*payload.DPOSProposalVote),
		pendingVotes:        make(map[elacom.Uint256]*payload.DPOSProposalVote),
		precociousProposals: make(map[elacom.Uint256]*payload.DPOSProposal),
		consensusView:       NewConsensusView(tolerance, publicKey, NewProducers(producers, dposStartHeight), viewListener),
		onConfirm:           onConfirm,
		unConfirm:           unConfirm,
		timeSource:          medianTime,
	}
}
