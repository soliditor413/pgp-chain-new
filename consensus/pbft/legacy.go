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

import (
	"errors"
	"fmt"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/types/payload"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/dpos"
)

// This file is the seam between go-ethereum types and the two interfaces dpos
// exposes. It is the only place that knows both, which is what keeps dpos free of
// core/types (and its cgo BLS dependency) and consensus/pgp free of dpos.

// dBlock adapts *types.Block to dpos.DBlock.
//
// The three getters are not a renaming exercise: GetHash must return the PBFT
// SEAL hash, not the block hash, because that is what a confirm commits to
// (confirm.Proposal.BlockHash is the seal hash). A pool keyed by block hash
// would never match a proposal.
type dBlock struct{ *types.Block }

func newDBlock(block *types.Block) *dBlock { return &dBlock{block} }

// GetHash returns the PBFT seal hash, i.e. what confirm.Proposal.BlockHash
// carries. See consensus/pgp.SealHash for the field list.
func (b *dBlock) GetHash() elacom.Uint256 { return elacom.Uint256(pgp.SealHash(b.Header())) }

func (b *dBlock) GetHeight() uint64 { return b.NumberU64() }

// Nonce is the ELA main chain height anchored in the header, not the PoW nonce.
// The wrapper's method shadows the embedded types.Block.Nonce so that dpos reads
// the uint64 it expects without a conversion at every call site.
func (b *dBlock) Nonce() uint64 { return b.Block.Nonce() }

func (b *dBlock) Time() uint64 { return b.Block.Time() }

// verifyConfirm is dpos.BlockPool's confirm gate.
//
// It delegates to consensus/pgp rather than reimplementing the threshold and
// signature checks, so the block-propagation path and the header-verification
// path cannot disagree about what a valid confirm is.
//
// The timestamp argument dpos passes is unused: pgp's barrier is the ELA height
// in header.Nonce, and the block's own time is already bound into the seal hash
// the proposal commits to.
func (e *Engine) verifyConfirm(confirm *payload.Confirm, elaHeight uint64, _ int64) error {
	if confirm == nil {
		return errors.New("pbft: nil confirm")
	}
	return e.Engine.VerifyConfirmAt(confirm, elaHeight)
}

// verifyBlock is dpos.BlockPool's block sanity gate, run before a block received
// over the DPoS channel is added to the pool.
//
// It deliberately duplicates a few of pgp.verifyHeader's field checks instead of
// calling it: verifyHeader needs a ChainHeaderReader and the parent header, and
// the pool receives blocks whose parent may not be imported yet. What is checked
// here is exactly what can be decided from the block alone.
func (e *Engine) verifyBlock(block dpos.DBlock) error {
	blk, ok := block.(*dBlock)
	if !ok {
		return errors.New("pbft: pool block is not an ethereum block")
	}
	header := blk.Header()

	if header.GasLimit > maxGasLimit {
		return fmt.Errorf("pbft: invalid gasLimit %d", header.GasLimit)
	}
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("pbft: gasUsed %d exceeds gasLimit %d", header.GasUsed, header.GasLimit)
	}
	if header.MixDigest != (common.Hash{}) {
		return fmt.Errorf("pbft: mix digest must be zero, have %x", header.MixDigest)
	}
	if header.UncleHash != types.CalcUncleHash(nil) {
		return fmt.Errorf("pbft: uncles are not allowed")
	}
	if header.Difficulty == nil || header.Difficulty.Cmp(diffInTurn) != 0 {
		return fmt.Errorf("pbft: invalid difficulty %v", header.Difficulty)
	}
	if len(blk.Uncles()) > 0 {
		return fmt.Errorf("pbft: block carries %d uncles", len(blk.Uncles()))
	}
	return nil
}
