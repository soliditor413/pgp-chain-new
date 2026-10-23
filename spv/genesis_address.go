package spv

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	elacom "github.com/elastos/Elastos.ELA/common"
	"github.com/elastos/Elastos.ELA/core/contract"
	"golang.org/x/crypto/ripemd160"
)

// CalculateGenesisAddress derives the ELA main chain address that this side
// chain's deposits are paid to, from the side chain genesis block hash.
//
// This address is not cosmetic: it is the only thing that makes the SPV filter
// return the right transactions. spv.NewService is configured with
// FilterType = filter.FTReturnSidechainDepositCoinFilter and with this address
// registered as an interested address, so the ELA peers send us exactly the
// blocks' cross chain outputs paying it — i.e. the deposits that back the 0x3F1
// getMainChainRechargeData precompile — instead of the whole main chain's
// transaction set.
//
// Layout, ported from pgp-chain cmd/pgp/main.go:361-398 (itself a port of
// Elastos.ELA.Client cli/wallet):
//
//	program = [len(reverse(genesisHash))] ++ reverse(genesisHash) ++ [CROSSCHAIN]
//	hash168 = ripemd160(sha256(program)) prefixed with PrefixCrossChain
//	address = base58(hash168 with ELA version bytes)
//
// The reversal is because an ELA hash string is printed in the opposite byte
// order to how it is hashed. Get it wrong and the derived address is a valid
// address that nobody ever pays, so the SPV filter matches nothing, the recharge
// store stays empty, and every deposit block fails with ErrRechargeUnavailable.
// There is no error message anywhere along that path.
func CalculateGenesisAddress(genesisBlockHash string) (string, error) {
	// Unlike Ethereum, ELA hash strings carry no 0x prefix.
	genesisBlockHash = trimHexPrefix(genesisBlockHash)
	raw, err := hex.DecodeString(genesisBlockHash)
	if err != nil {
		return "", errors.New("spv: genesis block hash is not hex")
	}
	reversed := elacom.BytesReverse(raw)

	buf := new(bytes.Buffer)
	buf.WriteByte(byte(len(reversed)))
	buf.Write(reversed)
	buf.WriteByte(byte(elacom.CROSSCHAIN))

	sum := sha256.Sum256(buf.Bytes())
	md := ripemd160.New()
	md.Write(sum[:])
	programHash, err := elacom.Uint168FromBytes(md.Sum([]byte{byte(contract.PrefixCrossChain)}))
	if err != nil {
		return "", errors.New("spv: genesis block bytes to program hash failed")
	}
	address, err := programHash.ToAddress()
	if err != nil {
		return "", errors.New("spv: genesis block hash to genesis address failed")
	}
	return address, nil
}
