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

package pgp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/elastos/Elastos.ELA/core/types/payload"
	elaCrypto "github.com/elastos/Elastos.ELA/crypto"

	"github.com/ethereum/go-ethereum/spv"
)

// The serialized size of the PBFT confirm parts that occupy header.Extra.
//
// This is NOT an RLP encoding. ELA serializes payload.Confirm with its own
// Bitcoin style writer (common.WriteVarBytes uses a CompactSize prefix,
// WriteUint32/WriteUint64 are little endian), and the whole struct is what sits
// in Extra:
//
//	proposal = VarBytes(Sponsor) + 32 byte BlockHash + uint32le ViewOffset
//	         + VarBytes(Sign)
//	         = (1+33) + 32 + 4 + (1+64) = 135 with a 33 byte compressed key
//	vote     = 32 byte ProposalHash + VarBytes(Signer) + 1 byte Accept
//	         + VarBytes(Sign)
//	         = 32 + (1+33) + 1 + (1+64) = 132
//	confirm  = proposal + uint64le(len(Votes)) + 8 * vote = 1199
//
// 1199 is the constant length of every mainnet legacy header 1..79458.
const (
	legacyProposalWireLen = 135
	legacyVoteWireLen     = 132

	// legacyConfirmMinLen is the shortest serialized payload.Confirm: one
	// proposal and a vote count word, with no votes. It is the floor
	// verifyHeader enforces on header.Extra.
	legacyConfirmMinLen = legacyProposalWireLen + 8
)

var (
	// ErrSealSPVBehind is returned when the ELA SPV service cannot supply the
	// arbiters a legacy confirm needs. It is deliberately separate from
	// ErrInvalidConfirm: the block may be perfectly valid, we simply cannot tell
	// yet, and an operator reading the log must be able to distinguish "waiting
	// for ELA" from "the block is forged".
	//
	// "Behind" is the only failure mode a running pgp node can hit. startNode
	// always calls startSpv before utils.StartNode (cmd/geth/main.go:390 vs
	// :393), and startSpv only skips a chain that does not run the legacy rules
	// at all (cmd/geth/spv_start.go:85). So the service exists before the first
	// legacy header is verified, and a nonce != 0 rejection here means the
	// listener has not reached that ELA height yet — never that it was never
	// started.
	//
	// core.BlockChain reports every header verification failure through
	// reportBadBlock, so a block hitting this error is logged as "BAD BLOCK" and
	// written to the diagnostic bad block store. That does NOT poison it:
	// badBlockRecords is write-only in this tree and ReadBadBlocks has no caller,
	// so re-delivering the block once the SPV has caught up imports it normally.
	ErrSealSPVBehind = errors.New("pgp: ELA SPV is behind the height the confirm needs")

	// errSealProducerConfig reports a producer set that cannot be used at all
	// because the configuration itself is wrong (not hex, not a public key).
	// It is kept separate from ErrSealSPVBehind on purpose: an operator who sees
	// "SPV is behind" goes and checks the ELA sync, and that is the wrong place
	// to look when the real problem is a typo in the configured keys.
	errSealProducerConfig = errors.New("pgp: the configured PBFT producer set is unusable")

	errConfirmTooShort  = errors.New("pgp: confirm is shorter than a proposal")
	errConfirmTruncated = errors.New("pgp: confirm is truncated")
	errCompactNonCanon  = errors.New("pgp: non canonical length prefix")
	errSponsorTooLong   = errors.New("pgp: proposal sponsor is longer than a public key")
	errSignTooLong      = errors.New("pgp: proposal signature is longer than a signature")
	errConfirmVoteCount = errors.New("pgp: confirm declares more votes than its bytes can hold")
	errConfirmTrailing  = errors.New("pgp: confirm has trailing bytes")
)

// readCompactSize reads an ELA VarUint (Bitcoin CompactSize) at off and returns
// the value plus the number of bytes consumed.
//
// It duplicates common.ReadVarUint rather than calling it because the caller has
// to know *where* the field ends before anything is allocated; see decodeConfirm.
// The canonicality rules and the width of each discriminant are copied from
// Elastos.ELA/common/serialize.go:265-317 so that a buffer accepted here is
// accepted there and vice versa.
func readCompactSize(b []byte, off int) (uint64, int, error) {
	if off < 0 || off >= len(b) {
		return 0, 0, errConfirmTruncated
	}
	switch disc := b[off]; disc {
	case 0xff:
		if len(b)-off < 9 {
			return 0, 0, errConfirmTruncated
		}
		v := binary.LittleEndian.Uint64(b[off+1:])
		if v < 0x100000000 {
			return 0, 0, fmt.Errorf("%w: 0xff prefix with value %d", errCompactNonCanon, v)
		}
		return v, 9, nil
	case 0xfe:
		if len(b)-off < 5 {
			return 0, 0, errConfirmTruncated
		}
		v := uint64(binary.LittleEndian.Uint32(b[off+1:]))
		if v < 0x10000 {
			return 0, 0, fmt.Errorf("%w: 0xfe prefix with value %d", errCompactNonCanon, v)
		}
		return v, 5, nil
	case 0xfd:
		if len(b)-off < 3 {
			return 0, 0, errConfirmTruncated
		}
		v := uint64(binary.LittleEndian.Uint16(b[off+1:]))
		if v < 0xfd {
			return 0, 0, fmt.Errorf("%w: 0xfd prefix with value %d", errCompactNonCanon, v)
		}
		return v, 3, nil
	default:
		return uint64(disc), 1, nil
	}
}

