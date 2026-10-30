// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth

// Assembly of the DPoS stack: the independent TCP server that carries consensus
// messages, and the address routing that tells the producer set where to dial.
//
// Two channels, deliberately separate (docs/design/pbft-port-design.md M4):
//
//	consensus messages  ->  dpos.Network, its own TCP listener on :20338
//	address discovery   ->  devp2p ELAMSG (0x08), relayed by dpos.Routes
//
// Neither works alone. The TCP server only dials PIDs whose address it knows, and
// the only source of addresses is the relay; the relay only knows which PIDs to
// ask about because the engine announces them. So this file wires both, and
// installing one without the other produces a node that looks healthy and never
// reaches consensus.
//
// The peer lifecycle comes from the other direction: eth/protocols/eth64 reports
// PeerUp/PeerDown/ElaMsg to eth/handler_eth64.go, which turns them into the
// dpos.ETNewPeer / ETDonePeer / ETElaMsg events that dpos.Routes subscribes to.

import (
	"bytes"
	"fmt"
	"os"

	"github.com/elastos/Elastos.ELA/dpos/p2p/peer"
	"github.com/elastos/Elastos.ELA/events"
	elamsgp2pmsg "github.com/elastos/Elastos.ELA/p2p/msg"

	"github.com/ethereum/go-ethereum/dpos"
	"github.com/ethereum/go-ethereum/dpos/elamsg"
	"github.com/ethereum/go-ethereum/eth/protocols/eth64"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/spv"
)

// defaultMaxNodePerHost is pgp-chain's cap: a producer set is expected to span
// hosts, and three per host is enough for a dev rig without letting one machine
// impersonate a majority.
const defaultMaxNodePerHost = 3

