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

package pbft

import (
	"bytes"
	"math/big"
	"sync"
	"testing"
	"time"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	emsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	dpeer "github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// stubTransport records what the engine tried to send. It is a Transport, so the
// 8 method set is the contract being exercised: if the interface grows, this
// stops compiling rather than silently swallowing a message kind.
type stubTransport struct {
	mu        sync.Mutex
	toPeer    []elap2p.Message
	sentTo    []dpeer.PID
	broadcast []elap2p.Message
}

func (s *stubTransport) BroadcastBlock(*types.Block)                      {}
func (s *stubTransport) BroadcastProposal(*payload.DPOSProposal)          {}
func (s *stubTransport) BroadcastVote(*emsg.Vote)                         {}
func (s *stubTransport) BroadcastConfirm(*payload.Confirm, uint64)        {}
func (s *stubTransport) BroadcastResetView(*emsg.ResetView)               {}
func (s *stubTransport) BroadcastRaw(msg elap2p.Message)                  { s.record(nil, msg) }
func (s *stubTransport) BroadcastRawExcept(m elap2p.Message, _ dpeer.PID) { s.record(nil, m) }

func (s *stubTransport) SendToPeer(pid dpeer.PID, msg elap2p.Message) error {
	s.record(&pid, msg)
	return nil
}

func (s *stubTransport) record(pid *dpeer.PID, msg elap2p.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pid != nil {
		s.toPeer = append(s.toPeer, msg)
		s.sentTo = append(s.sentTo, *pid)
		return
	}
	s.broadcast = append(s.broadcast, msg)
}

func (s *stubTransport) peerMessages() []elap2p.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]elap2p.Message(nil), s.toPeer...)
}

func testPID(fill byte) dpeer.PID {
	var pid dpeer.PID
	for i := range pid {
		pid[i] = fill
	}
	return pid
}

// newListenerEngine builds a one-producer engine whose key is attached, i.e. the
// shape an operating producer has once the assembly layer has opened its
// keystore. MarkRecovered is not called on purpose: the recovery tests below
// assert on the gate itself.
func newListenerEngine(t *testing.T) (*Engine, dpeer.PID) {
	t.Helper()
	accounts, keys := newProducerSet(t, 1)
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: keys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	t.Cleanup(func() { engine.Close() })
	return engine, testPID(0x01)
}

// TestNetworkListenerDoesNotPanicOnAnEmptyEngine calls every method of the
// listener on an engine that has no key, no chain and no transport.
//
// This is not a tautology. The whole 24-method set is invoked from dpos.Network's
// single message goroutine, and an out-of-range index or a nil map write there
// takes the consensus node down with a panic that no recover() catches. A
// verifier-only node (production disabled, EmptyBlockPeriod set, or simply a node
// whose keystore failed to open) runs exactly this configuration, so every guard
// in network_listener.go has to hold on it.
func TestNetworkListenerDoesNotPanicOnAnEmptyEngine(t *testing.T) {
	engine := New(&params.LegacyConfig{}, &Config{})
	defer engine.Close()

	id := testPID(0x02)
	hash := elacom.Uint256{1, 2, 3}

	// Block messages: a nil payload must be dropped by the decoder, not by a
	// panic in the handler.
	badBlock := dmsg.NewBlockMsg(nil)

	engine.OnPing(id, 7)
	engine.OnPong(id, 7)
	engine.OnBlock(id, badBlock)
	engine.OnInv(id, hash)
	engine.OnGetBlock(id, hash)
	engine.OnGetBlocks(id, 1, 2)
	engine.OnResponseBlocks(id, nil)
	engine.OnRequestConsensus(id, 1)
	engine.OnResponseConsensus(id, &dmsg.ConsensusStatus{})
	engine.OnRequestProposal(id, hash)
	engine.OnIllegalProposalReceived(id, &payload.DPOSIllegalProposals{})
	engine.OnIllegalVotesReceived(id, &payload.DPOSIllegalVotes{})

	engine.OnProposalReceived(id, &payload.DPOSProposal{})
	engine.OnVoteAccepted(id, &payload.DPOSProposalVote{})
	engine.OnVoteRejected(id, &payload.DPOSProposalVote{})

	engine.OnChangeView()
	engine.OnBadNetwork()
	engine.OnRecover()
	engine.OnResponseResetViewReceived(&emsg.ResetView{})

	engine.OnBlockReceived(id, badBlock, false)
	engine.OnConfirmReceived(id, &payload.Confirm{}, 0)

	engine.OnSmallCroTxReceived(id, dmsg.NewSmallCroTx("sig", "tx"))
	engine.OnFailedWithdrawTxReceived(id, dmsg.NewFailedWithdrawTx("sig", "hash"))

	engine.OnProducersMsg(dmsg.NewProducersMsg(0, 0, nil))
}

