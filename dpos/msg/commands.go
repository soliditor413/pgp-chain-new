// Copyright (c) 2017-2020 The Elastos Foundation
// Use of this source code is governed by an MIT
// license that can be found in the LICENSE file.
//
// Ported from pgp-chain/dpos/msg/commands.go and pgp-chain/chainbridge-core/dpos_msg/message.go.
//
// The strings below are the DPoS wire contract: a peer dispatches incoming
// messages by CMD(), and dpos/network.go's createMessage rejects (and drops the
// connection on) any command it does not recognise. They must therefore stay
// byte for byte identical to the ones pgp-chain 1.9.7 puts on the wire, and
// none of them may be removed even when this client does not implement the
// business behind it (see docs/design/pbft-port-design.md D10).

package msg

const (
	// Commands pgp-chain adds on top of the Elastos dpos/p2p/msg table.

	// CmdConfirm carries a payload.Confirm, i.e. the aggregated votes that end
	// up in header.Extra.
	CmdConfirm = "confirm"

	// CmdSmallCroTx and CmdFailedWithdrawTx carry ELA gateway traffic. The
	// business side is deliberately not ported (no chainbridge, no
	// smallcrosstx); the codecs stay so a peer that sends one is answered by a
	// no-op listener instead of a decoding error.
	CmdSmallCroTx       = "smallCroTx"
	CmdFailedWithdrawTx = "failedWTx"

	// Commands from the chainbridge package (chainbridge-core/dpos_msg). Kept
	// here so dpos does not depend on chainbridge at all.

	CmdDArbiter                 = "arbiter"
	CmdRequireArbiters          = "req_arbiter"
	CmdRequireArbitersSignature = "req_a_sig"
	CmdFeedbackArbiterSignature = "ret_a_sig"

	// CmdProducers announces a producer set change. Unlike the four above this
	// one is on the consensus path: it is how a node learns the next-turn
	// arbiter set without waiting for the ELA SPV listener.
	CmdProducers = "producers"
)
