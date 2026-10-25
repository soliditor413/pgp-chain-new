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

// Package pgp implements the read-only consensus engine of the pgp-chain
// legacy history, i.e. the blocks that were produced by the PBFT/DPoS rules of
// pgp-chain before the switch to Parlia.
//
// The engine never produces blocks: all sealing related methods return an
// explicit error instead of silently succeeding. It validates headers, and
// verifies the PBFT confirm embedded in header.Extra when
// params.LegacyConfig.VerifySeal is on. Blocks whose header.Nonce is zero are
// verified against the fixed producer set in params.LegacyConfig.Producers and
// need no ELA SPV database; blocks with a non-zero nonce need it to resolve the
// arbiter set for that ELA height (see params.LegacyConfig.SPVDataDir).
package pgp

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/gopool"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// legacyBlockPeriod is the minimum distance in seconds between two legacy
	// block timestamps. pgp-chain hardcodes `blockPeriod = 3` in
	// consensus/pbft.New and rejects `parent.Time+period > header.Time`.
	legacyBlockPeriod = 3

	// allowedFutureBlockTime is the tolerance applied to locally received
	// headers whose timestamp is slightly in the future.
	allowedFutureBlockTime = 15 * time.Second

	// maxGasLimit is the largest gas limit a header may declare (2^63-1).
	maxGasLimit = uint64(0x7fffffffffffffff)
)

// diffInTurn is the only difficulty a legacy block may carry. pgp-chain's
// consensus/pbft.verifyHeader rejects every other value for number > 0, so a
// header with difficulty 1 is *not* valid even though CalcDifficulty can return
// it for out-of-turn producers.
var diffInTurn = big.NewInt(2)

var (
	errUnknownBlock      = errors.New("unknown block")
	errInvalidGasLimit   = errors.New("invalid gasLimit")
	errInvalidGasUsed    = errors.New("invalid gasUsed")
	errInvalidMixDigest  = errors.New("invalid mix digest")
	errInvalidUncleHash  = errors.New("invalid uncle hash")
	errUnclesNotAllowed  = errors.New("uncles not allowed")
	errInvalidDifficulty = errors.New("invalid difficulty")
	errInvalidTimestamp  = errors.New("invalid timestamp")
	errInvalidExtra      = errors.New("invalid extra data")

	// ErrInvalidConfirm is returned when the PBFT confirm in header.Extra does
	// not prove what it has to prove: it is not bound to this header, its
	// proposal or a vote signature does not verify, a signer is not a producer,
	// or too few distinct producers accepted it.
	ErrInvalidConfirm = errors.New("pgp: invalid PBFT confirm")

	// errChainForkBlock is returned for a block at an already confirmed height
	// whose confirm carries a smaller view offset than the canonical one, i.e. a
	// fork attempt aiming at double finality at that height.
	errChainForkBlock = errors.New("pgp: chain fork block")

	// errDoubleSignBlock is returned when the same height and view offset are
	// confirmed by two different blocks.
	errDoubleSignBlock = errors.New("pgp: double sign block")

	// errProductionNotSupported is returned by every block production entry
	// point. The legacy chain is read-only for this client.
	errProductionNotSupported = errors.New("pgp: block production not supported by the legacy engine")

	// ErrFinalizeNotImplemented is no longer returned by anything. It is kept
	// so that external references keep compiling, but note the wording it used
	// to carry was wrong: the fee split is NOT part of Finalize on pgp-chain.
	// Finalize only recomputes the state root and clears the uncle hash
	// (pgp-chain/consensus/pbft/pbft.go:522-547); the 65:35 split, the pass
	// balance and the full gas refund all happen per transaction inside
	// TransitionDb (pgp-chain/core/state_transition.go:331-352).
	ErrFinalizeNotImplemented = errors.New("pgp: legacy finalization not implemented")
)

