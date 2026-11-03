// Copyright (c) 2017-2019 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/network.go.
//
// Two deviations from upstream, both deliberate:
//
//   - The five chainbridge (layer2) message cases are gone from processMessage
//     but KEPT in createMessage. The business behind arbiter / req_arbiter /
//     req_a_sig / ret_a_sig is not ported (no chainbridge, see
//     docs/design/pbft-port-design.md D10), yet createMessage must still be able
//     to decode them: it returns an error on an unknown command and p2p treats
//     that error as connection-fatal, so dropping a case here would turn every
//     encounter with a chainbridge-era peer into a dropped DPoS connection.
//     Their codecs live in dpos/msg/layer2.go.
//
//   - Stop is idempotent (see the comment on it). Upstream sends on an
//     unbuffered channel and blocks forever on a second call.

package dpos

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	dmsg "github.com/ethereum/go-ethereum/dpos/msg"

	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
	"github.com/elastos/Elastos.ELA/dpos/account"
	"github.com/elastos/Elastos.ELA/dpos/dtime"
	"github.com/elastos/Elastos.ELA/dpos/p2p"
	"github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	elap2p "github.com/elastos/Elastos.ELA/p2p"
	peer2 "github.com/elastos/Elastos.ELA/p2p/peer"
)

// NetworkConfig is everything the DPoS TCP server needs from the engine.
type NetworkConfig struct {
	IPAddress         string
	Magic             uint32
	DefaultPort       uint16
	MaxNodePerHost    uint32
	DPoSV2StartHeight uint32

	Account    account.Account
	MedianTime dtime.MedianTimeSource
	Listener   NetworkEventListener
	DataPath   string

	NodeVersion        string
	GetCurrentHeight   func(pid peer.PID) uint64
	ProposalDispatcher *Dispatcher
	PublicKey          []byte
	AnnounceAddr       func()
}

type DPOSNetwork interface {
	Start()
	Stop() error

	SendMessageToPeer(id peer.PID, msg elap2p.Message) error
	BroadcastMessage(msg elap2p.Message)

	UpdatePeers(currentPeers []peer.PID, nextPeers []peer.PID)
	GetActivePeers() []p2p.Peer
}

// PeersDumper is the optional half of the DPoS server that reports the connect
// list with each peer's link direction.
//
// It is deliberately NOT part of DPOSNetwork. That interface is what the engine
// talks to while driving consensus, and every implementer of it must exist in
// tests: dpos_transport_test.go, peerset_watch_test.go and the recovery tests
// all stand up stubs. Adding a method there would force four unrelated files to
// grow a fake just so an RPC could read a table.
//
// The concrete *Network always satisfies this (network.go's DumpPeersInfo), so
// a type assertion is enough. Upstream exposes the same method on the same
// object (pgp-chain/consensus/pbft/network.go:131) and attaches it to the same
// RPC.
//
// One caveat the caller has to respect, and upstream documents it too
// (Elastos.ELA/dpos/p2p/interface.go:129-131): this walks the outbound, inbound
// and connect lists under the server's state lock, so it is not free. It is for
// an operator asking a question, not for a loop.
type PeersDumper interface {
	DumpPeersInfo() []*p2p.PeerInfo
}

type StatusSyncEventListener interface {
	OnPing(id peer.PID, height uint32)
	OnPong(id peer.PID, height uint32)
	OnBlock(id peer.PID, block *dmsg.BlockMsg)
	OnInv(id peer.PID, blockHash common.Uint256)
	OnGetBlock(id peer.PID, blockHash common.Uint256)
	OnGetBlocks(id peer.PID, startBlockHeight, endBlockHeight uint32)
	OnResponseBlocks(id peer.PID, blockConfirms []*dmsg.BlockMsg)
	OnRequestConsensus(id peer.PID, height uint64)
	OnResponseConsensus(id peer.PID, status *dmsg.ConsensusStatus)
	OnRequestProposal(id peer.PID, hash common.Uint256)
	OnIllegalProposalReceived(id peer.PID, proposals *payload.DPOSIllegalProposals)
	OnIllegalVotesReceived(id peer.PID, votes *payload.DPOSIllegalVotes)
}