// TestOnInvRequestsTheBlockOnce pins the sync path's two invariants: the request
// goes to the sender as a unicast (a broadcast would make every producer answer
// the same GetBlock), and the same hash is not requested twice while the first
// request is still outstanding.
func TestOnInvRequestsTheBlockOnce(t *testing.T) {
	engine, id := newListenerEngine(t)
	transport := &stubTransport{}
	engine.SetTransport(transport)

	hash := elacom.Uint256{9, 9, 9}
	engine.OnInv(id, hash)

	deadline := time.Now().Add(2 * time.Second)
	for len(transport.peerMessages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	msgs := transport.peerMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one unicast message, got %d", len(msgs))
	}
	getBlock, ok := msgs[0].(*emsg.GetBlock)
	if !ok {
		t.Fatalf("expected *emsg.GetBlock, got %T", msgs[0])
	}
	if !getBlock.BlockHash.IsEqual(hash) {
		t.Fatalf("requested the wrong hash: %s", getBlock.BlockHash.String())
	}
	if got := transport.sentTo[0]; got != id {
		t.Fatalf("GetBlock went to %s, want the sender %s", got.String(), id.String())
	}

	// Second announcement while the first is outstanding: no new request.
	engine.OnInv(id, hash)
	time.Sleep(50 * time.Millisecond)
	if n := len(transport.peerMessages()); n != 1 {
		t.Fatalf("duplicate OnInv produced %d requests, want 1", n)
	}
}

// TestOnInvIgnoresNonProducers is the guard that keeps an arbitrary DPoS peer
// from making this node serve block bodies: OnInv is only answered when the
// engine's key is in the configured producer set.
//
// The engine is given a perfectly valid key that is NOT one of the producers --
// the shape of a node that was demoted, or that was pointed at the wrong
// keystore. It must answer nothing.
func TestOnInvIgnoresNonProducers(t *testing.T) {
	_, producerKeys := newProducerSet(t, 3)
	outsiders, _ := newProducerSet(t, 1)

	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, &Config{Enable: true})
	engine.attachAccount(outsiders[0])
	defer engine.Close()

	if engine.IsProducer() {
		t.Fatal("test setup is wrong: the outsider key is in the producer set")
	}

	transport := &stubTransport{}
	engine.SetTransport(transport)
	engine.OnInv(testPID(0x03), elacom.Uint256{4, 5, 6})

	time.Sleep(50 * time.Millisecond)
	if n := len(transport.peerMessages()); n != 0 {
		t.Fatalf("a node outside the producer set answered OnInv (%d messages)", n)
	}
}

