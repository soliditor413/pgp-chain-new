package spv

import (
	"testing"

	"github.com/elastos/Elastos.ELA.SPV/bloom"
	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	elacommon "github.com/elastos/Elastos.ELA/common"
	elacom "github.com/elastos/Elastos.ELA/core/types/common"
)

// TestTransactionListenerFilterContract pins the three values that decide
// whether the SPV bloom filter ever contains anything.
//
// Address() is the load-bearing one: RegisterTransactionListener only writes the
// filter address when Address() != "", and it returns immediately after doing so.
// An empty address means an empty filter, which means every merkle block arrives
// with no transactions and the recharge store stays empty while coverage marches
// to the ELA head.
func TestTransactionListenerFilterContract(t *testing.T) {
	l := &transactionListener{address: "XNQWEZ7aqNyJHvav8j8tNo2ZQypuTsWQk6"}
	if l.Address() != "XNQWEZ7aqNyJHvav8j8tNo2ZQypuTsWQk6" {
		t.Fatalf("Address() = %q, want the configured genesis address", l.Address())
	}
	if l.Type() != elacom.TransferCrossChainAsset {
		t.Fatalf("Type() = %v, want TransferCrossChainAsset", l.Type())
	}
	flags := l.Flags()
	if flags&spv.FlagNotifyInSyncing == 0 {
		t.Error("Flags() is missing FlagNotifyInSyncing: synced blocks would be skipped, leaving holes in the height index")
	}
	if flags&spv.FlagNotifyConfirmed == 0 {
		t.Error("Flags() is missing FlagNotifyConfirmed: live blocks would not be indexed")
	}
}

// TestTransactionListenerNotifyIndexes covers the listener's write path without a
// live SPV service (service is nil, so the receipt is skipped). The record must
// land in the installed store at the proof height.
func TestTransactionListenerNotifyIndexes(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}

	s := newTestStore()
	previous := GetRechargeStore()
	SetRechargeStore(s)
	defer SetRechargeStore(previous)

	l := &transactionListener{address: "XNQWEZ7aqNyJHvav8j8tNo2ZQypuTsWQk6"}
	l.Notify(elacommon.Uint256{}, bloom.MerkleProof{Height: elaHeight30115}, tx)

	if err := s.AdvanceCoverage(elaHeight30115, testElaBlockHash); err != nil {
		t.Fatalf("AdvanceCoverage: %v", err)
	}
	datas, fee, err := s.RechargeAt(elaHeight30115, elaHash30115)
	if err != nil {
		t.Fatalf("RechargeAt: %v", err)
	}
	if len(datas) != 1 || fee.String() != "150000" {
		t.Fatalf("stored %d records with fee %s, want 1 record with fee 150000", len(datas), fee)
	}
}

// TestTransactionListenerNotifyWithoutStore must not panic when the listener is
// invoked before SpvDbInit has installed a store: the SPV service can deliver a
// notification before the node has opened its database.
func TestTransactionListenerNotifyWithoutStore(t *testing.T) {
	tx, err := decodeElaTransaction(realElaTx30115(t))
	if err != nil {
		t.Fatalf("decodeElaTransaction: %v", err)
	}

	previous := GetRechargeStore()
	SetRechargeStore(nil)
	defer SetRechargeStore(previous)

	l := &transactionListener{}
	l.Notify(elacommon.Uint256{}, bloom.MerkleProof{Height: elaHeight30115}, tx)

	if store := GetRechargeStore(); store != nil {
		t.Fatal("Notify installed a store")
	}
}
