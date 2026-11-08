package router

// Regression tests for a header batch that straddles the fork height.
//
// The bug these pin down: a batch carrying F-2, F-1 and F is handed to
// Router.VerifyHeaders as one slice, and F's parent lives in that slice rather
// than in the database -- blockchain.insertChain and
// HeaderChain.ValidateHeaderChain both validate before writing, so this is the
// normal case for a syncing node, not a race. Any implementation that loses the
// batch prefix makes such a batch impossible to verify, which stops every node
// that is more than one batch behind at the fork height, forever.

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// boundaryChain is a header store that only knows what was written before the
// batch arrived.
type boundaryChain struct {
	cfg *params.ChainConfig
	db  map[uint64]*types.Header
}

func newBoundaryChain(cfg *params.ChainConfig) *boundaryChain {
	return &boundaryChain{cfg: cfg, db: make(map[uint64]*types.Header)}
}

func (c *boundaryChain) put(h *types.Header) { c.db[h.Number.Uint64()] = h }

func (c *boundaryChain) Config() *params.ChainConfig  { return c.cfg }
func (c *boundaryChain) CurrentHeader() *types.Header { return c.db[0] }
func (c *boundaryChain) GetHeader(h common.Hash, n uint64) *types.Header {
	if x, ok := c.db[n]; ok && x.Hash() == h {
		return x
	}
	return nil
}
func (c *boundaryChain) GetHeaderByNumber(n uint64) *types.Header         { return c.db[n] }
func (c *boundaryChain) GetHeaderByHash(common.Hash) *types.Header        { return nil }
func (c *boundaryChain) GenesisHeader() *types.Header                     { return c.db[0] }
func (c *boundaryChain) GetTd(common.Hash, uint64) *big.Int               { return big.NewInt(0) }
func (c *boundaryChain) GetHighestVerifiedHeader() *types.Header          { return c.db[0] }
func (c *boundaryChain) GetVerifiedBlockByHash(common.Hash) *types.Header { return nil }

// boundaryHeader builds a header the legacy rules accept: difficulty 2 (in
// turn), a 3 second period, and an extra field long enough for the confirm
// floor.
func boundaryHeader(num uint64, parent common.Hash) *types.Header {
	return &types.Header{
		ParentHash: parent,
		Number:     big.NewInt(int64(num)),
		GasLimit:   30_000_000,
		Difficulty: big.NewInt(2),
		UncleHash:  types.CalcUncleHash(nil),
		Extra:      make([]byte, 200),
		Time:       1_700_000_000 + num*3,
	}
}

// parentAwareEngine stands in for Parlia. It has the same ancestor resolution as
// the real engines (parents first, database second) and nothing else, which is
// the only thing a fork-crossing batch exercises.
type parentAwareEngine struct {
	*fakeEngine
	parentAwareCalls int
}

func resolveAncestor(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header == nil || header.Number == nil {
		return errors.New("header without number")
	}
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	}
	if parent == nil || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	return nil
}

func (e *parentAwareEngine) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return resolveAncestor(chain, header, nil)
}

func (e *parentAwareEngine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	for i, header := range headers {
		results <- resolveAncestor(chain, header, headers[:i])
	}
	return abort, results
}

func (e *parentAwareEngine) VerifyHeaderWithParents(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	e.parentAwareCalls++
	return resolveAncestor(chain, header, parents)
}

func forkCrossingFixture(t *testing.T, fork uint64) (*params.LegacyConfig, *boundaryChain, []*types.Header) {
	t.Helper()
	cfg := &params.ChainConfig{
		ChainID:        big.NewInt(860621),
		HomesteadBlock: big.NewInt(1),
		Parlia:         &params.ParliaConfig{},
		Legacy:         &params.LegacyConfig{ForkHeight: fork, VerifySeal: false},
	}
	chain := newBoundaryChain(cfg)
	// Only F-3 is on disk. The batch carries F-2, F-1 and F.
	h := boundaryHeader(fork-3, common.Hash{})
	chain.put(h)
	h2 := boundaryHeader(fork-2, h.Hash())
	h1 := boundaryHeader(fork-1, h2.Hash())
	h0 := boundaryHeader(fork, h1.Hash())
	return cfg.Legacy, chain, []*types.Header{h2, h1, h0}
}

// TestForkCrossingBatchVerifies is the fix's regression guard. Before the fix
// the batch failed with "unknown ancestor" at F-1.
func TestForkCrossingBatchVerifies(t *testing.T) {
	const fork = uint64(100)
	legacy, chain, batch := forkCrossingFixture(t, fork)

	posa := &parentAwareEngine{fakeEngine: &fakeEngine{name: "posa"}}
	r := New(pgp.New(legacy), posa, fork)

	abort, results := r.VerifyHeaders(chain, batch)
	defer close(abort)

	for i := range batch {
		if err := <-results; err != nil {
			t.Fatalf("header #%d of a fork-crossing batch failed: %v", batch[i].Number.Uint64(), err)
		}
	}
	// Exactly one header is post-fork, so the Parlia side must have been asked
	// for exactly one parent-aware verification.
	if posa.parentAwareCalls != 1 {
		t.Fatalf("Parlia verified %d headers, want 1", posa.parentAwareCalls)
	}
}

// TestSplittingWithoutParentsStillFails pins why the batch has to carry its own
// prefix. Splitting the slice and calling VerifyHeaders twice is the obvious
// fix and it does not work: the second call starts at F, whose parent F-1 sits
// in the first slice and was never written to the database.
//
// If someone ever "simplifies" the router back to a plain split, this test still
// passes while TestForkCrossingBatchVerifies fails, which is the point.
func TestSplittingWithoutParentsStillFails(t *testing.T) {
	const fork = uint64(100)
	legacy, chain, batch := forkCrossingFixture(t, fork)

	legacyAbort, legacyResults := pgp.New(legacy).VerifyHeaders(chain, batch[:2])
	defer close(legacyAbort)
	for i := 0; i < 2; i++ {
		if err := <-legacyResults; err != nil {
			t.Fatalf("the legacy slice must verify on its own, header %d: %v", i, err)
		}
	}

	posa := &parentAwareEngine{fakeEngine: &fakeEngine{name: "posa"}}
	posaAbort, posaResults := posa.VerifyHeaders(chain, batch[2:])
	defer close(posaAbort)
	if err := <-posaResults; !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("a plain split must still fail at the fork block, got %v", err)
	}
}

// TestForkCrossingBatchWithoutParentAwareEngine keeps the old behaviour for
// engines that do not implement consensus.ParentAwareVerifier: they still get
// called, and they report the failure themselves instead of the router
// panicking on a failed type assertion.
func TestForkCrossingBatchWithoutParentAwareEngine(t *testing.T) {
	const fork = uint64(100)
	legacy, chain, batch := forkCrossingFixture(t, fork)

	// fakeEngine has no VerifyHeaderWithParents and answers nil to everything.
	r := New(pgp.New(legacy), &fakeEngine{name: "posa"}, fork)
	abort, results := r.VerifyHeaders(chain, batch)
	defer close(abort)

	for i := range batch {
		if err := <-results; err != nil {
			t.Fatalf("header #%d failed: %v", batch[i].Number.Uint64(), err)
		}
	}
}
