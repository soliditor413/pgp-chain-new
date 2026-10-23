// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.

package eth64

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
)

// TestProtocolLength pins the message code window of the protocol.
//
// p2p/peer.go's matchProtocols assigns every subprotocol a window by accumulating
// `offset += Length`, in registration order. eth/64 is appended after eth/68,
// eth/70, snap and bsc, so changing 17 to 18 would shift every protocol after it
// by one code and silently misroute their messages at runtime. Nothing in the
// tree validates this number, hence the assertion.
func TestProtocolLength(t *testing.T) {
	if got := protocolLengths[ETH64]; got != 17 {
		t.Fatalf("eth/64 message window changed: have %d, want 17 (pgp-chain protocolLengths)", got)
	}
	if ProtocolName != "eth" {
		t.Fatalf("protocol name changed: have %q, want \"eth\"", ProtocolName)
	}
}

// TestHashOrNumberRLP checks both branches of the origin union, including the
// exact bytes pgp-chain produces.
func TestHashOrNumberRLP(t *testing.T) {
	// Branch 1: a zero hash selects the number. 5 encodes as the single byte 0x05.
	number := HashOrNumber{Number: 5}
	blob, err := rlp.EncodeToBytes(&number)
	if err != nil {
		t.Fatalf("failed to encode number origin: %v", err)
	}
	if want := "05"; hex.EncodeToString(blob) != want {
		t.Fatalf("number origin encoding mismatch: have %x, want %s", blob, want)
	}

	var decoded HashOrNumber
	if err := rlp.DecodeBytes(blob, &decoded); err != nil {
		t.Fatalf("failed to decode number origin: %v", err)
	}
	if decoded.Number != 5 || decoded.Hash != (common.Hash{}) {
		t.Fatalf("number origin round trip mismatch: %+v", decoded)
	}

	// Branch 2: a non-zero hash is encoded as a 32 byte string.
	hash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	byHash := HashOrNumber{Hash: hash}
	blob, err = rlp.EncodeToBytes(&byHash)
	if err != nil {
		t.Fatalf("failed to encode hash origin: %v", err)
	}
	if want := "a0" + strings.Repeat("11", 32); hex.EncodeToString(blob) != want {
		t.Fatalf("hash origin encoding mismatch: have %x, want %s", blob, want)
	}
	decoded = HashOrNumber{}
	if err := rlp.DecodeBytes(blob, &decoded); err != nil {
		t.Fatalf("failed to decode hash origin: %v", err)
	}
	if decoded.Hash != hash || decoded.Number != 0 {
		t.Fatalf("hash origin round trip mismatch: %+v", decoded)
	}

	// Both fields set is a producer bug and must not silently pick one.
	both := HashOrNumber{Hash: hash, Number: 7}
	if _, err := rlp.EncodeToBytes(&both); err == nil {
		t.Fatal("encoding an ambiguous origin must fail")
	}
}

// TestGetBlockHeadersRequestGolden pins the wire bytes of the header request
// against the layout pgp-chain decodes: [origin, amount, skip, reverse] with the
// origin union, and Reverse as a bool (not the uint used by newer clients).
func TestGetBlockHeadersRequestGolden(t *testing.T) {
	// Number origin, amount 1, skip 0, reverse false.
	byNumber := GetBlockHeadersRequest{
		Origin:  HashOrNumber{Number: 5},
		Amount:  1,
		Skip:    0,
		Reverse: false,
	}
	blob, err := rlp.EncodeToBytes(&byNumber)
	if err != nil {
		t.Fatalf("failed to encode header request: %v", err)
	}
	// c4 = list of 4 bytes, 05 = number 5, 01 = amount 1, 80 = skip 0, 80 = false.
	if want := "c405018080"; hex.EncodeToString(blob) != want {
		t.Fatalf("number origin request mismatch:\n have %x\n want %s", blob, want)
	}

	// Hash origin, amount 192, skip 0, reverse true.
	hash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	byHash := GetBlockHeadersRequest{
		Origin:  HashOrNumber{Hash: hash},
		Amount:  192,
		Skip:    0,
		Reverse: true,
	}
	blob, err = rlp.EncodeToBytes(&byHash)
	if err != nil {
		t.Fatalf("failed to encode hash header request: %v", err)
	}
	// e5 = list of 37 bytes: a0+32 hash, 81c0 = 192, 80 = skip 0, 01 = true.
	want := "e5a0" + strings.Repeat("11", 32) + "81c080" + "01"
	if hex.EncodeToString(blob) != want {
		t.Fatalf("hash origin request mismatch:\n have %x\n want %s", blob, want)
	}

	// The decoded request must round trip, so we can also serve it.
	var decoded GetBlockHeadersRequest
	if err := rlp.DecodeBytes(blob, &decoded); err != nil {
		t.Fatalf("failed to decode header request: %v", err)
	}
	if decoded.Origin.Hash != hash || decoded.Amount != 192 || decoded.Skip != 0 || !decoded.Reverse {
		t.Fatalf("header request round trip mismatch: %+v", decoded)
	}
}

