# PGP 私有链分叉激活测试方案（模拟主网 PBFT→BSC 共识升级）

适用场景：`PGPChainConfig` 里 BSC 侧的块高/时间戳分叉字段已经排好，但 `Legacy.ForkHeight` 尚未确定，
需要在本地私链上把"分叉触发 → 各分叉功能生效"整条链路跑通。

本文所有结论都对着代码与实测，标注了 **[已验证]** / **[待执行]**，不把没跑过的写成跑过的。

---

## 0. 一句话

`ForkHeight` 是唯一的触发开关：它决定"第几个块开始由 Parlia 出块"，同时（在修好一处代码后）
自动把 13 个 BSC 块分叉钉到同一个高度。**只要 ForkHeight 一变，块分叉全部跟着走；时间分叉不会跟着走，
必须单独决定放哪里。** 所以测试方案的核心不是"怎么改很多字段"，而是"改对一个字段 + 确认它确实扩散到了那 13 个字段"。

---

## 1. 前提条件

| # | 前提 | 为什么 | 校验方式 |
|---|---|---|---|
| P1 | 二进制含 `pgp-fork-v5` 镜像（`imageHash = 0x7a98039c…`） | v4 有 `SystemReward.doInit` 缺 operator 的缺陷（双签奖励必然回滚）与 `BSCValidatorSet` 空集合越界（首次 breathe 停链） | `go test ./core/systemcontracts/pgp/ -run TestImage` |
| P2 | 链 ID 用 **860621** 且 `legacy.allowForeignGenesis = true` | 系统合约里 `bscChainID = 860621` 是编译期常量，D 块注入会断言它等于 chainId；用别的 chainId 则 D 块直接被拒，且双签取证的签名域永远错（F-01） | 起链日志里 D 块不报 `bscChainID` 不匹配 |
| P3 | `2 <= SystemContractDeployHeight <= ForkHeight-2` | D 块是系统合约**唯一**的初始化路径（block-1 初始化已删除，F-18）；F 块要读 0x1000 的验证人集合，留一个块给"注入失败后重试" | `fork-probe` 会直接标出非法组合 |
| P4 | `ForkHeight >= 4` | 由 P3 推出：D≥2 且 D≤F-2 | `fork-probe` |
| P5 | `parliaGenesisValidators` 非空、顺序固定、`cabinets <= 席数` | 初始集合按 slice 顺序 RLP 编码进 `BSCValidatorSet.init`，换顺序 = 换状态根；空集合是硬启动失败 | 起链后 `getMiningValidators()` 与 genesis 顺序一致 |
| P6 | 时间分叉要么 `= 0`（创世即激活）、要么**严格晚于 F 块的出块时间** | 落在这两者之间会让 legacy 段的块套用 post-fork 的 header 规则（Cancun 会往每个 header 写 `BlobGasUsed/ExcessBlobGas/WithdrawalsHash`），历史块字节被改写 | `fork-probe --at F-1` |
| P7 | `legacy.producers` 与 `verifySeal` 配对 | 私链的 12 个主网 arbiter 公钥对不上自己的 PBFT 生产者时，legacy 段每块验签失败、同步直接停。私链要么把 producer 换成自己的 arbiter，要么显式关掉 verifySeal（仅调试） | legacy 段能连续出块 |
| P8 | `ApplyPGPForkHeight` 在**空库 init** 路径上真的被调用 | 见 §2.0：修之前"只写 ForkHeight、块分叉留 nil"会在 `geth init` 阶段被拒，配置文档里的写法不成立 | `go test ./core/ -run TestForkHeight` |

### 1.1 关于 P8（本轮发现并修复的阻塞项）

`params/pgp_config.go` 的注释写明：钉住 `ForkHeight` 后要把 13 个 BSC 块分叉**改回 nil**，
由 `ApplyPGPForkHeight` 填进去。但实测这条不成立：

```text
$ geth init genesis.json      # 空 datadir
Fatal: unsupported fork ordering: hertzfixBlock not enabled, but keplerTime enabled at timestamp 0
```