// TestOnGetBlockServesAPooledBlock pins the serve path end to end.
//
// The failure this guards against is a byte-level one, not a type one: what
// OnGetBlock must produce is a BlockMsg whose payload decodes back to the very
// block the pool holds. Encoding the *dBlock wrapper instead of the *types.Block
// it embeds, or encoding the seal hash instead of the block, both compile and
// both produce a message the peer silently cannot import. (The other mistake --
// asserting the pooled value to *types.Block -- is caught by the compiler, since
// *types.Block does not implement dpos.DBlock.)
func TestOnGetBlockServesAPooledBlock(t *testing.T) {
	engine, id := newListenerEngine(t)
	transport := &stubTransport{}
	engine.SetTransport(transport)

	block := types.NewBlockWithHeader(&types.Header{
		Number:     big.NewInt(7),
		Time:       uint64(time.Now().Unix()),
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	})
	engine.blockPool.AddToBlockMap(newDBlock(block))

	sealHash := elacom.Uint256(pgp.SealHash(block.Header()))
	engine.OnGetBlock(id, sealHash)

	deadline := time.Now().Add(2 * time.Second)
	for len(transport.peerMessages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	msgs := transport.peerMessages()
	if len(msgs) != 1 {
		t.Fatalf("OnGetBlock answered with %d messages, want 1", len(msgs))
	}
	blockMsg, ok := msgs[0].(*dmsg.BlockMsg)
	if !ok {
		t.Fatalf("expected *dmsg.BlockMsg, got %T", msgs[0])
	}

	// The bytes must round-trip back to the same block, otherwise the peer gets
	// something it cannot import even though a message was sent.
	var decoded types.Block
	if err := decoded.DecodeRLP(rlp.NewStream(bytes.NewReader(blockMsg.GetData()), 0)); err != nil {
		t.Fatalf("served bytes do not decode: %v", err)
	}
	if decoded.Hash() != block.Hash() {
		t.Fatalf("served block %s, want %s", decoded.Hash().String(), block.Hash().String())
	}
}

// TestRecoverIsSingleFlight pins the recovery gate.
//
// Two concurrent status exchanges would both write statusMap and both call
// RecoverAbnormal, and the second would move the view a second time -- the
// classic "one hiccup becomes a two-view stall" bug. beginRecover must admit one
// caller only, and finishRecover must reopen the slot exactly once.
func TestRecoverIsSingleFlight(t *testing.T) {
	engine, _ := newListenerEngine(t)

	if !engine.beginRecover() {
		t.Fatal("first beginRecover was refused")
	}
	if engine.beginRecover() {
		t.Fatal("second beginRecover was admitted while one was in flight")
	}
	if !engine.isRecoverStarted() {
		t.Fatal("isRecoverStarted is false during an exchange")
	}

	if !engine.finishRecover() {
		t.Fatal("finishRecover did not claim the in-flight exchange")
	}
	if engine.finishRecover() {
		t.Fatal("finishRecover claimed an exchange twice")
	}
	if engine.isRecoverStarted() {
		t.Fatal("isRecoverStarted stayed true after finishRecover")
	}
	if !engine.beginRecover() {
		t.Fatal("the slot did not reopen after finishRecover")
	}
}

// TestRecoverAbnormalStateNeedsEnoughPeers is the M4 acceptance floor: with a
// single reachable peer and a 12-producer set, a status exchange cannot reach the
// majority threshold, so it must not start at all -- otherwise every restart
// burns the full recoverWaitTimeout before producing.
func TestRecoverAbnormalStateNeedsEnoughPeers(t *testing.T) {
	accounts, keys := newProducerSet(t, 12)
	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: keys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.SetTransport(&stubTransport{})
	engine.SetPeerCounter(func() int { return 1 })
	defer engine.Close()

	if engine.recoverAbnormalState() {
		t.Fatal("recovery started with 1 peer and a majority threshold of 8")
	}
	if engine.isRecoverStarted() {
		t.Fatal("the single-flight slot was claimed by a refused recovery")
	}
}

// TestPickRecoverStatusPrefersHighestOffset covers the bucket choice, which is
// all that is left of this function now: it decides whose WorkingHeight gets
// adopted. Picking the wrong bucket silently rewinds the set.
//
// The median it used to return is covered by
// TestRecoveryAnchorsOnTheHeadNotOnThePeerClocks below, because the anchor is no
// longer this function's business.
func TestPickRecoverStatusPrefersHighestOffset(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	status := func(offset uint32, start time.Time) *dmsg.ConsensusStatus {
		return &dmsg.ConsensusStatus{ViewOffset: offset, ViewStartTime: start, WorkingHeight: uint64(offset) + 100}
	}

	m := map[uint32]map[string]*dmsg.ConsensusStatus{
		0: {
			"a": status(0, base.Add(-30*time.Second)),
			"b": status(0, base.Add(-31*time.Second)),
		},
		3: {
			"c": status(3, base.Add(1*time.Second)),
			"d": status(3, base.Add(3*time.Second)),
			"e": status(3, base.Add(2*time.Second)),
		},
	}
	if got := countStatuses(m); got != 5 {
		t.Fatalf("countStatuses = %d, want 5", got)
	}

	picked := pickRecoverStatus(m)
	if picked == nil || picked.ViewOffset != 3 {
		t.Fatalf("picked offset %v, want 3", picked)
	}
	if picked.WorkingHeight != 103 {
		t.Fatalf("picked working height %d, want one from the offset-3 bucket (103)", picked.WorkingHeight)
	}

	if s := pickRecoverStatus(nil); s != nil {
		t.Fatalf("empty status map produced %v", s)
	}
}

// TestRecoveryAnchorsOnTheHeadNotOnThePeerClocks is the regression test for the
// last known anchor divergence entry point.
//
// Five nodes run the same recovery exchange over the same head, and each of them
// receives a different set of peer statuses -- which is the normal case, because
// the status map holds whatever answered this node. The head header is identical
// on all five, so the anchors have to be identical too. Under the implementation
// this replaces, the anchor was the median of the received bucket's
// ViewStartTime, and the cases below land in three different places:
//
//   - a peer window a whole number of tolerances behind ours ("36s behind")
//     rounds to the same point as ours, so the gap that looks alarming is the
//     harmless one. 36 is a multiple of 6: this is the case that proves the
//     obvious construction cannot split the grid at all.
//   - a peer window half a tolerance off ours ("3s behind", i.e. exactly one
//     blockPeriod) rounds to a different grid point, and the two nodes then
//     disagree about whose turn it is. This is not an exotic input: a peer
//     anchors its window on the last block it imported, blocks are 3 seconds
//     apart, so peers sit 3 seconds apart as often as they sit 6.
//   - two peer windows are averaged (medianInt64 returns the mean of the middle
//     pair for an even sample), so the anchor lands between them, on a
//     coordinate no node occupies at all.
//
// The last case pins what the old usableViewStart filter used to guard: a peer
// that has never opened a view reports the zero time, which serialises as the
// unix epoch and used to decode as a real coordinate, i.e. an offset in the
// hundreds of millions. It is now structurally harmless -- nothing on this path
// reads a peer's start time -- and the case is here so that a future change that
// starts reading it again has to face the epoch.
func TestRecoveryAnchorsOnTheHeadNotOnThePeerClocks(t *testing.T) {
	accounts, producerKeys := newProducerSet(t, 6)

	// A head 93 seconds old: the chain this path exists for is the one that has
	// stopped producing, so the head is not fresh. 93 rather than 90 on purpose --
	// 90 is a multiple of signTolerance, so the derived offset would sit exactly on
	// a boundary and which side the run falls on would be measuring the wall clock.
	const headAge = 93 * time.Second
	now := time.Now()
	headTime := uint64(now.Add(-headAge).Unix())
	chain := &sealTestChain{genesis: &types.Header{
		Number:     big.NewInt(21),
		Time:       headTime,
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}}

	// recover runs one exchange to completion and returns the anchor the node
	// landed on, and the window start it derived from it.
	recover := func(t *testing.T, offset uint32, peers map[string]time.Time) (uint64, time.Time) {
		t.Helper()
		engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, &Config{Enable: true})
		engine.attachAccount(accounts[0])
		engine.AttachChain(chain)
		t.Cleanup(func() { _ = engine.Close() })

		bucket := make(map[string]*dmsg.ConsensusStatus, len(peers))
		for name, start := range peers {
			bucket[name] = &dmsg.ConsensusStatus{
				ViewOffset:    offset,
				ViewStartTime: start,
				WorkingHeight: 21,
			}
		}
		engine.statusMapMu.Lock()
		engine.statusMap = map[uint32]map[string]*dmsg.ConsensusStatus{offset: bucket}
		engine.statusMapMu.Unlock()

		if !engine.beginRecover() {
			t.Fatal("beginRecover was refused")
		}
		engine.finishRecoverAndApply()
		view := engine.dispatcher.GetConsensusView()
		return view.GetAnchor(), view.GetViewStartTime()
	}

	cases := []struct {
		name   string
		offset uint32
		peers  map[string]time.Time
	}{
		{
			name:   "one peer, window 72s behind this node",
			offset: 5,
			peers:  map[string]time.Time{"a": now.Add(-72 * time.Second)},
		},
		{
			name:   "one peer, window 36s behind the first (a whole number of views)",
			offset: 11,
			peers:  map[string]time.Time{"b": now.Add(-108 * time.Second)},
		},
		{
			name:   "one peer, window one blockPeriod behind the first",
			offset: 5,
			peers:  map[string]time.Time{"c": now.Add(-75 * time.Second)},
		},
		{
			name:   "two peers 3s apart, so the median is averaged",
			offset: 5,
			peers: map[string]time.Time{
				"a": now.Add(-72 * time.Second),
				"c": now.Add(-75 * time.Second),
			},
		},
		{
			name:   "a peer that never opened a view",
			offset: 0,
			peers:  map[string]time.Time{"virgin": {}},
		},
	}

	var (
		want   uint64
		starts []time.Time
	)
	for i, tc := range cases {
		got, start := recover(t, tc.offset, tc.peers)
		starts = append(starts, start)
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("%s: anchored at %d, want %d (%d seconds apart) -- a peer's view "+
				"start time reached the anchor", tc.name, got, want, int64(got)-int64(want))
		}
	}

	// And the shared anchor has to be the head's timestamp itself: not merely a value
	// equal across nodes, and not merely a value sitting on the head's grid.
	//
	// This used to assert "headTime plus a whole number of tolerances, inside one
	// tolerance of now", which is the shape the anchor had while RecoverAbnormal
	// rebuilt it as anchor + signTolerance*floor((now-anchor)/signTolerance)
	// (dpos/dispatcher.go:804 before it was removed). Two nodes produce that shape
	// identically only if they land in the same grid window; the rig showed what
	// happens when they do not -- six producers split 4/2 with anchors 16:24:26Z and
	// 16:24:32Z, the lagging pair then reporting a viewOffset one lower for good and
	// losing the vote. Snapping to the current window is the part that makes the
	// anchor per-node, so the assertion is exact equality: any whole-window
	// translation of the head's timestamp is a different origin, hence a different
	// grid.
	if want != headTime {
		t.Fatalf("anchor %d, want the head's timestamp %d (%d seconds, %d whole "+
			"tolerances apart): the anchor is the grid origin, every producer reads the "+
			"same Time out of the same header, and a translated origin is a per-node grid",
			want, headTime, int64(want)-int64(headTime),
			(int64(want)-int64(headTime))/int64(signTolerance.Seconds()))
	}

	// The value that IS supposed to be near now is the window start: reanchorLocked
	// places it at the beginning of the window the caller's clock falls in
	// (dpos/consensusview.go:167-181), and the head is 93s old here, so that window
	// start is headTime+90s = 3s behind now. This is the assertion the old one was
	// reaching for, moved to the field it actually describes.
	for _, start := range starts {
		if behind := now.Sub(start); behind < 0 || behind > signTolerance {
			t.Fatalf("viewStartTime %v is %v away from the node's clock; the window the "+
				"derived offset selects must be the one now falls in (head is %v old, "+
				"signTolerance %v)", start, behind, headAge, signTolerance)
		}
	}
	t.Logf("head at %d, all five nodes anchored at %d (+0s), window start %v (%v behind their clocks)",
		headTime, want, starts[0], now.Sub(starts[0]))
}