// TestStatusPacketFieldOrder pins the 6 field positional layout of the eth/64
// status message. pgp-chain decodes it positionally, so a reordered or extra
// field breaks the handshake silently.
func TestStatusPacketFieldOrder(t *testing.T) {
	status := &StatusPacket{
		ProtocolVersion: 64,
		NetworkID:       860621,
		TD:              big.NewInt(0x1234),
		Head:            common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222"),
		Genesis:         common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333"),
		ForkID:          forkid.ID{Hash: [4]byte{0xde, 0xad, 0xbe, 0xef}, Next: 3},
	}
	blob, err := rlp.EncodeToBytes(status)
	if err != nil {
		t.Fatalf("failed to encode status: %v", err)
	}
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(blob, &fields); err != nil {
		t.Fatalf("failed to split status fields: %v", err)
	}
	if len(fields) != 6 {
		t.Fatalf("status field count mismatch: have %d, want 6", len(fields))
	}
	// Positional assertions: any reordering breaks the handshake with pgp peers.
	var (
		version uint32
		network uint64
		td      *big.Int
		head    common.Hash
		genesis common.Hash
		id      forkid.ID
	)
	values := []interface{}{&version, &network, &td, &head, &genesis, &id}
	for i, target := range values {
		if err := rlp.DecodeBytes(fields[i], target); err != nil {
			t.Fatalf("field %d has an unexpected type: %v", i, err)
		}
	}
	if version != 64 || network != 860621 {
		t.Fatalf("field order mismatch: version=%d network=%d", version, network)
	}
	if td.Cmp(big.NewInt(0x1234)) != 0 || head != status.Head || genesis != status.Genesis {
		t.Fatalf("field order mismatch: td=%v head=%x genesis=%x", td, head, genesis)
	}
	if id != status.ForkID {
		t.Fatalf("fork id mismatch: have %+v, want %+v", id, status.ForkID)
	}
}

// TestBodyDecodesPGPShape feeds a 2 element [transactions, uncles] body, which is
// what pgp-chain sends, into types.Body. types.Body has a third optional
// Withdrawals field, so it must accept the shorter list.
func TestBodyDecodesPGPShape(t *testing.T) {
	type pgpBlockBody struct {
		Transactions []*types.Transaction
		Uncles       []*types.Header
	}
	to := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	tx := types.NewTransaction(7, to, big.NewInt(1), 21000, big.NewInt(1), nil)

	blob, err := rlp.EncodeToBytes(&pgpBlockBody{Transactions: []*types.Transaction{tx}, Uncles: nil})
	if err != nil {
		t.Fatalf("failed to encode a pgp shaped body: %v", err)
	}
	var body types.Body
	if err := rlp.DecodeBytes(blob, &body); err != nil {
		t.Fatalf("types.Body rejected a 2 element body: %v", err)
	}
	if len(body.Transactions) != 1 || body.Transactions[0].Hash() != tx.Hash() {
		t.Fatalf("transaction round trip failed: %+v", body.Transactions)
	}
	if len(body.Uncles) != 0 || body.Withdrawals != nil {
		t.Fatalf("unexpected optional fields: uncles=%d withdrawals=%d", len(body.Uncles), len(body.Withdrawals))
	}
	// And the list-of-bodies envelope the protocol actually uses.
	var bodiesBlob []byte
	bodiesBlob, err = rlp.EncodeToBytes([]*pgpBlockBody{{Transactions: []*types.Transaction{tx}}})
	if err != nil {
		t.Fatalf("failed to encode bodies list: %v", err)
	}
	var bodies []*types.Body
	if err := rlp.DecodeBytes(bodiesBlob, &bodies); err != nil {
		t.Fatalf("BlockBodies list rejected: %v", err)
	}
	if len(bodies) != 1 || len(bodies[0].Transactions) != 1 {
		t.Fatalf("bodies list round trip failed: %+v", bodies)
	}
}

// TestSyncerBoundaryLatch pins the hard-boundary latch and its reset.
//
// A block the chain refuses without importing anything is a dead end: retrying
// it every syncLoopInterval only repeats the same failure. The latch must park
// the syncer, and Reset must be able to release it (that is the entry point for
// a future rule that makes the block executable).
func TestSyncerBoundaryLatch(t *testing.T) {
	h := newHandler(nil, 860621)
	defer h.Close()
	s := newChainSyncer(h)

	if s.blocked.Load() {
		t.Fatal("a fresh syncer must not be latched")
	}
	s.blocked.Store(true)
	if !s.blocked.Load() {
		t.Fatal("latch did not stick")
	}
	s.Reset()
	if s.blocked.Load() {
		t.Fatal("Reset did not clear the latch")
	}
	// Reset also schedules a round, otherwise the syncer would sit idle until
	// the next ticker tick.
	select {
	case <-s.triggerCh:
	case <-time.After(time.Second):
		t.Fatal("Reset did not trigger a sync round")
	}
}