原因是 `ApplyPGPForkHeight` 只在 `core/genesis.go` 的 `chainConfigOrDefault` 里被调用，
而那只有一个入口：**数据库里已经有创世块**（兼容性复核）。空库 init 走的是
`SetupGenesisBlockWithOverride` → `Genesis.Commit` → `CheckConfigForkOrder`，这条路上没人填，
于是 nil 被 `CheckConfigForkOrder` 读成"未配置"，与 `keplerTime = 0` 的顺序冲突。

修复（本轮已提交）：在 `Genesis.Commit` 里、`CheckConfigForkOrder` 之前调用 `ApplyPGPForkHeight()`。
幂等、只填 nil、不动哨兵与显式值，因此主网当前配置（全哨兵）行为不变。
回归测试：`core/genesis_pgp_forkfill_test.go`（两条：nil 会被填；哨兵与显式值不被覆盖）。

**结论：修好之后"只写 ForkHeight"才是可行的配置形态。** 如果暂时不想改代码，则必须显式写全 13 个块分叉 = F（见 §2.2 备选）。

---

## 2. 配置修改点

### 2.0 先看当前状态（离线，一条命令）

```bash
go run ./scripts/fork-probe                        # 当前 PGPChainConfig 解析出的完整分叉表
go run ./scripts/fork-probe --fork-height 200      # 假设只钉 ForkHeight=200 会变成什么样
go run ./scripts/fork-probe --genesis <你的genesis.json> --at 199 --genesis-ts <T0>
```

**[已验证]** 实测输出（当前预设 + `--fork-height 200`）：

```text
ForkHeight: 200
部署高度   : 0  (0 = 不注入系统合约；有 ForkHeight 时这是非法组合)
推导填充   : 0 个 nil 字段被填成 ForkHeight
```

**`推导填充 = 0` 就是那个坑**：13 个块分叉此刻都是 `MaxInt64` 哨兵而不是 nil，
`ApplyPGPForkHeight` 只填 nil，于是 ForkHeight 钉了也一个都不动 —— 链会在 200 切到 Parlia，
但 Luban/Plato/Hertz 永不激活，配置看起来完全正常。

### 2.1 生产预设 `PGPChainConfig` 要动的字段（`params/pgp_config.go`）

| 字段 | 现在 | 钉高后改成 | 说明 |
|---|---|---|---|
| `Legacy.ForkHeight` | `math.MaxUint64` | `H` | 唯一触发开关 |
| `Legacy.SystemContractDeployHeight` | `0` | `H-2`（区间 [2, H-2]） | 0 与"有 ForkHeight"互斥，checkDeployHeight 会拒 |
| `Legacy.ParliaGenesisValidators` | `[]` | 真实验证人列表（**顺序固定**） | 空 = 硬启动失败 |
| `Legacy.ParliaGenesisVotingPowers` | `[]` | 与上一行等长 | |
| `Legacy.ParliaGenesisVoteAddrs` | `[]` | 48B BLS 票地址，等长 | 不填则无 fast finality |
| `Legacy.ParliaGenesisCabinets` | `21` | `<= 席数` | |
| 13 个 BSC 块分叉 | `MaxInt64` | **`nil`** | Ramanujan/Niels/MirrorSync/Bruno/Euler/Gibbs/Planck/Moran/Luban/Plato/Hertz/Hertzfix/Nano |
| `KeplerTime`/`FeynmanTime`/`FeynmanFixTime` | `0` | 保持 `0` | 顺序校验要求它们是前缀 |
| `CancunTime` … `JennerTime` | `MaxInt64` | 需要启用的改成 `T_fork`，`Cancun == Haber == Bohr` | `T_fork` 取 F 块实际出块时间（含余量），绝不落在 legacy 段 |

> 只改这一处的话，`PGPChainConfig` 就带了一个"假的主网高度"。**只适合本地分支/CI，不要提交。**

### 2.2 私有链 genesis（推荐，不动生产预设）

私链的权威配置是 genesis 里的 `config`，`core.chainConfigOrDefault` 对自定义 genesis 也会做推导。
最小模板（4 节点 / F=200 / D=198）：

