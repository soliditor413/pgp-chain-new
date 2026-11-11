// Command fork-probe prints the fork schedule a pgp chain will actually run
// with, after Legacy.ForkHeight has been folded in.
//
// It exists because the schedule is spread over ~30 fields of ChainConfig and
// the interesting failures are all silent: a leftover MaxInt64 sentinel keeps a
// fork off forever, a nil field is read as "not configured" rather than
// "derive it", and a time fork pinned before the fork block rewrites headers
// that legacy producers already signed. Printing the resolved table before
// starting a chain turns all three into something you can read.
//
//	go run ./scripts/fork-probe                      # mainnet preset as shipped
//	go run ./scripts/fork-probe --fork-height 200    # what a pinned height resolves to
//	go run ./scripts/fork-probe --genesis g.json     # a private chain's genesis
//	go run ./scripts/fork-probe --genesis g.json --at 200   # truth values at a block
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/params"
)

type genesisFile struct {
	Config *params.ChainConfig `json:"config"`
}

type row struct {
	name  string
	kind  string // "block" or "time"
	block uint64 // for block forks
	time  uint64 // for time forks
	set   bool
}

func collect(c *params.ChainConfig) []row {
	var out []row
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fv := v.Field(i)
		switch {
		case strings.HasSuffix(f.Name, "Block") && fv.Type() == reflect.TypeOf((*big.Int)(nil)):
			if fv.IsNil() {
				out = append(out, row{name: f.Name, kind: "block"})
				continue
			}
			b := fv.Interface().(*big.Int)
			out = append(out, row{name: f.Name, kind: "block", block: b.Uint64(), set: true})
		case strings.HasSuffix(f.Name, "Time") && fv.Type() == reflect.TypeOf((*uint64)(nil)):
			if fv.IsNil() {
				out = append(out, row{name: f.Name, kind: "time"})
				continue
			}
			u := *fv.Interface().(*uint64)
			out = append(out, row{name: f.Name, kind: "time", time: u, set: true})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind == "block"
		}
		return out[i].name < out[j].name
	})
	return out
}

func describe(r row, forkHeight uint64) string {
	sentinel := uint64(math.MaxInt64)
	switch {
	case !r.set:
		return "nil              (未配置：不会被推导填上，若本链有 ForkHeight 请改为显式值)"
	case r.kind == "block":
		tag := ""
		switch {
		case r.block >= sentinel:
			tag = "  ← 哨兵(=永不激活)"
		case forkHeight != 0 && forkHeight != math.MaxUint64 && r.block == forkHeight:
			tag = "  ← == ForkHeight"
		case forkHeight != 0 && r.block < forkHeight:
			tag = "  ← 早于 ForkHeight（legacy 段就会生效，小心）"
		}
		return fmt.Sprintf("%-12d%s", r.block, tag)
	default:
		tag := ""
		switch {
		case r.time >= sentinel:
			tag = "  ← 哨兵(=永不激活)"
		case r.time == 0:
			tag = "  ← 创世即激活"
		}
		return fmt.Sprintf("%-12d%s", r.time, tag)
	}
}

// activeAt answers "does this fork apply at block N / at the timestamp that
// block N would carry", which is the question the chain asks itself per block.
func activeAt(r row, block uint64, ts uint64) string {
	if !r.set {
		return "-"
	}
	if r.kind == "block" {
		if r.block >= math.MaxInt64 {
			return "否"
		}
		if block >= r.block {
			return "是"
		}
		return "否"
	}
	if r.time >= math.MaxInt64 {
		return "否"
	}
	if ts >= r.time {
		return "是"
	}
	return "否"
}

