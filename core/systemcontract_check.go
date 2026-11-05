// Copyright 2026 The pgp-chain Authors
// This file is part of the pgp-chain library.
//
// The pgp-chain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package core

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// checkSystemContractsAtHead verifies that a database whose head is already past
// Legacy.SystemContractDeployHeight really has the contract code installed.
//
// It exists as a pure function so the negative path can be tested without
// building a chain: the inline version in loadLastState was reachable only by
// hand-crafting a database that a node would refuse to open.
//
// A database written by a binary without the deployment has a head past the
// deploy height and no code at 0x1000. Carrying on would produce blocks whose
// state diverges from the rest of the network, and the divergence stays silent
// until the first epoch boundary, where verifyValidators reads the validator
// set from that address.
func checkSystemContractsAtHead(legacy *params.LegacyConfig, headNumber uint64, statedb vm.StateDB) error {
	if legacy == nil || legacy.SystemContractDeployHeight == 0 {
		return nil
	}
	if headNumber < legacy.SystemContractDeployHeight {
		return nil
	}
	if statedb == nil {
		return fmt.Errorf("system contracts cannot be verified at head %d: no state", headNumber)
	}
	if len(statedb.GetCode(common.HexToAddress(systemcontracts.ValidatorContract))) == 0 {
		return fmt.Errorf("system contracts are missing at head %d although the deploy height is %d: this "+
			"database was produced by a binary without the deployment, resync with the matching binary",
			headNumber, legacy.SystemContractDeployHeight)
	}
	return nil
}