```json
{
  "config": {
    "chainId": 860621,
    "homesteadBlock": 1, "eip150Block": 2, "eip155Block": 3, "eip158Block": 3,
    "byzantiumBlock": 3, "constantinopleBlock": 3, "petersburgBlock": 3,
    "istanbulBlock": 3, "berlinBlock": 3, "londonBlock": 3,
    "keplerTime": 0, "feynmanTime": 0, "feynmanFixTime": 0,
    "parlia": {},
    "legacy": {
      "forkHeight": 200,
      "systemContractDeployHeight": 198,
      "allowForeignGenesis": true,
      "gasFeeRecipient": "0x...",
      "gasFeeRecipientRatio": 65,
      "passBalance": 0,
      "verifySeal": true,
      "producers": ["02...33字节 P-256 公钥", "..."],
      "parliaGenesisValidators": ["0x...", "0x...", "0x...", "0x..."],
      "parliaGenesisVotingPowers": [100, 100, 100, 100],
      "parliaGenesisVoteAddrs": ["0x...48B", "..."],
      "parliaGenesisCabinets": 4
    }
  },
  "extraData": "0x", "gasLimit": "0x1c9c380", "difficulty": "0x1",
  "alloc": {}, "number": "0x0", "gasUsed": "0x0", "parentHash": "0x00...00",
  "mixHash": "0x00...00", "coinbase": "0xfffffffffffffffffffffffffffffffffffffffe",
  "nonce": "0x0", "timestamp": "0x..."
}
```

要点：
- **不要写那 13 个块分叉**（留空/nil），交给 `ApplyPGPForkHeight`。写了反而要自己保证 13 个值一致。
- `alloc` 留空：让 0x1000..0x3000 在 D 块之前是空账户，这样"协议注入"这条路才真的被验证（否则合约由 genesis 带进来，D 块注入变成空操作）。
- `extraData` 用 `"0x"`：block 0 是 legacy 块，Parlia 不解析它的 extra；快照锚点在 `F-1`，集合从 chain config 读。

**备选（不改代码时）**：把那 13 个块分叉**显式写成 F**。功能等价，代价是每个都要写对、
且 ForkHeight 一改就得改 13 处。`fork-probe` 会把任何一处不一致标出来。

### 2.3 不建议用的捷径

`--override.passedforktime=<T>` 会把 Shanghai/Kepler/Feynman/FeynmanFix/Cancun/Haber/HaberFix/Bohr/Pascal/Prague
**一次性全部**设成 T。在 legacy 段还在跑的链上用它，等于把 Cancun 提前到历史块，
header 会被改写、状态根与旧块不一致。只有两种情况可以用：链已经过了 F；或 T 严格晚于 F 块时间。

---

## 3. 分叉触发方式

| 维度 | 机制 | 测试上的含义 |
|---|---|---|
| 块高触发 | 第 `F` 个块由 Parlia 产出（`consensus/router` 按高度分派），`F-1` 及以前是 PBFT | 触发是**自然出块**，没有"立即生效"的开关；F=200/3s ≈ 10 分钟。想快就把 F 压到 40–60（需 ≥4） |
| 时间触发 | 时间分叉比较的是 header 时间戳 | `T_fork` 只能用"预估 F 块时间戳"的方式对齐：`T_F ≈ T0 + 3*F`；给 1–2 个块的余量，宁可晚一块也不要落在 legacy 段 |
| 不可逆 | 一旦有节点在 F 之后出过块，就不能回退 ForkHeight | 测试期间改 F 必须**重新 init + 清 datadir** |
| 与 breathe 的关系 | breathe 块（默认 86400s，devnet 可 `--override.breatheblockinterval`）才会从 StakeHub 重算集合 | 若 F 之后第一个 breathe 块之前 StakeHub 仍为空：v5 镜像会保留原集合继续出块，但选举等于没上线。用 `run.sh check` 断言 `election.total > 0` |

触发顺序与依赖：

```text
创世(T0) → D 块(注入 13 个系统合约 + init) → F-1(最后个 legacy 块) → F(Parlia 首块)
                              ↘ 必须 2 <= D <= F-2
F → 首个 post-fork breathe 块（重算验证人集合，要求 StakeHub 非空）
```

---

## 4. 验证项与预期结果

