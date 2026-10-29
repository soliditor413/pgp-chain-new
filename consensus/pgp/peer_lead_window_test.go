package pgp

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestPeerAcceptsFifteenSecondsOfLead pins the clock gate a peer applies to a
// header dated in that peer's own future.
//
// It exists because a comment depends on it. consensus/pbft/seal.go waits for
// header.Time before asking for votes, header.Time is at most allowedFutureBlockTime
// plus one period ahead (15s + 3s), and the comment there now says what releasing
// the block earlier would cost. That answer is two-tiered and this constant is the
// boundary: inside allowedFutureBlockTime a peer accepts the header on first sight
// with no deferral at all, beyond it VerifyHeader returns ErrFutureBlock and the
// deferral happens in core/blockchain.go instead. Without this test the comment's
// claim about the first tier would be unguarded.
//
// Only the boundary is asserted here. What core does with an ErrFutureBlock --
// queue it, retry it every few seconds, refuse it beyond maxTimeFutureBlocks --
// belongs to core and is deliberately out of scope: this test is about the rule a
// verifier applies to a timestamp, not about the client that carries the block.
func TestPeerAcceptsFifteenSecondsOfLead(t *testing.T) {
	genesis := &types.Header{Number: big.NewInt(0), Time: 1762218000}
	chain := &testChain{genesis: genesis}

	cases := []struct {
		lead int64
		want error
	}{
		{0, nil},
		{14, nil},
		{15, nil},                      // allowedFutureBlockTime, inclusive
		{16, consensus.ErrFutureBlock}, // one second past the boundary
		{31, consensus.ErrFutureBlock}, // past maxTimeFutureBlocks too; same error at this layer
	}
	for _, c := range cases {
		header := newTestHeader(genesis)
		header.Time = uint64(time.Now().Unix() + c.lead)

		err := New(nil).VerifyHeader(chain, header)
		if c.want == nil {
			// nil is the whole header being accepted, not merely this gate being
			// passed: newTestHeader builds one that satisfies every other rule.
			if err != nil {
				t.Errorf("lead=%ds: refused with %v; a peer accepts up to allowedFutureBlockTime (15s) on first sight", c.lead, err)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("lead=%ds: got %v, want %v", c.lead, err, c.want)
		}
	}
}
