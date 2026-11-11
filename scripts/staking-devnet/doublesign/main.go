// Command doublesign builds a pair of conflicting headers for
// SlashIndicator.submitDoubleSignEvidence.
//
// Why not run two nodes with the same key: the evidence only needs two headers
// with the same number and the same parent hash, both sealed by the same key.
// Cloning a real header and re-signing it with a different GasLimit produces
// exactly that, deterministically, without partitioning a network.
//
// The seal is types.SealHash(header, chainId) -- NOT parlia.Parlia.SealHash,
// which strips the vote attestation and is used for a different purpose. The
// precompile (0x68) recovers the signer from types.SealHash and compares the
// two recovered keys, so signing anything else produces "validator not
// migrated" at the contract, which looks like a registration bug.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
)

const extraSeal = 65

// mirror of core/vm.DoubleSignEvidence: [chainId, header1, header2]
type evidence struct {
	ChainID      *big.Int
	HeaderBytes1 []byte
	HeaderBytes2 []byte
}

type output struct {
	Height          uint64 `json:"height"`
	Signer          string `json:"signer"`
	Header1         string `json:"header1"`
	Header2         string `json:"header2"`
	PrecompileInput string `json:"precompileInput"`
	SealHash1       string `json:"sealHash1"`
	SealHash2       string `json:"sealHash2"`
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		die("用法: doublesign <emit|verify> [flags]")
	}
	switch os.Args[1] {
	case "emit":
		emit(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	default:
		die("未知子命令 %q", os.Args[1])
	}
}

func emit(args []string) {
	fs := flag.NewFlagSet("emit", flag.ExitOnError)
	rpcURL := fs.String("rpc", "http://127.0.0.1:18560", "JSON-RPC endpoint")
	height := fs.Uint64("height", 0, "block height whose producer signs both headers")
	keystoreDir := fs.String("keystore", "", "geth keystore directory holding the validator key")
	passwordFile := fs.String("password-file", "", "file with the keystore password")
	chainID := fs.Int64("chain-id", 860621, "chain id (must match the system contracts' bscChainID)")
	outPath := fs.String("out", "evidence.json", "where to write the evidence JSON")
	if err := fs.Parse(args); err != nil {
		die("%v", err)
	}
	if *keystoreDir == "" || *passwordFile == "" {
		die("--keystore 与 --password-file 必填")
	}

	client, err := ethclient.Dial(*rpcURL)
	if err != nil {
		die("连接 %s 失败: %v", *rpcURL, err)
	}
	defer client.Close()

	h1, err := client.HeaderByNumber(context.Background(), new(big.Int).SetUint64(*height))
	if err != nil {
		die("取区块 %d 失败: %v", *height, err)
	}
	if len(h1.Extra) < extraSeal {
		die("区块 %d 的 extra 只有 %d 字节，放不下 65 字节 seal", *height, len(h1.Extra))
	}

	key, err := findKey(*keystoreDir, *passwordFile, h1.Coinbase)
	if err != nil {
		die("%v", err)
	}

	cid := big.NewInt(*chainID)
	// Gate 1: the header really was sealed by this key. If this fails, the RLP
	// round-trip changed a field and any evidence we build would be rejected --
	// better to stop than to submit a dud.
	if signer := recoverSigner(h1, cid); signer != h1.Coinbase {
		die("区块 %d 的 coinbase=%s，但 seal 恢复出 %s：RLP 往返不等价，停止",
			*height, h1.Coinbase.Hex(), signer.Hex())
	}

	h2 := *h1
	h2.Extra = append([]byte(nil), h1.Extra...)
	// Only GasLimit changes. It is part of SealHash, so the two headers hash
	// differently and need different signatures; everything the precompile
	// compares (number, parent hash) stays identical.
	h2.GasLimit = h1.GasLimit + 1

	sig, err := crypto.Sign(types.SealHash(&h2, cid).Bytes(), key.PrivateKey)
	if err != nil {
		die("重签失败: %v", err)
	}
	copy(h2.Extra[len(h2.Extra)-extraSeal:], sig)

	if s1, s2 := recoverSigner(h1, cid), recoverSigner(&h2, cid); s1 != s2 || s1 != h1.Coinbase {
		die("两个 header 的 signer 不一致：%s vs %s", s1.Hex(), s2.Hex())
	}

	b1, err := rlp.EncodeToBytes(h1)
	if err != nil {
		die("编码 header1 失败: %v", err)
	}
	b2, err := rlp.EncodeToBytes(&h2)
	if err != nil {
		die("编码 header2 失败: %v", err)
	}
	ev := evidence{ChainID: cid, HeaderBytes1: b1, HeaderBytes2: b2}
	input, err := rlp.EncodeToBytes(&ev)
	if err != nil {
		die("编码证据失败: %v", err)
	}

	// Gate 2: mirror the precompile's five checks locally.
	if err := checkEvidence(&ev); err != nil {
		die("本地预检失败: %v", err)
	}

	res := output{
		Height:          *height,
		Signer:          h1.Coinbase.Hex(),
		Header1:         "0x" + hex.EncodeToString(b1),
		Header2:         "0x" + hex.EncodeToString(b2),
		PrecompileInput: "0x" + hex.EncodeToString(input),
		SealHash1:       types.SealHash(h1, cid).Hex(),
		SealHash2:       types.SealHash(&h2, cid).Hex(),
	}
	raw, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(*outPath, append(raw, '\n'), 0o600); err != nil {
		die("写 %s 失败: %v", *outPath, err)
	}
	fmt.Printf("height=%d signer=%s\nheader1=%dB header2=%dB 证据=%dB\n写入了 %s\n",
		res.Height, res.Signer, len(b1), len(b2), len(input), *outPath)
}