分三层，**层 1 能覆盖全部分叉，层 2 只能覆盖有可观测行为的那些，层 3 是明确"验不了"的**。
不要把层 3 写成 PASS。

### 层 1：配置矩阵（离线，[已验证]）

```bash
go run ./scripts/fork-probe --genesis <genesis.json> --at $((F-1)) --genesis-ts <T0>
go run ./scripts/fork-probe --genesis <genesis.json> --at $((F))   --genesis-ts <T0>
go test ./core/   -run TestForkHeight                              # nil 会被填、哨兵不被覆盖
go test ./params/ -run 'TestApplyPGPForkHeight|TestForkOrder'      # 推导语义与顺序校验
```

| 预期 | 判据 |
|---|---|
| 13 个 BSC 块分叉全部 `== ForkHeight` | `fork-probe` 每行标 `← == ForkHeight` |
| `KeplerTime/FeynmanTime/FeynmanFixTime == 0` | 标 `← 创世即激活` |
| 其余时间分叉要么哨兵、要么 `>= T_F` | 不得出现介于 `T0` 与 `T_F` 之间的值 |
| `F-1` 时点：所有 BSC 块分叉为"否"，而 Kepler/Feynman 等执行层时间分叉为"是" | 这正是 legacy 段的形态：执行层规则已开、BSC 共识规则未开，靠 `case rules.IsPGPLegacy` 的优先级保证历史块不被改写 |
| `F` 时点：13 个块分叉全"是" | 保证首块就是 post-fork 规则 |

**[已验证]** `--at 199`（F=200）的实测片段：

```text
HertzBlock             block  否
LubanBlock             block  否
PlatoBlock             block  否
FeynmanFixTime         time   是
FeynmanTime            time   是
KeplerTime             time   是
```

### 层 2：链上可观测（[待执行]，每条都有明确探针）

| # | 验证项 | 探针 | 预期 |
|---|---|---|---|
| V1 | 共识在 F 切换 | 逐块取 miner：`1..F-1` ⊆ PBFT 生产者集合，`F..head` ⊆ Parlia 验证人集合 | 两集合不相交时零交叉；日志 `Created pgp-chain height router forkHeight=F parlia=true`、`PBFT: Parlia fork reached … height=F` |
| V2 | **Luban** 在 F 生效（块分叉里唯一有强可观测的） | `F-1` 与 `F` 的 `extraData` 长度 | legacy 段约 407B（32 vanity + PBFT confirm）；`F` 起 97B（32 + 65 seal）；epoch 块 `33 + N*68 + 1 + 65 (+attestation)` |
| V3 | 系统合约在 D 注入（不是创世带进来） | D-1 与 D 的 `eth_getCode` | D-1 全空；D 起 0x1000/0x1001/0x1002/0x2002/0x2005 长度与镜像 manifest 一致（v5：0x1000 = 29031） |
| V4 | 分叉后 0x66/0x68 才可用 | 同一笔 `cast call` 在 `F-1` 与 `F` 上执行 | `F-1`：`case rules.IsPGPLegacy` 优先，0x66 不在 legacy 预编译集 → 调用无返回/空；`F`：返回 `0x01`。这条同时解释"为什么 `createValidator` 不能在分叉前做" |
| V5 | Parlia 初始集合来自 genesis 顺序 | `getMiningValidators()` | 与 `parliaGenesisValidators` 逐项、同序一致；`numOfCabinets() == cabinets` |
| V6 | 选举在分叉后接管 | 首个 post-fork breathe 块的系统交易 `to=0x1000 selector=0x1e4c1524` | 交易成功；StakeHub 非空时集合按票力重排（gas ≈ 1.3M），空时保留原集合（gas ≈ 24.5k）且日志 `validator election returned an empty set` |
| V7 | **Nano** 黑名单（配置项，非分叉行为） | `core/types.NanoBlackList` 长度 + 相关地址的 tx 是否被拒 | 当前已清空：任何地址都不应因黑名单被拒 |
| V8 | 全网一致 | 每个节点在 `F-1`/`F`/head 的块哈希 | 全部相同（`expect.py hashes` 或 `cast block` 对比） |

### 层 3：只有配置级证据、没有独立链上探针的分叉

