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

import dposmsg "github.com/elastos/Elastos.ELA/dpos/p2p/msg"

// The DPoS version/ping/pong messages have two wire layouts, selected by a
// process-wide counter:
//
//	Elastos.ELA/dpos/p2p/msg/version.go:29  GetPayloadVersion()
//	Elastos.ELA/dpos/p2p/msg/version.go:35  SetPayloadVersion()
//	Elastos.ELA/dpos/p2p/msg/version.go:60/67/74/97  `if GetPayloadVersion() >= DPoSV2Version`
//
// Every version, ping and pong serialised after the switch is flipped carries the
// V2 field set. There is no per-peer or per-height override: the value is global
// and the change is one-way in practice.
//
// pgp-chain flips it unconditionally for any node with a PBFT config
// (eth/backend.go:190, `msg2.SetPayloadVersion(msg2.DPoSV2Version)` inside
// `if chainConfig.Pbft != nil`). That is what this port does too, and it is why
// Config.DPoSV2StartHeight is carried but never decides anything -- see the field
// comment in config.go.
//
// SetWireVersionV2 is exported so the assembly layer can flip the switch at a
// point that is testable, and so the call site reads as a deliberate protocol
// decision rather than a stray library call. It MUST run before the DPoS server
// starts: the switch has to be set before the first handshake, or a 1.9.7 peer
// and this node disagree about the version message layout and the connection
// never completes -- with no error on either side, because both sides decode a
// message of the "wrong" length as a checksum failure and just drop the peer.
func SetWireVersionV2() {
	dposmsg.SetPayloadVersion(dposmsg.DPoSV2Version)
}

// WireVersion reports the DPoS payload version currently in force. It exists for
// startup logging and for the regression test; nothing in the consensus path
// branches on it.
func WireVersion() uint32 {
	return dposmsg.GetPayloadVersion()
}

// DPoSWireVersionV2 is the value SetWireVersionV2 installs, re-exported so tests
// assert against the Elastos constant rather than a literal.
const DPoSWireVersionV2 uint32 = dposmsg.DPoSV2Version
