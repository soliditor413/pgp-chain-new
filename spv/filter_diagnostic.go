package spv

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/elastos/Elastos.ELA.SPV/bloom"
	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/p2p/msg"

	"github.com/ethereum/go-ethereum/log"
)

// This file answers one question at start-up: does the bloom filter the SPV
// service is about to hand to ELA peers actually contain the address we care
// about?
//
// It exists because every other signal is blind to the failure. An empty filter
// is not an error anywhere in the stack: the ELA side skips address matching
// entirely when the filter is empty (the check is `len(bf.msg.Filter) != 0`),
// every merkle block then arrives with no transactions, ApplyElaBlock succeeds
// on all 2.3M blocks, coverage advances to the ELA head, and the recharge store
// is simply empty. The only place that becomes visible is a BSC block whose
// Recharge reverts, which is 2M ELA blocks too late to diagnose.
//
// The progress line in NotifyBlock ("ELA main chain sync progress", txs /
// crossChainTxs) does catch it, but only once the ELA sync is far enough along
// to be receiving matched transactions at all. On a fresh data directory that
// is hours away, so the filter is now asserted at registration time instead.

// txFilterSource is the accessor the Elastos SPV service exposes for the filter
// it will send to peers. GetFilter is a method on the unexported *spvservice
// type and is not part of the spv.SPVService interface, so it is reached
// through a structural type assertion. If a future Elastos.ELA.SPV drops or
// renames it the assertion fails and we log a warning rather than refusing to
// start — a missing diagnostic must not turn into an outage.
type txFilterSource interface {
	GetFilter() *msg.TxFilterLoad
}

// filterSnapshot is what the start-up line reports about the ELA bloom filter.
type filterSnapshot struct {
	// BloomBytes is the size of the bloom bitmap itself. Zero means the filter
	// was built from zero addresses and can never match anything.
	BloomBytes int
	// HashFuncs is the number of hash probes per element. Zero on an empty
	// filter, which is the second tell-tale.
	HashFuncs uint32
	// TxTypes is how many transaction types the filter will match on top of the
	// addresses. pgp's listener carries an address, so the SPV SDK stores
	// nothing here (RegisterTransactionListener only writes TxTypes for
	// address-less listeners) and zero is the expected value.
	TxTypes int
	// Matches is the end-to-end answer: the serialised filter, rebuilt the way a
	// peer rebuilds it, matches the address NewService registered.
	Matches bool
}

// inspectTxFilter decodes a serialised ELA transaction filter and reports both
// its shape and whether it matches programHash.
//
// TxFilterLoad.Data is a serialised msg.FilterLoad (var-bytes bitmap, hashFuncs,
// tweak, flags, var-uint txTypes), not a bare bitmap, so its length is never
// zero even for an empty filter — the bitmap inside it is what has to be
// measured.
func inspectTxFilter(tf *msg.TxFilterLoad, programHash *common.Uint168) (filterSnapshot, error) {
	if tf == nil {
		return filterSnapshot{}, errors.New("spv: the ELA service returned no transaction filter")
	}
	var load msg.FilterLoad
	if err := load.Deserialize(bytes.NewReader(tf.Data)); err != nil {
		return filterSnapshot{}, fmt.Errorf("spv: decoding the ELA transaction filter: %w", err)
	}

	snapshot := filterSnapshot{
		BloomBytes: len(load.Filter),
		HashFuncs:  load.HashFuncs,
		TxTypes:    len(load.TxTypes),
	}
	// Guard the match on a non-empty bitmap. The reason is not that probing an
	// empty filter crashes — it does not. bloom.Filter.matches loops
	// `for i := 0; i < HashFuncs; i++` and an empty filter has HashFuncs == 0
	// (NewFilter derives it from the bitmap size, which is zero), so the loop
	// body never runs and matches returns true. That vacuous "matches
	// everything" is precisely the false positive this file exists to rule out:
	// without the guard an empty filter would be reported as a healthy one that
	// covers the registered address. Do not delete it on the assumption that
	// something below would fail loudly — nothing below does.
	//
	// The ELA side guards the same way for the same reason: elanet's
	// matchTxAndUpdate only consults the bloom filter `if len(bf.msg.Filter) != 0`.
	if programHash != nil && len(load.Filter) != 0 {
		snapshot.Matches = bloom.LoadFilter(&load).Matches(programHash.Bytes())
	}
	return snapshot, nil
}

// logTxFilterState logs whether the ELA filter is installed and effective.
//
// Called from NewService immediately after RegisterTransactionListener, which
// is the only writer of both the filter database and (indirectly) this bitmap.
func logTxFilterState(service spv.SPVService, address string, programHash *common.Uint168) {
	source, ok := service.(txFilterSource)
	if !ok {
		log.Warn("pgp: cannot inspect the ELA transaction filter; "+
			"an empty filter would only be detectable from the txs counter in the ELA sync progress line",
			"address", address)
		return
	}
	logTxFilterSnapshot(source, address, programHash)
}

// logTxFilterSnapshot is the half of logTxFilterState that does not depend on
// the concrete SPV service type, so it can be driven by a one-method fake.
func logTxFilterSnapshot(source txFilterSource, address string, programHash *common.Uint168) {
	snapshot, err := inspectTxFilter(source.GetFilter(), programHash)
	if err != nil {
		log.Error("pgp: the ELA transaction filter could not be inspected; "+
			"an empty filter cannot be ruled out", "address", address, "err", err)
		return
	}

	if snapshot.BloomBytes == 0 {
		log.Error("pgp: the ELA transaction filter is EMPTY (0 bytes of bloom data), so no ELA "+
			"transaction can ever match it: every merkle block will arrive with no transactions, "+
			"the recharge store will stay empty while coverage advances, and the first BSC block "+
			"that reads a recharge will fail with ErrRechargeHeightNotCovered",
			"address", address, "hashFuncs", snapshot.HashFuncs, "txTypes", snapshot.TxTypes)
		return
	}
	if !snapshot.Matches {
		log.Error("pgp: the ELA transaction filter does not match the registered address, so this "+
			"node will sync the ELA chain without ever seeing a recharge for it",
			"address", address, "bloomBytes", snapshot.BloomBytes, "hashFuncs", snapshot.HashFuncs)
		return
	}

	// The literal one-liner the operator wants in the log: how big the filter
	// is and whether it covers the address. "addresses" is 1 by construction —
	// NewService registers exactly one listener, and it carries an address.
	log.Info("pgp: ELA transaction filter is installed and matches the registered address",
		"address", address, "programHash", programHash.String(),
		"addresses", 1, "bloomBytes", snapshot.BloomBytes,
		"hashFuncs", snapshot.HashFuncs, "txTypes", snapshot.TxTypes)
}