`Ramanujan / Niels / MirrorSync / Bruno / Euler / Gibbs / Planck / Moran / Plato / Hertz / Hertzfix`
在链上没有一个"一眼能看出翻转"的独立观测量（它们改的是奖励分配、precompile 集、gas 细则等，
需要构造特定交易才能区分）。对这 9 个分叉：

- **可以证明**：配置里它们的激活点 = F（层 1）；F 之后全网状态根一致、无分叉告警。
- **不能声称**：已经逐项验证了它们的语义。要真正覆盖，需要为每个分叉单独设计敌意交易/状态探针，
  属于下一轮工作，不要算进本轮 PASS。

---

## 5. 失败模式对照表

| 症状 | 根因 | 处置 |
|---|---|---|
| `geth init` 报 `unsupported fork ordering: hertzfixBlock not enabled, but keplerTime enabled at timestamp 0` | 13 个块分叉是 nil 且没走推导（P8 未修） | 打上 `Genesis.Commit` 里的 `ApplyPGPForkHeight`；或显式写全 13 个 = F |
| `legacy.forkHeight N requires legacy.systemContractDeployHeight` | D=0 但 F 已钉 | 给 D（2..F-2） |
| `legacy.systemContractDeployHeight D requires a fixed legacy.forkHeight` | 想注入但没有 F | 先钉 F |
| 起链即报 `bscChainID` 不匹配 | chainId ≠ 860621 且未 `allowForeignGenesis` | 用 860621 + `allowForeignGenesis: true` |
| legacy 段一块都不出 / 验签失败 | `producers` 是主网 arbiter、与私链 PBFT 生产者不匹配 | 换成本链 arbiter，或调试期关 `verifySeal` |
| F 块产不出来，`Commit new sealing work` 反复失败 | D 块注入没成功 / 0x1000 无代码 / 集合空 | 先查 D 块日志 `pgp system contract initialised`，再查 `eth_getCode` |
| F 之后 Luban 没生效（extra 仍是 legacy 形态） | 13 个块分叉停在哨兵 | `fork-probe` 确认 `推导填充`/`== ForkHeight` |
| 首个 post-fork breathe 块卡住 | 旧镜像（v4）的空集合越界 | 换 v5 镜像；`run.sh check` 断言 `election.total > 0` |
| 时间分叉"提前生效"，历史块状态根对不上 | `T_fork` 落在 legacy 段内 | `fork-probe --at F-1` 确认该时点全为"否" |

---

## 6. 复现命令

### 6.1 用现成脚手架（最省事，[待执行]）

```bash
cd bsc
go build -o build/bin/geth ./cmd/geth
go build -o build/bin/bootnode ./cmd/bootnode

# 先看配置矩阵
go run ./scripts/fork-probe --genesis .devnet/e/net/genesis.json --at 39 --genesis-ts <T0>

export PGP_STAKING_HOME=<workspace>/.devnet/forktest
./scripts/staking-devnet/run.sh init e     # 8 节点 / 4 生产者 + 4 验证人 / F=40 / D=20
./scripts/staking-devnet/run.sh prepare
./scripts/staking-devnet/run.sh start      # 前台，另开终端
./scripts/staking-devnet/run.sh window     # 距 breathe 边界还剩多久
./scripts/staking-devnet/run.sh check      # 分叉前：应报"还没分叉"
./scripts/staking-devnet/run.sh wait 41
./scripts/staking-devnet/run.sh register   # 必须在首个 post-fork breathe 之前
./scripts/staking-devnet/run.sh check --deep
```

> 注意：这条路用的是 `gen-genesis.py`，它**显式写全**了 13 个块分叉（等价于推导结果），
> 所以它验证的是"分叉后的链上行为"，**不验证** §2.0 那条 nil→推导 的配置路径。
> 配置路径由层 1 的 `fork-probe` + `go test ./core/ -run TestForkHeight` 覆盖。

### 6.2 手搓最小私链（4 节点，验证 §2.2 的 genesis 形态，[待执行]）

