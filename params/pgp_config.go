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

package params

import (
	"bytes"
	"encoding/hex"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// LegacyConfig carries everything needed to reproduce the pgp-chain history,
// i.e. the blocks that were produced by the PBFT/DPoS consensus before the
// switch to Parlia.
//
// IMPORTANT: no field of this struct (and no field added to ChainConfig) may be
// named with a "Block" or "Time" suffix. core/forkid.gatherForks discovers fork
// activation points by reflecting over such suffixes, so a field like
// "LegacyForkBlock" would silently change the EIP-2124 fork checksum and make
// every handshake with a pgp peer fail with ErrForkIDRejected.
type LegacyConfig struct {
	// ForkHeight is the first block produced by the new Parlia consensus.
	// Blocks [0, ForkHeight) are validated and executed with the legacy pgp
	// rules. It is a placeholder (math.MaxUint64) until the fork height is
	// fixed, which keeps the whole chain on the legacy rules.
	ForkHeight uint64 `json:"forkHeight,omitempty"`

	// GasFeeRecipient is the PG gas fee collector. Legacy rules route
	// GasFeeRecipientRatio percent of every transaction fee to this address and
	// the remainder to the block producer. pgp-chain calls the same field
	// DeveloperContract, but it is really the PG fee sink.
	GasFeeRecipient common.Address `json:"gasFeeRecipient"`

	// GasFeeRecipientRatio is the percentage (0-100) of the transaction fee that
	// is routed to GasFeeRecipient. pgp-chain mainnet uses 65.
	GasFeeRecipientRatio uint64 `json:"gasFeeRecipientRatio,omitempty"`

	// PassBalance is the temporary balance granted to recharge / refundWithdraw
	// senders for the duration of the execution. pgp-chain reads it from the
	// launch configuration rather than from the code.
	PassBalance uint64 `json:"passBalance,omitempty"`

	// VerifySeal enables PBFT confirm signature verification while replaying the
	// legacy history. Disabling it only checks hash linkage and is meant for
	// debugging, never for production.
	VerifySeal bool `json:"verifySeal,omitempty"`

	// Producers is the PBFT producer set that signed every legacy block whose
	// header.Nonce is zero, as compressed (33 byte) P256 public keys in hex.
	//
	// It is the pgp-chain Pbft.Producers list (pgp-chain/params/config.go:87-100).
	// Blocks 1..16404 of mainnet carry nonce 0, i.e. they were produced before
	// the ELA main chain had a height to anchor to, so their confirm signatures
	// have to be checked against this fixed list instead of the SPV arbiters.
	//
	// This is NOT spv.MainnetProducers (spv/nextturn_dposinfo.go): that is the
	// next-turn DPoS arbiter list, twelve different keys. Mixing them up makes
	// every nonce-0 block fail verification.
	//
	// The name deliberately does not end in "Block" or "Time"; see the warning
	// on LegacyConfig.
	Producers []string `json:"producers,omitempty"`

	// SPVDataDir points at the ELA SPV database that backs the legacy
	// precompiled contracts (arbiters, main chain headers, cross chain data).
	SPVDataDir string `json:"spvDataDir,omitempty"`

	// DynamicArbiterHeight is the ELA main chain height from which the SPV
	// listener stops treating the chain as proof-of-work and starts
	// maintaining the next-turn DPOS producer set, i.e. the height from which a
	// pgp node pre-connects to the arbiters that will produce under PBFT.
	//
	// pgp-chain does not keep this as a source constant either: it is the
	// default of the --spv.arbiter.height flag
	// (pgp-chain/cmd/utils/flags.go:808-812, mainnet 2075778, testnet 1496588
	// applied at :1652-1654), which SetEthConfig stores on the chain config
	// (pgp-chain/eth/backend.go:227-229) and cmd/pgp/main.go:456-463 reads back
	// out of it before starting the SPV service.
	//
	// Scope: listener bookkeeping only. Nothing on the block execution path
	// reads this value, so a wrong value cannot change a state root — it can
	// only change whether the listener ever reports a consensus mode switch.
	// That is exactly why leaving it at math.MaxUint64, as this port did while
	// the listener was half-written, failed silently instead of loudly:
	// InitNextTurnDposInfo returned early forever and the "turn to Dpos mode"
	// transition could never fire.
	DynamicArbiterHeight uint64 `json:"dynamicArbiterHeight,omitempty"`
}

// DefaultGasFeeRecipientRatio is the fee split used by pgp-chain mainnet since
// genesis: 65% to the PG fee sink, 35% to the block producer.
const DefaultGasFeeRecipientRatio = 65

// DefaultGasFeeRecipient is the PG gas fee collector used by pgp-chain mainnet.
var DefaultGasFeeRecipient = common.HexToAddress("0x4909fcaf28f539a0307a306d2788174aa733d911")

var (
	// DefaultPassBalance is the temporary balance granted to recharge /
	// refundWithdraw senders for the duration of the execution. pgp-chain does
	// not keep it as a source constant: it is the default of the --pass.balance
	// flag (pgp-chain cmd/utils/flags.go:772-776), and that default is what the
	// mainnet nodes run with.
	DefaultPassBalance = uint64(1000000000000000000) // 1e18

	// DefaultDynamicArbiterHeight is the mainnet value of pgp-chain's
	// --spv.arbiter.height flag (pgp-chain cmd/utils/flags.go:808-812).
	DefaultDynamicArbiterHeight = uint64(2075778)

	// DefaultTestnetDynamicArbiterHeight is the value SetEthConfig substitutes
	// for the testnet when the flag is not set explicitly
	// (pgp-chain cmd/utils/flags.go:1652-1654). It is kept as documentation and
	// for a future legacy testnet config; nothing in this tree uses it yet,
	// because the only legacy chain here is mainnet.
	DefaultTestnetDynamicArbiterHeight = uint64(1496588)

	// DefaultLegacyProducers is the PBFT producer set of pgp-chain mainnet,
	// copied verbatim and in order from pgp-chain/params/config.go:87-100
	// (Pbft.Producers). Twelve compressed P256 public keys, so a confirm needs
	// int(12*2/3) = 8 of them to pass.
	//
	// Not to be confused with spv.MainnetProducers, the twelve next-turn DPoS
	// arbiters, whose first key is 03244cbf…. The two lists are disjoint; using
	// the wrong one makes every nonce-0 block unverifiable.
	DefaultLegacyProducers = []string{
		"02a91b4ccc0204e2964719fe5035c06af1d5141968eb1e5117f74feee625eac4d5",
		"024d54af50167af60148ff4b0f05db6722791a1f83f7afa835b550697afa939e03",
		"02829bacc672b100921547c85ea9540cd1c1678f2225e6190adab9acdd0dad5809",
		"034aac70edd1721813c12264deb0acc89f316b30cf96d2c0a4c4acd481e458dba3",
		"0328b23ba4b9f76d5128e112fd330c26d335b5851caeb46cdba2802c1a6dbdfdb1",
		"03cbca407a573e6ca5111e596e797114631eee9acfb8fcb88675186af1f64fbc2f",
		"030dd6df8a118dd40d33dc58a97982a9a47b1b9336f7e20381518907e0817d8422",
		"02941df229c032d90153ef73b486fbdec83ae08835b5c40f42626bf1d1575fe8b1",
		"02e85e1bf846063773e9325b718acb8b117a00ddf09e4666dc15d7ca689292ee3a",
		"02c731d08da8500fe116f9b3e6d97e50dba4e04784433ea41e24a13d652768a8ae",
		"03cef969f3d4abdd1669f0ac7b8ff5985b21e5ed3aaaabaf552046f81de0e40358",
		"03f66f89112812ef12649e51192083a3787843d92efea0cccbc4c349e3fd6d6eb3",
	}

	// PGPGenesisHash is the hash of the pgp-chain mainnet genesis block.
	PGPGenesisHash = common.HexToHash("0xaab1ef4455d93b45f440a8aaed032f2c38da03a06a0843d6f9b059dbfdd2a5b5")

	// PGPChainConfig is the chain configuration used to sync and validate the
	// pgp-chain mainnet.
	//
	// Every fork that pgp-chain ever activated sits at block 1, 2 or 3, so the
	// EIP-2124 fork list gathered by core/forkid is [1 2 3], exactly the same as
	// pgp-chain's own client. ShanghaiTime is deliberately earlier than the
	// genesis timestamp: core/forkid drops time based forks at or before
	// genesis, and keeping it in the past preserves the fork checksum.
	//
	// Parlia is intentionally nil for now: the whole chain still runs on the
	// legacy rules and the height router that switches to Parlia is introduced
	// together with LegacyConfig.ForkHeight.
	PGPChainConfig = &ChainConfig{
		ChainID:             big.NewInt(860621),
		HomesteadBlock:      big.NewInt(1),
		DAOForkBlock:        nil,
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(2),
		EIP155Block:         big.NewInt(3),
		EIP158Block:         big.NewInt(3),
		ByzantiumBlock:      big.NewInt(3),
		ConstantinopleBlock: big.NewInt(3),
		PetersburgBlock:     big.NewInt(3),
		IstanbulBlock:       big.NewInt(3),
		MuirGlacierBlock:    nil,
		BerlinBlock:         big.NewInt(3),
		LondonBlock:         big.NewInt(3),
		ShanghaiTime:        newUint64(1761657650),
		Legacy: &LegacyConfig{
			ForkHeight:           math.MaxUint64,
			GasFeeRecipient:      DefaultGasFeeRecipient,
			GasFeeRecipientRatio: DefaultGasFeeRecipientRatio,
			PassBalance:          DefaultPassBalance,
			DynamicArbiterHeight: DefaultDynamicArbiterHeight,
			Producers:            DefaultLegacyProducers,
		},
	}
)

// IsPGPLegacy reports whether the given block belongs to the pgp-chain history
// and therefore has to be validated and executed with the legacy rules.
//
// A nil block number is treated as legacy, which is what genesis time call sites
// (genesis construction, fork id computation) need.
func (c *ChainConfig) IsPGPLegacy(num *big.Int) bool {
	if c == nil || c.Legacy == nil {
		return false
	}
	if num == nil {
		return true
	}
	return num.Uint64() < c.Legacy.ForkHeight
}

// IsPGPLegacyAt is IsPGPLegacy for call sites that work with (number, time).
// The time argument is accepted for symmetry with the other fork predicates and
// is currently unused, because pgp-chain never activated a rule by timestamp.
func (c *ChainConfig) IsPGPLegacyAt(num *big.Int, time uint64) bool {
	_ = time
	return c.IsPGPLegacy(num)
}

// GasFeeSplit returns the legacy fee sink and the percentage routed to it.
func (c *ChainConfig) GasFeeSplit() (common.Address, uint64) {
	if c == nil || c.Legacy == nil {
		return common.Address{}, 0
	}
	return c.Legacy.GasFeeRecipient, c.Legacy.GasFeeRecipientRatio
}

// PassBalance returns the temporary balance granted to recharge / refundWithdraw
// senders. Zero means the borrow is disabled.
func (c *ChainConfig) PassBalance() uint64 {
	if c == nil || c.Legacy == nil {
		return 0
	}
	return c.Legacy.PassBalance
}

// LegacyProducers returns the PBFT producer set used to verify the confirms of
// legacy blocks whose header.Nonce is zero.
//
// A nil Legacy block, or an empty list inside it, falls back to the mainnet set.
// The fallback is deliberate: an empty list is not "no producers", it is "this
// config predates the field", and treating it as empty would make every nonce-0
// block unverifiable instead of merely unconfigured.
func (c *ChainConfig) LegacyProducers() []string {
	if c == nil || c.Legacy == nil || len(c.Legacy.Producers) == 0 {
		return DefaultLegacyProducers
	}
	return c.Legacy.Producers
}

// DynamicArbiterHeight returns the ELA height from which the SPV listener
// maintains the next-turn DPOS producer set. It is the value the node hands to
// spv.NewService.
//
// A zero in the chain config means "not configured", not "zero". Two ways to get
// there: a datadir whose stored chain config was written by a build that predates
// the field (core.SetupGenesisBlockWithOverride returns the *stored* config, so
// the JSON on disk wins over the literal in this file), and a genesis built by
// hand without a Legacy block. Zero is not a usable value — it would turn the
// bookkeeping on at ELA height 0, which no real pgp node ever did — so it falls
// back to the mainnet flag default instead.
func (c *ChainConfig) DynamicArbiterHeight() uint64 {
	if c == nil || c.Legacy == nil {
		return 0
	}
	if c.Legacy.DynamicArbiterHeight == 0 {
		return DefaultDynamicArbiterHeight
	}
	return c.Legacy.DynamicArbiterHeight
}

// ELA Minter (0x…64) entry points that the legacy state transition treats
// specially.
//
// The selectors are the ABI method ids of the contract deployed in the pgp-chain
// genesis state, i.e. keccak256 of the canonical signature (verified against the
// ELAMinterABI shipped by pgp-chain in spv/elaminter.go):
//
//	Recharge(bytes32,bytes)
//	refundWithdraw(bytes32)
//
// Deriving them from the signature instead of hardcoding the four bytes keeps
// them auditable.
var (
	elaMinterRechargeSelector       = crypto.Keccak256([]byte("Recharge(bytes32,bytes)"))[:4]
	elaMinterRefundWithdrawSelector = crypto.Keccak256([]byte("refundWithdraw(bytes32)"))[:4]
)

// IsPGPRechargeTx reports whether the transaction is an ELA Minter Recharge call.
//
// pgp-chain gates the pass balance borrow, the full gas refund and the
// "already completed" check on this (pgp-chain/spv/elaminter.go:44-56). The test
// is a pure local ABI match and needs no SPV data.
func IsPGPRechargeTx(input []byte, to *common.Address) bool {
	return isELAMinterCall(input, to, elaMinterRechargeSelector)
}

// IsPGPRefundWithdrawTx reports whether the transaction is an ELA Minter
// refundWithdraw call (pgp-chain/spv/elaminter.go:58-70).
func IsPGPRefundWithdrawTx(input []byte, to *common.Address) bool {
	return isELAMinterCall(input, to, elaMinterRefundWithdrawSelector)
}

func isELAMinterCall(input []byte, to *common.Address, selector []byte) bool {
	if to == nil || *to != ELAMINTER {
		return false
	}
	return bytes.HasPrefix(input, selector)
}

// completedMappingSlot is the storage slot of ELAMinter's
// `mapping(bytes32 => bool) completed`.
//
// It was read out of the deployed bytecode in params/pgp_contracts.go
// (ELAMINTER_CODE), not assumed. Three independent sites key the mapping with
// PUSH1 0x01, and each of them lays the argument out at memory 0x00 and the slot
// at memory 0x20 before hashing, i.e. keccak256(arg . slot):
//
//	getter  completed(bytes32)    0x0182  PUSH1 1 PUSH1 0x20 MSTORE, SLOAD
//	Recharge(bytes32,bytes)       0x04a5  PUSH1 1 PUSH1 0x20 MSTORE, SSTORE
//	refundWithdraw(bytes32)       0x0905  PUSH1 1 PUSH1 0x20 MSTORE, SSTORE
//
// Recharge and refundWithdraw therefore share one mapping: settling a withdraw
// also marks that hash's recharge as done. That shared mapping is exactly why
// the state transition has to discriminate by method instead of by storage.
const completedMappingSlot = 1

// CompletedSlot returns the storage key of ELAMinter's completed[elaHash], i.e.
// keccak256(abi.encode(elaHash, completedMappingSlot)) - the same derivation
// solidity performs on a mapping read.
//
// Reading that slot from the executing block's own StateDB replaces pgp-chain's
// IsCompleted, which ABI-packed the call and issued it with nil call options
// (pgp-chain/spv/elaminter.go:113-134); nil resolves to the node's latest state,
// so the answer depended on when the node ran and a replay could disagree with
// the block that was produced. The contract's own storage is the answer, and it
// is available at the exact point in history being executed.
func CompletedSlot(elaHash common.Hash) common.Hash {
	var buf [64]byte
	copy(buf[:32], elaHash[:])
	buf[32+31] = byte(completedMappingSlot)
	return crypto.Keccak256Hash(buf[:])
}

// CompletedSetValue is what ELAMinter stores in completed[hash] once a Recharge
// or refundWithdraw has settled. The mapping is a bool, so the slot holds
// 0x00..01; an untouched slot - the state of a hash the contract never saw -
// reads as zero and therefore as "not completed".
//
// Not a constant because common.Hash is an array type. Treat it as read-only.
var CompletedSetValue = common.Hash{31: 1}

// LegacyRechargeElaHash extracts the ELA main chain tx hash from a
// Recharge(bytes32,bytes) call, i.e. the first ABI argument.
//
// Only Recharge carries the hash in a position pgp-chain looked at:
// IsCompletedByTxInput matches the "Recharge" method id and returns "not
// completed" for everything else, refundWithdraw included
// (pgp-chain/spv/elaminter.go:86-96). Returning ok=false here reproduces that.
//
// The decode is a plain slice rather than accounts/abi on purpose: bytes32 is a
// static type, so the first argument always sits at input[4:36]. Keeping params
// free of the ABI machinery matters because every package imports params.
func LegacyRechargeElaHash(input []byte) (string, bool) {
	const selectorLen = 4
	if len(input) < selectorLen+32 {
		return "", false
	}
	if !bytes.HasPrefix(input, elaMinterRechargeSelector) {
		return "", false
	}
	return "0x" + hex.EncodeToString(input[selectorLen:selectorLen+32]), true
}
