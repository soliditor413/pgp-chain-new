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

package vm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// PGPRechargeData is one ELA recharge output as the 0x3f1 precompile reports it.
//
// The fields and their order are the Solidity struct the ELA contracts decode
// (pgp-chain/core/vm/contracts.go:1501-1510):
//
//	struct RechargeData {
//	    address targetAddress;
//	    uint256 targetAmount;
//	    uint256 fee;
//	    bytes   targetData;
//	}
//
// This is a copy of spv.RechargeData rather than that type itself because
// core/vm must not import spv: spv imports core/vm to register the precompiles,
// so the dependency would be a cycle. Keeping the encoder on a local type is
// also what makes it testable without a database.
type PGPRechargeData struct {
	TargetAddress common.Address
	TargetAmount  *big.Int
	Fee           *big.Int
	TargetData    []byte
}

// Errors returned by EncodePGPRechargeDatas. They are programming errors on the
// SPV side (a record that cannot be expressed in ABI), not user input errors, and
// they must never be silently turned into a zero amount: a zero there mints
// nothing, so the transaction would succeed with the wrong result instead of
// failing loudly.
var (
	ErrPGPRechargeNilAmount = errors.New("pgp: recharge data has no targetAmount")
	ErrPGPRechargeNilFee    = errors.New("pgp: recharge data has no fee")
	ErrPGPRechargeNegative  = errors.New("pgp: recharge data amount is negative")
)

// The ABI types of the four struct fields plus the length word. They are built
// once because abi.NewType parses a string and allocates, and this encoder runs
// for every 0x3f1 call.
var (
	pgpRechargeTypeUint256 abi.Type
	pgpRechargeTypeAddress abi.Type
	pgpRechargeTypeBytes   abi.Type
)

func init() {
	var err error
	if pgpRechargeTypeUint256, err = abi.NewType("uint256", "", nil); err != nil {
		panic(fmt.Sprintf("pgp: abi uint256 type: %v", err))
	}
	if pgpRechargeTypeAddress, err = abi.NewType("address", "", nil); err != nil {
		panic(fmt.Sprintf("pgp: abi address type: %v", err))
	}
	if pgpRechargeTypeBytes, err = abi.NewType("bytes", "", nil); err != nil {
		panic(fmt.Sprintf("pgp: abi bytes type: %v", err))
	}
}

// EncodePGPRechargeDatas produces the exact return data of the 0x3f1
// getMainChainRechargeData precompile.
//
// The shape is a FLAT abi.Arguments list, not abi.encode(RechargeData[]):
//
//	[uint256 count] ++ per element [address, uint256, uint256, bytes]
//
// It is copied that way from pgp-chain/core/vm/contracts.go:1514-1568, and the
// difference is observable in the first word. A real `RechargeData[]` argument
// would begin with the offset 0x20 pointing at the array; pgp's flat list begins
// with the element count itself. Any Solidity caller of 0x3f1 written against
// pgp reads word 0 as the count, so encoding the tidier `T[]` form here would
// shift every field by one word and silently mis-decode on the ELA side.
//
// Consequently the total length is 32*(1 + 4*N) for empty targetData, plus 32
// per element that carries a non-empty targetData, plus that payload rounded up
// to a multiple of 32.
//
// An empty slice returns 32 zero bytes (count = 0), NOT an empty return value.
// The count word always exists; returning nothing would make the caller's
// abi.decode revert instead of seeing "no recharges".
func EncodePGPRechargeDatas(datas []PGPRechargeData) ([]byte, error) {
	arguments := make(abi.Arguments, 0, 1+4*len(datas))
	arguments = append(arguments, abi.Argument{Name: "count", Type: pgpRechargeTypeUint256})
	for range datas {
		arguments = append(arguments,
			abi.Argument{Name: "targetAddress", Type: pgpRechargeTypeAddress},
			abi.Argument{Name: "targetAmount", Type: pgpRechargeTypeUint256},
			abi.Argument{Name: "fee", Type: pgpRechargeTypeUint256},
			abi.Argument{Name: "targetData", Type: pgpRechargeTypeBytes},
		)
	}

	values := make([]interface{}, 0, 1+4*len(datas))
	values = append(values, new(big.Int).SetInt64(int64(len(datas))))
	for i := range datas {
		amount, err := pgpRechargeAmount("targetAmount", datas[i].TargetAmount)
		if err != nil {
			return nil, fmt.Errorf("pgp: recharge data #%d: %w", i, err)
		}
		fee, err := pgpRechargeAmount("fee", datas[i].Fee)
		if err != nil {
			return nil, fmt.Errorf("pgp: recharge data #%d: %w", i, err)
		}
		values = append(values, datas[i].TargetAddress, amount, fee, datas[i].TargetData)
	}

	packed, err := arguments.Pack(values...)
	if err != nil {
		return nil, fmt.Errorf("pgp: encode recharge data: %w", err)
	}
	return packed, nil
}

// pgpRechargeAmount validates one of the two integer fields.
//
// A nil *big.Int reaches the ABI encoder as a nil pointer and panics inside
// reflect rather than returning an error, so it is rejected here; the returned
// value is a fresh copy so nothing the caller keeps can be mutated under us.
func pgpRechargeAmount(field string, v *big.Int) (*big.Int, error) {
	if v == nil {
		if field == "fee" {
			return nil, ErrPGPRechargeNilFee
		}
		return nil, ErrPGPRechargeNilAmount
	}
	if v.Sign() < 0 {
		return nil, fmt.Errorf("%w: %s = %s", ErrPGPRechargeNegative, field, v)
	}
	return new(big.Int).Set(v), nil
}