// startPBFTNetwork builds and starts the DPoS network for a producing node.
//
// It returns nil and does nothing in three cases, all of them legitimate:
//
//   - production is disabled: a verifier has no DPoS identity, so it is in
//     nobody's producer set and nothing would dial it;
//   - no keystore could be opened: same, plus there is no key to sign the address
//     records with;
//   - the producer set has a single member: there is nobody to connect to and --
//     more importantly -- recoverAbnormalState refuses to run with fewer peers
//     than a majority, so a transport would hold the recovery gate shut forever
//     and the node would never seal. Leaving the transport nil is what lets
//     StartMining release the gate (see its HasTransport check). That is the
//     single-node devnet shape.
//
// Any other error is returned so a real misconfiguration (bad port, unwritable
// data path) is reported at startup instead of surfacing as a stalled chain.
func (s *Ethereum) startPBFTNetwork() error {
	engine := s.pbftEngine
	if engine == nil || s.blockchain == nil || s.config == nil {
		return nil
	}
	if !engine.IsProductionEnabled() {
		return nil
	}
	account := engine.DPOSAccount()
	if account == nil {
		log.Warn("PBFT: no producer key, DPoS network not started (node only verifies)")
		return nil
	}

	chainCfg := s.blockchain.Config()
	if chainCfg.Legacy == nil {
		return nil
	}
	if n := len(chainCfg.Legacy.Producers); n < 2 {
		log.Info("PBFT: single-producer set, DPoS network not started", "producers", n)
		return nil
	}

	cfg := &s.config.Pbft
	if cfg.IPAddress == "" {
		return fmt.Errorf("pbft: producer set has %d members but no listen address was configured (--pbft.net.address)",
			len(chainCfg.Legacy.Producers))
	}
	if cfg.Port == 0 {
		// A zero port asks the kernel for an ephemeral one, and the address this
		// node then advertises is the port it did NOT get, so the whole set would
		// dial into nothing. Refuse rather than listen on a port nobody knows.
		return fmt.Errorf("pbft: no DPoS listen port configured (--pbft.net.port)")
	}
	if cfg.DataDir == "" {
		return fmt.Errorf("pbft: no DPoS data dir resolved (eth/backend.go should have set one)")
	}
	// Zero here is not "unlimited": the server admits an inbound connection only
	// when hostConnManger[ip] < MaxNodePerHost (dpos/p2p/server.go:584), so zero
	// rejects every peer -- and logs it at Debug, which is why a rig configured
	// from a file rather than flags would look like a firewall problem.
	if cfg.MaxNodePerHost == 0 {
		cfg.MaxNodePerHost = defaultMaxNodePerHost
	}
	// The version string is echoed in the DPoS handshake and shows up in peer
	// dumps; a blank one makes an operator unable to tell nodes apart.
	if cfg.NodeVersion == "" {
		if v, _ := version.Info(); v != "" {
			cfg.NodeVersion = v
		} else {
			cfg.NodeVersion = "unknown"
		}
	}
	// p2p.NewServer does not create its address database directory; without this
	// the first address write fails where the error is easy to miss.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("pbft: cannot create DPoS data dir %s: %w", cfg.DataDir, err)
	}

	publicKey := account.PublicKeyBytes()

	network, err := dpos.NewNetwork(&dpos.NetworkConfig{
		IPAddress:      cfg.IPAddress,
		Magic:          cfg.Magic,
		DefaultPort:    cfg.Port,
		MaxNodePerHost: cfg.MaxNodePerHost,

		// Carried for parity only: the wire version is pinned process-wide by
		// pbft.SetWireVersionV2 before any connection can open (wireversion.go),
		// which is what upstream's unconditional call does too.
		DPoSV2StartHeight: cfg.DPoSV2StartHeight,

		Account:    account,
		MedianTime: engine.GetTimeSource(),
		Listener:   engine,
		DataPath:   cfg.DataDir,

		NodeVersion: cfg.NodeVersion,
		// The ping/pong nonce is the ELA height, not the pgp height: it is the
		// anchor that decides which producer set applies. Getting this wrong does
		// not break the handshake, it makes the peer's idea of where it stands
		// wrong in exactly the recharge window where it matters.
		GetCurrentHeight: func(peer.PID) uint64 { return spv.GetSpvHeight() },
		// nil on purpose: the trimmed port never reads it (ProposalDispatcher
		// appears only as that struct field under dpos/). The dispatcher the
		// engine uses is the one it built in New.
		ProposalDispatcher: nil,
		PublicKey:          publicKey,
		// The network asks for a re-announcement through the event bus instead of
		// a direct call, because dpos.Network must not import dpos.Routes.
		AnnounceAddr: func() { events.Notify(dpos.ETAnnounceAddr, nil) },
	})
	if err != nil {
		return fmt.Errorf("pbft: cannot create DPoS network: %w", err)
	}

	// One call, because the three hooks describe one fact. A transport without a
	// peer counter is a node whose recovery refuses to run; a counter without a
	// transport is a node that never sends.
	engine.AttachNetwork(network)
	engine.AttachBlockStore(s.blockchain, s.blockchain)
	engine.SetIsCurrent(s.pbftIsCurrent)

	routes := dpos.New(&dpos.Config{
		PID:        publicKey,
		Addr:       fmt.Sprintf("%s:%d", cfg.IPAddress, cfg.Port),
		TimeSource: engine.GetTimeSource(),
		Sign:       account.Sign,
		IsCurrent:  engine.IsCurrent,
		RelayAddr: func(iv *elamsgp2pmsg.InvVect, _ interface{}) {
			// The record goes out over devp2p, not over the DPoS channel: the
			// DPoS channel is the thing we are trying to bootstrap.
			inv := elamsgp2pmsg.NewInv()
			inv.AddInvVect(iv)
			buf := new(bytes.Buffer)
			if err := inv.Serialize(buf); err != nil {
				log.Error("PBFT: cannot serialize DAddr inventory", "err", err)
				return
			}
			eth64.BroadcastElaMsg(&elamsg.ElaMsg{Type: elamsg.Inv, Msg: buf.Bytes()})
		},
		OnCipherAddr: func(pid peer.PID, cipher []byte) {
			// The payoff: a peer just said where it listens, so hand the address
			// to the TCP server. Everything before this is gossip; this call is
			// the only thing in the whole path that can produce a connection.
			addr, err := account.DecryptAddr(cipher)
			if err != nil {
				log.Error("PBFT: cannot decrypt DAddr cipher", "peer", pid.String(), "err", err)
				return
			}
			log.Debug("PBFT: direct link address learned", "peer", pid.String(), "addr", addr)
			network.AddDirectLinkAddr(pid, addr)
		},
	})

	go routes.Start()
	network.Start()

	log.Info("DPoS network started",
		"listen", fmt.Sprintf("%s:%d", cfg.IPAddress, cfg.Port),
		"producers", len(chainCfg.Legacy.Producers),
		"dataDir", cfg.DataDir)
	return nil
}

// pbftIsCurrent is the O2 form of dpos.Config.IsCurrent: the chain is caught up
// when the downloader has nothing left to fetch.
//
// Upstream also returns true when the highest known block is a bad block one
// above the head. That branch rescues a node whose only peer offers an unusable
// tip; it needs engine.IsBadBlock, which has no counterpart here, and it is a
// recovery concern rather than a sync one, so it moves to M6.
//
// The downloader is looked up on every call rather than captured, because this
// closure is installed before Downloader() is guaranteed to exist.
func (s *Ethereum) pbftIsCurrent() bool {
	if s.blockchain == nil {
		return false
	}
	dl := s.Downloader()
	if dl == nil {
		// Nothing to wait for. Announcing is the right answer here rather than the
		// safe one: a node that never announces because a subsystem is absent is
		// worse than one that announces early and retries.
		return true
	}
	progress := dl.Progress()
	return progress.CurrentBlock >= progress.HighestBlock
}
