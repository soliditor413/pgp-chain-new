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
	"fmt"
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

	// SystemContractDeployHeight is the block at which the pgp-chain system
	// contracts are installed and initialised by the protocol. It must satisfy
	// 1 <= SystemContractDeployHeight <= ForkHeight-2: the fork block reads the
	// validator set from 0x1000, and one spare block is kept so a failed
	// injection can be retried instead of halting the chain at the fork.
	// Zero disables the injection (non-pgp chains).
	SystemContractDeployHeight uint64 `json:"systemContractDeployHeight,omitempty"`

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

	// AllowForeignGenesis lets a chain keep pgp mainnet's chain id (860621) while
	// running on a genesis that is not PGPGenesisHash.
	//
	// It exists for devnets only. The system contracts carry
	// `bscChainID = 860621` as a compile-time constant and SlashIndicator hands
	// it to the 0x68 precompile as the domain of the double-sign evidence, so a
	// devnet on a different chain id can never rehearse slashing: the recovered
	// signer is always wrong and the evidence check reverts with "validator not
	// migrated" (F-01). Reusing 860621 is the only way to exercise that path
	// before mainnet, and it conflicts with the safety net in
	// core.checkPGPDatabaseMatch, which otherwise refuses to start a 860621
	// configuration on a database whose genesis is not the pgp one.
	//
	// Leave it false everywhere else: the net it disables is what stops a
	// mainnet configuration from silently running on top of a BSC datadir.
	AllowForeignGenesis bool `json:"allowForeignGenesis,omitempty"`

	// VerifySeal enables PBFT confirm signature verification while replaying the
	// legacy history. It is ON for mainnet: verifying the blocks is the point,
	// and an implementation that is only reachable by an operator flag verifies
	// nothing in practice. Turning it off is a debugging measure, never a
	// production setting, because it leaves every legacy header unauthenticated.
	//
	// On mainnet it cannot be turned off through configuration at all: the
	// value that reaches the engine is PGPChainConfig itself (see below), so
	// switching it off means editing this file and rebuilding. A JSON on disk
	// that says false is overwritten, not honoured.
	//
	// The switch also applies to datadirs created before the field existed:
	// SetupGenesisBlockWithOverride resolves the genesis through
	// chainConfigOrDefault, which returns PGPChainConfig itself when the hash
	// matches the built-in pgp genesis, so a stale JSON on disk cannot pin this
	// back to false.
	VerifySeal bool `json:"verifySeal,omitempty"`

	// Producers is the PBFT producer set that signed every legacy block whose
	// header.Nonce is zero, as compressed (33 byte) P256 public keys in hex.
	//
	// It is the pgp-chain Pbft.Producers list (pgp-chain/params/config.go:87-100).
	// Blocks 1..16404 of mainnet carry nonce 0, i.e. they were produced before
	// the ELA main chain had a height to anchor to, so their confirm signatures
	// have to be checked against this fixed list instead of the SPV arbiters.
	//
	// This is NOT the next-turn DPoS arbiter list. pgp-chain keeps one in
	// spv/nextturn_dposinfo.go:17 (spv.MainnetProducers, first key 03244cbf…);
	// it is not ported here, because nothing in this tree reads it, and it is
	// disjoint from the twelve keys below — all twelve recorded mainnet
	// confirms in consensus/pgp/testdata/seal_vectors.json, nonce-0 and
	// non-zero alike, are signed by keys from this list and none from that one.
	// Substituting it would make every nonce-0 block fail verification.
	//
	// Non-zero-nonce blocks are not checked against this list at runtime; they
	// use spv.GetProducers(elaHeight), the ELA arbiter set the SPV database
	// reports for the height in header.Nonce.
	//
	// The name deliberately does not end in "Block" or "Time"; see the warning
	// on LegacyConfig.
	//
	// An empty list is a hard failure, not "no producers configured, so skip the
	// check": every nonce-0 block becomes unverifiable and the sync stops. That
	// cannot happen on pgp mainnet, because SetupGenesisBlockWithOverride
	// resolves the genesis through chainConfigOrDefault, which returns
	// PGPChainConfig itself once the genesis hash matches the built-in one, and
	// then rewrites a differing stored config. It can happen on a custom genesis
	// that reuses the legacy rules; such a chain must list its own producers
	// rather than silently inheriting mainnet's, which is why there is no
	// fallback to DefaultLegacyProducers anywhere.
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

	// ParliaGenesisValidators is the initial Parlia validator set, in the exact
	// order it must be encoded into BSCValidatorSet.init(bytes,uint256).
	//
	// These four fields exist because the initial validator set is NOT a
	// compile-time constant any more. It used to live in
	// BSCValidatorSet.INIT_VALIDATORSET_BYTES / INIT_NUM_OF_CABINETS, which
	// forced a separate bytecode per validator count -- so a 6-validator devnet
	// and a 21-validator mainnet could never share a binary, and picking the
	// wrong build silently produced a different state root at the fork block.
	// The contract now takes both as init() arguments and the consensus engine
	// packs them from here, so the single deployed bytecode serves both.
	//
	// CONSENSUS-CRITICAL ORDERING: the RLP payload is built in slice order, so
	// swapping two entries changes the payload, changes the state root written
	// at ForkHeight, and splits the chain. Do not treat this list as a set.
	//
	// An empty list on a Parlia chain is a hard startup failure, not "no
	// validators configured yet": BSCValidatorSet.init requires a non-empty set,
	// and a zero numOfCabinets would make getMiningValidators underflow.
	//
	// The name deliberately does not end in "Block" or "Time"; see the warning
	// on LegacyConfig. core/forkid.gatherForks reflects over those suffixes and
	// would otherwise treat this list as a fork activation point.
	ParliaGenesisValidators []string `json:"parliaGenesisValidators,omitempty"`

	// ParliaGenesisFeeAddrs is the per-validator fee address, parallel to
	// ParliaGenesisValidators. Optional: when empty every validator uses its own
	// consensus address.
	//
	// Note that these addresses are inert on this chain. Block rewards reach a
	// validator through IStakeHub.distributeReward(consensusAddress) when the set
	// is rotated (BSCValidatorSet.sol:213-219), not through Validator.feeAddress,
	// so H2 and later never pay to this field. It is carried only because the
	// 4-tuple RLP layout has a slot for it.
	ParliaGenesisFeeAddrs []string `json:"parliaGenesisFeeAddrs,omitempty"`

	// ParliaGenesisVotingPowers is the voting power per validator, parallel to
	// ParliaGenesisValidators. Optional: when empty every validator gets
	// DefaultParliaVotingPower.
	ParliaGenesisVotingPowers []uint64 `json:"parliaGenesisVotingPowers,omitempty"`

	// ParliaGenesisCabinets is the number of cabinet seats seeded into
	// BSCValidatorSet.numOfCabinets at init. Zero means "one per validator",
	// which is what both the devnet and mainnet use.
	//
	// It is seeded as a real numOfCabinets value rather than kept as a fallback
	// constant, because numOfCabinets is the single source of truth for
	// getMiningValidators / getWorkingValidatorCount / the maxNumOfMaintaining
	// bound. Governance can change it later through updateParam("numOfCabinets"),
	// but not before H2: updateParam validates against maxElectedValidators,
	// which only exists once StakeHub has been initialized.
	ParliaGenesisCabinets uint64 `json:"parliaGenesisCabinets,omitempty"`

	// ParliaGenesisVoteAddrs is the per-validator BLS vote address (48 bytes,
	// hex), parallel to ParliaGenesisValidators. Optional.
	//
	// This is what makes fast finality possible at all: vote attestations are
	// BLS aggregate signatures, so the validator set has to know each
	// validator's BLS public key. On a chain that has gone through StakeHub the
	// keys arrive by validator registration and this field stays empty. A
	// devnet has no election, so without this field its validator set carries
	// zero vote addresses and no attestation can ever verify - which is exactly
	// what the first Luban run measured (audit 04-verification.md §1.2).
	//
	// Empty is byte-neutral, and that is a hard requirement rather than a
	// coincidence: the RLP tuple gains its fifth element only when this is set,
	// so a chain that leaves it empty - mainnet - produces the same payload, and
	// therefore the same state root at ForkHeight, as before this field existed.
	//
	// The length is 48 because that is what BLS12-381 public keys are; the value
	// is the compressed G1 point, i.e. what `geth bls account list` prints.
	// Whether a populated value is accepted is decided by the contract
	// (BSCValidatorSet.decodeValidator reads it as an optional fifth field), not
	// here, but a length other than 48 is rejected at startup because the
	// contract would store it and every attestation would then fail to verify
	// for a reason nothing on chain explains.
	ParliaGenesisVoteAddrs []string `json:"parliaGenesisVoteAddrs,omitempty"`
}

