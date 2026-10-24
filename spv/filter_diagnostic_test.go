package spv

import (
	"math"
	"testing"

	"github.com/elastos/Elastos.ELA.SPV/bloom"
	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/p2p/msg"
)

// testProgramHash builds a 21-byte ELA program hash without going through an
// address string, so the test does not depend on chain parameters.
func testProgramHash(seed byte) *common.Uint168 {
	var hash common.Uint168
	hash[0] = 0x4b // prefix of a normal main-chain program hash
	for i := 1; i < len(hash); i++ {
		hash[i] = seed
	}
	return &hash
}

// buildTxFilter mirrors what Elastos.ELA.SPV does in spvservice.GetFilter: a
// filter sized to the number of addresses, each added as its 21-byte form.
func buildTxFilter(addrs ...*common.Uint168) *msg.TxFilterLoad {
	f := bloom.NewFilter(uint32(len(addrs)), math.MaxUint32, 0, nil)
	for _, addr := range addrs {
		f.Add(addr.Bytes())
	}
	return f.ToTxFilterMsg(0)
}

// TestInspectTxFilterMatchesTheRegisteredAddress is the healthy case: the
// snapshot must show a non-empty bitmap and an end-to-end match against the
// address NewService registered.
func TestInspectTxFilterMatchesTheRegisteredAddress(t *testing.T) {
	addr := testProgramHash(0x11)

	snapshot, err := inspectTxFilter(buildTxFilter(addr), addr)
	if err != nil {
		t.Fatalf("inspectTxFilter: %v", err)
	}
	if snapshot.BloomBytes == 0 {
		t.Fatal("BloomBytes = 0 for a filter built from one address")
	}
	if snapshot.HashFuncs == 0 {
		t.Error("HashFuncs = 0 for a filter built from one address")
	}
	if !snapshot.Matches {
		t.Error("Matches = false although the filter was built from exactly that address")
	}
}

// TestInspectTxFilterDetectsAnEmptyFilter is the P2-3 regression test.
//
// An empty bloom filter is not an error anywhere in the SPV stack: the ELA side
// skips address matching entirely, merkle blocks come back with no
// transactions, ApplyElaBlock "succeeds" on every block and coverage advances.
// BloomBytes is the one number that separates that state from a healthy sync,
// and it has to be readable at start-up rather than 2M ELA blocks later.
//
// It also pins why inspectTxFilter does not simply ask the bloom filter whether
// it matches: bloom.Filter.matches returns true when HashFuncs is 0, and an
// empty filter has HashFuncs 0, so an unguarded probe would answer "matches
// everything" for a filter that can match nothing. Matches must stay false here.
func TestInspectTxFilterDetectsAnEmptyFilter(t *testing.T) {
	addr := testProgramHash(0x22)

	snapshot, err := inspectTxFilter(buildTxFilter(), addr)
	if err != nil {
		t.Fatalf("inspectTxFilter: %v", err)
	}
	if snapshot.BloomBytes != 0 {
		t.Fatalf("BloomBytes = %d for a filter built from no addresses, want 0", snapshot.BloomBytes)
	}
	if snapshot.HashFuncs != 0 {
		t.Errorf("HashFuncs = %d for an empty filter, want 0", snapshot.HashFuncs)
	}
	if snapshot.Matches {
		t.Error("Matches = true for an empty filter, which cannot contain any address")
	}
}

// TestInspectTxFilterReportsAnAddressItDoesNotContain covers the other broken
// state: a filter that exists but was built from the wrong address. BloomBytes
// alone would call that healthy, so Matches has to be checked separately.
func TestInspectTxFilterReportsAnAddressItDoesNotContain(t *testing.T) {
	registered := testProgramHash(0x33)
	other := testProgramHash(0x44)

	snapshot, err := inspectTxFilter(buildTxFilter(other), registered)
	if err != nil {
		t.Fatalf("inspectTxFilter: %v", err)
	}
	if snapshot.BloomBytes == 0 {
		t.Fatal("BloomBytes = 0 although the filter was built from an address")
	}
	if snapshot.Matches {
		t.Error("Matches = true for a filter built from a different address")
	}
}

// TestInspectTxFilterRejectsBadInput keeps the diagnostic itself from failing
// silently: a nil filter or undecodable bytes must surface as an error so the
// caller logs "cannot be ruled out" rather than reporting a healthy filter.
func TestInspectTxFilterRejectsBadInput(t *testing.T) {
	addr := testProgramHash(0x55)

	if _, err := inspectTxFilter(nil, addr); err == nil {
		t.Error("inspectTxFilter accepted a nil filter")
	}
	if _, err := inspectTxFilter(&msg.TxFilterLoad{Data: []byte{0x01}}, addr); err == nil {
		t.Error("inspectTxFilter accepted a truncated filter payload")
	}
}

// TestLogTxFilterStateIsLoudOnAnEmptyFilter checks the log line an operator
// actually sees. The message has to name the consequence, because the whole
// point of P2-3 is that nothing else in the stack reports this state.
func TestLogTxFilterStateIsLoudOnAnEmptyFilter(t *testing.T) {
	logs := captureLogs(t)
	addr := testProgramHash(0x66)

	logTxFilterSnapshot(fakeFilterSource{buildTxFilter()}, "EN-empty", addr)

	if got := logs.count("pgp: the ELA transaction filter is EMPTY (0 bytes of bloom data), " +
		"so no ELA transaction can ever match it: every merkle block will arrive with no transactions, " +
		"the recharge store will stay empty while coverage advances, and the first BSC block " +
		"that reads a recharge will fail with ErrRechargeHeightNotCovered"); got != 1 {
		t.Fatalf("empty-filter error lines = %d, want 1; got %d records: %v", got, len(logs.msgs), logs.msgs)
	}
}

// TestLogTxFilterStateReportsAHealthyFilter pins the start-up line that answers
// "is the filter installed, and for what" without waiting for the sync.
func TestLogTxFilterStateReportsAHealthyFilter(t *testing.T) {
	logs := captureLogs(t)
	addr := testProgramHash(0x77)
	filter := buildTxFilter(addr)

	logTxFilterSnapshot(fakeFilterSource{filter}, "EN-healthy", addr)

	attrs := logs.find("pgp: ELA transaction filter is installed and matches the registered address")
	if attrs == nil {
		t.Fatalf("no healthy-filter line was written; got %v", logs.msgs)
	}
	wantUint(t, attrs, "addresses", 1)
	if got, ok := attrs["address"]; !ok || got != "EN-healthy" {
		t.Errorf("address = %v, want EN-healthy", got)
	}
	// Compare against the snapshot rather than a literal: the bitmap size is
	// derived by the SPV library from the element count and false-positive rate,
	// so hard-coding it would pin an upstream formula instead of our behaviour.
	expected, err := inspectTxFilter(filter, addr)
	if err != nil {
		t.Fatalf("inspectTxFilter: %v", err)
	}
	wantUint(t, attrs, "bloomBytes", uint64(expected.BloomBytes))
	wantUint(t, attrs, "hashFuncs", uint64(expected.HashFuncs))
}

// fakeFilterSource stands in for the Elastos SPV service's GetFilter, which is
// not part of the spv.SPVService interface and so cannot be faked wholesale.
type fakeFilterSource struct {
	filter *msg.TxFilterLoad
}

func (f fakeFilterSource) GetFilter() *msg.TxFilterLoad { return f.filter }
