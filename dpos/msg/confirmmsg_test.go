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

package msg

import (
	"bytes"
	"testing"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"
)

// TestEmptyConfirmMsgHasABoundedMaxLength is the regression test for a crash that
// took down three of four devnet nodes the first time one of them sealed a block.
//
// The p2p layer asks a message for MaxLength before it reads the payload: the
// factory hands back an empty ConfirmMsg, the length check runs, and only then does
// anything deserialise. Answering that question by serialising the message means
// serialising a nil *payload.Confirm, which is a nil dereference on the goroutine
// reading from a peer -- not an error return, not a dropped message, the process
// dies.
func TestEmptyConfirmMsgHasABoundedMaxLength(t *testing.T) {
	msg := &ConfirmMsg{}

	// The assertion is that this returns at all. A panic here is the bug.
	max := msg.MaxLength()
	if max == 0 {
		t.Fatal("MaxLength is 0, so every confirm would be refused as oversized")
	}

	// And it has to be an upper bound, not a lower one: the length check is
	// `hdr.Length > MaxLength`, so a bound smaller than a real confirm refuses real
	// traffic. A confirm carrying two producers' signatures is the smallest useful
	// one on a set that needs a quorum.
	if max < uint32(1024) {
		t.Fatalf("MaxLength = %d, which cannot hold a confirm with signatures", max)
	}
}

// TestConfirmMsgSerializeRejectsANilConfirm keeps the other half: MaxLength no
// longer reaches Serialize with an empty message, but Serialize is still on the send
// path and a nil payload there must be an error rather than a crash.
func TestConfirmMsgSerializeRejectsANilConfirm(t *testing.T) {
	msg := &ConfirmMsg{}
	if err := msg.Serialize(new(bytes.Buffer)); err == nil {
		t.Fatal("serialising a confirm message with no confirm returned no error")
	}
}

// TestConfirmMsgRoundTrip is the control: the constant bound above is only
// defensible if a real confirm still fits and still decodes.
func TestConfirmMsgRoundTrip(t *testing.T) {
	// No signatures: a round trip only needs the wire format, and signing here
	// would drag the producer keys into a message-format test.
	confirm := &payload.Confirm{Proposal: payload.DPOSProposal{
		BlockHash:  elacom.Uint256{1, 2, 3},
		ViewOffset: 2,
		Sponsor:    []byte{0x02, 0x03},
	}}

	msg := NewConfirmMsg(confirm, 7)
	buf := new(bytes.Buffer)
	if err := msg.Serialize(buf); err != nil {
		t.Fatalf("serialise: %v", err)
	}
	if got, max := uint32(buf.Len()), msg.MaxLength(); got > max {
		t.Fatalf("a real confirm is %d bytes but MaxLength is %d, so the p2p layer would refuse it", got, max)
	}

	decoded := &ConfirmMsg{}
	if err := decoded.Deserialize(bytes.NewBuffer(buf.Bytes())); err != nil {
		t.Fatalf("deserialise: %v", err)
	}
	if decoded.Height != 7 {
		t.Fatalf("height = %d, want 7", decoded.Height)
	}
	if decoded.Confirm == nil || !decoded.Confirm.Proposal.BlockHash.IsEqual(confirm.Proposal.BlockHash) {
		t.Fatal("the decoded confirm does not carry the proposal that was sent")
	}
}