func main() {
	genesisPath := flag.String("genesis", "", "从 genesis.json 读 config（缺省用 params.PGPChainConfig）")
	forkHeight := flag.Uint64("fork-height", 0, "覆盖 Legacy.ForkHeight（0 = 不覆盖）")
	at := flag.Int64("at", 0, "额外打印 F-1/F/F+1 三行时是否需要真值表；给出块高则打印该块的激活情况")
	blockSeconds := flag.Uint64("block-seconds", 3, "块间隔，用于把块高换算成时间戳，仅供 --at 参考")
	genesisTs := flag.Uint64("genesis-ts", 0, "创世时间戳，配合 --at 估算该块时间（0 = 不估算）")
	flag.Parse()

	var cfg *params.ChainConfig
	src := "params.PGPChainConfig"
	if *genesisPath != "" {
		raw, err := os.ReadFile(*genesisPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读 genesis 失败: %v\n", err)
			os.Exit(1)
		}
		var g genesisFile
		if err := json.Unmarshal(raw, &g); err != nil {
			fmt.Fprintf(os.Stderr, "解析 genesis 失败: %v\n", err)
			os.Exit(1)
		}
		if g.Config == nil {
			fmt.Fprintln(os.Stderr, "genesis 里没有 config 字段")
			os.Exit(1)
		}
		cfg = g.Config
		src = *genesisPath
	} else {
		// 复制一份，避免改到全局预设
		b, _ := json.Marshal(params.PGPChainConfig)
		cfg = new(params.ChainConfig)
		if err := json.Unmarshal(b, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "复制预设失败: %v\n", err)
			os.Exit(1)
		}
	}

	if *forkHeight != 0 {
		if cfg.Legacy == nil {
			fmt.Fprintln(os.Stderr, "该配置没有 legacy 段，--fork-height 无意义")
			os.Exit(1)
		}
		cfg.Legacy.ForkHeight = *forkHeight
	}

	fh := uint64(0)
	deploy := uint64(0)
	if cfg.Legacy != nil {
		fh = cfg.Legacy.ForkHeight
		deploy = cfg.Legacy.SystemContractDeployHeight
	}

	// 与节点启动时同样的推导：只填 nil，不动哨兵与显式值。
	before := collect(cfg)
	cfg.ApplyPGPForkHeight()
	after := collect(cfg)
	filled := 0
	for i := range before {
		if !before[i].set && after[i].set {
			filled++
		}
	}

	fmt.Printf("配置来源 : %s\n", src)
	if fh == math.MaxUint64 {
		fmt.Printf("ForkHeight: %d (= MaxUint64，永不切 Parlia)\n", fh)
	} else {
		fmt.Printf("ForkHeight: %d\n", fh)
	}
	fmt.Printf("部署高度   : %d", deploy)
	switch {
	case deploy == 0:
		fmt.Println("  (0 = 不注入系统合约；有 ForkHeight 时这是非法组合)")
	case fh != math.MaxUint64 && deploy > fh-2:
		fmt.Printf("  ← 非法：必须 <= ForkHeight-2 (%d)\n", fh-2)
	default:
		fmt.Println()
	}
	fmt.Printf("推导填充   : %d 个 nil 字段被填成 ForkHeight\n", filled)
	if cfg.Legacy != nil {
		fmt.Printf("Parlia 验证人 %d 席 / cabinets %d\n",
			len(cfg.Legacy.ParliaGenesisValidators), cfg.Legacy.ParliaGenesisCabinets)
	}
	fmt.Println()

	fmt.Printf("%-22s %-6s %s\n", "字段", "类型", "激活点")
	fmt.Println(strings.Repeat("-", 86))
	for _, r := range after {
		fmt.Printf("%-22s %-6s %s\n", r.name, r.kind, describe(r, fh))
	}

	if *at > 0 {
		block := uint64(*at)
		var ts uint64
		if *genesisTs != 0 {
			ts = *genesisTs + *blockSeconds*block
		}
		fmt.Printf("\n在块 %d（假设 ts≈%d）上的激活真值：\n", block, ts)
		fmt.Printf("%-22s %-6s %s\n", "字段", "类型", "是否生效")
		fmt.Println(strings.Repeat("-", 86))
		for _, r := range after {
			fmt.Printf("%-22s %-6s %s\n", r.name, r.kind, activeAt(r, block, ts))
		}
	}
}