// TestRecoveryAnchorWithoutAHeadIsTheClockNotNull covers the one degraded case in
// recoveryAnchor.
//
// A node that has not read a head header yet has no shared coordinate, so it
// anchors on its own clock and logs it. What it must NOT do is return 0:
// ConsensusView.reanchorLocked treats 0 as "never anchored", so a zero here would
// leave the node deriving offset 0 forever instead of the 15 views its head is
// old, i.e. pinned on the wrong duty slot with no log line to explain it. The
// engine below has no chain attached, which is exactly that window.
func TestRecoveryAnchorWithoutAHeadIsTheClockNotNull(t *testing.T) {
	engine, _ := newListenerEngine(t)

	anchor := engine.recoveryAnchor()
	if anchor <= 0 {
		t.Fatalf("recoveryAnchor returned %d with no head header; 0 is the never-anchored sentinel", anchor)
	}
	got := time.Unix(anchor/int64(time.Second), 0)
	if drift := time.Since(got); drift < -time.Minute || drift > time.Minute {
		t.Fatalf("the fallback anchor is %v, which is %v away from now; it must be this node's clock",
			got, drift)
	}
	t.Logf("no head header: anchored on the local clock at %v instead of leaving the sentinel", got.UTC())
}

// TestAnnounceHookIsOptional documents that re-announcing is a hook the assembly
// layer may omit. A nil hook must not be called and must not panic: OnBlockReceived
// reaches this path from the DPoS message goroutine.
func TestAnnounceHookIsOptional(t *testing.T) {
	engine, _ := newListenerEngine(t)

	engine.announceAddr() // no hook installed: no-op

	var calls int
	engine.SetAnnounceAddr(func() { calls++ })
	engine.announceAddr()
	if calls != 1 {
		t.Fatalf("announce hook called %d times, want 1", calls)
	}
}