// DefaultParliaVotingPower is the voting power given to a genesis validator when
// ParliaGenesisVotingPowers does not spell it out. 100 matches the BSC devnet
// configuration; the genesis set is static until Feynman, so the absolute value
// only starts to matter when StakeHub takes over the election.
const DefaultParliaVotingPower uint64 = 100

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
	// pgp-chain also carries a next-turn DPoS arbiter list
	// (spv/nextturn_dposinfo.go:17, spv.MainnetProducers, first key
	// 03244cbf…). It is disjoint from the keys below and is deliberately not
	// ported: no mainnet confirm recorded in
	// consensus/pgp/testdata/seal_vectors.json is signed by any key from it,
	// including the non-zero-nonce ones. Do not substitute it for this list —
	// every nonce-0 block would become unverifiable.
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

		// BSC block-number forks. All of them are pinned at the "not scheduled"
		// sentinel instead of being left nil, and the difference matters:
		// CheckConfigForkOrder reads a nil block fork as "not configured" and
		// rejects a chain that goes on to enable any later timestamp fork, while
		// a value >= MaxInt64 is read as "explicitly not scheduled" and skipped
		// (params/config.go:1646-1661).
		//
		// The real heights are not known yet: they are derived from
		// Legacy.ForkHeight, which mainnet has not pinned. When it is pinned,
		// THESE MUST BE SET BACK TO nil. ApplyPGPForkHeight only fills fields
		// that are still nil (params/pgp_forks.go:62-66), so a leftover MaxInt64
		// keeps Luban and Hertz off forever - no fast finality, no BSC
		// precompiles - and the configuration would look perfectly fine.
		RamanujanBlock:  big.NewInt(math.MaxInt64),
		NielsBlock:      big.NewInt(math.MaxInt64),
		MirrorSyncBlock: big.NewInt(math.MaxInt64),
		BrunoBlock:      big.NewInt(math.MaxInt64),
		EulerBlock:      big.NewInt(math.MaxInt64),
		GibbsBlock:      big.NewInt(math.MaxInt64),
		PlanckBlock:     big.NewInt(math.MaxInt64),
		MoranBlock:      big.NewInt(math.MaxInt64), // not in forkOrder, but it gates PrecompiledContractsMoran
		LubanBlock:      big.NewInt(math.MaxInt64),
		PlatoBlock:      big.NewInt(math.MaxInt64),
		HertzBlock:      big.NewInt(math.MaxInt64),
		HertzfixBlock:   big.NewInt(math.MaxInt64),
		NanoBlock:       big.NewInt(math.MaxInt64), // block blacklist account in blacklist.go and PrecompiledAddressesNano

		// BSC timestamp forks, in two groups.
		//
		// Zero means "active since genesis". These are execution-layer rules whose
		// legacy-segment carve-outs (core/vm/contracts.go's `case rules.IsPGPLegacy`,
		// the legacy jump table, the legacy fee path) already keep historical
		// blocks byte-identical, so enabling them cannot rewrite the past.
		//
		// The zero group is a prefix of the fork order and cannot be extended
		// past Cancun: the order check compares timestamps, so a fork pinned at 0
		// after one pinned at MaxInt64 reads as a rewind.
		KeplerTime:     newUint64(math.MaxInt64),
		FeynmanTime:    newUint64(0),
		FeynmanFixTime: newUint64(0),

		// MaxInt64 means "configured, never reached" - the field exists so the
		// fork order check is satisfied and so the intent is visible, but no block
		// ever activates it. These are the ones that must NOT run during the
		// legacy era:
		//
		//   Cancun  writes BlobGasUsed/ExcessBlobGas/WithdrawalsHash into every
		//           header miner/worker.go builds, with no IsPGPLegacy guard, so
		//           enabling it early changes the header of already-mined blocks.
		//   Haber   is ordered right after Cancun and shares its activation time on
		//           BSC mainnet, so it moves with it. Its own effect (the 0x100
		//           secp256r1 precompile) is execution-layer and legacy-carved-out,
		//           so deferring it costs nothing before the fork.
		//   Bohr    requires ParentBeaconRoot on every Parlia header, but only the
		//           Cancun branch fills it - the two must be switched on together,
		//           at or after the fork.
		//   Prague  adds RequestsHash to the header.
		//   Lorentz/Maxwell/Fermi change snap.BlockInterval (1500/750/450ms),
		//           the epoch length, and start writing millisecond timestamps.
		//
		// When the fork height is pinned, move the ones being enabled to T_fork,
		// keeping CancunTime == HaberTime == BohrTime.
		CancunTime:   newUint64(math.MaxInt64),
		HaberTime:    newUint64(math.MaxInt64),
		HaberFixTime: newUint64(math.MaxInt64),
		BohrTime:     newUint64(math.MaxInt64),
		PascalTime:   newUint64(math.MaxInt64), // only reflashes system contracts; must exist for Prague
		PragueTime:   newUint64(math.MaxInt64),
		LorentzTime:  newUint64(math.MaxInt64),
		MaxwellTime:  newUint64(math.MaxInt64),
		FermiTime:    newUint64(math.MaxInt64),
		OsakaTime:    newUint64(math.MaxInt64),
		MendelTime:   newUint64(math.MaxInt64),
		PasteurTime:  newUint64(math.MaxInt64),
		JennerTime:   newUint64(math.MaxInt64),
		// BPO1-5, Amsterdam and UBT are deliberately absent: BSC's own mainnet
		// never activated them (BPO1/BPO2 carry "will be skipped in BSC").
		Parlia: &ParliaConfig{},

		// Required the moment any of CancunTime/PragueTime/OsakaTime is non-nil:
		// CheckConfigForkOrder refuses a configured blob fork without an entry
		// (params/config.go:1711-1716). BSC's blob parameters are static, so the
		// three entries carry the same numbers as its mainnet config.
		BlobScheduleConfig: &BlobScheduleConfig{
			Cancun: DefaultCancunBlobConfig,
			Prague: DefaultPragueBlobConfigBSC,
			Osaka:  DefaultOsakaBlobConfigBSC,
		},

		Legacy: &LegacyConfig{
			ForkHeight: math.MaxUint64,
			// Zero means "do not inject". It is the only legal value while
			// ForkHeight is the never-fork sentinel: checkDeployHeight rejects any
			// non-zero deploy height on a chain without a fixed fork height
			// (params/pgp_config.go:486-488). Once ForkHeight is pinned it has to
			// become a concrete 2 <= D <= ForkHeight-2.
			SystemContractDeployHeight: 0,
			GasFeeRecipient:            DefaultGasFeeRecipient,
			GasFeeRecipientRatio:       DefaultGasFeeRecipientRatio,
			PassBalance:                DefaultPassBalance,
			DynamicArbiterHeight:       DefaultDynamicArbiterHeight,
			Producers:                  DefaultLegacyProducers,
			VerifySeal:                 true,
			ParliaGenesisValidators:    []string{},
			ParliaGenesisVotingPowers:  []uint64{},
			ParliaGenesisVoteAddrs:     []string{},
			ParliaGenesisCabinets:      21,
		},
	}

	// DefaultLegacyTestnetProducers are the ELA test-network CR arbiters, in the
	// order pgp-chain's TestnetChainConfig lists them
	// (pgp-chain/params/config.go:162-176).
	//
	// They are not interchangeable with the mainnet set. Any note that
	// verifies the PBFT confirm against these keys (VerifySeal) would reject every
	// legacy block if the wrong network's arbiters were listed, and the failure
	// looks like a consensus bug rather than a config error.
	DefaultLegacyTestnetProducers = []string{
		"03e435ccd6073813917c2d841a0815d21301ec3286bc1412bb5b099178c68a10b6",
		"038a1829b4b2bee784a99bebabbfecfec53f33dadeeeff21b460f8b4fc7c2ca771",
		"02435df9a4728e6250283cfa8215f16b48948d71936c4600b3a5b1c6fde70503ae",
		"027d44ee7e7a6c6ff13a130d15b18c75a3b47494c3e54fcffe5f4b10e225351e09",
		"02ad972fbfce4aaa797425138e4f3b22bcfa765ffad88b8a5af0ab515161c0a365",
		"0373eeae2bac0f5f14373ca603fe2c9caa9c7a79c7793246cec415d005e2fe53c0",
		"03503011cc4e44b94f73ed2c76c73182a75b4863f23d1e7083025eead945a8e764",
		"0270b6880e7fab8d02bea7d22639d7b5e07279dd6477baa713dacf99bb1d65de69",
		"030eed9f9c1d70307beba52ddb72a24a02582c0ee626ec93ee1dcef2eb308852dd",
		"026bba43feb19ce5859ffcf0ce9dd8b9d625130b686221da8b445fa9b8f978d7b9",
		"02bf9e37b3db0cbe86acf76a76578c6b17b4146df101ec934a00045f7d201f06dd",
		"03111f1247c66755d369a8c8b3a736dfd5cf464ca6735b659533cbe1268cd102a9",
	}

	// PGPTestnetChainConfig is what --chapel selects: the pgp test network,
	// following the ELA test chain.
	//
	// It mirrors PGPChainConfig on purpose. A test network that differs in its
	// rules cannot be used to predict the mainnet, so every fork height and the
	// whole Legacy/Parlia wiring are copied rather than re-derived. Three things
	// do differ, and each has a reason:
	//
	//   - ChainID is 12345, matching pgp-chain's TestnetChainConfig. This is only
	//     safe because ForkHeight is MaxUint64: with no fork scheduled there is no
	//     D block, so the system-contract gate that asserts
	//     bscChainID() == ChainID (the F-01 fix; the image hardcodes 860621) never
	//     runs. A testnet that schedules a Parlia fork must either use 860621 or
	//     ship a re-imaged contract set - otherwise D is rejected at run time.
	//   - The producer set is the ELA test network's arbiters. VerifySeal checks
	//     confirms against it, so the mainnet list would reject every block.
	//   - DynamicArbiterHeight is left at zero (unset). The accessor still falls
	//     back to the mainnet constant, which is why startSpv overrides it for
	//     non-mainnet selections; see cmd/geth/spv_start.go.
	PGPTestnetChainConfig = &ChainConfig{
		ChainID:             big.NewInt(12345),
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
		LubanBlock:          big.NewInt(math.MaxInt64),
		Parlia:              &ParliaConfig{},
		Legacy: &LegacyConfig{
			ForkHeight:                math.MaxUint64,
			GasFeeRecipient:           DefaultGasFeeRecipient,
			GasFeeRecipientRatio:      DefaultGasFeeRecipientRatio,
			PassBalance:               DefaultPassBalance,
			DynamicArbiterHeight:      0,
			Producers:                 DefaultLegacyTestnetProducers,
			VerifySeal:                true,
			ParliaGenesisValidators:   []string{},
			ParliaGenesisVotingPowers: []uint64{},
			ParliaGenesisCabinets:     21,
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

// checkDeployHeight validates Legacy.SystemContractDeployHeight.
//
// It runs from CheckConfigForkOrder before the non-BSC early return, because a
// legacy chain that has not configured Parlia yet is exactly the chain most
// likely to get this wrong, and IsNotInBSC would otherwise skip the check.
//
// The rules, and why each exists:
//
//   - zero means "no injection", which is the only legal value on a chain with
//     no fork at all (ForkHeight is a placeholder there);
//   - one spare block below the fork, because the fork block reads the
//     validator set out of 0x1000: if the injection fails at D the operator can
//     still resync and retry at D+1 without the chain being stuck at the fork;
//   - the deploy height has to be in the legacy era, because the very next
//     Parlia block assumes the code exists, and the legacy rules are what
//     produced every block up to ForkHeight-1.
//
// A negative value cannot be expressed: the field is uint64, so "D < 1" and
// "D == 0" are the same rule and the zero case above already covers it.
func (c *ChainConfig) checkDeployHeight() error {
	if c == nil || c.Legacy == nil {
		return nil
	}
	d := c.Legacy.SystemContractDeployHeight
	f := c.Legacy.ForkHeight
	if d == 0 {
		// There is exactly one initialisation path: the deploy-height injection.
		// A chain that forks without one would run Parlia against system
		// contracts that were never initialised -- 0x1000 answers an empty
		// validator set and the first block after the fork fails with nothing
		// pointing at the cause. The block-1 initialisation path that used to
		// cover this is gone (F-18); MaxUint64 means "no fork", which is what
		// mainnet runs today.
		if f != 0 && f != math.MaxUint64 {
			return fmt.Errorf("legacy.forkHeight %d requires legacy.systemContractDeployHeight: the "+
				"block-1 initialisation path was removed, so a forked chain that does not inject the "+
				"system contracts has no way to initialise them", f)
		}
		return nil
	}
	if f == 0 || f == math.MaxUint64 {
		return fmt.Errorf("legacy.systemContractDeployHeight = %d requires a fixed legacy.forkHeight, got %d", d, f)
	}
	// Two is the hard floor, not one: without EIP-150 the 63/64 gas cap does not
	// apply, the initialisers forward their whole remaining budget into nested
	// calls and run out of gas. PGPChainConfig schedules EIP-150 at block 2, so a
	// deploy height of 1 would fail on mainnet with a bare "out of gas".
	if d < 2 {
		return fmt.Errorf("legacy.systemContractDeployHeight %d is before EIP-150 (block 2): the initialisers "+
			"forward all remaining gas and would run out", d)
	}
	if f < 3 {
		return fmt.Errorf("legacy.forkHeight %d leaves no room for a system contract deploy height: it must be at "+
			"least 3 so that 1 <= deployHeight <= forkHeight-2", f)
	}
	if d > f-2 {
		return fmt.Errorf("legacy.systemContractDeployHeight %d must be at most forkHeight-2 (%d, forkHeight %d): "+
			"the fork block reads the validator set from 0x1000, and the spare block allows a retry", d, f-2, f)
	}
	if !c.IsPGPLegacy(new(big.Int).SetUint64(d)) {
		return fmt.Errorf("legacy.systemContractDeployHeight %d is not in the legacy era (forkHeight %d): the "+
			"contracts have to be installed before the fork", d, f)
	}
	return nil
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

// DynamicArbiterHeight returns the ELA height from which the SPV listener
// maintains the next-turn DPOS producer set. It is the value the node hands to
// spv.NewService.
//
// A zero inside a Legacy block means "not configured", not "zero". Zero is not a
// usable value — it would turn the bookkeeping on at ELA height 0, which no real
// pgp node ever did — so it falls back to the mainnet flag default instead.
//
// That fallback is unreachable on pgp mainnet, and the reason is worth spelling
// out because the opposite is easy to assume: SetupGenesisBlockWithOverride
// resolves the config through chainConfigOrDefault (core/genesis.go:443), whose
// first question is params.GetBuiltInChainConfig(ghash)
// (params/config.go:603-616). For the pgp genesis hash that returns
// PGPChainConfig itself, so the JSON on disk is never consulted and the literal
// in this file always wins; a differing stored copy is then rewritten
// (core/genesis.go:469-471), and even if that write were skipped, newCfg — the
// config the node runs with — is already PGPChainConfig. A datadir created
// before this field existed therefore picks up the value on its own, with no
// migration step.
//
// What can actually reach zero is a config that is not one of the built-in ones,
// e.g. a hand-written custom genesis carrying a Legacy block without the field.
// A nil Legacy block is a separate case: it returns 0, not the default, because
// a chain that does not run the legacy rules has no arbiter height to be wrong
// about.
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
