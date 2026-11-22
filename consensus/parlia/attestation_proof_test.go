package parlia

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
)

func sampleAttestation(t testing.TB, count int) ([][]byte, common.Hash, []byte) {
	t.Helper()
	digest := (&types.VoteData{SourceNumber: 1, TargetNumber: 2}).Hash()
	keys := make([][]byte, count)
	signatures := make([]bls.Signature, count)
	for i := range keys {
		key, err := bls.RandKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = key.PublicKey().Marshal()
		signatures[i] = key.Sign(digest[:])
	}
	return keys, digest, bls.AggregateSignatures(signatures).Marshal()
}

func TestAttestationProofVersion(t *testing.T) {
	keys, digest, proof := sampleAttestation(t, 3)
	if err := verifyAttestationProof(attestationBLSv1, keys, 3, digest, proof); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		version attestationProofVersion
		keys    [][]byte
		quorum  int
		digest  common.Hash
		proof   []byte
	}{
		{"unsupported version", 2, keys, 3, digest, proof},
		{"missing quorum", attestationBLSv1, keys, 4, digest, proof},
		{"different vote", attestationBLSv1, keys, 3, common.Hash{}, proof},
		{"invalid key", attestationBLSv1, [][]byte{nil}, 1, digest, proof},
		{"invalid signature", attestationBLSv1, keys, 3, digest, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyAttestationProof(tc.version, tc.keys, tc.quorum, tc.digest, tc.proof) == nil {
				t.Fatal("unexpected acceptance")
			}
		})
	}
}

func BenchmarkAttestationProof(b *testing.B) {
	for _, count := range []int{4, 21, 64} {
		b.Run(fmt.Sprintf("BLSv1/%d", count), func(b *testing.B) {
			keys, digest, proof := sampleAttestation(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := verifyAttestationProof(attestationBLSv1, keys, count, digest, proof); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