```bash
# 1) 生成 genesis（用 §2.2 模板，F=60/D=58，4 个验证人，块分叉留空）
geth init genesis.json --datadir node0

# 2) 起 4 个节点（每个换 --port/--http.port/--miner.etherbase/--unlock）
geth --datadir node0 --networkid 860621 --mine --miner.etherbase 0x... \
     --unlock 0x... --password pw.txt --allow-insecure-unlock \
     --http --http.api eth,net,web3,admin --bootnodes <ens> \
     --override.breatheblockinterval 3000      # 让 breathe 不落在观测窗口内

# 3) 逐块验证（V1–V5）
cast block 59 --rpc-url http://127.0.0.1:18560 --json | jq '.miner,.extraData,.number'
cast block 60 --rpc-url http://127.0.0.1:18560 --json | jq '.miner,.extraData,.number'
cast call 0x0000000000000000000000000000000000001000 "getMiningValidators()(address[],bytes[])" \
     --rpc-url http://127.0.0.1:18560
```

---

## 7. 上线前检查单（把 F 真正定下来那天照这个走）

| # | 项 | 判据 |
|---|---|---|
| 1 | `fork-probe` 显示 13 个块分叉 `== ForkHeight`、`推导填充 == 13` | 输出对照 |
| 2 | `D ∈ [2, F-2]` 且 > 0 | `fork-probe` 不报"非法" |
| 3 | `ParliaGenesisValidators` 顺序已冻结并写进变更单（顺序换 = 状态根换） | 评审记录 |
| 4 | `T_fork`（需要启用的时间分叉）≥ `T0 + 3*F`，且 `Cancun == Haber == Bohr` | `fork-probe` |
| 5 | 私链/测试网先按同参数跑一遍 V1–V8 | 本方案 §4 |
| 6 | 首个 post-fork breathe 块之前 StakeHub 非空（或明确接受"保留原集合"） | `run.sh check` 退出码 0 |
| 7 | 镜像 `imageHash` 与二进制一致，`TestImageProvenanceIsCommitted` 绿 | `go test ./core/systemcontracts/pgp/` |

---

## 8. 实测结果（Run E，13 个块分叉全部 = F=40）

**配置形态**：`ALL_FORKS_AT_FORK_HEIGHT=1` 生成，13 个 BSC 块分叉全部 40，`keplerTime/feynmanTime/feynmanFixTime = 0`，
其余时间分叉未配置；D=20；4 个 PBFT 生产者（node0-3）+ 4 个 POSA 验证人（node4-7）不相交；8 节点。

`fork-probe` 确认：`13 个 ← == ForkHeight`，`推导填充 0`（显式写了，不需要推导）。

| 验证项 | 实测 | 判定 |
|---|---|---|
| V1 共识切换 | legacy 1..39 只由 node0-3 出块；40..47 只由 node4-7 出块 | PASS |
| V2 Luban | extraLen：block 39 = **407**，block 40 = **97**，41 = 97 | PASS |
| V3 D 块注入 | 0x1000 = **29031** / 0x1001 = 13386 / 0x2002 = 47087 B（与 v5 manifest 一致） | PASS |
| V4 预编译切换 | block 39 调 0x66/0x68 → **空返回 `0x`**（不是预编译，当空账户处理）；block 45 → **`execution reverted`**（预编译存在，拒绝空输入） | PASS |
| V5 初始集合 | `getMiningValidators()` = node4..7，顺序与 genesis 一致；`cabinets = 4` | PASS |
| V6 空集合 breathe | 首个 post-fork breathe = **block 82**，StakeHub 为空时链继续出块（v5 长度保护），随后注册 → `election.total = 4` | PASS |
| V8 全网一致 | 8 节点 hash 一致 @120；`check --deep` 除 election 外全绿（注册前） | PASS |

### 8.1 顺带发现：`eth_call` 打 legacy 块会打崩 RPC handler（已修）

做 V4 的前后对比时发现：`eth_call` 打到**分叉前**的块会返回 `-32603 method handler crashed`，
分叉后正常。栈顶是 `internal/ethapi/api.go:1044` 的 `msg.GasPrice.Sign()`，`msg.GasPrice` 为 nil。

