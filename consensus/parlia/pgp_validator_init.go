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

package parlia

import (
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/systemcontracts/pgp"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// This file adapts Parlia to a chain whose history was produced by another
// consensus and only switches to Parlia at a non-zero height.
//
// Upstream Parlia assumes it owns the chain from block 0: the validator set is
// parsed out of the genesis header's extra field (parlia.snapshot, number == 0),
// and the system contracts are initialised in the block whose number is 1
// (parlia.go:1465, :1573). Neither holds here. The pgp genesis header carries a
// PBFT producer set and a P-256 confirm signature, not a 20-byte address list
// and a 65-byte secp256k1 seal, so parsing it as a Parlia header yields garbage;
// and the first Parlia block is block ForkHeight, not block 1, so the system
// contracts would never be initialised at all.
//
// Two things are therefore added:
//
//  1. A snapshot anchor at ForkHeight-1 whose validator set is read from the
//     chain config instead of from a header. It is exact rather than
//     approximate: before ForkHeight the validator set is a constant, because
//     it comes from LegacyConfig.
//  2. The validator set that the deploy-time injection feeds to
//     BSCValidatorSet.init(bytes,uint256) at Legacy.SystemContractDeployHeight,
//     i.e. before the fork, instead of the compile-time constant the contract
//     used to read. The init itself does NOT run at ForkHeight any more: see
//     shouldInitContract.

// The validator-set encoding itself lives in core/systemcontracts/pgp: the
// protocol's deployment step (core/pgp_systemcontract_deploy.go) needs the same
// bytes, and core cannot import parlia. These are the thin local names the
// engine's own callers and tests keep using.
type pgpGenesisValidatorSet = pgp.ValidatorSet

// newPGPGenesisValidatorSet validates LegacyConfig's validator fields through
// the shared encoder.
func newPGPGenesisValidatorSet(cfg *params.LegacyConfig) (*pgpGenesisValidatorSet, error) {
	return pgp.FromLegacy(cfg)
}

// pgpValidatorInitArgs builds the two arguments BSCValidatorSet.init takes.
func pgpValidatorInitArgs(cfg *params.LegacyConfig) ([]byte, uint64, error) {
	return pgp.ValidatorInitArgs(cfg)
}

// pgpValidatorSetInitSelector is the 4-byte selector of
// BSCValidatorSet.init(bytes,uint256), i.e. keccak256 of the canonical
// signature.
//
// It is derived rather than hardcoded so that it stays auditable, and it is
// asserted against the compiled ABI in New() so that a stale ABI - one still
// exposing the pre-C1-a no-argument init() - fails at startup instead of at
// ForkHeight.
var pgpValidatorSetInitSelector = crypto.Keccak256([]byte("init(bytes,uint256)"))[:4]

// pgpPlainInitSelector is the 4-byte selector of the no-argument init() that the
// other six system contracts still expose.
//
// initContract() feeds the same calldata to seven addresses. Upstream could pack
// "init" once from validatorSetABI because all seven took no arguments. Now only
// the validator contract does, and the ABI no longer describes a no-argument
// init() at all, so Pack("init") cannot produce these four bytes any more. They
// are derived here instead.
//
// It evaluates to 0xe1c7392a, which is what signer/fourbyte/4byte.json records
// for init() and what the previous no-argument ABI method id was - so the six
// non-validator contracts keep receiving exactly the calldata they always did.
var pgpPlainInitSelector = crypto.Keccak256([]byte("init()"))[:4]

// pgpInitPlan is the pre-computed validator init arguments, built once at
// engine construction so that a bad validator configuration fails at startup
// rather than at the block that would consume it. Consumers: the deploy-time
// injection (core/systemcontracts/deploy_pgp.go) and the startup log.
//
// The calldata is deliberately not packed here: Pack needs the ABI, and the
// deploy step builds it where the ABI is already needed for the selector.
type pgpInitPlan struct {
	// payload is the RLP blob passed as the first init() argument.
	payload []byte

	// cabinets is the value passed as the second init() argument.
	cabinets uint64
}

// newPGPInitPlan returns the init plan for a chain config, or nil when no
// injection is needed.
//
// A nil Legacy block, or a ForkHeight still at math.MaxUint64, means "this
// chain never switches", so there is nothing to inject and no validator set to
// demand. That case must stay silent: PGPChainConfig ships with
// ForkHeight = math.MaxUint64 precisely so that a node syncing pgp mainnet
// today does not need a Parlia validator set in its config.
func newPGPInitPlan(chainConfig *params.ChainConfig) (*pgpInitPlan, error) {
	if chainConfig == nil || chainConfig.Legacy == nil {
		return nil, nil
	}
	if chainConfig.Legacy.ForkHeight == math.MaxUint64 {
		return nil, nil
	}
	payload, cabinets, err := pgpValidatorInitArgs(chainConfig.Legacy)
	if err != nil {
		return nil, err
	}
	return &pgpInitPlan{payload: payload, cabinets: cabinets}, nil
}

// shouldInitContract reports whether Finalize/FinalizeAndAssemble run the
// system-contract initialisation for this block.
//
// Block 1 only. Upstream initialises there and so does this chain: block 1 is a
// legacy block whose alloc carries no code, so the call is a no-op, and
// replaying the history depends on it staying a no-op.
//
// The Parlia-era initialisation deliberately does NOT run here any more. It
// happens at Legacy.SystemContractDeployHeight, before the fork block, in the
// protocol's own deployment step (core/systemcontracts/deploy_pgp.go), which
// installs real code and initialises it. Running initContract at ForkHeight as
// well would send init() to contracts that are already initialised, they would
// revert, and the revert would fail the fork block and halt the chain.
func (p *Parlia) shouldInitContract(number *big.Int) bool {
	if number == nil {
		return false
	}
	return number.Cmp(common.Big1) == 0
}

// pgpSnapshotAnchorNumber returns the height at which the Parlia snapshot
// chain is anchored, i.e. the last block produced before the switch.
//
// ok is false when the chain has no Parlia switch, in which case upstream's
// number == 0 anchor is the right one and this file stays out of the way.
func (p *Parlia) pgpSnapshotAnchorNumber() (number uint64, ok bool) {
	// A fork at height 0 would mean Parlia from genesis, where upstream's own
	// anchor applies and there is no ForkHeight-1 block to anchor at.
	if p.pgpInit == nil || p.chainConfig.Legacy.ForkHeight == 0 || p.chainConfig.Legacy.ForkHeight == math.MaxUint64 {
		return 0, false
	}
	return p.chainConfig.Legacy.ForkHeight - 1, true
}

// pgpAnchorSnapshot builds the snapshot for the last legacy block.
//
// It is built from the chain config rather than by parsing a header, because
// that header was produced by the legacy consensus and does not contain a
// Parlia validator list. This is exact, not an approximation: before ForkHeight
// the validator set is constant by construction.
//
// Recents is empty, which upstream explicitly tolerates for snapshots built by
// newSnapshot from a header (parlia.go:896-899): the double-sign check becomes
// more lenient, the block time check is not consulted during apply, and the
// slash decision is never verified against a transaction list. The cost is that
// a validator may sign twice in the first few blocks after the switch; the
// benefit is not having to invent a history that never happened.
func (p *Parlia) pgpAnchorSnapshot(number uint64, hash common.Hash) (*Snapshot, error) {
	cfg := p.chainConfig.Legacy
	set, err := newPGPGenesisValidatorSet(cfg)
	if err != nil {
		return nil, err
	}

	// Zero BLS public keys, in the same shape parseValidators produces for a
	// post-Luban epoch header whose validators have no BLS key registered yet.
	//
	// This is not a placeholder that upstream never exercises: verifyValidators
	// and prepareValidators both substitute a zero key explicitly on the Luban
	// fork block (parlia.go:1271-1277, :1035-1041), so a set of zero keys is a
	// state BSC itself passes through. Vote attestation verification cannot be
	// reached from here, because it returns early when a header carries no
	// attestation, and a zero key cannot be turned into a signature.
	voteAddrs := make([]types.BLSPublicKey, len(set.Consensus))

	snap := newSnapshot(p.config, p.signatures, number, hash, set.Consensus, voteAddrs, p.ethAPI)
	snap.BlockInterval = defaultBlockInterval
	snap.TurnLength = defaultTurnLength
	// EpochLength is left at newSnapshot's defaultEpochLength. On a chain where
	// Lorentz is not activated - which is the case for this fork, since its
	// time-based forks cannot be pinned in genesis - the epoch length never
	// changes away from that default, so the anchor matches what apply() will
	// go on to compute.
	return snap, nil
}