// TestSyncerDoesNotLatchOnShutdown is the regression guard for the false
// "hard boundary": a node that is shutting down must not look like a chain that
// refused a block.
//
// InsertChain reports core.ErrChainStopped / core.ErrInsertionInterrupted when a
// batch happens to be in flight while Close runs. Before this guard the syncer
// latched on that error, which parks it for the rest of the process life and
// permanently inflates eth64/sync/boundary-stops - and it made two teammates
// chase a chain divergence that never existed.
func TestSyncerDoesNotLatchOnShutdown(t *testing.T) {
	remote, local := p2p.MsgPipe()
	defer remote.Close()
	defer local.Close()

	peer := NewPeer(ETH64, "test-peer", local)
	header := &types.Header{Number: big.NewInt(1923)}

	// The two sentinels must be recognisable, including through a wrap, because
	// InsertChain returns them through fmt.Errorf on several paths.
	if !core.IsChainStopped(core.ErrChainStopped) {
		t.Fatal("IsChainStopped does not recognise ErrChainStopped")
	}
	if !core.IsChainStopped(core.ErrInsertionInterrupted) {
		t.Fatal("IsChainStopped does not recognise ErrInsertionInterrupted")
	}
	if !core.IsChainStopped(fmt.Errorf("inserting chain: %w", core.ErrChainStopped)) {
		t.Fatal("IsChainStopped does not unwrap")
	}
	if core.IsChainStopped(errLegacyExecutionNotAligned) {
		t.Fatal("IsChainStopped must not match a boundary condition")
	}

	// Case 1: shutdown sentinel, handler still open.
	h := newHandler(nil, 860621)
	s := newChainSyncer(h)
	s.stopAtBoundary(peer, 1923, header, fmt.Errorf("block %d: %w", 1923, core.ErrChainStopped))
	if s.blocked.Load() {
		t.Fatal("a shutting-down chain must not latch the syncer")
	}

	// Case 2: handler already closed, ordinary error. Nothing to record.
	h.Close()
	s.stopAtBoundary(peer, 1923, header, errors.New("state root mismatch"))
	if s.blocked.Load() {
		t.Fatal("stopAtBoundary must not latch after the handler is closed")
	}

	// Case 3: the latch must still work, otherwise the two cases above prove
	// nothing. A live handler plus a real refusal is exactly what the latch is
	// for.
	h2 := newHandler(nil, 860621)
	defer h2.Close()
	s2 := newChainSyncer(h2)
	s2.stopAtBoundary(peer, 1923, header, errors.New("state root mismatch"))
	if !s2.blocked.Load() {
		t.Fatal("a live handler must still latch on a real boundary")
	}
}

// TestDispatchDiscardsUnknownCode is the guard against the 0x08 collision: pgp
// sends ELAMSG (dpos traffic) on 0x08, eth/68 uses 0x08 for
// NewPooledTransactionHashes. Receiving it must not tear the connection down.
func TestDispatchDiscardsUnknownCode(t *testing.T) {
	remote, local := p2p.MsgPipe()
	defer remote.Close()
	defer local.Close()

	peer := NewPeer(ETH64, "test-peer", local)
	h := newHandler(nil, 860621)
	defer h.Close()

	done := make(chan error, 1)
	go func() { done <- h.dispatch(peer) }()

	// A dpos style message on the colliding code, with a payload we must discard.
	if err := p2p.Send(remote, ELAMSG, []interface{}{"dpos", big.NewInt(1)}); err != nil {
		t.Fatalf("failed to send ELAMSG: %v", err)
	}
	// Several unknown codes in a row must all be discarded, not just the first.
	for code := uint64(0x09); code <= 0x0c; code++ {
		if err := p2p.Send(remote, code, []interface{}{"x"}); err != nil {
			t.Fatalf("failed to send unknown code %d: %v", code, err)
		}
	}
	// Give the dispatcher a moment, then close the pipe so it returns.
	time.Sleep(100 * time.Millisecond)
	remote.Close()

	select {
	case err := <-done:
		// Closing the pipe surfaces as a read error/EOF; anything that looks like a
		// protocol violation would mean the unknown code killed the connection.
		if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "closed") {
			t.Fatalf("dispatch must not fail on unknown message codes, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return after the pipe was closed")
	}
}

// TestDispatchRejectsOversizedMessage checks the size guard before decoding.
func TestDispatchRejectsOversizedMessage(t *testing.T) {
	remote, local := p2p.MsgPipe()
	defer remote.Close()
	defer local.Close()

	peer := NewPeer(ETH64, "test-peer", local)
	h := newHandler(nil, 860621)
	defer h.Close()

	done := make(chan error, 1)
	go func() { done <- h.dispatch(peer) }()

	// A payload larger than the protocol cap must be rejected before decoding.
	big := bytes.Repeat([]byte{0x00}, protocolMaxMsgSize+1)
	if err := p2p.Send(remote, BlockHeadersMsg, big); err != nil {
		t.Fatalf("failed to send oversized message: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dispatch accepted an oversized message")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch did not reject the oversized message")
	}
}