// NetworkEventListener is what the engine must implement to receive decoded
// DPoS traffic.
//
// OnLayer2Msg is NOT here. Upstream routes the four chainbridge arbiter messages
// through it; this port decodes those messages (createMessage) and then drops
// them in processMessage, because the layer2 listener was chainbridge business.
type NetworkEventListener interface {
	StatusSyncEventListener

	OnProposalReceived(id peer.PID, proposal *payload.DPOSProposal)
	OnVoteAccepted(id peer.PID, proposal *payload.DPOSProposalVote)
	OnVoteRejected(id peer.PID, proposal *payload.DPOSProposalVote)

	OnChangeView()
	OnBadNetwork()
	OnRecover()
	OnResponseResetViewReceived(msg *msg.ResetView)

	OnBlockReceived(id peer.PID, b *dmsg.BlockMsg, confirmed bool)
	OnConfirmReceived(id peer.PID, c *payload.Confirm, height uint64)

	OnSmallCroTxReceived(id peer.PID, c *dmsg.SmallCroTx)
	OnFailedWithdrawTxReceived(id peer.PID, c *dmsg.FailedWithdrawTx)

	OnProducersMsg(msg *dmsg.ProducersMsg)
}

// ErrNetworkStopped is what SendMessageToPeer returns once Stop has begun.
//
// It exists so a caller can tell "the server refused this message" apart from
// "there is no server": after the Parlia fork the DPoS server is gone for good,
// and a producer-path caller reading the error needs to know it will never
// succeed again rather than retry.
var ErrNetworkStopped = errors.New("dpos network stopped")

type messageItem struct {
	ID      peer.PID
	Message elap2p.Message
}

type Network struct {
	listener     NetworkEventListener
	publicKey    []byte
	announceAddr func()

	p2pServer    p2p.Server
	messageQueue chan *messageItem

	// quit is closed, never sent on, and stopOnce makes Stop idempotent. See
	// (*Network).Stop.
	quit     chan struct{}
	stopOnce sync.Once

	// stopped is set before quit is closed, and every entry point that would
	// touch the server -- or post to a channel whose only reader is the Start
	// goroutine -- checks it.
	//
	// Why it is needed at all: p2pServer.Stop() makes the server's peerHandler
	// exit (its select takes the quit case and never reads s.query again), but
	// nothing makes the *interface* safe afterwards. ConnectedPeers and
	// DumpPeersInfo write a request into that unread channel and block forever
	// on the reply; the channel posts block on a channel the Start loop no
	// longer selects. That is the observed symptom: RPC pbft.status() deadlocks
	// the moment the chain crosses the Parlia fork, because retire stops the
	// server and the RPC keeps asking it questions. See the p2p package's
	// server.go:828 (Stop) and :632-660 (peerHandler select).
	stopped atomic.Bool

	badNetworkChan chan bool
	changeViewChan chan bool
	recoverChan    chan bool

	GetCurrentHeight func(pid peer.PID) uint64
}

// GetActivePeers answers nil once the network is stopped.
//
// The nil is load-bearing, not cosmetic: ConnectedPeers sends a request into a
// channel the server's peerHandler stops reading the moment it exits, so asking
// a stopped server this question is a permanent block. Every caller (RPC
// pbft.status, the recovery gate) treats nil as "no peers", which is the truth
// after the fork.
func (n *Network) GetActivePeers() []p2p.Peer {
	if n.stopped.Load() {
		return nil
	}
	return n.p2pServer.ConnectedPeers()
}

func (n *Network) notifyFlag(flag p2p.NotifyFlag) {
	if n.stopped.Load() {
		return
	}
	if flag == p2p.NFBadNetwork {
		n.badNetworkChan <- true

		// Trigger announce address when network go bad.
		n.announceAddr()
	}
}

func (n *Network) Start() {
	n.p2pServer.Start()

	go func() {
	out:
		for {
			select {
			case msgItem := <-n.messageQueue:
				n.processMessage(msgItem)
			case <-n.changeViewChan:
				n.changeView()
			case <-n.badNetworkChan:
				n.badNetwork()
			case <-n.recoverChan:
				n.recover()
			case <-n.quit:
				break out
			}
		}
	}()
}

// Stop shuts the server down and is safe to call more than once.
//
// Upstream does `n.quit <- true` on an unbuffered channel, which blocks forever
// if the event loop has already exited (a second Stop) or was never started
// (Stop before Start). Both are reachable from the assembly layer during a
// failed startup, and a hang there is indistinguishable from a slow boot.
// routes.go:218-223 in the same package already gets this right; this is the
// same CAS-once + close shape.
//
// stopped goes up before quit closes, so a concurrent caller that has not yet
// taken its early return sees the flag on its own check rather than racing the
// close. The reverse order would leave a window in which a caller reads
// stopped == false, then blocks on a channel whose reader has just exited.
func (n *Network) Stop() error {
	var err error
	n.stopOnce.Do(func() {
		n.stopped.Store(true)
		close(n.quit)
		err = n.p2pServer.Stop()
	})
	return err
}

