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

// The pbft_ RPC namespace: the only way to see the DPoS side of a running node.
//
// Everything consensus-relevant on this chain happens on a second TCP server
// that net_peerCount knows nothing about. devp2p can be fully meshed -- four
// peers, all connected -- while the DPoS connect list is empty or half-dialed,
// and no existing RPC distinguishes those two states. That is not a cosmetic
// observability gap: it is exactly the ambiguity that makes "peers are
// connected but blocks stop" so hard to read from the outside.
//
// Upstream exposes the same four methods on the same object
// (pgp-chain/consensus/pbft/api.go:21-60, registered at
// pgp-chain/consensus/pbft/pbft.go:859-866). This port adds Status, which
// upstream does not have, because the questions that actually get asked of a
// stuck bench are about the view -- see its doc comment.
//
// The namespace is Public:false, matching upstream. It has to be named
// explicitly in --http.api (or --ws.api); see node/rpcstack.go:641-650, which
// logs "Unavailable modules in HTTP API list" and silently skips it otherwise.

import (
	"time"

	dpp2p "github.com/elastos/Elastos.ELA/dpos/p2p"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/rpc"
)

// PeerInfo is one row of the DPoS connect list as the RPC reports it.
//
// It is NOT dpp2p.PeerInfo. That struct's ConnState is an enum whose JSON form
// is a number, and the whole point of this table is the direction in words
// ("2WayConnection", "InboundOnly", "OutboundOnly", "NoneConnection") -- the
// asymmetry between two nodes that each think they dialed the other is the
// first thing to look at when votes go missing.
//
// OwnerPublicKey is dropped from upstream's peerInfo
// (pgp-chain/consensus/pbft/network.go:115-121): it is the chainbridge arbiter
// owner key, chainbridge is not ported (design D10), so the field could only
// ever be an empty column.
type PeerInfo struct {
	// NodePublicKey is the peer PID, hex encoded: a 33-byte compressed P-256
	// public key, which is also the producer identity in the legacy config.
	NodePublicKey string `json:"nodepublickey"`

	// IP is host:port. "unknown" here means the sync layer wants this peer but
	// never learned its address -- the DAddr relay has not completed.
	IP string `json:"ip"`

	// ConnState is one of NoneConnection / OutboundOnly / InboundOnly /
	// 2WayConnection.
	ConnState string `json:"connstate"`

	// NodeVersion is what the remote advertised in its Version message.
	NodeVersion string `json:"nodeversion"`

	// IsProducer says whether this PID is in the voting set. A row that is
	// connected but not a producer is a live connection that cannot contribute
	// a vote.
	IsProducer bool `json:"isproducer"`
}

// Status is the consensus state of this node.
//
// It exists because every field here is needed together to interpret a stall,
// and none of them is otherwise reachable over RPC:
//
//   - activedpospeers < majoritycount means votes cannot arrive, full stop.
//   - activedpospeers >= majoritycount with finishedheight stuck means the votes
//     arrive and are rejected, which is a view problem, not a network problem.
//   - viewoffset differing between nodes is that view problem, stated directly:
//     peers only accept a proposal carrying their own offset, so a set whose
//     offsets have drifted rejects every proposal from the node that is ahead or
//     behind. See dpos/dispatcher.go's ProcessProposal.
//   - isrecovered false means production is gated off entirely and Prepare is
//     returning ErrWaitRecoverStatus, which the miner logs only at Debug.
type Status struct {
	IsProducer          bool   `json:"isproducer"`
	IsOnDuty            bool   `json:"isonduty"`
	IsRecovered         bool   `json:"isrecovered"`
	IsProductionEnabled bool   `json:"isproductionenabled"`
	IsRunning           bool   `json:"isrunning"`
	ViewOffset          uint32 `json:"viewoffset"`
	ProducerCount       int    `json:"producercount"`
	MajorityCount       int    `json:"majoritycount"`
	ActiveDPoSPeers     int    `json:"activedpospeers"`
	FinishedHeight      uint64 `json:"finishedheight"`
	DPoSNetworkRunning  bool   `json:"dposnetworkrunning"`

	// DutyIndex is whose turn the rotation believes it is on, and the two
	// timestamps are the anchors the offset is derived from. Two nodes with
	// equal offsets can still diverge on the next tick if their viewStartTime
	// differ, so the raw anchors are reported rather than just the derived
	// offset.
	DutyIndex      uint32 `json:"dutyindex"`
	ViewStartTime  string `json:"viewstarttime"`
	ChangeViewTime string `json:"changeviewtime"`
	HeadHeight     uint64 `json:"headheight"`

	// DutyProducer is the key the rotation puts on duty for this view:
	// (dutyindex + viewoffset) % producercount, resolved to a producer. It is
	// the one field that turns the two numbers above into an answer, and it
	// earns its place: on the six-producer rig two nodes reported different
	// viewoffset values that BOTH pointed at a key holder, so the chain had two
	// producers claiming the same round at once. Reading two offset columns per
	// node and doing the modulo in your head is how that went unnoticed.
	//
	// Compare it against isonduty on the same line, and across nodes: a set in
	// agreement reports one identical dutyproducer everywhere, and every node
	// whose isonduty is true is that key.
	DutyProducer string `json:"dutyproducer"`

	// Anchor and Now are the two inputs viewoffset is derived from:
	// viewoffset = (now - anchor) / signature tolerance, in whole seconds.
	//
	// They are here because viewoffset alone is not checkable. A node reported
	// viewoffset 0 while the last reset in its log carried an anchor half an hour
	// old, which derives to 285 rather than 0, and that reading was consistent
	// with three different faults: a derivation that is not in force, an anchor
	// written by something other than the reset, or a view clock that is not the
	// wall clock. Printing the inputs makes the arithmetic checkable in one line
	// per node instead of three hypotheses.
	//
	// Now is the same clock the derivation uses, which is the median time source
	// when one is installed, not necessarily time.Now().
	Anchor uint64 `json:"anchor"`

	// WallOffset is how far the clock the derivation uses sits from time.Now(),
	// in seconds, measured on the same line so a reader does not have to
	// subtract two machines' clocks. NowOffset is the time source's own
	// Offset(), i.e. the median of the samples it was fed -- 0 means it was
	// never fed any, which is the answer to "is AdjustedTime() polluted" that
	// cannot be read off a log.
	WallOffset int64 `json:"walloffset"`
	NowOffset  int64 `json:"nowoffset"`

	Now uint64 `json:"now"`
}