// LegacyExecutionReady is the bad-block gate of the eth/64 downloader.
//
// It is deliberately NOT a "is Finalize implemented" switch. The state root of a
// legacy block only matches when the whole legacy execution rule set is aligned,
// and every single item below changes the root if it is off:
//
//   - Context.BaseFee = 0 instead of nil (core/evm.go)
//   - 0x44 returns 0 on the legacy jump table (core/vm/jump_table.go)
//   - per transaction 65:35 fee split (core/state_transition.go payLegacyFee)
//   - refunds capped at gasUsed/2 (core/state_transition.go calcRefund)
//   - EIP-3607 EOA check skipped (core/state_transition.go preCheck)
//   - EIP-3860 initcode word gas not charged (core/state_transition.go execute)
//   - PG bloom over logs + from + to (core/types/bloom9.go and its callers)
//
// Flipping this to false makes the downloader stop before handing a block to
// core.BlockChain.InsertChain, which keeps an unexecutable block out of the
// on-disk bad block list. It is the kill switch for a rule regression, not a
// development toggle.
//
// Known gap, does NOT affect this flag: the twelve ELA cross chain precompiles
// (pgp-chain/core/vm/contracts.go:149-171) are not implemented, so a legacy
// block whose transactions call one of them still produces a wrong root. None of
// the blocks imported so far touch them.
func LegacyExecutionReady() bool {
	return true
}

// Engine is the read-only consensus engine for the pgp-chain legacy history.
type Engine struct {
	config *params.LegacyConfig
}

// New creates the legacy engine. A nil config is allowed and leaves every
// optional check disabled.
func New(config *params.LegacyConfig) *Engine {
	if config == nil {
		config = &params.LegacyConfig{}
	}
	return &Engine{config: config}
}

// Author implements consensus.Engine. The legacy chain has no signer recovery
// in this client, so the coinbase is reported as-is.
func (e *Engine) Author(header *types.Header) (common.Address, error) {
	if header == nil {
		return common.Address{}, errUnknownBlock
	}
	return header.Coinbase, nil
}

// SealHash returns the hash of a block prior to it being sealed.
//
// It must stay byte for byte identical to pgp-chain's consensus/pbft.SealHash,
// otherwise every PBFT confirm signature replay would fail:
// 14 header fields, in this order, with Extra and the chain id left out.
func SealHash(header *types.Header) common.Hash {
	enc, err := rlp.EncodeToBytes([]interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		// header.Extra is intentionally omitted: pgp-chain comments it out in
		// encodeSigHeader. Extra carries the PBFT confirm, i.e. the signatures
		// over this very hash, so including it would be circular.
		header.MixDigest,
		header.Nonce,
	})
	if err != nil {
		panic("pgp: can't encode sig header: " + err.Error())
	}
	return crypto.Keccak256Hash(enc)
}

// SealHash implements consensus.Engine.
func (e *Engine) SealHash(header *types.Header) common.Hash { return SealHash(header) }

// VerifyHeader implements consensus.Engine.
func (e *Engine) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return e.verifyHeader(chain, header, nil)
}