// PostChangeViewTask and PostRecoverTask hand work to the Start goroutine over
// unbuffered channels. Once Stop has closed quit that goroutine is gone, so the
// send would block forever -- these are the same hang as GetActivePeers, one
// channel earlier in the path.
func (n *Network) PostChangeViewTask() {
	if n.stopped.Load() {
		return
	}
	n.changeViewChan <- true
}

func (n *Network) PostRecoverTask() {
	if n.stopped.Load() {
		return
	}
	n.recoverChan <- true
}

// processMessage dispatches a decoded message to the listener.
//
// The four chainbridge (layer2) commands decoded by createMessage have no case
// here on purpose: they fall through and the message is dropped. That is the
// whole of "chainbridge is not ported" -- the wire still parses, the business is
// absent.
func (n *Network) processMessage(msgItem *messageItem) {
	m := msgItem.Message
	switch m.CMD() {
	case msg.CmdReceivedProposal:
		msgProposal, processed := m.(*msg.Proposal)
		if processed {
			n.listener.OnProposalReceived(msgItem.ID, &msgProposal.Proposal)
		}
	case msg.CmdAcceptVote:
		msgVote, processed := m.(*msg.Vote)
		if processed {
			n.listener.OnVoteAccepted(msgItem.ID, &msgVote.Vote)
		}
	case msg.CmdRejectVote:
		msgVote, processed := m.(*msg.Vote)
		if processed {
			n.listener.OnVoteRejected(msgItem.ID, &msgVote.Vote)
		}
	case msg.CmdPing:
		msgPing, processed := m.(*msg.Ping)
		if processed {
			n.listener.OnPing(msgItem.ID, uint32(msgPing.Nonce))
		}
	case msg.CmdPong:
		msgPong, processed := m.(*msg.Pong)
		if processed {
			n.listener.OnPong(msgItem.ID, uint32(msgPong.Nonce))
		}
	case elap2p.CmdBlock:
		blockMsg, processed := m.(*dmsg.BlockMsg)
		if processed {
			n.listener.OnBlock(msgItem.ID, blockMsg)
		}
	case msg.CmdInv:
		msgInv, processed := m.(*msg.Inventory)
		if processed {
			n.listener.OnInv(msgItem.ID, msgInv.BlockHash)
		}
	case msg.CmdGetBlock:
		msgGetBlock, processed := m.(*msg.GetBlock)
		if processed {
			n.listener.OnGetBlock(msgItem.ID, msgGetBlock.BlockHash)
		}
	case msg.CmdGetBlocks:
		msgGetBlocks, processed := m.(*msg.GetBlocks)
		if processed {
			n.listener.OnGetBlocks(msgItem.ID, msgGetBlocks.StartBlockHeight, msgGetBlocks.EndBlockHeight)
		}
	case msg.CmdResponseBlocks:
		// Disabled upstream as well (pgp-chain comments the body out): the
		// payload is a block list and the handler was never written.
	case msg.CmdRequestConsensus:
		msgRequestConsensus, processed := m.(*dmsg.RequestConsensus)
		if processed {
			n.listener.OnRequestConsensus(msgItem.ID, msgRequestConsensus.Height)
		}
	case msg.CmdResponseConsensus:
		msgResponseConsensus, processed := m.(*dmsg.ResponseConsensus)
		if processed {
			n.listener.OnResponseConsensus(msgItem.ID, &msgResponseConsensus.Consensus)
		}
	case msg.CmdRequestProposal:
		msgRequestProposal, processed := m.(*msg.RequestProposal)
		if processed {
			n.listener.OnRequestProposal(msgItem.ID, msgRequestProposal.ProposalHash)
		}
	case msg.CmdIllegalProposals:
		msgIllegalProposals, processed := m.(*msg.IllegalProposals)
		if processed {
			n.listener.OnIllegalProposalReceived(msgItem.ID, &msgIllegalProposals.Proposals)
		}
	case msg.CmdIllegalVotes:
		msgIllegalVotes, processed := m.(*msg.IllegalVotes)
		if processed {
			n.listener.OnIllegalVotesReceived(msgItem.ID, &msgIllegalVotes.Votes)
		}
	case dmsg.CmdConfirm:
		msgConfirm, processed := m.(*dmsg.ConfirmMsg)
		if processed {
			n.listener.OnConfirmReceived(msgItem.ID, msgConfirm.Confirm, msgConfirm.Height)
		}
	case dmsg.CmdSmallCroTx:
		msgCro, processed := m.(*dmsg.SmallCroTx)
		if processed {
			n.listener.OnSmallCroTxReceived(msgItem.ID, msgCro)
		}
	case dmsg.CmdFailedWithdrawTx:
		withdrawTx, processed := m.(*dmsg.FailedWithdrawTx)
		if processed {
			n.listener.OnFailedWithdrawTxReceived(msgItem.ID, withdrawTx)
		}
	case msg.CmdResetConsensusView:
		msgI, processed := m.(*msg.ResetView)
		if processed {
			n.listener.OnResponseResetViewReceived(msgI)
		}
	case dmsg.CmdProducers:
		msgI, processed := m.(*dmsg.ProducersMsg)
		if processed {
			n.listener.OnProducersMsg(msgI)
		}
	}
}