// proposalWireLen returns the offset of the vote count field, i.e. the length of
// the serialized proposal. It reads lengths only; it does not look at any value.
//
// The 33/64 byte ceilings come from the upstream deserializer, which passes
// crypto.NegativeBigLength and crypto.SignatureLength to ReadVarBytes: a sponsor
// longer than a public key is rejected there, so rejecting it here keeps the two
// in agreement instead of letting our length math run past the real field.
func proposalWireLen(extra []byte) (int, error) {
	sponsorLen, n, err := readCompactSize(extra, 0)
	if err != nil {
		return 0, err
	}
	if sponsorLen > elaCrypto.NegativeBigLength {
		return 0, fmt.Errorf("%w: %d bytes", errSponsorTooLong, sponsorLen)
	}
	off := n + int(sponsorLen)

	// BlockHash (32) and ViewOffset (4) are fixed size.
	if len(extra)-off < 32+4 {
		return 0, errConfirmTruncated
	}
	off += 32 + 4

	signLen, n, err := readCompactSize(extra, off)
	if err != nil {
		return 0, err
	}
	if signLen > elaCrypto.SignatureLength {
		return 0, fmt.Errorf("%w: %d bytes", errSignTooLong, signLen)
	}
	return off + n + int(signLen), nil
}

// decodeConfirm parses header.Extra into a payload.Confirm.
//
// The parsing is delegated to payload.Confirm.Deserialize so that the accepted
// byte sequences are exactly the ones pgp-chain accepts. What is NOT delegated is
// the vote count: Deserialize does
//
//	signCount, _ := common.ReadUint64(r)
//	p.Votes = make([]DPOSProposalVote, signCount)
//
// with no upper bound (Elastos.ELA core/types/payload/confirm.go:45-64), so a
// header claiming 2^40 votes allocates a terabyte inside the header verification
// goroutine and takes the node down. header.Extra comes straight off the wire, so
// that is a remote crash, not a local misuse.
//
// The count is therefore bounded against the bytes that actually remain before
// Deserialize is called: every vote is at least 132 bytes, so a count larger than
// remaining/132 cannot be honoured by any buffer of this size. That leaves the
// allocation at most len(extra)/132 entries.
//
// The count is a fixed 8 byte little endian word (common.WriteUint64), NOT a
// CompactSize like the length prefixes around it, which is the second trap here.
// Parsing it as a varint consumes 7 bytes too few and the damage does not show up
// at the parse site: Deserialize reads the first vote's bytes as the vote array,
// everything after it shifts, and the failure surfaces much later as a bogus
// "vote commits to another proposal" on a block that is perfectly valid.
//
// Trailing bytes are rejected too. Deserialize stops after the last vote and
// never checks that it consumed the whole reader, which would let an attacker
// append arbitrary bytes: the header hash and Extra both change, every signature
// still verifies, and two different headers carry the same confirm.
func decodeConfirm(extra []byte) (*payload.Confirm, error) {
	if len(extra) < legacyConfirmMinLen {
		return nil, fmt.Errorf("%w: %d bytes, want at least %d", errConfirmTooShort, len(extra), legacyConfirmMinLen)
	}
	countOffset, err := proposalWireLen(extra)
	if err != nil {
		return nil, err
	}
	if len(extra)-countOffset < 8 {
		return nil, fmt.Errorf("%w: no room for the vote count", errConfirmTruncated)
	}
	// Fixed 8 byte little endian, not a CompactSize; see the doc comment for the
	// symptom of reading it as one.
	count := binary.LittleEndian.Uint64(extra[countOffset:])
	remaining := uint64(len(extra) - countOffset - 8)
	if count > remaining/legacyVoteWireLen {
		return nil, fmt.Errorf("%w: %d votes in %d bytes, at most %d fit",
			errConfirmVoteCount, count, remaining, remaining/legacyVoteWireLen)
	}

	var confirm payload.Confirm
	r := bytes.NewReader(extra)
	if err := confirm.Deserialize(r); err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("%w: %d bytes after the last vote", errConfirmTrailing, r.Len())
	}
	return &confirm, nil
}

