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
	"math/big"
	"time"
)

const (
	// blockPeriod is the minimum distance in seconds between two block
	// timestamps, and the only value pgp-chain has ever run with
	// (consensus/pbft.New, `blockPeriod := 3`).
	//
	// It is a constant, not a configuration field, on purpose: two nodes that
	// disagree on the period disagree on which block is valid, which is a chain
	// split rather than a configuration difference. See
	// docs/design/pbft-port-design.md D8.
	blockPeriod = 3

	// signTolerance is how long one view may fail to produce a block before the
	// proposer rotates. pgp-chain derives it as 2 x blockPeriod.
	//
	// The value is consensus critical in a weaker sense than blockPeriod: nodes
	// with different tolerances would still accept each other's blocks, but they
	// would rotate proposers at different moments, halving throughput during any
	// hiccup. Treat it as fixed.
	signTolerance = 2 * blockPeriod * time.Second

	// maxGasLimit is the largest gas limit a header may declare (2^63-1).
	maxGasLimit = uint64(0x7fffffffffffffff)
)

// diffInTurn is the only difficulty a legacy block may carry.
//
// pgp-chain's CalcDifficulty returns 1 for an out-of-turn producer, but
// verifyHeader rejects every value other than 2 for number > 0, so a header
// carrying 1 is never valid. Returning 2 unconditionally is therefore the
// honest implementation: the out-of-turn header would have been discarded
// anyway (see docs/design/pbft-port-design.md D5).
var diffInTurn = big.NewInt(2)