// VerifyHeaders implements consensus.Engine. The implementation follows
// consensus/clique and consensus/parlia: one worker runs the sequential
// verifications and streams them into a buffered channel.
func (e *Engine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	gopool.Submit(func() {
		for i, header := range headers {
			err := e.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	})
	return abort, results
}

// verifyHeader checks whether a header conforms to the legacy PBFT/DPoS rules.
// The caller may pass the ascendant headers that are not part of the local
// chain yet, which saves a database lookup per header.
func (e *Engine) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// The legacy rules only ever apply to blocks after genesis.
	if header.Number == nil || header.Number.Uint64() == 0 {
		return errUnknownBlock
	}
	// Don't waste time checking blocks from the future.
	if time.Now().Add(allowedFutureBlockTime).Unix() < int64(header.Time) {
		return consensus.ErrFutureBlock
	}
	if header.GasLimit > maxGasLimit {
		return fmt.Errorf("%w: have %d, max %d", errInvalidGasLimit, header.GasLimit, maxGasLimit)
	}
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("%w: have %d, gasLimit %d", errInvalidGasUsed, header.GasUsed, header.GasLimit)
	}
	// No fork protection on the legacy chain, so the mix digest is always zero.
	if header.MixDigest != (common.Hash{}) {
		return fmt.Errorf("%w: have %x", errInvalidMixDigest, header.MixDigest)
	}
	// Uncles are meaningless in PBFT.
	if header.UncleHash != types.CalcUncleHash(nil) {
		return fmt.Errorf("%w: have %x", errInvalidUncleHash, header.UncleHash)
	}
	if header.Difficulty == nil || header.Difficulty.Cmp(diffInTurn) != 0 {
		return fmt.Errorf("%w: have %v, want %v", errInvalidDifficulty, header.Difficulty, diffInTurn)
	}
	// header.Extra is the serialized PBFT payload.Confirm, and nothing else: the
	// 1199 bytes of a mainnet legacy header are one proposal plus eight votes in
	// ELA's own encoding, with no vanity prefix to skip. Only the floor can be
	// checked here; the structure is validated in decodeConfirm.
	if len(header.Extra) < legacyConfirmMinLen {
		return fmt.Errorf("%w: have %d bytes, want at least %d", errInvalidExtra, len(header.Extra), legacyConfirmMinLen)
	}

	number := header.Number.Uint64()
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time+legacyBlockPeriod > header.Time {
		return fmt.Errorf("%w: have %d, parent %d, period %d", errInvalidTimestamp, header.Time, parent.Time, legacyBlockPeriod)
	}
	// The PBFT confirm in Extra is what proves a real producer set agreed on this
	// block. params.PGPChainConfig turns the check on, so mainnet runs it; the
	// switch exists for chains that reuse these rules with a different producer
	// set, and to make the cost measurable by turning it off in a build.
	if e.config.VerifySeal {
		return e.verifySeal(chain, header)
	}
	return nil
}

// verifySeal checks the PBFT confirm attached to a header
// (pgp-chain/consensus/pbft/pbft.go:489-548).
//
// It is drivable from nothing but the header and its Extra, so it can be pointed
// at recorded mainnet blocks without a running chain.
func (e *Engine) verifySeal(chain consensus.ChainHeaderReader, header *types.Header) error {
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	confirm, err := decodeConfirm(header.Extra)
	if err != nil {
		return err
	}

	// Bind the confirm to this exact block. Everything else in the confirm is
	// portable: without this check, any confirm captured from the network can be
	// pasted onto an arbitrary forged header, and the signatures would still
	// verify because they only ever commit to the proposal.
	//
	// The comparison is a plain array conversion, no byte reversal: SealHash
	// produces the hash in ELA's byte order already, verified against mainnet
	// block 30115 whose proposal carries exactly these 32 bytes.
	if want := SealHash(header); common.Hash(confirm.Proposal.BlockHash) != want {
		return fmt.Errorf("%w: proposal commits to block %x, seal hash is %x",
			ErrInvalidConfirm, common.Hash(confirm.Proposal.BlockHash), want)
	}

	if err := e.verifyConfirm(confirm, header.Nonce.Uint64()); err != nil {
		return err
	}

	oldHeader := chain.GetHeaderByNumber(number)
	if oldHeader == nil {
		return nil
	}
	oldConfirm, err := decodeConfirm(oldHeader.Extra)
	if err != nil {
		// Soft failure, copied deliberately. Historical headers are not all
		// confirms — the genesis-adjacent blocks in particular — and turning a
		// parse error on one of them into a verification error would stop the
		// sync dead on a block the network considers canonical.
		return nil
	}
	// The `number < current.Number` half is not an optimisation. Without it, an
	// ordinary view change at the tip (same height, smaller view offset) would
	// make the node reject its own canonical block.
	current := chain.CurrentHeader()
	if confirm.Proposal.ViewOffset < oldConfirm.Proposal.ViewOffset &&
		current != nil && number < current.Number.Uint64() {
		return fmt.Errorf("%w: view offset %d, canonical block has %d at height %d",
			errChainForkBlock, confirm.Proposal.ViewOffset, oldConfirm.Proposal.ViewOffset, number)
	}
	if confirm.Proposal.ViewOffset == oldConfirm.Proposal.ViewOffset && oldHeader.Hash() != header.Hash() {
		// A nonce of MaxUint64 means "the ELA height was unknown when this block
		// was sealed", so two such blocks can legitimately differ. Mainnet only
		// ever carries 0, 2076195 and 2076267, so this branch does not fire on
		// the real history; it exists to keep the sentinel meaning what it says.
		if oldHeader.Nonce.Uint64() != math.MaxUint64 && header.Nonce.Uint64() != math.MaxUint64 {
			return fmt.Errorf("%w: height %d, view offset %d, canonical block %s, this block %s",
				errDoubleSignBlock, number, confirm.Proposal.ViewOffset, oldHeader.Hash(), header.Hash())
		}
	}
	return nil
}

