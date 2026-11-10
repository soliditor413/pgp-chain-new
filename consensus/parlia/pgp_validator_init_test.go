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
	"bytes"
	"encoding/hex"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// devnetValidators is the six-address devnet set, in config order.
//
// The order is load bearing: the RLP payload is built in slice order, so this
// list fixes the bytes every test below compares against.
var devnetValidators = []string{
	"0x53781e106a2e3378083bdcede1874e5c2a7225f8",
	"0xd9758863f280c25b0d1f2f81705e3725ccd5ac49",
	"0x9b170f914ef3541cd29ee672a11c56d4b96b4b6f",
	"0xfd7f1f6e2c5157a33dda2e91f58f32862f864d70",
	"0x8aef2209eab2f60bc13da5e669ad832709f5e71d",
	"0x05f9b7a73f6ba0298c5c1943204a6a54839166b2",
}

// Reference payload, produced by
// bsc-genesis-contract/scripts/gen-pgp-genesis-config.py for the six devnet
// validators with votingPower 100 and no fee-address overrides.
//
// This is the G5 cross-implementation check: three independent encoders (this
// one, the Python script, and scripts/validators.js) must agree byte for byte.
// Agreement is the only thing that makes an encoder reviewable before it has
// ever run on chain, because a wrong-but-well-formed payload decodes fine and
// only shows up as a state root nobody else reproduces.
const devnetPayloadHex = "f9019080f9018cf8409453781e106a2e3378083bdcede1874e5c2a7225f89453781e106a2e3378083bdcede1874e5c2a7225f89453781e106a2e3378083bdcede1874e5c2a7225f864" +
	"f84094d9758863f280c25b0d1f2f81705e3725ccd5ac4994d9758863f280c25b0d1f2f81705e3725ccd5ac4994d9758863f280c25b0d1f2f81705e3725ccd5ac4964" +
	"f840949b170f914ef3541cd29ee672a11c56d4b96b4b6f949b170f914ef3541cd29ee672a11c56d4b96b4b6f949b170f914ef3541cd29ee672a11c56d4b96b4b6f64" +
	"f84094fd7f1f6e2c5157a33dda2e91f58f32862f864d7094fd7f1f6e2c5157a33dda2e91f58f32862f864d7094fd7f1f6e2c5157a33dda2e91f58f32862f864d7064" +
	"f840948aef2209eab2f60bc13da5e669ad832709f5e71d948aef2209eab2f60bc13da5e669ad832709f5e71d948aef2209eab2f60bc13da5e669ad832709f5e71d64" +
	"f8409405f9b7a73f6ba0298c5c1943204a6a54839166b29405f9b7a73f6ba0298c5c1943204a6a54839166b29405f9b7a73f6ba0298c5c1943204a6a54839166b264"

func devnetLegacyConfig() *params.LegacyConfig {
	return &params.LegacyConfig{
		ForkHeight:                1100,
		ParliaGenesisValidators:   append([]string(nil), devnetValidators...),
		ParliaGenesisVotingPowers: []uint64{100, 100, 100, 100, 100, 100},
		ParliaGenesisCabinets:     6,
	}
}

// TestPGPValidatorInitArgsMatchesReferencePayload is the primary acceptance
// check of C1-c: the Go encoder must reproduce the Python payload exactly.
func TestPGPValidatorInitArgsMatchesReferencePayload(t *testing.T) {
	payload, cabinets, err := pgpValidatorInitArgs(devnetLegacyConfig())
	if err != nil {
		t.Fatalf("pgpValidatorInitArgs: %v", err)
	}
	if cabinets != 6 {
		t.Fatalf("cabinets = %d, want 6", cabinets)
	}
	want, err := hex.DecodeString(devnetPayloadHex)
	if err != nil {
		t.Fatalf("reference hex is malformed: %v", err)
	}
	if len(payload) != 403 {
		t.Errorf("payload length = %d, want 403", len(payload))
	}
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload mismatch\n got: 0x%x\nwant: 0x%x", payload, want)
	}
	t.Logf("payload keccak256 = 0x%x", crypto.Keccak256(payload))
}

// TestPGPValidatorInitArgsPackageTypeIs0x80 pins the RLP integer encoding of
// packageType.
//
// 0 must encode as the empty string (0x80), not as a zero byte (0x00). Both
// decode to 0 in the contract, so this can only be caught by inspecting the
// bytes - which is exactly what broke the JS encoder earlier.
func TestPGPValidatorInitArgsPackageTypeIs0x80(t *testing.T) {
	payload, _, err := pgpValidatorInitArgs(devnetLegacyConfig())
	if err != nil {
		t.Fatalf("pgpValidatorInitArgs: %v", err)
	}
	want, _ := hex.DecodeString(devnetPayloadHex)
	if len(payload) < 4 {
		t.Fatalf("payload too short: %d bytes", len(payload))
	}
	// f9 0190 = list, long form, 400 bytes of body; then the package type.
	if !bytes.Equal(payload[:3], want[:3]) {
		t.Fatalf("list header = 0x%x, want 0x%x", payload[:3], want[:3])
	}
	if payload[3] != 0x80 {
		t.Fatalf("packageType encoded as 0x%02x, want 0x80 (RLP empty string)", payload[3])
	}
}

