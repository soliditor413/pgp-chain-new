package spv

import (
	"math/big"

	"github.com/elastos/Elastos.ELA.SPV/interface/store"
	"github.com/elastos/Elastos.ELA.SPV/util"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
)

// Compile-time lock on the API surface that the legacy ELA precompiles consume.
// Renaming or reshaping either method must fail here, not in core/vm:
//
//	pgp-chain/core/vm/contracts.go:1412 getMainChainBlockByHeight
//	  spv.SpvService.GetELAHeader(h) -> header.Previous() / Bits() / MerkleRoot() / Hash() / Height
//	pgp-chain/core/vm/contracts.go:1457 getMainChainLatestHeight
//	  spv.SpvService.HeaderStore().GetBest() -> header.Height
//
// util.Header embeds util.BlockHeader, which is where Previous/Bits/MerkleRoot/Hash
// come from; Height is a field on util.Header itself.
var (
	_ func(*Service, uint32) (*util.Header, error) = (*Service).GetELAHeader
	_ func(*Service) store.HeaderStore             = (*Service).HeaderStore

	// Nil safe facade used by the precompiles.
	// NOTE: there is intentionally no exported Ready(); see ready() in
	// service_access.go for why it must never become an execution gate.
	_ func(uint32) (*util.Header, error) = GetELAHeader
	_ func() (*util.Header, error)       = GetHeaderBest
	_ func() ([]string, int, error)      = GetArbiters
	_ func() bool                        = LegacyStateViewInstalled

	// LegacyStateView data methods. The interface is the seam the replay layer
	// plugs into; these assertions keep the shape stable across phases.
	//
	// Every one of them can fail, including ELAHeight: a view that cannot answer
	// must not be able to masquerade as a view that answered 0 / "not failed".
	//
	// Note the absence of anything completion related. IsCompleted and
	// IsMainTxCompleted were removed on purpose: they read this chain's
	// ELAMinter state, not ELA data, so they do not belong on an ELA-alignment
	// seam. See the comment block in elaminter.go.
	_ func(LegacyStateView) (uint32, error)                          = LegacyStateView.ELAHeight
	_ func(LegacyStateView, string) (RechargeDatas, *big.Int, error) = LegacyStateView.Recharge
	_ func(LegacyStateView, string) (bool, error)                    = LegacyStateView.IsFailedElaTx

	// RechargeStore: the single source behind the six execution-path read points
	// (#3 FindOutputFeeAndaddressByTxHash, #4 FindOutRechargeInput,
	// #5 GetRechargeDataByTxhash, #6/#7 the two halves of IsFailedElaTx,
	// #8 the SavePayloadInfo "already seen" check).
	_ func(ethdb.KeyValueStore) *RechargeStore                              = NewRechargeStore
	_ func(*RechargeStore, uint64, string, RechargeDatas) error             = (*RechargeStore).PutRecharge
	_ func(*RechargeStore, uint64, string) error                            = (*RechargeStore).MarkFailedElaTx
	_ func(*RechargeStore, uint64, common.Hash) error                       = (*RechargeStore).AdvanceCoverage
	_ func(*RechargeStore, uint64, string) (RechargeDatas, *big.Int, error) = (*RechargeStore).RechargeAt
	_ func(*RechargeStore, uint64, string) (bool, error)                    = (*RechargeStore).IsFailedElaTxAt
	_ func(*RechargeStore, uint32) LegacyStateView                          = (*RechargeStore).HeightView

	// The view lifecycle core/ drives once per block. UninstallHeightView and
	// CurrentRechargeView are the pair that makes a forgotten install fail
	// loudly instead of silently reusing the previous block's ELA height.
	_ func(uint32) error     = InstallHeightView
	_ func()                 = UninstallHeightView
	_ func() LegacyStateView = CurrentRechargeView

	// The error-returning replacements for the three zero-value read points.
	_ func(string) (*big.Int, common.Address, *big.Int, error)    = FindOutputFeeAndaddressByTxHash
	_ func(string) ([]byte, error)                                = FindOutRechargeInput
	_ func(string) (bool, error)                                  = IsFailedElaTx
	_ func(string, string, string, string) (RechargeDatas, error) = BuildRechargeDatas
)