// VerifyUncles implements consensus.Engine.
func (e *Engine) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errUnclesNotAllowed
	}
	return nil
}

// ErrRequestsNotAllowed is returned for a legacy header or execution result that
// carries EIP-7685 requests.
var ErrRequestsNotAllowed = errors.New("pgp: requests not allowed before the fork")

// VerifyRequests implements consensus.Engine. The legacy chain predates
// EIP-7685, and both sides of the check are unreachable for a valid block: the
// legacy headers are 15 field RLP structs, so RequestsHash is always nil, and
// core/postExecution only fills the request list when Prague is active, which it
// never is on the legacy rules. Each field is therefore rejected on its own
// instead of only when both appear together, so a malformed or post-fork header
// routed here by mistake cannot slip through.
func (e *Engine) VerifyRequests(header *types.Header, requests [][]byte) error {
	if header.RequestsHash != nil || len(requests) > 0 {
		return ErrRequestsNotAllowed
	}
	return nil
}

// NextInTurnValidator implements consensus.Engine. The producer schedule of the
// legacy chain comes from the ELA SPV database, so it cannot be resolved here.
func (e *Engine) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	return common.Address{}, errors.New("pgp: producer schedule requires the ELA SPV database")
}

// Prepare implements consensus.Engine. The legacy chain is read-only, so there
// is nothing to prepare; block production fails in Seal.
func (e *Engine) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	return nil
}

// Finalize implements consensus.Engine.
//
// pgp-chain's Finalize does exactly two things to the state: recompute the root
// and reset the uncle hash (pgp-chain/consensus/pbft/pbft.go:522-547). There is
// no block reward — the DPoS chain pays producers nothing for sealing — and the
// 65:35 fee split is NOT applied here either: pgp-chain routes the fee inside
// the per-transaction state transition (pgp-chain/core/state_transition.go:331-352),
// which this client mirrors in core/state_transition.go. Doing it here as well
// would double count it.
//
// It intentionally does not implement core's blockAssembler interface
// (FinalizeAndAssemble): leaving it out makes core.AssembleBlock fall back to
// this method. Production still fails earlier, in Seal.
func (e *Engine) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state vm.StateDB,
	txs *[]*types.Transaction, uncles []*types.Header, withdrawals []*types.Withdrawal,
	receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) error {
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	header.UncleHash = types.CalcUncleHash(nil)
	return nil
}

// Seal implements consensus.Engine. The legacy chain is read-only for this
// client: producing a block would require the PBFT round machinery.
func (e *Engine) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	return errProductionNotSupported
}

// CalcDifficulty implements consensus.Engine. pgp-chain seals every block with
// diffInTurn, see consensus/pbft.CalcDifficulty.
func (e *Engine) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return new(big.Int).Set(diffInTurn)
}

// Delay implements consensus.Engine.
func (e *Engine) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	return nil
}

// Close implements consensus.Engine.
func (e *Engine) Close() error { return nil }