func (n *Network) changeView() {
	n.listener.OnChangeView()
}

func (n *Network) UpdatePeers(currentPeers []peer.PID, nextPeers []peer.PID) {
	if n.stopped.Load() {
		return
	}
	Info("[UpdatePeers]", "self account:", common.BytesToHexString(n.publicKey))
	for _, p := range currentPeers {
		if bytes.Equal(n.publicKey, p[:]) {
			n.p2pServer.ConnectPeers(currentPeers, nextPeers)
			return
		}
	}

	for _, p := range nextPeers {
		if bytes.Equal(n.publicKey, p[:]) {
			n.p2pServer.ConnectPeers(currentPeers, nextPeers)
			return
		}
	}
	Info("[UpdatePeers] i am not in peers", "self account:", common.BytesToHexString(n.publicKey))
	n.p2pServer.ConnectPeers(nil, nil)
}

// handleMessage is the p2p server's inbound callback. It is handed to the
// server at construction (NewNetwork), and the server's own shutdown is what
// stops it being called -- but a message already in flight when quit closes
// would land here after the Start goroutine exited and block on messageQueue.
func (n *Network) handleMessage(pid peer.PID, msg elap2p.Message) {
	if n.stopped.Load() {
		return
	}
	n.messageQueue <- &messageItem{pid, msg}
}

func (n *Network) badNetwork() {
	Info("bad network")
	n.listener.OnBadNetwork()
}

func (n *Network) recover() {
	Info("network recover")
	n.listener.OnRecover()
}

func (n *Network) AddDirectLinkAddr(pid peer.PID, addr string) {
	if n.stopped.Load() {
		return
	}
	n.p2pServer.AddAddr(pid, addr)
}

func (n *Network) SendMessageToPeer(id peer.PID, msg elap2p.Message) error {
	if n.stopped.Load() {
		return ErrNetworkStopped
	}
	Info("[SendMessageToPeer] msg:", msg.CMD(), id.String())
	return n.p2pServer.SendMessageToPeer(id, msg)
}

func (n *Network) BroadcastMessage(msg elap2p.Message) {
	if n.stopped.Load() {
		return
	}
	Info("[BroadcastMessage] msg:", msg.CMD())
	n.p2pServer.BroadcastMessage(msg)
}

// DumpPeersInfo returns an array consisting of all peers state in connect list.
//
// This function is safe for concurrent access and is part of the IServer
// interface implementation.
//
// It answers nil once stopped, for the same reason GetActivePeers does: the
// dump walks server state under a lock the shutdown path is tearing down, and
// reaches the unread s.query channel. pbft.status's peer table (the RPC
// DumpDPoSPeersInfo) is the caller that hung.
func (n *Network) DumpPeersInfo() []*p2p.PeerInfo {
	if n.stopped.Load() {
		return nil
	}
	return n.p2pServer.DumpPeersInfo()
}

