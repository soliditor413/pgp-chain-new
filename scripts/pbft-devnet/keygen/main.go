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

// Command keygen creates the Elastos-format arbiter keystores a PBFT devnet
// needs, and prints the compressed P-256 public keys the genesis file has to
// list in legacy.producers.
//
// Run it with `go run ./scripts/pbft-devnet/keygen`, never as part of geth: it
// is a one-off provisioning tool, not something a node should link.
//
// WHY THIS IS NOT `geth account new`
//
// The producer key of a pgp-chain block is an Elastos P-256 key, not an EVM
// secp256k1 key. dpos.GetDposAccount opens the file with
// github.com/elastos/Elastos.ELA/account.Open, which reads the Elastos wallet
// format (a JSON with an AES-encrypted master key, an IV and per-account
// encrypted 96 byte blobs). A file written by accounts/keystore -- scrypt + mac
// -- parses as garbage and account.Open returns "password wrong" or a JSON
// unmarshal error. This tool therefore writes the Elastos format, and it
// re-opens every wallet through the exact production code path
// (dpos.GetDposAccount) before reporting success.
//
// The password is never written anywhere. It comes from -password or from the
// PGP_DEVNET_PASSWORD / PGP_DEVNET_PASSWORD_FILE environment variables.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	elaaccount "github.com/elastos/Elastos.ELA/account"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/dpos"
)

// keystoreFile is the name pattern of one arbiter wallet inside the output
// directory. One file per arbiter, not one file with N accounts: each node is
// handed exactly its own key, so a compromised devnet node cannot sign for the
// others.
const keystoreFile = "arbiter-%d.dat"

// accountInfo is one line of the generated manifest.
type accountInfo struct {
	Index int `json:"index"`
	// Keystore is the absolute path of the Elastos wallet to hand to
	// --pbft.keystore.
	Keystore string `json:"keystore"`
	// PublicKey is the 33 byte compressed P-256 key, hex, no 0x - the same
	// spelling params.DefaultLegacyProducers uses.
	PublicKey string `json:"publicKey"`
	// MinerAddress is the 20 byte EVM coinbase this tool derives for the
	// arbiter, to be passed as --pbft.miner.address. It is a devnet
	// convention (see deriveMinerAddress), not a pgp mainnet one.
	MinerAddress string `json:"minerAddress"`
	// ELAAddress is the Elastos base58 address of the same key, printed for
	// orientation only. Nothing in this tree reads it.
	ELAAddress string `json:"elaAddress"`
}

// manifest is written next to the keystores and consumed by run.sh.
type manifest struct {
	// Producers is the sorted list of compressed public keys, i.e. exactly
	// what goes into genesis config.legacy.producers. Sorted because
	// dpos.NewProducers sorts the set before assigning duty turns, so this
	// order is the on-duty order: Producers[0] proposes block 1.
	Producers []string      `json:"producers"`
	Accounts  []accountInfo `json:"accounts"`
}

func main() {
	var (
		out      = flag.String("out", "keys", "directory the arbiter keystores are written to")
		n        = flag.Int("n", 4, "number of arbiters to create")
		password = flag.String("password", "", "keystore password (prefer PGP_DEVNET_PASSWORD or PGP_DEVNET_PASSWORD_FILE)")
		force    = flag.Bool("force", false, "overwrite an existing keystore in -out")
		verify   = flag.Bool("verify", false, "do not create anything; re-open the keystores in -out and check them against producers.json")
	)
	flag.Parse()

	if *n < 1 {
		fatal("-n must be at least 1")
	}
	pass, err := resolvePassword(*password)
	if err != nil {
		fatal("%v", err)
	}
	if len(pass) == 0 {
		fatal("empty password is refused: the Elastos wallet would be trivially decryptable")
	}

	outDir, err := filepath.Abs(*out)
	if err != nil {
		fatal("cannot resolve %q: %v", *out, err)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		fatal("cannot create %s: %v", outDir, err)
	}

	if *verify {
		verifyKeystores(outDir, pass)
		return
	}

	m := manifest{Producers: make([]string, 0, *n), Accounts: make([]accountInfo, 0, *n)}
	for i := 0; i < *n; i++ {
		info, err := createArbiter(outDir, i, pass, *force)
		if err != nil {
			fatal("arbiter %d: %v", i, err)
		}
		m.Accounts = append(m.Accounts, *info)
		m.Producers = append(m.Producers, info.PublicKey)
	}
	// Sort both slices by public key so the manifest's first entry is the
	// producer of block 1 (duty index 0 of the sorted set).
	sort.Slice(m.Producers, func(i, j int) bool { return m.Producers[i] < m.Producers[j] })
	sort.Slice(m.Accounts, func(i, j int) bool { return m.Accounts[i].PublicKey < m.Accounts[j].PublicKey })

	blob, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fatal("cannot marshal manifest: %v", err)
	}
	path := filepath.Join(outDir, "producers.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		fatal("cannot write %s: %v", path, err)
	}

	// The manifest is what run.sh consumes; the human readable summary goes to
	// stderr so `... | jq` on stdout stays usable.
	fmt.Printf("%s\n", blob)
	fmt.Fprintf(os.Stderr, "\ncreated %d arbiter keystores in %s\n", *n, outDir)
	fmt.Fprintf(os.Stderr, "manifest: %s\n", path)
	fmt.Fprintf(os.Stderr, "producers for genesis config.legacy.producers:\n")
	for _, k := range m.Producers {
		fmt.Fprintf(os.Stderr, "  \"%s\",\n", k)
	}
}

