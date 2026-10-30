// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package dpos

import (
	"testing"

	"github.com/elastos/Elastos.ELA/dpos/p2p/msg"
	elap2p "github.com/elastos/Elastos.ELA/p2p"

	dmsg "github.com/ethereum/go-ethereum/dpos/msg"
)

// wireCommands is every command a pgp-chain 1.9.7 peer can put on the DPoS
// channel: the 24 cases of pgp-chain/dpos/network.go:398-446, with the four
// chainbridge ones spelled through this tree's own constants.
//
// It is a hand-maintained table on purpose. The point is to fail when someone
// deletes a row from messageForCMD, so it must not be derived from the code it
// guards.
var wireCommands = []string{
	// Elastos-native, from dpos/p2p/msg.
	elap2p.CmdBlock,
	msg.CmdAcceptVote,
	msg.CmdReceivedProposal,
	msg.CmdRejectVote,
	msg.CmdInv,
	msg.CmdGetBlock,
	msg.CmdGetBlocks,
	msg.CmdResponseBlocks,
	msg.CmdRequestConsensus,
	msg.CmdResponseConsensus,
	msg.CmdRequestProposal,
	msg.CmdIllegalProposals,
	msg.CmdIllegalVotes,
	msg.CmdSidechainIllegalData,
	msg.CmdResponseInactiveArbitrators,
	msg.CmdResetConsensusView,

	// pgp-chain additions.
	dmsg.CmdConfirm,
	dmsg.CmdSmallCroTx,
	dmsg.CmdFailedWithdrawTx,
	dmsg.CmdProducers,

	// chainbridge (layer2): decoded, never dispatched.
	dmsg.CmdDArbiter,
	dmsg.CmdRequireArbiters,
	dmsg.CmdRequireArbitersSignature,
	dmsg.CmdFeedbackArbiterSignature,
}

// TestWireCommandTableIsComplete is the guard rail for D10.
//
// Deleting a case from messageForCMD looks like dead-code cleanup and is not: an
// unknown command makes createMessage return an error, and p2p's read loop
// treats that as connection-fatal. A peer running pgp-chain 1.9.7 that asks for
// an arbiter signature would then lose its DPoS link to us for good.
func TestWireCommandTableIsComplete(t *testing.T) {
	if len(wireCommands) != 24 {
		t.Fatalf("wireCommands has %d entries, want 24 (the pgp-chain 1.9.7 createMessage table)", len(wireCommands))
	}

	seen := make(map[string]string, len(wireCommands))
	for _, cmd := range wireCommands {
		if prev, dup := seen[cmd]; dup {
			t.Fatalf("command %q listed twice (%q and now)", cmd, prev)
		}
		seen[cmd] = cmd

		m := messageForCMD(cmd)
		if m == nil {
			t.Errorf("messageForCMD(%q) = nil; a 1.9.7 peer can send this and the DPoS connection would be dropped", cmd)
			continue
		}
		// Round-trip: the concrete type must report the command it was built
		// for. Two cases accidentally returning the same type would break the
		// decode without breaking the nil check above.
		if got := m.CMD(); got != cmd {
			t.Errorf("messageForCMD(%q).CMD() = %q, want %q", cmd, got, cmd)
		}
	}
}

// TestMessageForCMDRejectsUnknown pins the negative path: an unrecognised
// command must produce nil so createMessage can turn it into an error.
func TestMessageForCMDRejectsUnknown(t *testing.T) {
	for _, cmd := range []string{"", "unknown_cmd", "Confirm", "producers "} {
		if m := messageForCMD(cmd); m != nil {
			t.Errorf("messageForCMD(%q) = %T, want nil", cmd, m)
		}
	}
}

// undispatchableListener satisfies NetworkEventListener by embedding the
// interface itself. Any method that processMessage calls on it panics with a nil
// dereference, which is exactly the signal we want if a chainbridge case is ever
// added back to processMessage.
type undispatchableListener struct {
	NetworkEventListener
}

// TestChainbridgeMessagesAreDecodedButNotDispatched is the executable form of
// "chainbridge is not ported".
//
// Both halves matter and they pull in opposite directions:
//
//   - messageForCMD must NOT return nil (else a 1.9.7 peer is disconnected);
//   - processMessage must NOT dispatch to the listener (else we would be
//     running chainbridge business we deliberately dropped).
//
// A future reader who deletes layer2.go fails the first half. One who wires the
// four cases back into processMessage fails the second half.
func TestChainbridgeMessagesAreDecodedButNotDispatched(t *testing.T) {
	arbiterCommands := []string{
		dmsg.CmdDArbiter,
		dmsg.CmdRequireArbiters,
		dmsg.CmdRequireArbitersSignature,
		dmsg.CmdFeedbackArbiterSignature,
	}

	n := &Network{listener: undispatchableListener{}}

	for _, cmd := range arbiterCommands {
		m := messageForCMD(cmd)
		if m == nil {
			t.Fatalf("messageForCMD(%q) = nil; the codec must stay so createMessage can answer a 1.9.7 peer", cmd)
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("processMessage dispatched %q to the listener (%v); chainbridge business is not ported, so the case must be absent", cmd, r)
				}
			}()
			n.processMessage(&messageItem{Message: m})
		}()
	}
}
