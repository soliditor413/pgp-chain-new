package parlia

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

// This version identifies an internal verification rule, not a field in legacy headers.
type attestationProofVersion uint8

const attestationBLSv1 attestationProofVersion = 1

// verifyAttestationProof receives the canonical snapshot's selected voter keys.
// Only a consensus rule may select the version. Neither RPC nor the proof selects it.
func verifyAttestationProof(version attestationProofVersion, keys [][]byte, quorum int, digest common.Hash, proof []byte) error {
	if version != attestationBLSv1 {
		return fmt.Errorf("unsupported attestation proof version %d", version)
	}
	voters := make([]bls.PublicKey, 0, len(keys))
	for _, encoded := range keys {
		key, err := bls.PublicKeyFromBytes(encoded)
		if err != nil {
			return fmt.Errorf("BLS public key converts failed: %v", err)
		}
		voters = append(voters, key)
	}
	if len(voters) < quorum {
		return errors.New("invalid attestation, not enough validators voted")
	}
	sig, err := bls.SignatureFromBytes(proof)
	if err != nil {
		return fmt.Errorf("BLS signature converts failed: %v", err)
	}
	if !sig.FastAggregateVerify(voters, digest) {
		return errors.New("invalid attestation, signature verify failed")
	}
	return nil
}