func verify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	path := fs.String("evidence", "evidence.json", "evidence JSON produced by emit")
	chainID := fs.Int64("chain-id", 860621, "chain id")
	if err := fs.Parse(args); err != nil {
		die("%v", err)
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		die("读 %s 失败: %v", *path, err)
	}
	var doc output
	if err := json.Unmarshal(raw, &doc); err != nil {
		die("解析 %s 失败: %v", *path, err)
	}
	h1b, err := hex.DecodeString(strings.TrimPrefix(doc.Header1, "0x"))
	if err != nil {
		die("header1 不是 hex: %v", err)
	}
	h2b, err := hex.DecodeString(strings.TrimPrefix(doc.Header2, "0x"))
	if err != nil {
		die("header2 不是 hex: %v", err)
	}
	ev := evidence{ChainID: big.NewInt(*chainID), HeaderBytes1: h1b, HeaderBytes2: h2b}
	if err := checkEvidence(&ev); err != nil {
		die("证据不合法: %v", err)
	}
	fmt.Printf("OK signer=%s number=%s\n", doc.Signer, new(big.Int).SetBytes(decodeNumber(h1b)))
}

// decodeNumber is only used for the human-readable verify output; the real
// checks live in checkEvidence.
func decodeNumber(b []byte) []byte {
	var h types.Header
	if err := rlp.DecodeBytes(b, &h); err != nil {
		return nil
	}
	return h.Number.Bytes()
}

func checkEvidence(ev *evidence) error {
	var h1, h2 types.Header
	if err := rlp.DecodeBytes(ev.HeaderBytes1, &h1); err != nil {
		return fmt.Errorf("header1: %w", err)
	}
	if err := rlp.DecodeBytes(ev.HeaderBytes2, &h2); err != nil {
		return fmt.Errorf("header2: %w", err)
	}
	if h1.Number.Cmp(h2.Number) != 0 {
		return fmt.Errorf("number 不同")
	}
	if h1.ParentHash != h2.ParentHash {
		return fmt.Errorf("parentHash 不同")
	}
	if len(h1.Extra) < extraSeal || len(h2.Extra) < extraSeal {
		return fmt.Errorf("extra 太短")
	}
	s1 := h1.Extra[len(h1.Extra)-extraSeal:]
	s2 := h2.Extra[len(h2.Extra)-extraSeal:]
	if string(s1) == string(s2) {
		return fmt.Errorf("两个 seal 相同")
	}
	if types.SealHash(&h1, ev.ChainID) == types.SealHash(&h2, ev.ChainID) {
		return fmt.Errorf("两个 SealHash 相同")
	}
	if recoverSigner(&h1, ev.ChainID) != recoverSigner(&h2, ev.ChainID) {
		return fmt.Errorf("两个 header 的 signer 不同")
	}
	return nil
}

func recoverSigner(h *types.Header, chainID *big.Int) common.Address {
	sig := h.Extra[len(h.Extra)-extraSeal:]
	pub, err := crypto.SigToPub(types.SealHash(h, chainID).Bytes(), sig)
	if err != nil {
		return common.Address{}
	}
	return crypto.PubkeyToAddress(*pub)
}

func findKey(dir, passwordFile string, coinbase common.Address) (*keystore.Key, error) {
	pw, err := os.ReadFile(passwordFile)
	if err != nil {
		return nil, fmt.Errorf("读密码文件失败: %w", err)
	}
	password := strings.TrimRight(string(pw), "\r\n")
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("在 %s 下没有找到 keystore", dir)
	}
	for _, f := range files {
		if st, err := os.Stat(f); err != nil || st.IsDir() {
			continue
		}
		key, err := keystore.DecryptKey(mustRead(f), password)
		if err != nil {
			continue
		}
		if key.Address == coinbase {
			return key, nil
		}
	}
	return nil, fmt.Errorf("没有找到 coinbase=%s 对应的 keystore（目录 %s）", coinbase.Hex(), dir)
}

func mustRead(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		die("读 %s 失败: %v", path, err)
	}
	return b
}
