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
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// The twelve ELA precompiles of the pgp-chain legacy (pre-Parlia) rules.
//
// pgp-chain keeps them in core/vm/contracts.go:149-171 as
// "PrecompiledContractsShangHai", i.e. the nine Ethereum precompiles plus the
// twelve ELA contracts below, and selects that map for every block because
// Shanghai is the last fork it ever activated. They are consensus critical: their
// addresses are part of the EIP-2929 warm set (core/vm/contracts.go
// ActivePrecompiles) and their gas is charged up-front by RequiredGas, so an
// address that is missing here changes both the gas of any call that touches it
// and the result of the call itself.
//
// The names and decimal values are copied verbatim from
// pgp-chain/params/protocol_params.go:185-196 so that the ELA SPV port can be
// diffed against the reference one-to-one.
//
// Naming warning: pgp's P256VerifyAddress (0x3e9) is NOT the RIP-7212 /
// EIP-7951 P-256 precompile that bsc keeps at 0x100 (see core/vm/contracts.go
// PrecompiledContractsP256Verify). It verifies an NIST P-256 signature encoded
// in pgp's own input layout and needs the ELA crypto library, not secp256r1.
var (
	ArbiterAddress            = big.NewInt(1000) // 0x3e8
	P256VerifyAddress         = big.NewInt(1001) // 0x3e9
	SignatureVerifyByPbk      = big.NewInt(1002) // 0x3ea
	PledgeBillVerify          = big.NewInt(1003) // 0x3eb
	PledgeBillTokenID         = big.NewInt(1004) // 0x3ec
	PledgeBillTokenDetail     = big.NewInt(1005) // 0x3ed
	PledgeBillTokenVersion    = big.NewInt(1006) // 0x3ee
	GetMainChainBlockByHeight = big.NewInt(1007) // 0x3ef
	GetMainChainLatestHeight  = big.NewInt(1008) // 0x3f0
	GetMainChainRechargeData  = big.NewInt(1009) // 0x3f1
	GetWithdrawData           = big.NewInt(1010) // 0x3f2
	VerifySmallCrossTx        = big.NewInt(1011) // 0x3f3
)

// Gas charged up-front by each ELA precompile.
//
// pgp-chain charges the whole cost in RequiredGas: Run() has no gas argument, so
// nothing is charged inside the implementation. The values are copied from
// pgp-chain/params/protocol_params.go:153-171; the commented name is pgp's own
// constant, kept here so a future fix can be cross-checked against the reference.
const (
	PGPArbitersGas                 uint64 = 1000 // ArbitersBaseGas
	PGPP256VerifyGas               uint64 = 1000 // P256VerifyBaseGas
	PGPPbkVerifySignatureGas       uint64 = 1000 // PbkVerifySignature
	PGPPledgeBillVerifyGas         uint64 = 1000 // PledgeBillVerifyGas
	PGPPledgeBillTokenIDGas        uint64 = 0    // GetPledgeBillTokenID
	PGPPledgeBillTokenDetailGas    uint64 = 0    // GetPledgeBillTokenDetail
	PGPPledgeBillPayloadVersionGas uint64 = 0    // GetPledgeBillTokenID (pgp reuses it)
	PGPGetMainChainBlockGas        uint64 = 1000 // GetMainChainBlock
	PGPGetMainChainLatestHeightGas uint64 = 0    // GetMainChainBlockLatestHeight
	PGPGetMainChainRechargeDataGas uint64 = 0    // literal 0 in RequiredGas
	PGPGetWithdrawDataGas          uint64 = 0    // literal 0 in RequiredGas
	PGPVerifySmallCrossTxGas       uint64 = 0    // literal 0 in RequiredGas
)

// PGPLegacyPrecompileAddresses lists the twelve ELA precompile addresses in the
// same order as the declarations above. core/vm uses it to build both the
// dispatch map and the EIP-2929 warm list, so there is exactly one place to edit
// when the set changes.
var PGPLegacyPrecompileAddresses = []common.Address{
	common.BytesToAddress(ArbiterAddress.Bytes()),
	common.BytesToAddress(P256VerifyAddress.Bytes()),
	common.BytesToAddress(SignatureVerifyByPbk.Bytes()),
	common.BytesToAddress(PledgeBillVerify.Bytes()),
	common.BytesToAddress(PledgeBillTokenID.Bytes()),
	common.BytesToAddress(PledgeBillTokenDetail.Bytes()),
	common.BytesToAddress(PledgeBillTokenVersion.Bytes()),
	common.BytesToAddress(GetMainChainBlockByHeight.Bytes()),
	common.BytesToAddress(GetMainChainLatestHeight.Bytes()),
	common.BytesToAddress(GetMainChainRechargeData.Bytes()),
	common.BytesToAddress(GetWithdrawData.Bytes()),
	common.BytesToAddress(VerifySmallCrossTx.Bytes()),
}