// TestPGPValidatorInitArgsFeeAddressOrdering checks that the tuple slot the
// contract calls feeAddress and the one it calls BBCFeeAddress are filled in the
// order the contract reads them.
//
// Getting this wrong swaps two 20-byte addresses: the payload stays
// well-formed, decodes cleanly, and produces a different state root.
func TestPGPValidatorInitArgsFeeAddressOrdering(t *testing.T) {
	cfg := devnetLegacyConfig()
	cfg.ParliaGenesisFeeAddrs = []string{
		"0x00000000000000000000000000000000000000f1",
		"0x00000000000000000000000000000000000000f2",
		"0x00000000000000000000000000000000000000f3",
		"0x00000000000000000000000000000000000000f4",
		"0x00000000000000000000000000000000000000f5",
		"0x00000000000000000000000000000000000000f6",
	}
	payload, _, err := pgpValidatorInitArgs(cfg)
	if err != nil {
		t.Fatalf("pgpValidatorInitArgs: %v", err)
	}

	// Each 20-byte address is RLP-encoded as a 0x94-prefixed string, so the
	// tuple on the wire reads:
	//	94 <consensus> 94 <bscFee> 94 <fee> 64
	// bscFee defaults to the consensus address and fee comes from the config,
	// which makes the two distinguishable.
	consensus := common.HexToAddress(devnetValidators[0]).Bytes()
	fee := common.HexToAddress(cfg.ParliaGenesisFeeAddrs[0]).Bytes()

	const rlp20 = byte(0x80 + common.AddressLength) // 0x94
	withPrefix := func(addrs ...[]byte) []byte {
		out := make([]byte, 0, len(addrs)*(1+common.AddressLength))
		for _, a := range addrs {
			out = append(out, rlp20)
			out = append(out, a...)
		}
		return out
	}

	wantSeq := withPrefix(consensus, consensus, fee)
	if !bytes.Contains(payload, wantSeq) {
		t.Fatalf("expected [consensus|consensus|fee] in the payload; tuple order is wrong\nwant seq: %x", wantSeq)
	}
	// And the reverse order must not appear.
	badSeq := withPrefix(consensus, fee, consensus)
	if bytes.Contains(payload, badSeq) {
		t.Fatalf("payload contains [consensus|fee|consensus]; bscFee and fee are swapped")
	}
}

// TestPGPValidatorInitArgsDefaults checks the two optional fields.
func TestPGPValidatorInitArgsDefaults(t *testing.T) {
	cfg := &params.LegacyConfig{
		ForkHeight:              1100,
		ParliaGenesisValidators: append([]string(nil), devnetValidators...),
	}
	payload, cabinets, err := pgpValidatorInitArgs(cfg)
	if err != nil {
		t.Fatalf("pgpValidatorInitArgs: %v", err)
	}
	// Empty powers -> DefaultParliaVotingPower (100), which is what the
	// reference payload was built with, so it must match exactly.
	want, _ := hex.DecodeString(devnetPayloadHex)
	if !bytes.Equal(payload, want) {
		t.Fatalf("defaulting powers to %d should reproduce the reference payload", params.DefaultParliaVotingPower)
	}
	if cabinets != 6 {
		t.Fatalf("cabinets = %d, want 6 (zero means one seat per validator)", cabinets)
	}
}