func NewNetwork(cfg *NetworkConfig) (*Network, error) {
	network := &Network{
		listener:         cfg.Listener,
		publicKey:        cfg.PublicKey,
		announceAddr:     cfg.AnnounceAddr,
		GetCurrentHeight: cfg.GetCurrentHeight,

		messageQueue:   make(chan *messageItem, 10000),
		quit:           make(chan struct{}),
		badNetworkChan: make(chan bool),
		changeViewChan: make(chan bool),
		recoverChan:    make(chan bool),
	}

	notifier := p2p.NewNotifier(p2p.NFNetStabled|p2p.NFBadNetwork, network.notifyFlag)
	var pid peer.PID
	copy(pid[:], cfg.Account.PublicKeyBytes())
	server, err := p2p.NewServer(&p2p.Config{
		DataDir:           cfg.DataPath,
		PID:               pid,
		EnableHub:         true,
		Localhost:         cfg.IPAddress,
		MagicNumber:       cfg.Magic,
		DefaultPort:       cfg.DefaultPort,
		TimeSource:        cfg.MedianTime,
		MaxNodePerHost:    cfg.MaxNodePerHost,
		CreateMessage:     createMessage,
		HandleMessage:     network.handleMessage,
		PingNonce:         network.GetCurrentHeight,
		PongNonce:         network.GetCurrentHeight,
		Sign:              cfg.Account.Sign,
		StateNotifier:     notifier,
		DPoSV2StartHeight: cfg.DPoSV2StartHeight,
		NodeVersion:       cfg.NodeVersion,
	})
	if err != nil {
		return nil, err
	}

	network.p2pServer = server
	return network, nil
}

// messageForCMD maps a wire command to the concrete type its payload decodes
// into, or nil if this build does not recognise the command.
//
// It is split out of createMessage so the wire table can be tested without
// fabricating a header, a checksum and a net.Conn -- see network_test.go. The
// split changes no behaviour: createMessage is "look up, then validate", and the
// validation (length, checksum, payload) is CheckAndCreateMessage's job.
//
// Every command a pgp-chain 1.9.7 peer can put on the DPoS channel must appear
// here, because createMessage's caller (p2p) treats a nil as connection-fatal.
// That includes the four chainbridge arbiter commands: processMessage has no
// case for them, so they are decoded and dropped. See D10 and dpos/msg/layer2.go.
func messageForCMD(cmd string) elap2p.Message {
	switch cmd {
	case elap2p.CmdBlock:
		return dmsg.NewBlockMsg([]byte{})
	case msg.CmdAcceptVote:
		return &msg.Vote{Command: msg.CmdAcceptVote}
	case msg.CmdReceivedProposal:
		return &msg.Proposal{}
	case msg.CmdRejectVote:
		return &msg.Vote{Command: msg.CmdRejectVote}
	case msg.CmdInv:
		return &msg.Inventory{}
	case msg.CmdGetBlock:
		return &msg.GetBlock{}
	case msg.CmdGetBlocks:
		return &msg.GetBlocks{}
	case msg.CmdResponseBlocks:
		return &msg.ResponseBlocks{}
	case msg.CmdRequestConsensus:
		return &dmsg.RequestConsensus{}
	case msg.CmdResponseConsensus:
		return &dmsg.ResponseConsensus{}
	case msg.CmdRequestProposal:
		return &msg.RequestProposal{}
	case msg.CmdIllegalProposals:
		return &msg.IllegalProposals{}
	case msg.CmdIllegalVotes:
		return &msg.IllegalVotes{}
	case msg.CmdSidechainIllegalData:
		return &msg.SidechainIllegalData{}
	case msg.CmdResponseInactiveArbitrators:
		return &msg.ResponseInactiveArbitrators{}
	case dmsg.CmdConfirm:
		return &dmsg.ConfirmMsg{}
	case dmsg.CmdSmallCroTx:
		return &dmsg.SmallCroTx{}
	case dmsg.CmdFailedWithdrawTx:
		return &dmsg.FailedWithdrawTx{}
	case dmsg.CmdDArbiter:
		return &dmsg.DArbiter{}
	case dmsg.CmdRequireArbiters:
		return &dmsg.RequireArbiter{}
	case dmsg.CmdRequireArbitersSignature:
		return &dmsg.RequireArbitersSignature{}
	case dmsg.CmdFeedbackArbiterSignature:
		return &dmsg.FeedBackArbitersSignature{}
	case msg.CmdResetConsensusView:
		return &msg.ResetView{}
	case dmsg.CmdProducers:
		return &dmsg.ProducersMsg{}
	}
	return nil
}

func createMessage(hdr elap2p.Header, r net.Conn) (message elap2p.Message, err error) {
	message = messageForCMD(hdr.GetCMD())
	if message == nil {
		return nil, errors.New("Received unsupported message, CMD " + hdr.GetCMD())
	}
	return peer2.CheckAndCreateMessage(hdr, message, r)
}