根因：`CallDefaults(blockContext.BaseFee, …)` 与 `ToMessage(header.BaseFee, …)` 用了**不同的 baseFee 来源**。
legacy 块 header 没有 `BaseFee`，但 block context 有，于是 `CallDefaults` 走 1559 分支只填了
`MaxFeePerGas/MaxPriorityFeePerGas`，`GasPrice` 留空；`ToMessage(nil)` 走「无 basefee」分支去读这个空
`GasPrice`，得到 nil Message 字段，`Sign()` 直接崩。

修法：`ToMessage` 改用 `blockContext.BaseFee`（与 `CallDefaults` 同源），并加一层 nil 兜底。
修复后 block 39 的三种费用写法（不给 / `gasPrice=0x0` / 只给 1559 字段）都返回 `0x`，不再 panic。
回归测试：`internal/ethapi` 的 `TestCallDefaultsAndToMessageUseTheSameBaseFee`。

> 这条直接影响本方案：**分叉前的状态无法"事后"用 `eth_call` 回放**。要么在分叉窗口内就把探针跑完，
> 要么确认这个修复已在二进制里。V4 之所以能给出干净的 A/B，是因为修复后可以按块高回放。

### 8.2 必须说清楚的一件事：13 个分叉钉在同一高度时，无法逐个观测

把 13 个块分叉全部钉在 F，也就意味着 **Parlia 的第一个块就是它们全部生效的块**——
不存在"某个分叉已生效、另一个还没"的链上窗口。所以在链上能看到的只有它们的**合成效果**（Luban 的 extra 形态最明显），
无法把 Plato/Hertz/Planck 等分开验证。

各分叉在本移植里的实际分量（非测试调用点数，`grep` 实测）：

| 分叉 | 调用点 | 作用 | 链上可独立观测 |
|---|---|---|---|
| Luban | 多处 | extra 布局、post-Luban 合约 ABI、预编译集 | ✅ extraLen 407→97 |
| Plato | 9 | forkchoice 最终性、finality reward、预编译集 | ⚠️ 间接（epoch 块系统交易） |
| Ramanujan | 3 | 出块时间与 backoff 规则 | ⚠️ 只能看时间戳抖动，不可靠 |
| Planck / Nano / Moran / Hertz | 各 1–2 | 预编译集（Nano 另含交易黑名单） | ⚠️ 需针对具体预编译地址构造探测 |
| Hertzfix | 2 | gas/费用细则 | ⚠️ 需构造特定交易 |
| Euler | 1 | pre-Luban 验证人 ABI 选择 | ❌ 本形态下走不到（Luban==F，pre-Luban 路径不存在） |
| **Niels / MirrorSync / Bruno / Gibbs** | **0** | **无**（本移植没有引用） | ❌ 占位字段，无法测 |

要逐个验证这 13 个，唯一办法是**按 `CheckConfigForkOrder` 允许的顺序把它们错开**（例如 Luban=F、
Plato=F+20、Hertz=F+40…），代价是链的形态偏离主网。这是下一轮的工作量，不是本轮能声称已完成的。

### 8.3 Cancun..Fermi 组：已启用，可工作（实测）

上一轮这组全是未配置状态，本轮补测：把它们全部钉在 `T_fork = T0+300`（F=40 之后约 49 块），
用 `BSC_TIME_FORKS=<ts>` 生成创世。**结论：可以启用，链在 450ms 出块下稳定运行。**

启用过程中踩到三个硬前置，缺一个就 `init` 失败：

| # | 前置 | 不满足时的报错 | 说明 |
|---|---|---|---|
| 1 | `feynmanFixTime` 必须同时给 | `unsupported fork ordering: feynmanFixTime not enabled, but cancunTime enabled` | 它在顺序链里卡在 feynman 和 cancun 之间且**非可选**。主网预设里是 0，只有自造 genesis 会漏 |
| 2 | 配置项名是 `blobSchedule`（不是 `blobScheduleConfig`） | `invalid chain configuration: missing entry for fork "cancun" in blobSchedule` | `ChainConfig.BlobScheduleConfig` 的 json tag 是 `blobSchedule` |
| 3 | 字段名是 `baseFeeUpdateFraction`（不是 `updateFraction`） | 同上 | `BlobConfig.UpdateFraction` 的 json tag |