// legacyMajorityCount mirrors pgp-chain's
// Producers.GetMajorityCountByTotalSigners (pgp-chain/dpos/producers.go:187-192):
// two thirds of totalSigner, rounded DOWN because the multiplication happens in
// float64 and the result is truncated on the way back to int.
//
// Both the float form and the rounding are load bearing. 12 producers need 8
// votes and 25 need 16, not 17; and one producer needs zero, which means a
// one-member producer set accepts a vote-less confirm. That looks like a bug and
// is the reference behaviour, so it is reproduced rather than fixed: mainnet has
// twelve producers and never reaches the case, and "fixing" it would reject
// confirms the chain actually accepted.
//
// The argument is the *total* signer count, not the number of producers that
// survived filtering. Upstream reaches this same expression through
// GetCRMajorityCount (dpos/producers.go:184), which passes defaultCRCSignerNumber
// — 12 as declared, overwritten with len(producers) when the set is loaded
// (dpos/producers.go:18, :38). That equivalence is why the elaHeight == 0 branch
// can call legacyMajorityCount(len(list)) and still match the reference exactly.
func legacyMajorityCount(totalSigner int) int {
	return int(float64(totalSigner) * 2 / 3)
}

// decodeProducers turns the configured hex public keys into the byte slices the
// confirm check compares against.
//
// Every non-empty entry is parsed with elaCrypto.DecodePoint, the same function
// verification uses, so a malformed key in the configuration fails when the
// producer set is first resolved instead of showing up later as an unverifiable
// confirm. Empty entries are passed through untouched: they are how pgb's own
// configuration represents a vacant seat, and checkConfirm skips them.
func decodeProducers(hexKeys []string) ([][]byte, error) {
	out := make([][]byte, 0, len(hexKeys))
	for i, key := range hexKeys {
		if key == "" {
			out = append(out, nil)
			continue
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(key, "0x"), "0X"))
		if err != nil {
			return nil, fmt.Errorf("%w: producer %d is not hex: %v", errSealProducerConfig, i, err)
		}
		if _, err := elaCrypto.DecodePoint(raw); err != nil {
			return nil, fmt.Errorf("%w: producer %d is not a valid public key: %v", errSealProducerConfig, i, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// checkProposal verifies the sponsor's signature over the proposal
// (pgp-chain/dpos/proposal.go:31-43).
func checkProposal(proposal *payload.DPOSProposal) error {
	pk, err := elaCrypto.DecodePoint(proposal.Sponsor)
	if err != nil {
		return fmt.Errorf("%w: proposal sponsor: %v", ErrInvalidConfirm, err)
	}
	if err := elaCrypto.Verify(*pk, proposal.Data(), proposal.Sign); err != nil {
		return fmt.Errorf("%w: proposal signature: %v", ErrInvalidConfirm, err)
	}
	return nil
}

// checkVote verifies one vote's signature (pgp-chain/dpos/proposalvote.go:34-46).
func checkVote(vote *payload.DPOSProposalVote) error {
	pk, err := elaCrypto.DecodePoint(vote.Signer)
	if err != nil {
		return fmt.Errorf("%w: vote signer: %v", ErrInvalidConfirm, err)
	}
	if err := elaCrypto.Verify(*pk, vote.Data(), vote.Sign); err != nil {
		return fmt.Errorf("%w: vote signature: %v", ErrInvalidConfirm, err)
	}
	return nil
}

// checkConfirm is the port of pgp-chain's dpos.CheckConfirm
// (pgp-chain/dpos/confirm.go:26-107). It enforces, in this order, that
//
//  1. the producer set is non-empty after dropping empty and all-zero entries;
//  2. the proposal sponsor is a member of it;
//  3. the proposal signature is valid;
//  4. every vote accepts, commits to this proposal, comes from a member, is not
//     a repeat of an earlier vote, and carries a valid signature;
//  5. the number of unique valid votes reaches minSignCount.
//
// Each of those is a forgery gate on its own: dropping the membership checks
// lets anyone sign, dropping deduplication lets one key satisfy a 8-of-12
// threshold by itself.
//
// producers should be the filtered set (no empty entries); producers with empty
// or zero entries are tolerated and skipped, exactly as upstream does.
func checkConfirm(confirm *payload.Confirm, producers [][]byte, minSignCount int) error {
	if len(producers) == 0 {
		return fmt.Errorf("%w: empty producer set", ErrInvalidConfirm)
	}

	producerSet := make(map[string]struct{}, len(producers))
	for _, producer := range producers {
		if len(producer) == 0 {
			continue
		}
		if bytes.Equal(producer, make([]byte, len(producer))) {
			continue
		}
		producerSet[string(producer)] = struct{}{}
	}
	if len(producerSet) == 0 {
		return fmt.Errorf("%w: producer set has no valid member", ErrInvalidConfirm)
	}

	if _, ok := producerSet[string(confirm.Proposal.Sponsor)]; !ok {
		return fmt.Errorf("%w: proposal sponsor %x is not a producer", ErrInvalidConfirm, confirm.Proposal.Sponsor)
	}
	if err := checkProposal(&confirm.Proposal); err != nil {
		return err
	}

	proposalHash := confirm.Proposal.Hash()
	signed := make(map[string]struct{}, len(confirm.Votes))
	validVotes := 0
	// The sponsor's own vote is inside this loop and counts towards
	// minSignCount, by construction rather than by luck: StartProposal and
	// StartVote both identify the actor with ac.PublicKeyBytes()
	// (pgp-chain/dpos/proposal.go:18, dpos/proposalvote.go:22), so the sealer
	// that signed the proposal is always one of the signers. That is what makes
	// 8-of-12 reachable — treat the sponsor as "extra" and a valid mainnet
	// confirm would count 7 and be rejected.
	for i := range confirm.Votes {
		vote := &confirm.Votes[i]
		if !vote.Accept {
			return fmt.Errorf("%w: vote %d is a reject vote", ErrInvalidConfirm, i)
		}
		if !proposalHash.IsEqual(vote.ProposalHash) {
			return fmt.Errorf("%w: vote %d commits to another proposal", ErrInvalidConfirm, i)
		}
		if _, ok := producerSet[string(vote.Signer)]; !ok {
			return fmt.Errorf("%w: vote %d signer %x is not a producer", ErrInvalidConfirm, i, vote.Signer)
		}
		if _, ok := signed[string(vote.Signer)]; ok {
			return fmt.Errorf("%w: vote %d repeats signer %x", ErrInvalidConfirm, i, vote.Signer)
		}
		if err := checkVote(vote); err != nil {
			return err
		}
		signed[string(vote.Signer)] = struct{}{}
		validVotes++
	}
	if validVotes < minSignCount {
		return fmt.Errorf("%w: need %d votes, got %d", ErrInvalidConfirm, minSignCount, validVotes)
	}
	return nil
}

// verifyConfirm resolves the producer set for elaHeight and checks the confirm
// against it (pgp-chain/consensus/pbft/pbft.go:1118-1143).
//
// elaHeight is header.Nonce, i.e. the ELA main chain height the block was sealed
// against. Zero means "the ELA chain had no height yet" — every mainnet block up
// to 16404 — and the fixed configured producer set applies. That branch does not
// touch the SPV database at all, which is why the first 16404 blocks can be
// verified with no ELA sync whatsoever.
//
// A non-zero height goes through spv.GetProducers, which already returns the
// filtered producer list plus the unfiltered total signer count. The total is
// what the threshold is computed from, not len(producers): the two differ when
// the arbiter set contains vacant seats, and using the smaller number would
// accept a confirm with too few signatures.
//
// Failures are returned, never swallowed. Not being able to check a signature is
// not the same as the signature being valid, so every "I cannot tell yet" path
// rejects the header with ErrSealSPVBehind instead of guessing.
func (e *Engine) verifyConfirm(confirm *payload.Confirm, elaHeight uint64) error {
	var (
		producers    [][]byte
		minSignCount int
	)
	if elaHeight == 0 {
		// No SPV gate here, deliberately: this branch never reads the database,
		// so blocks 1..16404 stay verifiable on a node with no ELA sync at all.
		list, err := decodeProducers(e.config.Producers)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return fmt.Errorf("%w: no configured producer set for elaHeight 0", errSealProducerConfig)
		}
		producers, minSignCount = list, legacyMajorityCount(len(list))
	} else {
		// The arbiters for an ELA height only exist once the SPV listener has
		// applied blocks up to that height, and HeaderStore().GetBest() is never
		// behind an applied block (headers are synced before bodies). So this
		// gate can only reject a block we genuinely cannot verify yet - it never
		// rejects one we could have verified.
		//
		// Rejecting is safe: the block is retried and imported normally once the
		// SPV catches up. See ErrSealSPVBehind for why the BAD BLOCK log line it
		// causes is diagnostic only.
		if spvHeight := spv.GetSpvHeight(); spvHeight < elaHeight {
			return fmt.Errorf("%w: need ELA height %d, SPV is at %d", ErrSealSPVBehind, elaHeight, spvHeight)
		}
		list, total, err := spv.GetProducers(elaHeight)
		if err != nil {
			// Same sentinel: the only realistic cause is SPV data that is not
			// there yet. Failing closed is the point - a missing seal check must
			// never look like a passing one.
			return fmt.Errorf("%w: ela height %d: %v", ErrSealSPVBehind, elaHeight, err)
		}
		producers, minSignCount = list, legacyMajorityCount(total)
	}
	return checkConfirm(confirm, producers, minSignCount)
}
