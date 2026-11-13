// Command txtypes sends one EIP-2718 transaction type at a time to a running
// pgp devnet and reports, as JSON, whether it made it on chain and what effect
// it had.
//
// It exists because the five types cannot be exercised uniformly from cast: the
// access-list type cannot be forced, blob needs a v0 sidecar (BSC rejects the
// PeerDAS v1 default), and a setcode transaction needs the authorization tuple
// signed with the account's own key rather than an unlocked node. Doing it here
// also means the expectation is derived from the same genesis chainConfig the
// node reads, instead of being hardcoded per height.
//
// The tool never decides "pass": it prints what happened, plus what the config
// says should have happened, and exits non-zero when they disagree.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

type genesisFile struct {
	Config *params.ChainConfig `json:"config"`
}

type result struct {
	Type        int                    `json:"type"`
	Phase       string                 `json:"phase"`
	Head        uint64                 `json:"head"`
	HeadTs      uint64                 `json:"headTs"`
	ForkHeight  uint64                 `json:"forkHeight"`
	CancunTime  *uint64                `json:"cancunTime"`
	PragueTime  *uint64                `json:"pragueTime"`
	Expect      string                 `json:"expect"`
	ExpectError string                 `json:"expectError,omitempty"`
	From        string                 `json:"from"`
	To          string                 `json:"to"`
	Nonce       uint64                 `json:"nonce"`
	Tip         string                 `json:"tip"`
	Value       string                 `json:"value"`
	Outcome     string                 `json:"outcome"` // accepted | rejected | reverted | pooled-but-unmined
	RawError    string                 `json:"rawError,omitempty"`
	TxHash      string                 `json:"txHash,omitempty"`
	Receipt     map[string]any         `json:"receipt,omitempty"`
	Checks      map[string]any         `json:"checks,omitempty"`
	Pass        bool                   `json:"pass"`
	Notes       []string               `json:"notes,omitempty"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}

func main() {
	genesis := flag.String("genesis", "", "genesis.json（唯一配置来源）")
	rpcURL := flag.String("rpc", "http://127.0.0.1:18560", "节点 RPC")
	keystoreDir := flag.String("keystore", "", "keystore 目录")
	passwordFile := flag.String("password-file", "", "keystore 密码文件")
	fromHex := flag.String("from", "", "发送账户地址（需与 keystore 匹配）")
	ty := flag.Int("type", 0, "交易类型 0..4（5 = 未知类型负向用例）")
	toHex := flag.String("to", "", "接收方地址")
	valueWei := flag.String("value", "1000000000000000", "转账金额（wei）")
	tipWei := flag.String("tip", "1000000000", "gas tip cap（wei），必须 >= 50000000")
	feeCapWei := flag.String("fee-cap", "2000000000", "gas fee cap（wei）")
	blobCount := flag.Int("blob-count", 1, "blob 数量（type 3）")
	delegateHex := flag.String("delegate", "", "setcode 的委托目标（type 4）")
	authNonceOff := flag.Int64("auth-nonce-off", 1, "授权 nonce 相对 tx nonce 的偏移（type 4）")
	emptyAuth := flag.Bool("empty-auth", false, "type 4 生成空授权列表（负向用例）")
	authSkipped := flag.Bool("expect-auth-skipped", false, "type 4 期望授权被静默跳过：代码不变、nonce 只 +1")
	expect := flag.String("expect", "auto", "auto | ok | reject")
	expectErrFlag := flag.String("expect-error", "", "覆盖期望的错误子串（负向用例：类型可用但交易非法时用）")
	timeout := flag.Duration("timeout", 120*time.Second, "等回执的超时")
	out := flag.String("json-out", "", "把结果 JSON 另存一份到该路径")
	flag.Parse()

	if *genesis == "" {
		die("必须给 --genesis")
	}
	if *ty < 0 || *ty > 5 {
		die("--type 只支持 0..4（5 是未知类型负向用例）")
	}

	raw, err := os.ReadFile(*genesis)
	if err != nil {
		die("读 genesis 失败: %v", err)
	}
	var g genesisFile
	if err := json.Unmarshal(raw, &g); err != nil || g.Config == nil {
		die("解析 genesis.config 失败: %v", err)
	}
	cfg := g.Config

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+60*time.Second)
	defer cancel()

	cli, err := ethclient.DialContext(ctx, *rpcURL)
	if err != nil {
		die("连接 %s 失败: %v", *rpcURL, err)
	}
	defer cli.Close()

	head, err := cli.HeaderByNumber(ctx, nil)
	if err != nil {
		die("取 head 失败: %v", err)
	}
	num, ts := head.Number.Uint64(), head.Time

	res := &result{
		Type:       *ty,
		Phase:      phaseOf(cfg, num, ts),
		Head:       num,
		HeadTs:     ts,
		ForkHeight: cfg.Legacy.ForkHeight,
		CancunTime: cfg.CancunTime,
		PragueTime: cfg.PragueTime,
		Tip:        *tipWei,
		Value:      *valueWei,
		From:       strings.ToLower(*fromHex),
		To:         strings.ToLower(*toHex),
	}

	// 期望值由 chainConfig 推导，不按窗口硬编码。
	shouldAccept, expectErr := expectFor(cfg, num, ts, *ty)
	switch *expect {
	case "ok":
		shouldAccept, expectErr = true, ""
	case "reject":
		shouldAccept = false // 保留推导出的错误串，除非下面被覆盖
	}
	if *expectErrFlag != "" {
		expectErr = *expectErrFlag
	}
	if shouldAccept {
		res.Expect = "accepted"
	} else {
		res.Expect = "rejected"
		res.ExpectError = expectErr
	}

	// 未知类型：RPC 的解码层就该拒，不需要 keystore。
	if *ty == 5 {
		// 0x05 是未定义的 EIP-2718 类型；body 用空列表即可。
		_, err := sendRaw(ctx, cli, common.FromHex("0x05c0"))
		res.Outcome = "accepted"
		if err != nil {
			res.Outcome = "rejected"
			res.RawError = err.Error()
		}
		res.Pass = res.Outcome == "rejected" && strings.Contains(res.RawError, "transaction type not supported")
		emit(res, *out)
		return
	}

	key, err := findKey(*keystoreDir, *passwordFile, common.HexToAddress(*fromHex))
	if err != nil {
		die("加载 keystore 失败: %v", err)
	}
	from := key.Address

	nonce, err := cli.PendingNonceAt(ctx, from)
	if err != nil {
		die("取 pending nonce 失败: %v", err)
	}
	res.Nonce = nonce

	to := common.HexToAddress(*toHex)
	value, _ := new(big.Int).SetString(*valueWei, 10)
	tip, _ := new(big.Int).SetString(*tipWei, 10)
	feeCap, _ := new(big.Int).SetString(*feeCapWei, 10)

	var codeBefore []byte
	if shouldAccept && *ty == 4 {
		codeBefore, _ = cli.CodeAt(ctx, from, nil)
	}
	var balFromBefore, balToBefore *big.Int
	if shouldAccept {
		balFromBefore, _ = cli.BalanceAt(ctx, from, nil)
		balToBefore, _ = cli.BalanceAt(ctx, to, nil)
	}

	tx, err := buildTx(cfg, key, *ty, nonce, to, value, tip, feeCap, *blobCount, common.HexToAddress(*delegateHex), *authNonceOff, *emptyAuth)
	if err != nil {
		die("构造交易失败: %v", err)
	}

	// 签名自检：Sender() 必须还原出发送账户，否则说明签名器与类型不匹配。
	signer := types.LatestSigner(cfg)
	if got, err := types.Sender(signer, tx); err == nil && got != from {
		die("签名自检失败: Sender()=%s，期望 %s", got, from)
	}

	if !shouldAccept {
		// 预期被拒：仍然发出去，把池层的错误串原样带回来。
		err := cli.SendTransaction(ctx, tx)
		if err != nil {
			res.Outcome = "rejected"
			res.RawError = err.Error()
			res.Pass = expectErr == "" || strings.Contains(normalizeErr(err.Error()), normalizeErr(expectErr))
			if expectErr != "" && !res.Pass {
				res.Notes = append(res.Notes, "错误串与预期不符")
			}
		} else {
			res.Outcome = "accepted"
			res.TxHash = tx.Hash().Hex()
			res.Notes = append(res.Notes, "预期被拒但发送成功")
			res.Pass = false
		}
		emit(res, *out)
		return
	}

	// 预期成功：发出去 → 等回执 → 校验。
	start := time.Now()
	if err := cli.SendTransaction(ctx, tx); err != nil {
		res.Outcome = "rejected"
		res.RawError = err.Error()
		res.Pass = false
		emit(res, *out)
		return
	}
	res.TxHash = tx.Hash().Hex()

	rcpt, err := waitReceipt(ctx, cli, tx.Hash(), *timeout)
	if err != nil {
		res.Outcome = "pooled-but-unmined"
		res.RawError = err.Error()
		if t, _, qerr := cli.TransactionByHash(ctx, tx.Hash()); qerr == nil && t != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("tx 仍在池中或已上链但回执超时（%s 后）", time.Since(start).Truncate(time.Second)))
		}
		res.Pass = false
		emit(res, *out)
		return
	}

	res.Receipt = receiptMap(rcpt)
	if rcpt.Status != types.ReceiptStatusSuccessful {
		res.Outcome = "reverted"
		res.Pass = false
		emit(res, *out)
		return
	}
	res.Outcome = "accepted"

	checks := map[string]any{}
	pass := rcpt.Status == types.ReceiptStatusSuccessful && int(rcpt.Type) == *ty
	if int(rcpt.Type) != *ty {
		res.Notes = append(res.Notes, fmt.Sprintf("回执 type=%d，期望 %d", rcpt.Type, *ty))
	}

	// 余额闭式：baseFee 恒 0，所以 sender 减少 value + gasUsed*effectiveGasPrice。
	if balFromBefore != nil {
		time.Sleep(200 * time.Millisecond)
		balFromAfter, _ := cli.BalanceAt(ctx, from, nil)
		balToAfter, _ := cli.BalanceAt(ctx, to, nil)
		spent := new(big.Int).Sub(balFromBefore, balFromAfter)
		// 实扣基准不是回执的 effectiveGasPrice：legacy 段的 header 没有 BaseFee，
		// TransactionToMessage 走 "baseFee == nil → gasPrice = GasFeeCap" 分支，
		// 于是 1559 交易按 feeCap 扣费、也按 feeCap 分配，而回执里报的是 tip。
		// 分叉后 header 有 BaseFee(0)，扣费 = min(feeCap, tip) = tip，两者一致。
		charged := rcpt.EffectiveGasPrice
		if blk, herr := cli.HeaderByNumber(ctx, rcpt.BlockNumber); herr == nil && blk.BaseFee == nil {
			charged = tx.GasFeeCap()
			checks["legacyChargedAtFeeCap"] = true
		}
		checks["chargedPrice"] = charged.String()
		checks["receiptEffectiveGasPrice"] = rcpt.EffectiveGasPrice.String()
		wantSpent := new(big.Int).Mul(new(big.Int).SetUint64(rcpt.GasUsed), charged)
		// 用交易自身的 value，而不是 --value：setcode 是自发交易，value 恒为 0。
		wantSpent.Add(wantSpent, tx.Value())
		// blob 交易额外烧 blob 费：blobGasUsed * blobGasPrice，和执行 gas 分开计。
		if rcpt.BlobGasUsed > 0 && rcpt.BlobGasPrice != nil {
			blobFee := new(big.Int).Mul(new(big.Int).SetUint64(rcpt.BlobGasUsed), rcpt.BlobGasPrice)
			wantSpent.Add(wantSpent, blobFee)
			checks["blobFee"] = blobFee.String()
		}
		checks["senderDelta"] = spent.String()
		checks["senderDeltaWant"] = wantSpent.String()
		if spent.Cmp(wantSpent) != 0 {
			res.Notes = append(res.Notes, "sender 余额变化与 value+gasUsed*effectiveGasPrice 不符")
			pass = false
		}
		if tx.Value().Sign() > 0 && to != from {
			if got := new(big.Int).Sub(balToAfter, balToBefore); got.Cmp(tx.Value()) != 0 {
				res.Notes = append(res.Notes, fmt.Sprintf("接收方余额变化 %s，期望 %s", got, tx.Value()))
				pass = false
			} else {
				checks["sinkDelta"] = got.String()
			}
		}
	}

	switch *ty {
	case 3:
		checks["blobGasUsed"] = rcpt.BlobGasUsed
		checks["blobGasPrice"] = rcpt.BlobGasPrice
		checks["blockMod5"] = rcpt.BlockNumber.Uint64() % 5
		if rcpt.BlobGasUsed == 0 {
			res.Notes = append(res.Notes, "receipt.blobGasUsed 为 0，blob 交易可能被当成普通转账处理")
			pass = false
		}
		if rcpt.BlockNumber.Uint64()%5 != 0 {
			res.Notes = append(res.Notes, "blob 交易被打包在 N%5 != 0 的块里")
			pass = false
		}
	case 4:
		code, err := cli.CodeAt(ctx, from, nil)
		if err != nil {
			die("取 code 失败: %v", err)
		}
		if *authSkipped {
			// 负向用例：授权 nonce 写错时 validateAuthorization 失败，错误被
			// state_transition.go:692-696 吞掉，交易仍然 status=1，但代码不变、
			// nonce 只被交易本身推一格。
			checks["codeBefore"] = "0x" + common.Bytes2Hex(codeBefore)
			checks["codeAfter"] = "0x" + common.Bytes2Hex(code)
			if string(code) != string(codeBefore) {
				res.Notes = append(res.Notes, "授权本应被跳过，但代码被改写了")
				pass = false
			}
			after, err := cli.PendingNonceAt(ctx, from)
			if err == nil {
				checks["nonceAfter"] = after
				checks["nonceDelta"] = after - nonce
				if after != nonce+1 {
					res.Notes = append(res.Notes, fmt.Sprintf("授权被跳过时 nonce 应只 +1，实际 %d", after-nonce))
					pass = false
				}
			}
			break
		}
		var want []byte
		if common.HexToAddress(*delegateHex) == (common.Address{}) {
			// EIP-7702：授权到零地址表示清除委托，代码应变为空。
			want = nil
		} else {
			want = delegationCode(common.HexToAddress(*delegateHex))
		}
		checks["code"] = "0x" + common.Bytes2Hex(code)
		checks["codeWant"] = "0x" + common.Bytes2Hex(want)
		if string(code) != string(want) {
			res.Notes = append(res.Notes, "委托代码未按预期写入（授权 nonce 错时会静默跳过）")
			pass = false
		}
		// 自授权会把 nonce 再推一格：tx 自增一次，授权应用再自增一次。
		after, err := cli.PendingNonceAt(ctx, from)
		if err == nil {
			checks["nonceAfter"] = after
			checks["nonceDelta"] = after - nonce
			if after != nonce+2 {
				res.Notes = append(res.Notes, fmt.Sprintf("setcode 后 nonce=%d，期望 %d（自授权 +2）", after, nonce+2))
				pass = false
			}
		}
	}

	res.Checks = checks
	res.Pass = pass
	emit(res, *out)
}

// expectFor is the single source of truth: same predicates the txpool and the
// block-level signer use, evaluated at the current head.
func expectFor(cfg *params.ChainConfig, num, ts uint64, ty int) (bool, string) {
	switch ty {
	case 0:
		return true, ""
	case 1:
		if cfg.IsBerlin(new(big.Int).SetUint64(num)) {
			return true, ""
		}
		return false, "type 1 rejected, pool not yet in Berlin"
	case 2:
		if cfg.IsLondon(new(big.Int).SetUint64(num)) {
			return true, ""
		}
		return false, "type 2 rejected, pool not yet in London"
	case 3:
		if cfg.IsCancun(new(big.Int).SetUint64(num), ts) {
			return true, ""
		}
		return false, "type 3 rejected, pool not yet in Cancun"
	case 4:
		if cfg.IsPrague(new(big.Int).SetUint64(num), ts) {
			return true, ""
		}
		return false, "type 4 rejected, pool not yet in Prague"
	}
	return false, "transaction type not supported"
}

func phaseOf(cfg *params.ChainConfig, num, ts uint64) string {
	if cfg.Legacy == nil || num < cfg.Legacy.ForkHeight {
		return "legacy"
	}
	if cfg.CancunTime != nil && ts < *cfg.CancunTime {
		return "pre-cancun"
	}
	return "post-cancun"
}

func buildTx(cfg *params.ChainConfig, key *keystore.Key, ty int, nonce uint64,
	to common.Address, value, tip, feeCap *big.Int, blobCount int,
	delegate common.Address, authNonceOff int64, emptyAuth bool) (*types.Transaction, error) {

	chainID := cfg.ChainID
	switch ty {
	case 0:
		return types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce: nonce, GasPrice: tip, Gas: 21000, To: &to, Value: value,
		}), types.LatestSigner(cfg), key.PrivateKey)

	case 1:
		return types.SignTx(types.NewTx(&types.AccessListTx{
			ChainID: chainID, Nonce: nonce, GasPrice: tip, Gas: 30000, To: &to, Value: value,
			AccessList: types.AccessList{{
				Address:     to,
				StorageKeys: []common.Hash{common.HexToHash("0x01")},
			}},
		}), types.LatestSigner(cfg), key.PrivateKey)

	case 2:
		return types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
			Gas: 21000, To: &to, Value: value,
		}), types.LatestSigner(cfg), key.PrivateKey)

	case 3:
		blob := kzg4844.Blob{}
		commit, err := kzg4844.BlobToCommitment(&blob)
		if err != nil {
			return nil, err
		}
		proof, err := kzg4844.ComputeBlobProof(&blob, commit)
		if err != nil {
			return nil, err
		}
		// 逐槽填充：全 0 blob 是合法域元素，KZG 能算。
		sc := &types.BlobTxSidecar{Version: types.BlobSidecarVersion0}
		for i := 0; i < blobCount; i++ {
			sc.Blobs = append(sc.Blobs, blob)
			sc.Commitments = append(sc.Commitments, commit)
			sc.Proofs = append(sc.Proofs, proof)
		}
		tx := types.NewTx(&types.BlobTx{
			ChainID:    uint256.MustFromBig(chainID),
			Nonce:      nonce,
			GasTipCap:  uint256.MustFromBig(tip),
			GasFeeCap:  uint256.MustFromBig(feeCap),
			Gas:        21000,
			To:         to,
			Value:      uint256.MustFromBig(value),
			BlobFeeCap: uint256.NewInt(1_000_000_000),
			BlobHashes: sc.BlobHashes(),
		})
		signed, err := types.SignTx(tx, types.LatestSigner(cfg), key.PrivateKey)
		if err != nil {
			return nil, err
		}
		return signed.WithBlobTxSidecar(sc), nil

	case 4:
		auth := types.SetCodeAuthorization{
			ChainID: *uint256.MustFromBig(chainID),
			Address: delegate,
			Nonce:   uint64(int64(nonce) + authNonceOff),
		}
		signedAuth, err := types.SignSetCode(key.PrivateKey, auth)
		if err != nil {
			return nil, err
		}
		list := []types.SetCodeAuthorization{signedAuth}
		if emptyAuth {
			list = nil
		}
		return types.SignTx(types.NewTx(&types.SetCodeTx{
			ChainID:   uint256.MustFromBig(chainID),
			Nonce:     nonce,
			GasTipCap: uint256.MustFromBig(tip),
			GasFeeCap: uint256.MustFromBig(feeCap),
			Gas:       120000,
			To:        key.Address,
			Value:     uint256.NewInt(0),
			AuthList:  list,
		}), types.LatestSigner(cfg), key.PrivateKey)
	}
	return nil, fmt.Errorf("unsupported type %d", ty)
}

func delegationCode(addr common.Address) []byte {
	return append([]byte{0xef, 0x01, 0x00}, addr.Bytes()...)
}

func sendRaw(ctx context.Context, cli *ethclient.Client, raw []byte) (common.Hash, error) {
	var hash common.Hash
	err := cli.Client().CallContext(ctx, &hash, "eth_sendRawTransaction", hexutil.Encode(raw))
	return hash, err
}

func waitReceipt(ctx context.Context, cli *ethclient.Client, hash common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := cli.TransactionReceipt(ctx, hash)
		if err == nil && r != nil {
			return r, nil
		}
		if err != nil && !errors.Is(err, ethereum.NotFound) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, fmt.Errorf("等待回执超时（%s）", timeout)
}

func receiptMap(r *types.Receipt) map[string]any {
	m := map[string]any{
		"status":            r.Status,
		"type":              r.Type,
		"blockNumber":       r.BlockNumber.Uint64(),
		"gasUsed":           r.GasUsed,
		"cumulativeGasUsed": r.CumulativeGasUsed,
		"effectiveGasPrice": r.EffectiveGasPrice.String(),
	}
	if r.BlobGasUsed > 0 {
		m["blobGasUsed"] = r.BlobGasUsed
		m["blobGasPrice"] = r.BlobGasPrice.String()
	}
	return m
}

// normalizeErr strips the Go client's wrapping so a substring check is stable
// across the RPC layer and the pool's own wording.
func normalizeErr(s string) string {
	s = strings.TrimPrefix(s, "rpc error: code = Unknown desc = ")
	return strings.ToLower(s)
}

func emit(res *result, out string) {
	b, _ := json.Marshal(res)
	fmt.Println(string(b))
	if out != "" {
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		_ = os.WriteFile(out, b, 0o644)
	}
	if !res.Pass {
		os.Exit(1)
	}
}

func findKey(dir, passwordFile string, want common.Address) (*keystore.Key, error) {
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
		blob, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		key, err := keystore.DecryptKey(blob, password)
		if err != nil {
			continue
		}
		if key.Address == want {
			return key, nil
		}
	}
	return nil, fmt.Errorf("没有找到 %s 对应的 keystore（目录 %s）", want.Hex(), dir)
}
