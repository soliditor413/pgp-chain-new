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

// Prepare rewrites the seal-carrying fields of the header it is given (Extra,
// Difficulty, MixDigest, UncleHash, Nonce) because the miner fills some of them
// in with values that are invalid on a legacy block. That rewrite is only correct
// for a round this node is actually going to run.
//
// The two "this node cannot produce at all" gates therefore have to answer before
// the rewrite: production disabled (--pbft.enable absent) and no signing key. Both
// used to run after it, which meant that a refused Prepare had already mutated the
// caller's header -- harmless today only because miner/worker.go's header is a
// local that is dropped on error, and impossible to state as "disabled means no
// side effects".

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// headerSealSnapshot is every field Prepare may rewrite, copied out so the check
// is a comparison rather than a claim.
type headerSealSnapshot struct {
	time       uint64
	extra      []byte
	difficulty *big.Int
	mixDigest  common.Hash
	uncleHash  common.Hash
	nonce      types.BlockNonce
}

func snapshotHeader(h *types.Header) headerSealSnapshot {
	snap := headerSealSnapshot{
		time:      h.Time,
		mixDigest: h.MixDigest,
		uncleHash: h.UncleHash,
		nonce:     h.Nonce,
	}
	if h.Extra != nil {
		snap.extra = append([]byte(nil), h.Extra...)
	}
	if h.Difficulty != nil {
		snap.difficulty = new(big.Int).Set(h.Difficulty)
	}
	return snap
}

// diff reports the fields that differ, in a form that names them.
func (s headerSealSnapshot) diff(h *types.Header) string {
	var out string
	add := func(format string, args ...any) { out += fmt.Sprintf(format+"\n", args...) }

	if s.time != h.Time {
		add("  Time:       %d -> %d", s.time, h.Time)
	}
	if !bytesEqual(s.extra, h.Extra) {
		add("  Extra:      %x -> %x", s.extra, h.Extra)
	}
	switch {
	case s.difficulty == nil && h.Difficulty != nil:
		add("  Difficulty: <nil> -> %v", h.Difficulty)
	case s.difficulty != nil && h.Difficulty == nil:
		add("  Difficulty: %v -> <nil>", s.difficulty)
	case s.difficulty != nil && s.difficulty.Cmp(h.Difficulty) != 0:
		add("  Difficulty: %v -> %v", s.difficulty, h.Difficulty)
	}
	if s.mixDigest != h.MixDigest {
		add("  MixDigest:  %x -> %x", s.mixDigest, h.MixDigest)
	}
	if s.uncleHash != h.UncleHash {
		add("  UncleHash:  %x -> %x", s.uncleHash, h.UncleHash)
	}
	if s.nonce != h.Nonce {
		add("  Nonce:      %x -> %x", s.nonce, h.Nonce)
	}
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newPrepareTestHeader is a header carrying exactly the values a miner leaves in
// place: a vanity extra, the wrong difficulty, a non-zero mix digest and a nonce.
func newPrepareTestHeader() *types.Header {
	return &types.Header{
		ParentHash:  common.HexToHash("0xbeef"),
		UncleHash:   common.HexToHash("0xc0ffee"),
		Coinbase:    common.HexToAddress("0x00000000000000000000000000000000000000aa"),
		Root:        common.HexToHash("0x01"),
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Number:      big.NewInt(1),
		GasLimit:    30_000_000,
		Time:        1_700_000_000,
		Extra:       []byte("deadbeef"),
		Difficulty:  big.NewInt(1),
		MixDigest:   common.HexToHash("0x1234"),
		Nonce:       types.EncodeNonce(7),
	}
}

// TestPrepareIsSideEffectFreeWhenRefused covers both "cannot produce at all" gates
// and asserts the header comes back byte for byte as it went in.
func TestPrepareIsSideEffectFreeWhenRefused(t *testing.T) {
	_, producerKeys := newProducerSet(t, 1)
	chain := &sealTestChain{genesis: &types.Header{
		Number:     big.NewInt(0),
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}}

	cases := []struct {
		name  string
		cfg   *Config
		want  error
		setup func(*Engine)
	}{
		{
			name: "production disabled",
			cfg:  nil,
			want: pgp.ErrProductionNotSupported,
		},
		{
			name: "no signer",
			cfg:  &Config{Enable: true},
			want: ErrNotProducer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, tc.cfg)
			defer engine.Close()
			if tc.setup != nil {
				tc.setup(engine)
			}

			header := newPrepareTestHeader()
			before := snapshotHeader(header)

			err := engine.Prepare(chain, header)
			if err != tc.want {
				t.Fatalf("Prepare returned %v, want %v", err, tc.want)
			}
			if diff := before.diff(header); diff != "" {
				t.Fatalf("Prepare refused (%v) but still rewrote the header:\n%s", err, diff)
			}
		})
	}
}

// TestPrepareDoesRewriteWhenItAccepts is the other half of the contract: the
// snapshot comparison above is only meaningful if Prepare really does rewrite the
// header when it accepts the work.
func TestPrepareDoesRewriteWhenItAccepts(t *testing.T) {
	accounts, producerKeys := newProducerSet(t, 1)
	chain := &sealTestChain{genesis: &types.Header{
		Number:     big.NewInt(0),
		Time:       uint64(1_700_000_000),
		Difficulty: new(big.Int).Set(diffInTurn),
		UncleHash:  types.CalcUncleHash(nil),
	}}

	engine := New(&params.LegacyConfig{VerifySeal: true, Producers: producerKeys}, &Config{Enable: true})
	engine.attachAccount(accounts[0])
	engine.MarkRecovered()
	engine.AttachChain(chain)
	defer engine.Close()

	header := newPrepareTestHeader()
	header.ParentHash = chain.genesis.Hash()
	before := snapshotHeader(header)

	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare refused a header it should accept: %v", err)
	}
	if header.Extra != nil {
		t.Fatalf("accepted Prepare left Extra as %x, want nil for a legacy block", header.Extra)
	}
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		t.Fatalf("accepted Prepare left Difficulty as %v, want %v", header.Difficulty, diffInTurn)
	}
	if header.MixDigest != (common.Hash{}) {
		t.Fatalf("accepted Prepare left MixDigest as %x, want zero", header.MixDigest)
	}
	if diff := before.diff(header); diff == "" {
		t.Fatal("the header is unchanged after an accepted Prepare, so the side-effect check above proves nothing")
	}
}