// API is the pbft RPC service.
type API struct {
	engine *Engine
}

// NewAPI wraps an engine in the RPC service. It tolerates a nil engine so a
// node without a legacy PBFT config never hands a nil service to the RPC
// registry; every method then answers with zero values.
func NewAPI(engine *Engine) *API { return &API{engine: engine} }

// APIs returns the pbft namespace, for the assembly layer to append to
// eth.Ethereum.APIs.
//
// Public:false is upstream's choice (pgp-chain/consensus/pbft/pbft.go:859-866)
// and is kept: the namespace mutates nothing except by re-announcing our
// address, but it does expose producer identities and connection topology,
// which is not something an unauthenticated public endpoint should hand out by
// default.
func (e *Engine) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: "pbft",
			Version:   "1.0",
			Service:   NewAPI(e),
			Public:    false,
		},
	}
}

// AnnounceDAddr re-runs the address announcement and reports whether it did.
//
// This is the manual kick for the one case the automatic path cannot cover: the
// first announcement happens while the engine is being constructed, before any
// devp2p peer exists, so its Inv goes to nobody. The retry that follows depends
// on the notifier declaring the network bad, which for small producer sets can
// never fire (Elastos.ELA/dpos/p2p/notifier.go:148-154 requires
// len(connected)/2 < len(peers)/3). An operator who has just fixed devp2p
// connectivity can therefore wait forever for a re-announce. Calling this dials
// the relay again immediately.
//
// Returns 1 for announced, 0 for nothing to announce, matching upstream's
// pgp-chain/consensus/pbft/api.go:21-26.
func (a *API) AnnounceDAddr() uint64 {
	if a.engine == nil {
		return 0
	}
	if a.engine.AnnounceDAddr() {
		return 1
	}
	return 0
}

// GetArbiterPeersInfo reports the connect list restricted to the voting set.
//
// This is the table to read when votes go missing: a producer whose row says
// NoneConnection can never deliver a vote, and a producer row that is absent
// altogether means our own peer set does not contain it -- a config or duty
// rotation mismatch rather than a network one.
//
// The nil-versus-empty distinction is upstream's and is kept because it is
// information: nil means this node has no producer key (it is a verifier and
// has no business peering), empty means it is a producer but the connect list
// is empty (nothing to dial or nothing dialed yet).
func (a *API) GetArbiterPeersInfo() []PeerInfo {
	if a.engine == nil {
		return nil
	}
	if a.engine.DPOSAccount() == nil {
		return nil
	}
	if !a.engine.IsProducer() {
		return []PeerInfo{}
	}
	return a.toPeerInfos(a.engine.DumpDPoSPeersInfo(), true)
}

// GetAllPeersInfo reports the whole connect list, producers or not, connected or
// not.
//
// Unlike GetArbiterPeersInfo this is the raw table, so it keeps the
// NoneConnection rows: "I am supposed to be dialing this peer and I cannot" is
// only visible here.
func (a *API) GetAllPeersInfo() []PeerInfo {
	if a.engine == nil {
		return nil
	}
	if a.engine.DPOSAccount() == nil {
		return nil
	}
	if !a.engine.IsProducer() {
		return []PeerInfo{}
	}
	return a.toPeerInfos(a.engine.DumpDPoSPeersInfo(), false)
}