实测（`eth_getBlockByNumber`）：

| 块 | ts | Δ | blobGasUsed | parentBeaconBlockRoot | requestsHash |
|---|---|---|---|---|---|
| 88 | 1794450597 | 3s | — | — | — |
| **89** | **1794450600** = T_fork | **0.45s** | ✅ | ✅ | ✅ |
| 90 | 1794450600 | 0.45s | ✅ | ✅ | ✅ |

同一块 89 上还叠了两件事：它同时是 **breathe 块**（T0+300），且 StakeHub 为空 →
`updateValidatorSetV2([],[],[])` 走了 v5 的长度保护（gas 24,528，保留原集合）。三件事撞在一个块上，链照常出块。

块间隔的量化：89..129 共 41 个块用了约 18 秒（≈440ms/块），`MixDigest` 里的毫秒分量按 450 步进
（150,600,50,500,950,…）。`header.Time` 仍是**秒**，毫秒部分存在 `MixDigest`（`Header.MilliTimestamp()`），
所以按秒做的时间计算（breathe 桶）不受影响。

启用这组的真实代价（不是"开了就更新"）：

| 变化 | 影响 |
|---|---|
| `snap.BlockInterval` 3000 → 1500（Lorentz）→ 750（Maxwell）→ 450ms（Fermi） | 出块节奏变了；脚手架里所有"3s 一块"的假设（T0 对齐、等待超时）都要复核 |
| `EpochLength` 200 → 500（Lorentz）→ 1000（Maxwell） | 验证人集合的 checkpoints 间隔变了，切换验证人的块距变长 |
| `turnLength` 改为从合约 `getTurnLength()` 读（Bohr） | 我们的 v5 合约实现了该方法（`turnLength==0` 时返回 1），所以没改变轮次；若治理把它设成别的值，出块行为会变 |
| header 增加 `blobGasUsed/excessBlobGas/parentBeaconBlockRoot/requestsHash` | **头哈希与不开时不同**；任何按 header 序列化/比对的工具都要能认这些字段 |

### 8.4 对主网配置的含义

- 把 13 个块分叉全部钉在 F 是**正确的生产形态**，`fork-probe` 会逐行确认。
- 不要在 `PGPChainConfig` 里保留 `MaxInt64` 哨兵就宣布"已排期"——那会让 Luban/Plato/Hertz 永不激活而配置看起来正常。
- 时间分叉里 `Cancun/Haber/Bohr` 若要在 F 之后启用，`T_fork` 必须取 F 块的真实出块时间并留余量；
  它们一旦早于 F 就会改写 legacy 段 header。

---

## 9. 本轮实际交付

| 文件 | 内容 | 状态 |
|---|---|---|
| `core/genesis.go` | 在 `Commit` 里补 `ApplyPGPForkHeight()`（修 §1.1 的阻塞项） | 已提交 `ed7a2f689` |
| `core/genesis_pgp_forkfill_test.go` | 两条回归：nil 会被填；哨兵/显式值不被覆盖 | 通过 |
| `scripts/fork-probe/main.go` | 离线分叉矩阵探针（§2.0 / §4 层 1 的工具） | 已提交 `65ebe59a2` |
| `scripts/parlia-devnet/gen-genesis.py` + `multi.sh` | `ALL_FORKS_AT_FORK_HEIGHT=1`：把 9 个历史块分叉也钉在 F，复刻主网形态 | 本轮新增 |
| `internal/ethapi/api.go` + `transaction_args_test.go` | 修 §8.1 的 legacy `eth_call` panic，加不变量测试 | 本轮新增 |
| `docs/test/pgp-fork-activation-test-plan.md` | 本文（含 §8 实测结果） | — |

**仍然没做的**：
- 13 个分叉的**逐项**验证（§8.2）：需要把它们错开钉高，属下一轮；本轮给的是合成效果 + 每个分叉的分量清单。
- 时间分叉 `Cancun/Haber/Bohr/Prague/...` 的链上生效验证：本轮它们都未配置（`MaxInt64`/缺席），
  只验证了 `Kepler/Feynman/FeynmanFix = 0` 这条前缀组的间接效果（0x66/0x68 在 F 后可用）。