// TestPGPValidatorInitArgsRejections covers every configuration mistake that
// must stop the node at startup rather than produce a well-formed payload that
// forks the chain.
func TestPGPValidatorInitArgsRejections(t *testing.T) {
	with := func(f func(*params.LegacyConfig)) *params.LegacyConfig {
		cfg := devnetLegacyConfig()
		f(cfg)
		return cfg
	}
	cases := []struct {
		name string
		cfg  *params.LegacyConfig
		want string
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: "no legacy config",
		},
		{
			name: "empty validator list",
			cfg:  with(func(c *params.LegacyConfig) { c.ParliaGenesisValidators = nil }),
			want: "parliaGenesisValidators is empty",
		},
		{
			name: "zero address",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisValidators[2] = "0x0000000000000000000000000000000000000000"
			}),
			want: "zero address",
		},
		{
			name: "duplicate address",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisValidators[3] = c.ParliaGenesisValidators[1]
			}),
			want: "must be unique",
		},
		{
			name: "short address",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisValidators[0] = "0x53781e106a2e3378083bdcede1874e5c2a7225f"
			}),
			want: "expected 40 hex characters",
		},
		{
			name: "non-hex address",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisValidators[0] = "0x53781e106a2e3378083bdcede1874e5c2a7225fz"
			}),
			want: "invalid hex character",
		},
		{
			name: "fee addresses of the wrong length",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisFeeAddrs = []string{"0x00000000000000000000000000000000000000f1"}
			}),
			want: "parliaGenesisFeeAddrs has 1 entries",
		},
		{
			name: "voting powers of the wrong length",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisVotingPowers = []uint64{100, 100}
			}),
			want: "parliaGenesisVotingPowers has 2 entries",
		},
		{
			name: "zero voting power",
			cfg:  with(func(c *params.LegacyConfig) { c.ParliaGenesisVotingPowers[4] = 0 }),
			want: "is zero",
		},
		{
			name: "cabinets larger than the set",
			cfg:  with(func(c *params.LegacyConfig) { c.ParliaGenesisCabinets = 21 }),
			want: "only 6 validators are configured",
		},
		{
			name: "more validators than the count byte can hold",
			cfg: with(func(c *params.LegacyConfig) {
				c.ParliaGenesisValidators = make([]string, 256)
				for i := range c.ParliaGenesisValidators {
					// 256 distinct addresses
					c.ParliaGenesisValidators[i] = common.BigToAddress(big.NewInt(int64(i + 1))).Hex()
				}
				c.ParliaGenesisVotingPowers = nil
				c.ParliaGenesisCabinets = 0
			}),
			want: "too many parlia genesis validators",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := pgpValidatorInitArgs(tc.cfg)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestNewPGPInitPlanSkipsUnforkedChains pins the case that matters for pgp
// mainnet today: ForkHeight is math.MaxUint64, so there is no switch, so the
// engine must not demand a validator set.
//
// Without this, every node syncing pgp mainnet would refuse to start.
func TestNewPGPInitPlanSkipsUnforkedChains(t *testing.T) {
	cases := []struct {
		name string
		cfg  *params.ChainConfig
	}{
		{"nil config", nil},
		{"no legacy block", &params.ChainConfig{ChainID: big.NewInt(1)}},
		{"fork height not fixed", &params.ChainConfig{
			ChainID: big.NewInt(1),
			Legacy:  &params.LegacyConfig{ForkHeight: math.MaxUint64},
		}},
		{"pgp mainnet config", params.PGPChainConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := newPGPInitPlan(tc.cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if plan != nil {
				t.Fatalf("expected no init plan, got one with %d validators", len(tc.cfg.Legacy.ParliaGenesisValidators))
			}
		})
	}
}

// TestNewPGPInitPlanBuildsForForkedChain covers the positive case and the fact
// that a broken set is rejected at construction time, not at ForkHeight.
func TestNewPGPInitPlanBuildsForForkedChain(t *testing.T) {
	cfg := &params.ChainConfig{ChainID: big.NewInt(8606211), Legacy: devnetLegacyConfig()}
	plan, err := newPGPInitPlan(cfg)
	if err != nil {
		t.Fatalf("newPGPInitPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("expected an init plan")
	}
	if plan.cabinets != 6 {
		t.Errorf("cabinets = %d, want 6", plan.cabinets)
	}
	if len(plan.payload) != 403 {
		t.Errorf("payload length = %d, want 403", len(plan.payload))
	}

	cfg.Legacy.ParliaGenesisValidators = nil
	if _, err := newPGPInitPlan(cfg); err == nil {
		t.Fatal("expected an error for an empty validator set")
	}
}

// TestPGPSnapshotAnchorNumber checks the anchor height.
func TestPGPSnapshotAnchorNumber(t *testing.T) {
	cfg := &params.ChainConfig{ChainID: big.NewInt(1), Legacy: devnetLegacyConfig()}
	plan, err := newPGPInitPlan(cfg)
	if err != nil {
		t.Fatalf("newPGPInitPlan: %v", err)
	}
	p := &Parlia{chainConfig: cfg, pgpInit: plan}

	anchor, ok := p.pgpSnapshotAnchorNumber()
	if !ok || anchor != 1099 {
		t.Fatalf("anchor = (%d, %v), want (1099, true)", anchor, ok)
	}

	// A fork at height 0 means Parlia from genesis, where upstream's own
	// number == 0 anchor applies.
	cfg0 := &params.ChainConfig{ChainID: big.NewInt(1), Legacy: devnetLegacyConfig()}
	cfg0.Legacy.ForkHeight = 0
	plan0, err := newPGPInitPlan(cfg0)
	if err != nil {
		t.Fatalf("newPGPInitPlan: %v", err)
	}
	if _, ok := (&Parlia{chainConfig: cfg0, pgpInit: plan0}).pgpSnapshotAnchorNumber(); ok {
		t.Fatal("a fork at height 0 must not move the snapshot anchor")
	}
}

// TestPGPAnchorSnapshotShape checks that the anchor snapshot carries the
// configured validator set, with indices assigned and zero BLS keys.
func TestPGPAnchorSnapshotShape(t *testing.T) {
	cfg := &params.ChainConfig{ChainID: big.NewInt(1), Legacy: devnetLegacyConfig()}
	plan, err := newPGPInitPlan(cfg)
	if err != nil {
		t.Fatalf("newPGPInitPlan: %v", err)
	}
	p := &Parlia{
		chainConfig: cfg,
		pgpInit:     plan,
		signatures:  lru.NewCache[common.Hash, common.Address](inMemorySignatures),
	}
	snap, err := p.pgpAnchorSnapshot(1099, common.HexToHash("0xdead"))
	if err != nil {
		t.Fatalf("pgpAnchorSnapshot: %v", err)
	}
	if snap.Number != 1099 {
		t.Errorf("Number = %d, want 1099", snap.Number)
	}
	if len(snap.Validators) != 6 {
		t.Fatalf("validators = %d, want 6", len(snap.Validators))
	}
	for _, addr := range devnetValidators {
		info, ok := snap.Validators[common.HexToAddress(addr)]
		if !ok {
			t.Fatalf("validator %s missing from the anchor snapshot", addr)
		}
		// Index must be set: without it indexOfVal falls back to a linear scan,
		// and difficulty (and therefore the seal) is computed from the index.
		if info.Index == 0 {
			t.Errorf("validator %s has Index 0, want a 1-based index", addr)
		}
		if info.VoteAddress != (types.BLSPublicKey{}) {
			t.Errorf("validator %s has a non-zero BLS key", addr)
		}
	}
	if snap.EpochLength != defaultEpochLength {
		t.Errorf("EpochLength = %d, want %d", snap.EpochLength, defaultEpochLength)
	}
	if snap.BlockInterval != defaultBlockInterval {
		t.Errorf("BlockInterval = %d, want %d", snap.BlockInterval, defaultBlockInterval)
	}
}

// TestPGPAnchorSnapshotCarriesVoteAddresses pins the second half of the F-22
// fix: the anchor used to hardcode zero BLS keys.
//
// Zero keys are not a harmless placeholder. IsActiveValidatorAt compares the
// local BLS public key against this list, so a zero anchor makes every node
// answer "not an active validator", the vote manager never signs, and no
// attestation is ever assembled - a chain that produces blocks and never
// finalises anything, with no error anywhere. That is exactly what the first
// BLS devnet did, and it took a debug-level log line to see it.
func TestPGPAnchorSnapshotCarriesVoteAddresses(t *testing.T) {
	// One validator is enough: the point is that the key reaches the snapshot
	// intact, not how many keys there are. A real BLS12-381 public key, because
	// FromLegacy rejects anything that is not a curve point.
	const vote = "0x8ad2f8706d49990cffddd1971451ee8b023851e81b3fb226f01224ea4239eb24da26ff1b328177511e636c668b55396b"

	legacy := &params.LegacyConfig{
		ForkHeight:                1100,
		ParliaGenesisValidators:   []string{devnetValidators[0]},
		ParliaGenesisVotingPowers: []uint64{100},
		ParliaGenesisCabinets:     1,
		ParliaGenesisVoteAddrs:    []string{vote},
	}
	cfg := &params.ChainConfig{ChainID: big.NewInt(1), Legacy: legacy}
	plan, err := newPGPInitPlan(cfg)
	if err != nil {
		t.Fatalf("newPGPInitPlan: %v", err)
	}
	p := &Parlia{
		chainConfig: cfg,
		pgpInit:     plan,
		signatures:  lru.NewCache[common.Hash, common.Address](inMemorySignatures),
	}
	snap, err := p.pgpAnchorSnapshot(1099, common.HexToHash("0xdead"))
	if err != nil {
		t.Fatalf("pgpAnchorSnapshot: %v", err)
	}
	info, ok := snap.Validators[common.HexToAddress(devnetValidators[0])]
	if !ok {
		t.Fatalf("validator %s missing from the anchor snapshot", devnetValidators[0])
	}
	want := types.BLSPublicKey(common.FromHex(vote))
	if info.VoteAddress != want {
		t.Fatalf("anchor vote address = %x, want %x", info.VoteAddress, want)
	}
}
