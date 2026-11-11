package types

import "github.com/ethereum/go-ethereum/common"

// This is introduced because of the Tendermint IAVL Merkle Proof verification exploitation.
var NanoBlackList = []common.Address{}
