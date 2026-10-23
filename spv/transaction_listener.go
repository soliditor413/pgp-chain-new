package spv

import (
	"github.com/elastos/Elastos.ELA.SPV/bloom"
	spv "github.com/elastos/Elastos.ELA.SPV/interface"
	"github.com/elastos/Elastos.ELA/common"
	elacom "github.com/elastos/Elastos.ELA/core/types/common"
	it "github.com/elastos/Elastos.ELA/core/types/interfaces"

	"github.com/ethereum/go-ethereum/log"
)

// transactionListener is the registration that makes the SPV filter non-empty.
//
// This is not an optimisation, it is the read path. RegisterTransactionListener
// (Elastos.ELA.SPV/interface/spvservice.go:131-150) stores the address in the
// bloom filter database and returns *without* recording the listener's TxType;
// the filter therefore contains exactly the addresses of listeners that carry
// one. With no address registered the peer sends merkle blocks in which nothing
// matches, block.Transactions is empty for every height, and ApplyElaBlock
// "succeeds" on 2.3M blocks while writing zero records. That is the failure this
// listener exists to prevent, and it is why NewService refuses to start when the
// registration fails.
//
// Only the read path is ported. pgp-chain's listener additionally ran
// SavePayloadInfo -> SendTransaction (a write path that submits ELA transactions
// and mutates in-memory signer state); none of that is here.
type transactionListener struct {
	address string
	service spv.SPVService
}

func (l *transactionListener) Address() string { return l.address }

func (l *transactionListener) Type() elacom.TxType { return elacom.TransferCrossChainAsset }

func (l *transactionListener) Flags() uint64 {
	return spv.FlagNotifyInSyncing | spv.FlagNotifyConfirmed
}

// Notify indexes one ELA transaction and acknowledges it.
//
// The tx arrives already decoded (peer.go builds it with
// transaction.GetTransactionByBytes and hands the SPV service an it.Transaction),
// so it goes straight to ApplyElaTransaction. Deliberately no networking, no
// transaction submission and no global signer state: the only write is the
// recharge record.
//
// A failure is logged and sync continues. Stopping here would not fix anything —
// the ELA block is already on the ELA chain — and the BSC block anchored on that
// ELA height will fail loudly with ErrRechargeHeightNotCovered when it executes,
// which is the signal we want. It does not advance coverage either: only
// ApplyElaBlock may do that.
//
// The receipt MUST be submitted even when indexing failed. It tells the SPV
// service the notification was delivered, which is what stops the same
// transaction being re-notified forever.
func (l *transactionListener) Notify(id common.Uint256, proof bloom.MerkleProof, tx it.Transaction) {
	if tx == nil {
		log.Error("pgp: ELA transaction notification carried a nil transaction",
			"elaHeight", proof.Height, "notifyId", id.String())
		return
	}
	if store := GetRechargeStore(); store != nil {
		if err := ApplyElaTransaction(store, uint64(proof.Height), tx); err != nil {
			log.Error("pgp: failed to index ELA transaction into the recharge store; "+
				"any BSC block anchored on this ELA height will fail with ErrRechargeHeightNotCovered",
				"elaHeight", proof.Height, "txHash", tx.Hash().String(), "err", err)
		}
	}
	if l.service == nil {
		return
	}
	if err := l.service.SubmitTransactionReceipt(id, tx.Hash()); err != nil {
		log.Error("pgp: submitting the ELA transaction receipt failed; the SPV service may re-notify this tx",
			"elaHeight", proof.Height, "txHash", tx.Hash().String(), "err", err)
	}
}