// createArbiter writes one wallet and proves it reads back.
//
// The read-back is the whole point of this function: a wallet that only looks
// right is worthless, because the failure surfaces at node start as
// "PBFT: cannot open arbiter keystore, node will only verify blocks" and the
// chain then simply never produces.
func createArbiter(dir string, index int, password []byte, force bool) (*accountInfo, error) {
	path := filepath.Join(dir, fmt.Sprintf(keystoreFile, index))

	// account.Create builds the database through FileStore.BuildDatabase, which
	// calls os.Exit(1) when the target already exists. Checking first turns that
	// process death into a message the operator can act on.
	if _, err := os.Stat(path); err == nil {
		if !force {
			return nil, fmt.Errorf("%s already exists (use -force to replace it)", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("cannot remove existing %s: %v", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	client, err := elaaccount.Create(path, password)
	if err != nil {
		return nil, fmt.Errorf("Elastos account.Create: %w", err)
	}
	if client == nil {
		// account.Create prints its own reason and returns nil.
		return nil, errors.New("Elastos account.Create returned no client")
	}
	ac := client.GetMainAccount()
	if ac == nil || ac.PublicKey == nil {
		return nil, errors.New("wallet has no main account with a public key")
	}
	pub, err := ac.PublicKey.EncodePoint(true)
	if err != nil {
		return nil, fmt.Errorf("cannot encode public key: %w", err)
	}
	if len(pub) != 33 {
		return nil, fmt.Errorf("compressed public key is %d bytes, want 33", len(pub))
	}

	// Tighten the mode even though FileStore writes 0600: the directory mode is
	// what stops another local user from swapping the file, and a keystore is a
	// private key.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("cannot chmod %s: %v", path, err)
	}

	// Verify through the production open path, not through account.Open alone:
	// this is the exact function consensus/pbft calls when it starts a producer.
	reopened, err := dpos.GetDposAccount(path, password)
	if err != nil {
		return nil, fmt.Errorf("read-back through dpos.GetDposAccount failed: %w", err)
	}
	if got := reopened.PublicKeyBytes(); !strings.EqualFold(hex.EncodeToString(got), hex.EncodeToString(pub)) {
		return nil, fmt.Errorf("read-back public key mismatch: wrote %x, read %x", pub, got)
	}

	return &accountInfo{
		Index:        index,
		Keystore:     path,
		PublicKey:    hex.EncodeToString(pub),
		MinerAddress: deriveMinerAddress(pub).Hex(),
		ELAAddress:   ac.Address,
	}, nil
}

// deriveMinerAddress produces the EVM coinbase a devnet arbiter seals with.
//
// It exists because header.Coinbase is part of the PBFT seal hash, so a producer
// has to be told an address, and -pbft.miner.address is the only way to do that
// without also entering the geth keystore. It is a one-way hash of the P-256
// public key, so every arbiter gets a distinct, stable devnet coinbase.
//
// It is NOT the pgp-chain mainnet derivation: on mainnet the coinbase is an EVM
// address the operator chose, and nothing derives it from the arbiter key. The
// consensus rules do not compare the two - verifyHeader never looks at Coinbase -
// so any 20 bytes would do; deriving them keeps the devnet block headers distinct
// per producer, which makes the block explorer output readable.
func deriveMinerAddress(compressedPub []byte) common.Address {
	sum := sha256.Sum256(compressedPub)
	return common.BytesToAddress(sum[12:])
}

// verifyKeystores re-opens every wallet listed in producers.json through
// dpos.GetDposAccount and compares the public key it yields with the one the
// manifest published.
//
// It answers the only question that matters before starting a producer: will
// consensus/pbft be able to open this key? The answer has to come from the
// production open path, because account.Open succeeding on a file that geth's
// own keystore code rejects (and vice versa) is exactly the confusion this tool
// exists to remove.
func verifyKeystores(dir string, password []byte) {
	path := filepath.Join(dir, "producers.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("cannot read %s: %v", path, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		fatal("cannot parse %s: %v", path, err)
	}
	if len(m.Accounts) == 0 {
		fatal("%s lists no accounts", path)
	}
	failed := 0
	for _, a := range m.Accounts {
		account, err := dpos.GetDposAccount(a.Keystore, password)
		if err != nil {
			fmt.Printf("FAIL %s: %v\n", a.Keystore, err)
			failed++
			continue
		}
		if got := hex.EncodeToString(account.PublicKeyBytes()); got != a.PublicKey {
			fmt.Printf("FAIL %s: public key %s, manifest says %s\n", a.Keystore, got, a.PublicKey)
			failed++
			continue
		}
		fmt.Printf("OK   %s %s\n", a.PublicKey, a.Keystore)
	}
	if failed > 0 {
		fatal("%d of %d keystores did not open", failed, len(m.Accounts))
	}
	fmt.Fprintf(os.Stderr, "all %d keystores open and match producers.json\n", len(m.Accounts))
}

// resolvePassword follows one rule: the password may come from the command line
// or the environment, and it is never written to a file by this tool.
func resolvePassword(flagValue string) ([]byte, error) {
	if flagValue != "" {
		return []byte(flagValue), nil
	}
	if file := os.Getenv("PGP_DEVNET_PASSWORD_FILE"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("cannot read PGP_DEVNET_PASSWORD_FILE %s: %v", file, err)
		}
		// A trailing newline is what every editor adds; strip it so a password
		// file and the same password typed at a prompt agree.
		return []byte(strings.TrimRight(string(raw), "\r\n")), nil
	}
	if env := os.Getenv("PGP_DEVNET_PASSWORD"); env != "" {
		return []byte(env), nil
	}
	return nil, errors.New("no password: set PGP_DEVNET_PASSWORD, PGP_DEVNET_PASSWORD_FILE, or pass -password")
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "keygen: "+format+"\n", args...)
	os.Exit(1)
}