// GetActivePeers reports the peers that are connected right now, from the live
// connection set rather than the connect list.
//
// The difference matters: the connect list says what the server intends, this
// says what exists. A peer that appears connected here but is missing from the
// connect list is a connection the server did not ask for, which is what a
// magic or address-book mismatch looks like from the inside.
func (a *API) GetActivePeers() []PeerInfo {
	if a.engine == nil {
		return nil
	}
	return a.toPeerInfos(a.engine.ActiveDPoSPeerInfos(), false)
}

// Status reports the consensus state of this node. See the Status type for why
// each field is there.
func (a *API) Status() Status {
	if a.engine == nil {
		return Status{}
	}
	var (
		view        *dpos.ConsensusView
		viewOffset  uint32
		dutyIndex   uint32
		viewStart   time.Time
		changeView  time.Time
		isRunning   bool
		majority    int
		producerCnt int
		dutyWho     string
		anchor      uint64
		now         uint64
		wallOffset  int64
		nowOffset   int64
	)
	if d := a.engine.dispatcher; d != nil {
		view = d.GetConsensusView()
	}
	if ts := a.engine.GetTimeSource(); ts != nil {
		// The two numbers that say whether the clock the offset is derived from
		// is the wall clock.
		wallOffset = int64(ts.AdjustedTime().Unix()) - time.Now().Unix()
		nowOffset = int64(ts.Offset() / time.Second)
	}
	if view != nil {
		viewOffset = view.GetViewOffset()
		dutyIndex = view.GetDutyIndex()
		viewStart = view.GetViewStartTime()
		changeView = view.GetChangeViewTime()
		isRunning = view.IsRunning()
		majority = view.GetMajorityCount()
		producerCnt = view.GetTotalProducersCount()
		if producer := view.CurrentOnDutyProducer(); len(producer) > 0 {
			dutyWho = common.Bytes2Hex(producer)
		}
		anchor = view.GetAnchor()
		now = uint64(view.GetClock().Unix())
	}
	if producerCnt == 0 {
		producerCnt = len(a.engine.Producers())
	}
	return Status{
		IsProducer:          a.engine.IsProducer(),
		IsOnDuty:            a.engine.IsOnduty(),
		IsRecovered:         a.engine.IsRecovered(),
		IsProductionEnabled: a.engine.IsProductionEnabled(),
		IsRunning:           isRunning,
		ViewOffset:          viewOffset,
		ProducerCount:       producerCnt,
		MajorityCount:       majority,
		ActiveDPoSPeers:     a.engine.ActiveDPoSPeers(),
		FinishedHeight:      a.engine.GetIrreversibleHeight(),
		DPoSNetworkRunning:  a.engine.DPoSNetworkRunning(),
		DutyIndex:           dutyIndex,
		ViewStartTime:       formatViewTime(viewStart),
		ChangeViewTime:      formatViewTime(changeView),
		HeadHeight:          a.engine.headNumber(),
		DutyProducer:        dutyWho,
		Anchor:              anchor,
		Now:                 now,
		WallOffset:          wallOffset,
		NowOffset:           nowOffset,
	}
}

// toPeerInfos converts and optionally filters the live structs into the RPC
// shape.
//
// The filter runs against the consensus view's producer set rather than the
// static config, because the voting set is the view's: after a producer-set
// change the two disagree, and it is the view that decides whose vote counts.
func (a *API) toPeerInfos(peers []*dpp2p.PeerInfo, producersOnly bool) []PeerInfo {
	view := a.consensusView()
	result := make([]PeerInfo, 0, len(peers))
	for _, p := range peers {
		if p == nil {
			continue
		}
		id := p.PID[:]
		isProducer := view != nil && view.IsProducers(id)
		if producersOnly && !isProducer {
			continue
		}
		result = append(result, PeerInfo{
			NodePublicKey: common.Bytes2Hex(id),
			IP:            p.Addr,
			ConnState:     p.State.String(),
			NodeVersion:   p.NodeVersion,
			IsProducer:    isProducer,
		})
	}
	return result
}

func (a *API) consensusView() *dpos.ConsensusView {
	if a.engine == nil || a.engine.dispatcher == nil {
		return nil
	}
	return a.engine.dispatcher.GetConsensusView()
}

// formatViewTime renders a view anchor, and renders the zero anchor as "".
//
// A zero viewStartTime is not "1970-01-01", it is "no view has started yet":
// ChangeView only writes the field once an offset elapses, and recovery can
// leave it unset. Printing the epoch would read as a clock problem.
func formatViewTime(t time.Time) string {
	if t.Unix() <= 0 {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
