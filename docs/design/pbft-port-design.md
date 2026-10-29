# PBFT 出块侧移植：让 bsc 成为可出块的 pgp 节点

| 项 | 值 |
|---|---|
| 状态 | 设计已定，M1 实施中 |
| 日期 | 2026-10-29 |
| 适用仓库 | `bsc`（链） |
| 上游参照 | `pgp-chain`（geth 1.9.7 改造版，PBFT/DPoS 现网实现） |
| 关联文档 | `docs/design/parlia-fork-design.md`（PoSA 阶段） |

## 零、这份文档解决什么问题

`parlia-fork-design.md` 把 `bsc` 改造拆成 T1–T7，全部围绕"**legacy 段只读回放 → 某个高度切 Parlia**"。它的前提是 legacy 段永远不需要出块：

> `consensus/router`（`consensus/router/router.go`）已实现"按高度路由"……**fork 前 `Seal` 直接报错**（设计如此）。

这个前提现在不成立了。新链在切 PoSA 之前必须能**用 PBFT 继续出块**，否则要么链停摆，要么只能等 PoSA 分叉才能上线。所以本文档补齐那份设计刻意留空的**出块侧**。

两份文档的边界：

```
       PBFT 阶段（本文档）              PoSA 阶段（parlia-fork-design.md）
   ─────────────────────────────  │  ─────────────────────────────────────
   出块 / 投票 / confirm / 网络    │  引导快照 / 系统合约注入 / PoSA 透传
   router.Seal 分叉前委派 pbft     │  router 分叉后委派 parlia
   生产者集 = SPV 动态集合         │  生产者集 = 链上 ValidatorSet 合约
                                   │
   共同依赖：consensus/router、params.IsPGPLegacy、core/state_transition 的 legacy 分支
```

互相引用点：本文档 §七 R9 指出 PoSA 阶段必须修改 `eth/backend.go` 的 `PoSA` 断言；对方文档 T4 的 fast finality 工作依赖本文档先把出块跑通。

---

## 一、目标与已定决策

让 `bsc` 成为可完整替代 `pgp-chain` 1.9.7 的节点：能作为 12 个仲裁人（producer）之一提案并出块，与现有旧节点**同网、wire 级互通**，支撑滚动升级。

| 议题 | 决策 |
|---|---|
| 范围 | 完整出块能力 + 与旧 1.9.7 节点互通（滚动升级期新旧节点共出块） |
| 共识消息传输 | 复用 Elastos DPoS p2p 独立 TCP（PID = 压缩公钥），不新建 devp2p 子协议 |
| DAddr 公告 | 复用 `eth/protocols/eth64` 已有但未接线的 `ELAMSG = 0x08` |
| 生产者集合 | 沿用 SPV 动态集合：`nonce == 0` 用配置的 12 个固定公钥，否则 `spv.GetProducers(header.Nonce)` |
| 可选模块 | **只**移植 `pbft` RPC namespace。**不移植** producer_stats/自动黑名单、`validators/` BPoS 合约集合、layer2/chainbridge 跨链业务 |

---

## 二、现状：已核实的事实

### 2.1 已有、本次要复用的基座

`bsc` 已经有一半机器装好了，缺的只是出块侧。

| 资产 | 位置 | 复用方式 |
|---|---|---|
| 只读 legacy 引擎 | `consensus/pgp/pgp.go`（421 行）、`confirm.go`（445 行） | **内嵌**，一行字节行为都不改 |
| `SealHash`（14 字段、无 chainId、无 Extra） | `consensus/pgp/pgp.go:163-187` | 出块与验块共用同一函数 |
| confirm 编解码 + 8-of-12 验签 | `consensus/pgp/confirm.go:205 / :326 / :406` | 出块侧只调 `payload.Confirm.Serialize`，与验证侧是同一对函数 |
| 12 条主网 golden 向量 | `consensus/pgp/testdata/seal_vectors.json`、`confirm_golden_test.go` | 双向自洽测试基准 |
| 高度路由 | `consensus/router/router.go`（`New(legacy, posa, forkHeight)`） | 只改 `Seal` 一处 |
| eth/64 完整 wire 实现 | `eth/protocols/eth64/`（8 文件） | `ELAMSG = 0x08` 常量已在 `protocol.go:76`，**缺 handler** |
| SPV 接缝 | `spv/spv_module.go:53` 全局 `PbftEngine PbftEngineView`，接口在 `:75-82` | 由新引擎直接实现 |
| legacy 执行规则 | `core/state_transition.go`（65:35 分费、`legacyGasRefund`、`borrowPassBalance`） | 无需改动 |

### 2.2 四条必须按真实代码走的更正

这四条都是"凭直觉会写错"的地方，实施时按右列来。

| # | 直觉 | 实际代码 | 影响 |
|---|---|---|---|
| 1 | `eth/backend.go:832` 硬断言 `*parlia.Parlia`，否则启动失败 | 那处是 `if parlia, ok := s.engine.(*parlia.Parlia); ok`，**非致命**，Parlia 分支被跳过但 `go s.miner.Start()` 照跑。真正的硬断言只有 `eth/backend.go:509-520`（votePool 段 `return nil, errors.New("Engine is not Parlia type")`） | PBFT 只要**不实现** `consensus.PoSA`，就不会触雷 |
| 2 | `Finalize` 只在组装区块时调用 | `core/state_processor.go:237` 在**每个导入块**的 `Process` 路径上都调 `Engine().Finalize(...)` | pgp-chain 在 `Finalize` 里推进 `dispatcher.FinishedProposal` 的做法**不能照搬**，否则侧链块、坏块也会推进终局高度 |
| 3 | 无 pending tx 时需要特判出空块 | `commitWork` 无空块拦截：`fillTransactions` 拿不到 tx 也照样 `w.commit(...)`；`commit()` 只受 `w.isRunning()` 门控 | 与 pgp 行为一致，**不需要改** |
| 4 | — | `miner/miner.go:76` 无条件建 `bidSimulator` 并 `setBestBidFetcher`；`worker.go:1560` 的门控是 `bidFetcher != nil && Difficulty.Cmp(diffInTurn) == 0`，而 PBFT 恒定 `Difficulty = 2 = diffInTurn` | **MEV/BidBlock 路径会被误触发**，可能拿 builder 块出块 → 必须加 legacy 门控 |

另有一条容易漏的（**M2 已补**）：legacy 链原本没有周期 resubmit——`miner/worker.go:534-536` 的定时器条件只对 `Clique.Period > 0` 或 `IsInBSC()` 生效，PBFT 链两者都不满足，非 on-duty 节点原本只能靠 6s view change 唤醒。现在该条件已加上第三个析取项，即

```
Clique.Period > 0  ||  IsInBSC()  ||  chainConfig.Legacy != nil
```

源码注释在 `miner/worker.go:526-533`。**`Recommit` 窗口刻意不与 3s 块周期绑定**：3s 的收集窗口对 3s 的块基本只会产出空块，它仍然只是"事务收集窗口"，不是出块节拍。

### 2.3 依赖层：不需要 vendor，不需要 replace

上游模块 `github.com/elastos/Elastos.ELA v0.9.8-0.20230822071649-363670e32ee4` 已被 `bsc/go.mod:29` require；`pgp-chain/go.mod:107` 用 `replace` 指向自带的 27MB 副本。

两者在 wire 相关包上**逐字节相同**（已 `diff -r` 验证）：

```
dpos/p2p   dpos/dtime   crypto   p2p
core/types/payload/confirm.go   core/types/payload/dposproposal.go      → 全部无差异
```

**结论：直接 import 上游模块，不引入 `replace`，不搬 vendored 副本。**

### 2.4 代码规模

| 来源 | 行数 | 处理 |
|---|---|---|
| `pgp-chain/dpos/`（20 文件） | 3852 | 主体照搬，10 个非测试文件进 `bsc/dpos/` |
| `pgp-chain/consensus/pbft/` | 3459 | 裁掉 `producer_stats.go`(611)、`layer2Listener.go`(91)、`blacklist_oracle_iface.go`(12)，余下 ~1.4k 重组 |
| 新增 `dpos/elamsg/` | ~60 | 新写 |

---

## 三、目标架构

```
                     ┌─ consensus/router（按高度路由，不实现 PoSA）
                     │     Seal: 分叉前 → pbft ；分叉后 → parlia
                     │
  eth/ethconfig ─────┤─ consensus/pbft   （新，出块引擎；内嵌 *pgp.Engine）
   CreateConsensus   │     ├ 继承: Author / VerifyHeader(s) / VerifyUncles
   Engine           │     │        VerifyRequests / SealHash
                     │     ├ 重写: Prepare / Seal / Finalize / CalcDifficulty / Delay
                     │     └ 新增: spv 视图 / pbft RPC / 不可逆高度
                     │
                     └─ consensus/pgp    （不动，只加 export.go 薄封装）

  consensus/pbft ──uses──> dpos（新）  状态机 + DPoS TCP
                              ├ dispatcher / consensusview / producers / blockpool / confirm
                              ├ routes  （DAddr 公告与路由）
                              ├ network （Elastos dpos/p2p TCP，独立端口）
                              └ msg/    （全表 codec，含 layer2 的 no-op 兜底）

  eth/protocols/eth64 ──ELAMSG 0x08──> dpos/routes   （DAddr / Inv / GetData 中继）
```

**为什么内嵌 `consensus/pgp` 而不是复制一份**：出块侧必须与验证侧共用同一份 confirm 序列化代码。另写一份等于第二处要维护的共识代码，而且 golden 向量保护不到它。内嵌后"节点验证自己刚出的块"天然走同一条路径。

---

## 四、关键设计决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | `pbft.Engine` **内嵌** `*pgp.Engine`，只覆盖 `Prepare/Seal/Finalize/CalcDifficulty/Delay/Close` | 见上。`go test ./consensus/pgp/...` 必须零回归 |
| D2 | `Finalize` **纯函数**：`Root = IntermediateRoot()` + `UncleHash = CalcUncleHash(nil)` | 因 `state_processor.go:237` 每块都调。副作用改由 `addConfirmToBlock`（出块侧）+ `ChainHeadEvent` 订阅（导入侧）驱动 |
| D3 | **不加宽** `consensus.Engine` | pgp-chain 给接口加了 3 个方法（`SignersCount`/`IsInBlockPool`/`GetValidatorsByHeight`），代价是改 6 个实现加全部 mock。bsc 侧改为包内窄接口 + core 侧可选断言 |
| D4 | `NextInTurnValidator` 返回 `(common.Address{}, nil)` | PBFT 的 in-turn 身份是 P-256 压缩公钥，没有合法的 `common.Address` 映射。硬凑一个会让 `miner` 的 in-turn 判断永远出错。on-duty 判断放在 `Prepare` 内部 |
| D5 | `CalcDifficulty` **恒返回 2** | pgp-chain 非 on-duty 时返回 1，但那个 header 会被丢弃。如果 bsc 侧"吞掉 Prepare 错误继续用该 header"，就会产出 `difficulty = 1` 的块，全网拒收 |
| D6 | `Prepare` **无条件** `header.Extra = nil`，并显式 `MixDigest = zero` | `worker.go:1063` 会预填 `Extra`（来自 `--miner.extradata`），`worker.go:1073` 会写 `MixDigest = genParams.random`。两者都会让 legacy 规则校验失败 |
| D7 | `Delay` 必须返回**非 nil** | `worker.go:1418` 收到 nil 会打 Warn 并跳过 `LOOP_WAIT`，从此不再收集新交易 |
| D8 | PBFT 参数（`period = 3s`、view 容差 6s、2/3 阈值）**用常量，不做 JSON 可覆盖** | 可配置 = 各节点可能不一致 = 出块节奏分裂 = 全网互拒。pgp-chain 也是硬编码 |
| D9 | 节点运维参数（keystore/端口/magic）放 `ethconfig.Config.Pbft`，**不进 `ChainConfig`** | `ChainConfig` 会写盘并与内置 genesis 比对，放端口/路径会导致 "stored config mismatch" 反复重写。另外 `params/pgp_config.go:33-37` 有硬约束：任何字段名不得以 `Block`/`Time` 结尾，否则 `core/forkid` 反射会改 EIP-2124 checksum，握手全失败 |
| D10 | DPoS 全表 CMD 的 **codec 全留**，layer2/smallCro/failedWTx 的 listener 做 no-op | `dpos/network.go:397-451` 的 `createMessage` 对未知 CMD 直接返回错误并**断开 DPoS 连接**。少注册一个 CMD 就断连 |

---

## 五、文件级改动清单

### 5.1 新增

| 包 | 文件 | 说明 |
|---|---|---|
| `dpos/elamsg/` | `elamsg.go` | `ElaMsg{Type uint8; Msg []byte}`，线格式 RLP。叶子包，`eth64` 与 `dpos` 都 import 它以避免循环依赖 |
| `dpos/msg/` | `commands.go`、`blockmsg.go`、`confirmmsg.go`、`producersmsg.go`、`consensusstatus.go`、`requestconsensus.go`、`responseconsensus.go`、`smallcrotxmsg.go`、`failedwithdrawmsg.go` | 照搬 `pgp-chain/dpos/msg/`。`CMD()` 字符串是 wire 契约 |
| `dpos/` | `dispatcher.go`(549)、`consensusview.go`(306)、`producers.go`(232)、`blockpool.go`(324)、`confirm.go`(107)、`account.go`、`signer.go`、`network.go`(451)、`routes.go`(645)、`event.go`、`log.go` | 搬 `pgp-chain/dpos/`。改动：`log.go` 换 go-ethereum log；`network.go` 的 `chainbridge-core/dpos_msg` 换本地常量；`Network.Stop()` 的 `quit <- true` 改 `sync.Once + close`（无缓冲 channel 会永久阻塞）；**新增 `signer.go`——上游 `Elastos.ELA/crypto.Sign` 在 Go 1.25 下必 panic，见 R15** |
| `consensus/pbft/` | `engine.go`、`prepare.go`、`seal.go`、`view.go`、`transport.go`、`legacy.go`、`config.go`、`consts.go`、`errors.go`；待做：`network.go`、`api.go`、`spvview.go`、`metrics.go` | `seal.go`/`network.go` 源自 `pgp-chain/consensus/pbft/{pbft,network}.go`，裁剪后重组。`transport.go` 是可 nil 的网络接缝，让 M1/M2 不依赖 p2p |
| `consensus/pgp/` | `export.go` | 纯包装：`DecodeConfirm`/`CheckConfirm`/`MajorityCount`/`DecodeProducers`/`BlockPeriod`，并导出 `ErrProductionNotSupported`。**无逻辑改动** |
| `core/` | `legacy_finality.go` | `irreversibleHeightProvider` 接口 + reorg 守卫。**M6 才做** |

### 5.2 修改

| 文件 | 位置 | 改动 |
|---|---|---|
| `consensus/router/router.go` | `:250-262` | `Seal` 分叉前从 `return errors.New(...)` 改为委派 `legacyEngine().Seal(...)`。**router 唯一必改点** |
| `eth/ethconfig/config.go` | `:287-313` | `pgp.New(config.Legacy)` → `pbft.New(config.Legacy, pbftCfg)`；`CreateConsensusEngine` 加 `*pbft.Config` 参数。`router.New` 签名不变 |
| `eth/backend.go` | `:333`、`:509`、`:575-600` | ① 传 `pbftCfg`；② `Legacy != nil` 时**跳过** `miner.SetExtra(makeExtraData(...))`：legacy 期的 `header.Extra` 被 confirm 独占，任何 vanity 预填都是纯脚枪（`Prepare` 本来也会置 nil，见 D6），跳过是为了不留一个容易被读错的假入口。**`SetExtra` 的 32 字节上限（`params.MaximumExtraDataSize`，`miner/miner.go:202`）不构成本处的依据**——它收到的是 `--miner.extradata` 的 vanity 串，永远不会是那个 1199 字节的 confirm，所以 confirm 长度与它无关；③ 装配 `pbft.AttachChain` / `spv.PbftEngine`，并由装配层自建 `pbftLoop`。**`pbft.Engine` 不提供 `Start` 方法**：它只有 `AttachChain`（`consensus/pbft/engine.go:236`）和 `Close`（`engine.go:319`）；出块驱动循环在 `eth/backend.go` 的 `pbftLoop`（订阅链头 → `OnBlockImported` → `StartMine`，见 `eth/backend.go:930`） |
| `cmd/utils/flags.go` | `:2954` + 新 flags | 第二个调用点同步改签名；新增 `--pbft.enable / .keystore / .keystore.password / .net.address / .net.port / .net.magic / .maxnodeperhost / .miner.address` |
| `miner/worker.go` | `:1093` | `Prepare` 的良性错误（`ErrSignerNotOnduty`/`ErrWaitRecoverStatus`/`ErrEpochTransitionPending`/`ErrAlreadyConfirmedBlock`）降为 Debug 并**中止本次生成**（不能继续用该 header） |
| `miner/worker.go` | `:534-536` | 周期 resubmit 条件加 `\|\| chainConfig.Legacy != nil`（**已完成**，源码注释在 `:526-533`）。**不动 `Recommit`**：它是事务收集窗口，与 3s 块周期绑死只会多出空块，见 §2.2 末段 |
| `miner/worker.go` | `:1560` | MEV 门控加 `&& w.chainConfig.Legacy == nil` |
| `eth/protocols/eth64/handler.go` | `dispatch` switch + `Backend` 接口 | 新增 `case ELAMSG:`（当前落 `default` 被 Discard）；新增 `BroadcastElaMessage`；`Backend` 加 `ElaMessageSink()`。**`protocolLengths` 恒 17 不动** |
| `eth/protocols/eth64/peer.go` | — | 新增 `SendElaMessage`；`run()` 里包一层 `elaPeer{p, pp}` 以提供 `Disconnect()` |
| `eth/handler.go`、`eth/handler_eth64.go` | — | `handler` 加 `dposBridge eth64.ElaMessageSink` 字段并注入，避免 `eth` 反向依赖 `dpos` |
| `spv/blocklistener.go` | `:358-402` | 补 `ETNextProducers` 等价回调（当前是 TODO）→ 驱动 `dpos.SetNextProducers`。**M5 才做** |

---

## 六、里程碑

> M1/M2 **完全不依赖 p2p**，用"1 生产者 + 本地 confirm"闭环。M3 起才引入网络。

### M1 引擎骨架 + 单机出块/验块闭环 ✅ 已完成

- **目标**：`consensus/pbft` 能产出带合法 confirm 的块，且该块被 `consensus/pgp` 的验证路径接受
- **验收（全部达成）**
  - `go test ./consensus/pgp/ ./consensus/pbft/ ./dpos/ ./consensus/router/` 全绿，12 条 golden 零回归
  - `TestSealProducesABlockTheReadOnlyVerifierAccepts`：12 个真实 P-256 生产者（阈值 8），一轮真实投票（proposer 自投 + 7 个 peer 票）后 Seal 出的块过 `pgp.New(legacy).VerifyHeader`
  - `TestGoldenConfirmSerializesBackToTheSameBytes`：12 条主网 `header.Extra` 经 `decodeConfirm → Serialize` 逐字节还原，证明出块侧写入与验证侧解码互为逆函数
  - `TestConfirmWireLengthIs1199For8Votes`（135 + 8 + 8×132 = 1199）
  - `TestSignProposalVerifiesWithElastosCrypto` / `TestSignVoteVerifiesWithElastosCrypto`：签名字节可被 `crypto.Verify` 接受（R15 回归闸）
- **实测结论**
  - `pgp.Engine` 被 `pbft.Engine` 内嵌后，`consensus.Engine` 全接口满足（编译期断言在 `engine.go`）；`dpos.ViewListener` 同样满足
  - **`dpos.StartProposal` 不登记 proposal**，必须紧跟 `dispatcher.ProcessProposal(self, proposal)`，否则所有投票被判 "not proposal to process vote"、每轮必然超时（已在 `seal.go` 修正并注明）
  - **上游 `crypto.Sign` 在 Go 1.25 下 nil-deref panic**，只读回放路径永远碰不到，首次出块必然崩进程（见 R15）
- **回滚**：只新增包 + router 一处分支，`ethconfig` 未改，单 commit revert 即恢复

### M2 miner 集成 + 单节点连续出块

- **目标**：单进程 1 生产者，3s 节奏连续 100 块；非 on-duty 时安静空转
- **验收**
  - `go test ./miner/... ./consensus/pbft/... ./eth/...`
  - devnet：60s 内 `eth_blockNumber >= 18`，块间隔 ∈ [3.0s, 3.3s]
  - `eth_getBlockByNumber(n).extra` 可被 `pgp.DecodeConfirm` 解开；`--miner.extradata=abc` 时 Extra 仍是纯 confirm
  - 重启后从磁盘恢复 head 继续出块
- **回滚**：`--pbft.enable=false` → `Prepare/Seal` 返回 `ErrProductionNotSupported`，行为回到当前只读回放

### M3 eth64 `ELAMSG` + DAddr 路由（可与 M2 并行）

- **目标**：两节点经 eth/64 `0x08` 交换 DAddr/Inv/GetData，`routes` 解出对端 DPoS 地址
- **验收**
  - `ElaMsg` 线格式 golden：`rlp.Encode(ElaMsg{Type:2, Msg:x})` 与 pgp 编码逐字节相等
  - 日志出现 `AddDirectLinkPeer address=...`
  - 收到非法 `0x08` 负载时连接**不断**（`discardedMeter` 增长）
- **回滚**：纯新增 case + 新增包，删 case 即恢复

### M3+ R12 未来父块门禁（M3 期间并行评估，不阻塞 M3 交付，M2 不合入）

- **问题**：`Seal` 阻塞上界今天是 **21s**（父块允许超前 15s + `period` 3s + 剩余 deadline 3s ⇒ 14+6=20s 实测），因为 `Prepare` 会为"时间戳在自己时钟看来属于未来"的父块开工（`prepare.go:116`，且时间戳不能往下压）。门禁形如 `D = parent.Time − now ≤ K`，插在 `prepare.go` 的 **header 改写（`:74-84`）之前**——需先把 `parent` 解析从其现位置 `:86` 上移，不是"`startView`(`:114`) 之前"（那样被拒时 header 已被改写，破 D6；见 R12 对策格子里的实测）。
- **为什么不在 M2**：1.9.7 节点没有这道门禁、会为未来父块开工，单方面加会造成生产行为非对称（同一条链上老节点出块、我们拒绝），在互通验证阶段是噪音源；且 `D ∈ (0,12]` 的真实分布零运行时证据，此时加闸门等于用一个未测量的假设去抑制出块。
- **K 的三轴取舍**（详见 R12）：阻塞上界 ≈ `K+6`（保守 `K+9`）；验证方容忍的时钟落后 `≥ 12 − K`（`K ≤ 12` 即满足非负）；拒绝率随 K 单调下降（`K=0` 会拒掉 `D ∈ (0,12]` 这些本可被全收的父块，约每 12 块一次）。**K 是待定参数，`K=0` 只是其中一个角。**
- **准入条件（两条都满足才谈合入，缺一条不合）**
  1. **多节点 `D` 分布实测**：≥4 节点 devnet 连续出块 ≥1000 高度，记录每块的 `D`；量化"出块人时钟比中位时间快时，下一代是否真空等"及真空等时长占比。没有这份数据，`K` 取任何值都无依据。
  2. **与 1.9.7 节点互通回归**：同一 `D>0` 父块上，老节点照常出块；我们在该高度拒绝时链仍由老节点推进，无分叉、无停滞；且确认拒绝只影响我们自己的轮次，不影响我们对老节点块的验证与导入。**这一条必须用真实 1.9.7 节点跑，不能模拟**：1.9.7 的未来块判定是**零容忍**（`pgp-chain/consensus/pbft/pbft.go:395`、`:421`：`seal==true` 且 header 不在 DPoS block pool 里时，`header.Time > now` 直接 `ErrFutureBlock`，没有 `allowedFutureBlockTime` 那样的宽容窗），而我们是 15s（`consensus/pgp/pgp.go:56`/`:227`）。也就是说 R12 里那个 `12−D` 是**我们验证方**的余量，不是 1.9.7 网络的余量——`D>0` 时老节点可能直接拒收我们的块，与门禁是否存在无关。这条同时说明：`allowedFutureBlockTime = 15s` 相对 1.9.7 本身就是一个放宽，属既有事实，不在本项范围内改，但互通验收必须覆盖到。
- **合入时须一并交付**：新错误哨兵；benign 分类（否则每个被拒轮次会在全部节点打 Warn）；`TestPrepareRefusesFutureParent`（含 `view.IsRunning()==false` 断言）；**被拒时 header 零改写**——复用 `TestPrepareIsSideEffectFreeWhenRefused` 加第三个 subtest（前两个已覆盖"生产关闭"与"无密钥"两条拒绝路径），用 D6 的哨兵值断言 difficulty/extra/mixdigest/nonce 未被触碰；`seal.go` 死代码 `:195-197` 已清除。

### M4 DPoS TCP p2p + 全量 dispatcher（多生产者）

- **目标**：4 节点 4 生产者（阈值 `int(4*2/3) = 2`）互连，proposal → vote → confirm → 出块 → 广播，连续 200 块无分叉
- **验收**
  - 200 块 4 节点 hash 完全一致
  - `kill -9` on-duty 节点，剩余 3 节点 ≤ 2 view（≈12s）内补上
  - `pbft_getArbiterPeersInfo` 返回 3 个 2WayConnection
- **回滚**：`--pbft.enable=false` 退化为 M2 单节点；不建 `dpos.Network` 即无监听端口

### M5 与旧 1.9.7 节点同网互通（滚动升级）

- **目标**：1 新 + 3 旧混跑 500 块双向互认
- **验收**
  - 新出的块被旧节点接受，`extra` hex 逐字节比对一致
  - 旧出的块被新节点导入，`--verbosity 3` 无 `BAD BLOCK`
  - `pbft_getArbiterPeersInfo` / `pbft_getActivePeers` 可用
- **回滚**：新节点 `--pbft.enable=false` → 纯跟随（继续验块不出块），这是滚动升级的安全网

### M6 收尾：不可逆高度 / 观测 / 运维手册

- **验收**：`TestLegacyReorgBelowIrreversible` 通过；`pbft_confirmLatency`/`pbft_viewChanges`/`pbft_peerCount` 可见；本文档补"运维手册"章节

**依赖图**

```
M1 ──> M2 ──┬──> M4 ──> M5 ──> M6
       M3 ──┘
```

M3 与 M2 并行（都改 `eth/backend.go`，约定 M2 先落装配、M3 在函数尾部追加 bridge 注入）。

---

## 七、风险清单

| # | 风险 | 对策 | 验证 |
|---|---|---|---|
| R1 | **confirm 字节不兼容**（滚动升级最致命） | 出块侧只用 `payload.Confirm.Serialize`，与验证侧同一对函数；新增双向自洽测试 | §八 8.1 全组 |
| R2 | `MixDigest` 三重语义（legacy 要求 0 / Parlia 存毫秒 / upstream PoW mix） | `Prepare` 显式清零 | `Prepare` 后 `MixDigest == zero` 且 `MilliTimestamp() == Time*1000` |
| R3 | `header.Nonce` 双重语义（legacy = ELA 高度 / Parlia 清零） | 按高度路由隔离，禁止共用 code path | 分叉点前后各出 10 块 |
| R4 | `dpos/p2p` 在 go 1.25 下编译不过 | M3 第一步先 `go build ./dpos/...`；失败则回退 vendored copy + replace | CI gate |
| R5 | `protocolLengths` 窗口偏移（错一位就静默打乱 snap/bsc 协议路由） | 加 `0x08` 的 case **不需要**改 Length；断言 `Length == 17` | 单测 + `admin_peers` 看协议协商 |
| R6 | `Prepare` 报错打断 miner 空转 + legacy 无周期重试 | M2 的三处改动 | 单生产者 100 块无缺口；4 节点停一台 30s 能恢复 |
| R7 | 无 `GetIrreversibleHeight` → reorg 风险 | 短期靠 `verifySeal` 的 fork/double-sign 检测（`pgp.go:310-341`）；M6 补 core 守卫 | 同高度换块被拒 / 合法 view change（更大 offset）被接受 |
| R8 | `Finalize` 副作用污染导入路径 | 纯函数 + 副作用移到 `addConfirmToBlock` 与链头订阅 | 导入"stateRoot 错误"的块后 `finishedHeight` 不变 |
| R9 | **切 Parlia 时 `PoSA` 断言失效**：`eth.engine` 是 router，router 不实现 `PoSA` → votePool 不创建、fast finality 整体失效 | 记为 PoSA 阶段的独立工作项：届时让 router 在链头过分叉点后转发 PoSA，或整体替换 `eth.engine` | `parlia-fork-design.md` T4 需据此修订 |
| R10 | MEV/BidBlock 路径污染 PBFT 出块 | `worker.go:1560` 加 `Legacy == nil` | legacy 链不进 `selectBidBlock` |
| R11 | `header.Extra` 被 prefill | `Prepare` 置 nil + legacy 跳过 `SetExtra` | `--miner.extradata=abc` 测试 |
| R12 | `Seal` 阻塞导致关停挂死（`taskLoop` 同步调用，`exitCh` 期间 `interrupt()` 执行不到）。**阻塞上限不是 6s**：`viewChangeTime = parent.Time + signTolerance`，而父块时间戳允许超前 `allowedFutureBlockTime = 15s`（`consensus/pgp/pgp.go:56`，校验在 `:227`），于是"等到 `header.Time`"这一段（`seal.go:168-179`）可以吃满 18s——**实测父块 +14s 时 `Seal` 阻塞 20s**（pre-delay 17s + deadline 3s），所以 `seal.go:47-51` 与 `TestSealExitsWhenTheMinerStops` 里"nothing here may wait longer than one signTolerance window"这句**曾经是假的**（**状态更新（代码锚点 `b5861d7b4`）**：`seal.go:46-62` 的头部注释已按分段改真——pre-delay ≤ `allowedFutureBlockTime + period` = 18s、`deadline` ≤ 3s（re-anchor ≤ 6s）、total ≈21s，并写明"不做 clamp、不在 `header.Time` 之前释放"是互通要求；**残留一处**：`seal_test.go:613-614` 的 doc comment 仍写 "bounds a hung view to one signTolerance window"，该用例的断言本身已改成 1s（`:678-681`，合理），但这句把整个调用的阻塞说成一个窗口，仍需改成分段描述）。**根因不在 `Seal` 等太久，而在 `Prepare` 肯为"时间戳在未来的父块"开工**：`header.Time = parent.Time + period`（`prepare.go:116`），且不能往下压——`pgp` 拒 `parent.Time + period > header.Time`，`prepare.go:117-123` 的 re-anchor 只在 `parent.Time + period < now` 时触发。设 D = `parent.Time − now_producer`（`verifyHeader` 允许 D ≤ 15），则 T = now+D+3、`Seal` 必等 D+3 秒；**我们验证方**的接受条件是 `now_v ≥ now+D−12`（15s 窗口是我们自己的，1.9.7 是零容忍，两者不可代入——见 R18），即**容忍的时钟落后 = 12−D**，D=15 时为 −3——子块生下来就没有在线验证方能收。**这个损失与 `Seal` 怎么等无关**：`verifyHeader` 只看 `header.Time`（`pgp.go:227`），改等待不改变任何人的判定 | 换杠杆，夹 `Seal` 是死路：**在 `Prepare` 里禁止为"未来父块"开工**——位置是**在 header 改写之前**（`prepare.go:74-84` 的 Difficulty/Extra/MixDigest/UncleHash/Nonce 改写之前；`nowTime` 在 `:59-62` 已就绪，但 `parent` 现在在 `:86` 才解析，**必须把这段解析连同其 `ErrUnknownAncestor` 检查一起上移**），加一道 `parent.Time > nowTime` 即返回错误（新错误如 `ErrParentInTheFuture`）。**写成"`startView`（`:114`）之前"是不够的**——`:114` 在改写之后，门禁放那里会让"被拒"的 `Prepare` 已经改过 header，直接破掉 `prepare.go:42-48` 自己声明的 D6 不变量（"refused Prepare 必须不动 header"）和守着它的 `TestPrepareIsSideEffectFreeWhenRefused`（实测：父块 +5s 时 difficulty/extra/mixdigest/nonce 四个哨兵全被改写）。`startView` 只是**第二个**理由：`:146` 会 `SetRunning()`，在它之后返回错误会留下 Running 却无轮次的永久停摆（B1）。顺带收益：`parent` 上移后，`ErrUnknownAncestor` 这条拒绝路径也变成零改写。**同一提交还必须改掉 `prepare.go:91-92` 的那句注释**——原文 `// The remaining gates cannot move above the rewrite: they depend on the parent header or on the view this round is about to open.` 本身就是一条假的约束：`parent == nil` 这道门禁只依赖 `header.ParentHash` 与 `header.Number`（miner 在调 `Prepare` 之前就设好了），既不依赖改写、也不依赖视图；它今天之所以在改写之后返回 `ErrUnknownAncestor`（`:86-88`），唯一原因是 `parent` 的解析被放在了改写之后——而这正是本节要挪开的东西。留着这句话的后果很具体：实现者读到它就会心安理得地把门禁放在改写之后，也就是本节刚修掉的那个 D6 违例。（这条由 team-lead 核 `Prepare` 实际语句顺序时指出，属本轮在清的同一族"注释声称强于代码"。）门禁形如 `D ≤ K`，K 拉动三个轴：①阻塞上界 ≤ `K + period + signTolerance`（保守值，两项都取上界；名义值 `K + period + period` = K+6，因为 `deadline` 是在 pre-delay 之后取的、恒 ≈3，两个上界不可能同时吃满）⇒ K=0 时为 6~9s；②验证方容忍的时钟落后 `≥ 12 − K`（`K ≤ 12` 才保证 ≥0——所以把 K 压到 0 不是为了余量，余量在 K=12 时就够）；③**拒绝率随 K 单调下降**：`D ∈ (0,12]` 的子块时间戳 `now+D+3 ≤ now+15`，在线验证方本来全收，`K=0` 把它们全部拒掉——而 `D>0` 恰恰是"上一个出块人时钟比网络中位时间快"的常态（12 个生产者里有一个快，它出的每一块都让下一块落在 `D ∈ (0,12]`），不是极端场景。所以 **K 是三轴取舍，`K=0` 只是"最耐时钟偏移 + 最短阻塞 + 拒绝最多"的那个角，不是唯一解**；上一版写成"唯一解"是把 K 当成无代价参数、只算了两轴，属过度声明。**裁决（team-lead）：M2 不合入。** 理由不是方案不好，而是生产行为非对称（1.9.7 节点没有这道门禁、会为未来父块开工，我们单方面拒绝会在互通验证阶段变成噪音源），且 `D ∈ (0,12]` 的实际分布零运行时证据；立项与准入条件见 §六 M3+。仍然成立的两条否决路径：①夹 `Seal` 的 pre-delay——**这条路不是"效果差"，是"不能走"**：1.9.7 对未来块零容忍（`pgp-chain/consensus/pbft/pbft.go:395`/`:421`；其正常入链路径恒 `verifySeals=true`，`pgp-chain/core/blockchain.go:1535`/`:1992`/`:2006`），所以这段等待**不是缺陷，但它保的不是"对方能不能收下"，而是"对方能不能当场入链"**——两层要分开：**验证器层**零容忍（1.9.7 `pbft.go:395`/`:421`）只决定"收到这一刻收不收"；**节点层**双方都有未来块队列（我们 `core/blockchain.go:2404`/`:2561` → `addFutureBlock:2224-2235`、`updateFutureBlocks:3356-3357`；1.9.7 `pgp-chain/core/blockchain.go:1664` → `addFutureBlock:1491-1498`、5s ticker `:2223-2224`），所以提前广播**不丢块**，代价是它在 peer 的队列里躺到 `header.Time` 之后（最坏 `D+3−B` 再加 ≤5s 的 ticker 粒度），而这段延迟里**没有任何节点能在该高度之上出块**——真实代价是这段停等，不是"打回"。**真风险另有其处**：`header.Time > peer_now + maxTimeFutureBlocks(30)` 时 `addFutureBlock` 直接返回错误、**不入队**（1.9.7 `:1493-1495`、我们 `:2225-2228`），那块对该 peer 才是真丢——这条见 R18。即使不考虑互通，夹也换不到余量（不改变 `header.Time`）。**所以 21s 只能从源头收窄**：靠 Prepare 门禁把 D 压到 ≤K，不要在 `Seal` 里夹。**另须记住：`allowedFutureBlockTime = 15s`（`pgp.go:56`，判定在 `:227`）是我们验证器的宽容度，不是 1.9.7 网络的宽容度，两者不可互相代入**（见 R18）；②在 `Prepare` 里把时间戳往下压——被 period 校验判死。另：`seal.go:195-197` 那个 `if deadline > signTolerance` **不可达**——两条独立证据：①**解析**：`deadline` 在 pre-delay 之后取，future-parent 分支恒 ≈3s，re-anchor 分支 ≤6s（= `signTolerance`），故 `> signTolerance` 永不成立；②**实测（来源 QA，非本文档作者，也非工程师）**：parent 偏移 −30s…+15s 全表，`deadline ∈ [2s, 5s]`，该分支一次都不成立（5s 那档属 re-anchor 分支，与解析上界 6s 相符）。**署名须保留 QA**：工程师没跑过这组数、并拒绝把它写进自己的 commit message，这个拒绝是对的，别把实测记到他名下。其注释 "this clamp is what makes it true" 与事实相反，不要把它当作预算实现——**已于 `b5861d7b4` 删除（M2 出厂前）**（`seal.go` 里不再有这个分支，也不再有任何 `deadline` 上限夹）；`seal.go` 的头注释同时按本节要求改写成真实上界（pre-delay ≤ 18s、`deadline` ≤ 3s、re-anchor 分支 ≤ 6s、总 ≈ 21s），并写明 pre-delay 是 1.9.7 互通的前提、不得夹；`TestSealExitsWhenTheMinerStops` 的注释补了"本用例无 pre-delay"这一前提 | **M2 出厂状态**：不合入门禁，阻塞上界仍是实测的 21s——所以 `seal.go:47-51` 那句"nothing here may wait longer than one signTolerance window"已改成真实上界（`b5861d7b4`），**理由不是"我们没做好"，而是这段 pre-delay 本身是 1.9.7 互通的必要条件，注释必须说明它为什么必须存在**（M2 不能带着假注释出厂），`TestSealExitsWhenTheMinerStops` 也已注明"本用例无 pre-delay"这一前提（`b5861d7b4`），且不得断言"一窗口内返回"。**M3+ 准入通过后才加闸门**：`TestPrepareRefusesFutureParent`（父块 +1s → `Prepare` 返回错误**且** `view.IsRunning() == false`，后半句锁住"门禁必须在 `startView` 之前"，否则 B1 复发）——放 `Prepare` 侧是为了确定性：producer 用 `GetNowTime()`（`dtime.MedianTimeSource.AdjustedTime`，`dpos/dispatcher.go:576`）、验证方用 `time.Now()`，在 `Seal` 侧断言"慢 4s 的验证方"必然 flaky。另：`TestSealHonoursStopDuringPreDelay`（`consensus/pbft/seal_stop_delay_test.go`，QA 出）覆盖的是**pre-delay 期间对 `stop` 的响应**，不是阻塞上界——它把 `header.Time` 直接推到 now+4s 而不用 parent+15s（后者会被 M3+ 门禁拒掉，用例得改一行才能跑），与本节闸门不重叠、不要合并。**本节事实锚点**：以上"已删/已改"对应代码 `b5861d7b4`（删 clamp + `seal.go` 头注释改真 + `seal_test.go` 注释补前提）；**`ab424d622` 及更早的提交里 clamp 仍在**——只 checkout 到那里时，本节"已删"的陈述不成立，勿据旧提交认定文档与代码矛盾 |
| R13 | SPV 事件链缺失 → 生产者集永不更新 | M5 在 `onBlockHandled` 的 `isNexturnBlock` 分支回调 `dpos.SetNextProducers` | ELA 高度过 `dynamicArbiterHeight` 后集合切换 |
| R14 | Elastos `hub.New` 注册全局 SIGINT、`ETDirectPeersChanged` 走全局事件总线 | 只创建一个 `dpos.Network`；`ETDirectPeersChanged` 必须继续走 Elastos events（上游代码改不动） | devnet 多节点观察 |
| R15 | **签名必崩**：`Elastos.ELA/crypto.Sign` 只填 `ecdsa.PrivateKey.D`，不填 `PublicKey.X/Y`。Go 1.20（pgp-chain）下无害；Go 1.24+ 起 P-256 走 FIPS 模块，`privateKeyToFIPS` 解引用 nil X → SIGSEGV。bsc 是 `go 1.25.0` | `dpos/signer.go` 自建 `p256Account`：从 D 补全公钥后调标准库 `ecdsa.Sign`，输出仍是同样的 `r‖s` 64 字节 P-256 签名，旧节点可验；`GetDposAccount` 改为返回该实现 | `TestSignProposalVerifiesWithElastosCrypto`（含翻位负样例） |
| R16 | 出块侧漏登 proposal：`dpos.StartProposal` 只签名不登记，投票全部被拒 → 每轮必然超时（静默，只打 debug 日志） | `Seal` 里 `StartProposal` 之后必须 `dispatcher.ProcessProposal(self, proposal)` | `TestSealProducesABlockTheReadOnlyVerifierAccepts` 走真实投票路径 |
| R17 | **阈值 ≤1 的生产者集把自己的 confirm 清掉 → 该高度永久卡死**（M2 devnet 唯一一次卡死就是它）。边界是 `legacyMajorityCount(n) = int(float64(n)*2/3) <= 1`（`consensus/pgp/confirm.go:254-256`），即 **n=1（阈值 0）和 n=2（阈值 1）都挂**，n=3 起正常（n=12 阈值 8，实测无影响）——不要只测 n=1 就以为覆盖了。在这种集合里提案者的自投票在 `dispatcher.ProcessVote`（`seal.go:161`）里**同步**走到 `onConfirm` → `trySendConfirm` 写入 `confirmCh`。若轮次 drain 放在这次自投票之后，`drainRoundChannels` 就会把刚产生的 confirm 吃掉 → 每轮等满 deadline 超时；更糟的是 `blockPool.AppendConfirm` 已记录该块，重试会撞 `ErrAlreadyConfirmedBlock`（`seal.go:74` 起）→ 链永久停在该高度 | drain（`drainRoundChannels` + `isSealOver=0`）必须在 `Seal` 内**任何可能产生 confirm 的动作之前**，即早于 `ProcessProposal`（`seal.go:112`）和自投票（`:156`/`:161`）。**语义上**："清上一轮遗留"与"本轮开始"是两个时刻，"本轮"的起点应由 `isSealing=1` 界分（这正是 `onConfirm` 已有的那道闸），所以依次做 `drainRoundChannels()` → `isSealOver=0` → `isSealing=1` 最紧。修复前它位于 `seal.go:165-166`，这正是本 bug；工作区已按上序落地（`drainRoundChannels` 在 `:149-150`，早于自投票 `:156`），不要重复"修"它。附带一条同源教训：轮次通道只能 **drain，不能 close**——`trySendConfirm` 会先取字段再发送，close 会让在途发送 panic | 闸门（M2 必须落地）：单生产者单轮出块用例（命名待定，`TestSingleProducerSealsInOneRound` 在树上**不存在**），**必须覆盖 n=1 与 n=2，且每个 n 连做高度 1/2/3**。只测高度 1 会假通过——"卡在高度 1"正是本 bug 的症状，只跑高度 1 的用例无法把它和"正常出完一个块"区分开。**测试前提**：现有 `sealTestChain`（`seal_test.go:49-80`）只服务创世，高度 2 直接 `ErrUnknownAncestor`，所以这条用例要求先把 harness 改成会记住自己封出的块（或显式喂 `OnBlockImported`），否则写不出来 |
| R18 | **未来块判定是规则级分歧：我们比 1.9.7 宽松 15s**。1.9.7（互通目标）`header.Time > now` 即 `ErrFutureBlock`，**零容忍**：`pgp-chain/consensus/pbft/pbft.go:395`（`VerifyHeaders`，`seals[i]=true` 时无条件生效，且位于 block pool 短路 `:399` **之前**，pool 救不了）、`:421`（`verifyHeader`）；其正常入链路径恒 `verifySeals=true`（`pgp-chain/core/blockchain.go:1535`/`:1992`/`:2006`），老节点没有任何豁免通道。本仓 `consensus/pgp/pgp.go:56` `allowedFutureBlockTime = 15s`，判定在 `:227`（`now+15s < header.Time` 才拒）。⇒ **`header.Time ∈ (now, now+15]` 的块：我们当场接受、老节点当场拒收**（"当场"两个字很要紧，见下）。**分三档写，不要写成"不是分叉"一句话**（"不是分叉"这个结论过强，team-lead 驳回了我上一版）：**①`header.Time ∈ (now_v, now_v+30]`（当场拒、可入队）**：老节点拒收后同样入队重试（`pgp-chain/core/blockchain.go:1664` → `addFutureBlock:1491`，5s ticker `:2223-2224`），时钟走过 `header.Time` 后能入链 ⇒ **不构成永久分叉**。**补一句精度**：这一档上下两段的性质不同——`(now_v, now_v+15]` 才是"我们当场收、1.9.7 当场拒"的真分歧带；`(now_v+15, now_v+30]` 里**我们自己也**返回 `ErrFutureBlock` 并入队（`core/blockchain.go:2404`/`:2561`），两边行为一致，那段没有规则分歧。也就是说 R18 这个 15s 差异的**有效作用带只有 15s 宽**，不要按 30s 去估它的影响面。**②但同一档里"短暂落后"不等于"无害"**：`updateFutureBlocks` 的 ticker 粒度是 **5s**，而块周期是 **3s**——未来块在队列里躺的最坏时长**超过一个出块槽位**。对端落后期间，如果它自己正好是下一个高度的 on-duty 生产者，它在队列排空前就**已经能基于 N−1 产出自己的 N 块**（这不是推测，是 `Prepare` 的形状决定的：1.9.7 `pbft.go:560` 与我方 `prepare.go:86` 都只按 `header.ParentHash` 取 `parent`，对端手里有 N−1 就够了；更关键的是 `p.Start(parent.Time)`（1.9.7 `:580`）/ `e.startView(parent.Time, headHeight)`（我方 `prepare.go:114`）用的是**同一个** N−1 时间戳作为轮次锚点，所以它算出的 on-duty 与全网一致——它不会"觉得自己不该出块"，它会**正确地认为自己该出块**；1.9.7 末尾那道 `if !p.IsOnduty()`（`:586`）也拦不住它，因为它是在 `p.Start(parent.Time)` **之后**才判的，用的正是同一个锚点）⇒ **可能出现同高度竞争块**，其消解取决于 confirm/vote 与 reorg 逻辑。**这一档的验收不能默认自愈**，必须单独验。**③`header.Time > peer_now + maxTimeFutureBlocks(30)`**：`addFutureBlock` 返回错误、**不入队**（`:1493-1495`，我们这侧同理 `core/blockchain.go:2225-2228`），该块对该 peer 永久丢弃，后续块因父块缺失也一并被丢——**只有这一档才是"分叉"**，等价于"对端时钟落后 > `30 − (D+3)`"，在我们 15s 宽容下最坏是落后 > 12s。这个 15s 在本仓的唯一先例是 ethash（`consensus/ethash/consensus.go:45` `allowedFutureBlockTimeSeconds = 15`，PoW），而 BSC 自己的 Parlia 同样零容忍（`consensus/parlia/parlia.go:585-587`）——我们这条 legacy 规则比**两边**都宽松 | **M2 不改**，交 M3 互通实测决策。理由：诚实出块人不会产出这种块（都等到 `header.Time` 才广播，这正是 `Seal` 的 pre-delay，与 1.9.7 `pbft.go:692` 同一个理由），所以是理论场景；但"我们的规则比被替代的共识更宽松"在线级互通这个目标下必须显式记录，不能靠"不会发生"。**收紧到 0 的代价**（也是现代实现普遍保留宽容窗的原因）：出块人时钟略快时，其块会被略慢的验证方判为未来块拒收 → 变成出块失败与 view 轮转；宽容窗就是用来吸收真实时钟抖动与传播延迟的，收到 0 等于要求全网时钟严格一致。**另记一条边界：切 Parlia 时规则收紧**——Parlia 零容忍（`parlia.go:585-587`），已入链的块不会被回溯校验，但分叉点之后的第一批块要求 `header.Time ≤ 各验证方 now`，15s 的宽容不延续 | 两侧行号可静态核对。M3 devnet 须**分三档**验，**不要笼统问"是否分叉"**：①构造 `header.Time ∈ (now_v, now_v+30]`（如 now+10）→ 确认我们 `VerifyHeader` 通过、1.9.7 返回 `ErrFutureBlock`，且对端**在队列里**（不是丢弃）——这一档判的是"有没有永久丢"，判据是队列排空后追平；②同一档的**竞争块**必须单独验：让那个落后对端在落后窗口内正好轮到 on-duty，看它是否产出同高度块、以及 confirm/vote 如何消解——**不许用"ticker 内自愈"把这一档跳过**；③构造 `header.Time > now_v+30`（等价对端时钟落后 >12s）→ 确认对端**永久丢弃**，只有这一档才预期停滞/分叉。三档用的时钟偏移要记进 M3 报告，收不收紧由实测的时钟偏差与 `D` 分布决定——与 §六 M3+ 准入条件 1 共用同一份数据 |
| R19 | **票数 map 无锁重建 → `fatal error: concurrent map read and map write`，进程直接死**（不是 race 警告，Go 运行时不走 panic 恢复、可能完全不留栈）。`CleanProposals`（`dpos/dispatcher.go:358-376`）在 `FinishedProposal` 释放 `finishedMu` **之后**把 `acceptVotes`/`rejectedVotes`/`pendingVotes`/`precociousProposals` 整体重新 `make`（`:361-363`、`:366`、`:370-373`），而读写这些 map 的路径**并不都持有同一把锁**。**两条独立交错**：①`FinishedProposal` 的两个调用者同时进入——`Seal → addConfirmToBlock → FinishedProposal`（miner 协程）与 `pbftLoop → OnBlockImported → FinishedProposal`（链头协程）；②**不需要两个调用者**——`CleanProposals` 的整体替换 vs `processVote` 的 `acceptVotes[hash]=vote`（`:206`）/`rejectedVotes[hash]=vote`（`:219`），只要有对端投票落在清理窗口内就撞。**另有三处同样无锁，团队原稿未列**：`ProcessProposal` 写 `precociousProposals`（`:85`）；`ResetAcceptVotes`（`:384-386`）与 `UpdatePrecociousProposals`（`:388-395`）——后两个在本仓**零调用者**，但上游在 `pgp-chain/consensus/pbft/network.go:1080` 与 `pgp-chain/consensus/pbft/pbft.go:1182` 调用它们，M3 接 p2p 时会被复活成测试覆盖不到的竞态路径。**可达性（决定排期；写机理，不写结论）**：**M2（无 transport）= 不可达**，三条机理叠加：①`ProcessVote` 全仓唯一非测试调用点是 `seal.go:193` 的自投票，与 `FinishedProposal` 同协程；②链头那次 `FinishedProposal`（`engine.go:364`，来自 `OnBlockImported`）被水位守卫 `height <= finishedHeight`（`dispatcher.go:257-261`）短路 `return`，**根本到不了 `CleanProposals`**——因为 `addConfirmToBlock`（`seal.go:315`）在块交给 miner **之前**就把水位推到了 h；③无外部块、无对端投票。**这条不再只是代码推导，它在生产日志里有签名（team-lead 亲自数的）**：`~/.pgp-devnet/logs/geth.log`（13:51:58 → 14:08:50，收到高度 107）里 `Successfully seal and write new block` **141** 次、`WARN FinishedProposal received fork` **141** 次、`seal timed out` **0** 次——**严格 1:1**。这一比一同时钉住两件事：链头那次 `FinishedProposal` **每块都真的来了**（否则 WARN 会少于出块数），而且**每次都真的被 `:257-261` 挡在 `CleanProposals` 之外**（否则要么 WARN 数对不上，要么出现 `seal timed out`）。两个方向的漏都能被它抓出来：若"来了但没被挡住"，就是 R19 已经在跑；若"挡住了但没来"，是我上面第②条机理写错了、M3 不该按"必然可达"排。**M3（接 DPoS p2p）= 必然可达**：①对端投票来自网络协程（上游 `network.go:756`/`:1082` 即此路）→ `processVote` 出现多协程调用者；②follower/追赶/重启补 confirm 会让链头那次 `FinishedProposal` **真的越过水位守卫**、走到 `CleanProposals`。**必须写机理而不是只写结论**——只写"M2 不可达 / M3 可达"会让读者推出两个相反的错误："单机 devnet 可能撞上"（不会，别拿长跑没撞当证据）、"接了 p2p 也安全"（会死）。**现状须诚实记录**：竞争**仍然存在**；`-race` 现在绿，是因为没有用例把这两条路径交叉驱动，不是因为不存在 | **不要新增 `votesMu`——用已有的 `mu`**。`dpos/dispatcher.go:34-38` 的锁序注释原文就是 "`mu` guards the vote maps"，且 `AddPendingVote`（`:164-166`）、`ProcessVote`（`:172-174`）、`setProcessingProposal`（`:150-159`）都已在用它；给同一批字段再添一把锁，正是 `:47-52` 自己点名的缺陷（"giving one field two locks is the defect, not the fix"）。做法：`CleanProposals` 在 `setProcessingProsalNil()`（`:360`，取 `proposalMu`，内部自闭合）**返回之后**再取 `mu`，把四张 map 的替换与 `proposalProcessFinished` 的重置一起包住，两把锁**不嵌套**（与 `setProcessingProposal` 已有的"先 proposalMu 放掉、再 mu"模式一致，锁序天然满足"`mu` 从不在持有 `proposalMu` 时获取"）。**另三处无锁点的处置已定，不留"或"**：①`ProcessProposal` 写 `precociousProposals`（`:85`）**纳入 `mu`**（M3 起由 p2p 路径进入）；②`ResetAcceptVotes`（`:384-386`）**删除**；③`UpdatePrecociousProposals`（`:388-395`）**删除**——删而不是留着加锁，因为它们的风险正是"今天死、M3 复活"，留着加锁只是把看不见的问题变成看得见但没人测的问题，git 历史能找回（与本项目删 `ErrNilChain` 同一判断）；M3 需要时带锁重引入。④`proposalProcessFinished` **只锁 `:364` 不够**：它的写点 `SetProposalProcessFinished`（`:126-128`）与读点 `GetProposalProcessFinished`（`:111-113`）同样无锁，三处必须一起纳入 `mu`；该字段本仓同样零调用者、上游 `network.go:712`/`:714` 调用（M3 复活），若按②③的同一判断处理则应连字段一起删——锁或删都可，**但不能只锁 `:364`**。**不能扩 `finishedMu`**：`:38-52` 明确它是最外层、从不与 `mu`/`proposalMu` 嵌套；扩过去就是破坏已确立的锁序。**不能把锁跨过 `:269-272`**：尾巴经 `ChangeView → listener.OnViewChanged → miner.RequestNewWork` 回调外部包，持锁调外部未知代码，死锁面无法静态排除。**取 `mu` 后必须守住的不变量**：`onConfirm`/`unConfirm` 回调链上不得同步调用 `CleanProposals`（Go mutex 不可重入，而 `ProcessVote` 持 `mu` 时会同步走到 `createConfirm → onConfirm`；今天 `engine.onConfirm`（`seal.go:388`）只做 `trySendConfirm` 的 channel send，所以安全——这条要写进注释，不能只靠今天的调用图）。**锁序登记——按现有边图写，不要用"永不嵌套"这种粗口径**：现存**唯一嵌套边是 `mu → proposalMu`**（`processVote:179`、`createConfirm:440`、`createUnConfirm:463` 在持 `mu` 时经 `GetProcessingProposal:487-491` 取 `proposalMu`），这条**允许且必须保留**；**禁止的只有反向边 `proposalMu → mu`**（取 `mu` 时不得持 `proposalMu`）——文件 `:34-38` 的 "never taken while `proposalMu` is held" 就是这个意思。`finishedMu` 在 `FinishedProposal` 里于 `:267` 先释放、`:270` 才调 `CleanProposals`，不产生 `finishedMu → mu`；三个调用点（`:270`/`:285`/`:300`）进入时都不持 `mu`。所以本补丁是**把 `CleanProposals` 拉回文件已声明的锁序，不是改锁序**。**"把 `CleanProposals` 里两段合成一段锁"不是风格问题，是 ABBA 死锁**：那样 `CleanProposals` 会持 `proposalMu` 求 `mu`，而 `ProcessVote` 正持 `mu` 经 `GetProcessingProposal` 求 `proposalMu`，双方各等对方 | 闸门：①**先把竞争写成两条，不要用"4 个并发 writer 打 `FinishedProposal`"这种构造**——`FinishedProposal` 的尾巴每次都 `SetReady` + `UpdateDutyIndex` + `ChangeView`（`dispatcher.go:268-272`），那条用例会**先红在 `ConsensusView` 的裸字段上（R20），红点不在 map 上**；加了 `mu` 也不会转绿，等于拿一个错误的红灯去验一个正确的补丁，最后只能把用例删掉或降级。（这个坑由 kou-dou-ma 在核 `GetViewOffset` 时发现，他据此撤掉了原来那版并发用例。**注意那条构造没有废弃，它归 R20**——#17 的验收第 4 条把它当作"恢复 4 写者"的判据，那里它是对的：R20 修的就是视图锁本身。同一个用例在两个票里判的是两件不同的事，不要因为本条不用就把它删掉。）正确的两条：①a `CleanProposals(false)` × `ProcessVote` 交叉——`changeView=false` 分支（`:366`）只重建 map、**完全不碰 `consensusView`**，红灯只可能落在本条；①b 若要覆盖多 writer，用 `ResetConsensus`/`ResetConsensusForEpochTransition` 之外另构造，不许经过 `ChangeView`；②新增专门打第二条交错（无需第二个调用者）的用例——现有 `TestFinishedHeightRace` 只覆盖 `FinishedProposal` 的多个 writer，打不到这条；③**变异验证**：去掉 `mu` 后新用例必须变红，否则只是加了一把没人用的锁、`-race` 绿是假绿；④`go test -race -count=10 ./dpos/` 绿。**排期**：M2 验收通过之后、M3 开工之前——理由不是"M3 并发面变宽"，而是 **M2 由机理保证不可达、M3 必然可达**（见上方可达性三条机理）。措辞上两个方向都不能错：写成"M2 也可能撞"会让人拿长跑没出事件当反证；写成"只是并发面变宽"会低估 M3 的必然性。**M3 的观测判据（必须写进 M3 验收，否则运维会误判）**：M3 上线后那行 `WARN FinishedProposal received fork` 应逐块降到接近 0。**它归零的那一刻，正是 `CleanProposals` 开始跑在链头协程上、本条暴露面从"零"变成"每块一次"的时刻**——WARN 消失不是"系统变干净了"，是风险从 0 变成 1。所以顺序是硬的：**本条的 `mu` 必须先落地，再让 M3 去打通那条路径**。反向也要写清楚：如果 M3 上线后 WARN **仍逐块打印**，说明链头 `FinishedProposal` 仍被水位守卫短路，即 M3 的入链/confirm 路径没真正打通——那是另一个问题（比 R19 更严重），不要记成"R19 还没触发"。**现场指纹（给运维）**：`eth_blockNumber` 停住 + 进程消失 + 日志尾部只有正常 PBFT 行、无 Warn/Error、退出码非 0 → 先怀疑本条，不要先怀疑出块逻辑；撞上时保全现场（日志、退出码、datadir 清单），别重启覆盖 |
| R20 | **`ConsensusView` 全部状态字段无锁，却是三个协程的共享写目标**（R19 的邻缺陷。**两处独立发现，须并列署名**：我在核 `Seal` 的 deadline 与签名域读链时认定，kou-dou-ma 在核 `GetViewOffset`（`:247-249`）裸读 `viewOffset` 时认定，结论一致。分工要说清以免后人误判覆盖面——**他只核了 `dispatcher.go` 内的调用面**，`prepare.go` / `seal.go` / view loop 这三条读写链是我读代码得出的，**`dpos/` 全仓的读写点没有人做过穷尽核对**；动手加锁前必须按访问器名逐个 `grep` 一遍，不要拿本条当清单用）。`dpos/consensusview.go:35-46` 的 `consensusStatus`/`viewOffset`/`viewStartTime`/`viewChangeTime`/`isDposOnDuty` 都是裸字段，全部访问器无一取锁：`resetViewOffset:48`、`SetRunning:52`、`SetReady:56`、`IsRunning:60`、`UpdateDutyIndex:184`、`ChangeView:191-216`、`GetChangeViewTime:239`、`GetViewOffset:247`、`ResetView:251`、`SetChangViewTime:255-259`。**三个并发写方**：①view loop 协程（`consensus/pbft/view.go:80` 起 `viewTick = 1s` 的 ticker，`view.go:113` 与 `TryChangeView` → `ChangeView`）；②miner 协程（`prepare.go:135-147 startView` → `SetChangViewTime`/`ResetView`/`SetRunning`）；③链头协程（`dispatcher.go:268-272` `FinishedProposal` 尾巴 → `SetReady`/`UpdateDutyIndex`/`ChangeView`）。读方包括 `Seal`（`seal.go:224` 用 `GetChangeViewTime()` 算 deadline、`:119` 用 `GetViewOffset()` 定签名域）与 `CleanProposals`（`dispatcher.go:369`）。**后果比 R19 轻但同样落在共识判定路径上**：`viewChangeTime` 是 `time.Time`（多字），撕裂读会拿到垃圾时间 → `deadline` 变负或极大 → `Seal` 立刻 abort 或长时间阻塞；`viewOffset` 撕裂 → 轮次归属错乱、双提案风险。**不是 fatal**（不像 map 会杀进程），所以不会在 devnet 上自动暴露。**当前 `-race` 绿不构成证据**：实测 `go test -race -run TestFailedRoundDoesNotStallTheNode ./consensus/pbft/`（3.4s）无报告，原因是 `viewTick = 1s` 且 `ChangeView` 只在 `offset > 0` 时写，**现有用例没有稳定跨过这个窗口**——这是覆盖缺口 | **需独立立项**（#13 明确划在 R19 之外；**已立项为 #17，owner kou-dou-ma，requirement 见票**；票里已把本节的三条前置抄进去：#13 先落、开工前自行穷举访问面、`ChangeView` 的 `OnViewChanged` 必须放到放锁之后）。推荐 **A：给 `ConsensusView` 加一把 `sync.RWMutex`，只护这 5 个裸字段**，`producers`/`listener` 不动；`ChangeView` 持锁时**只更新字段，`OnViewChanged` 回调必须放到放锁之后**——否则又变成"持锁调外部"（`OnViewChanged → miner.RequestNewWork`），与 R19 那条同型。B：不改 `ConsensusView`，把 view loop 的 tick 与 `Seal`/`Prepare` 的读串行到同一协程——架构改动大、且会把 `Seal` 绑到 tick 上，不推荐。**排期**：与 R19 同批（M2 验收后、M3 前），排在 R19 之后紧接；理由是 M3 会再加入网络协程这个**第四写方**，而它是唯一一条"不 fatal、只能靠 `-race` 发现"的缺陷。**与 R19 的闸门①互为硬约束**：R19 要求"先造红"，但任何经过 `FinishedProposal` 尾巴的并发构造都会先红在本条上，所以 R19 的用例必须走 `CleanProposals(false)` × `ProcessVote`；反过来，本条的"跨 tick 造红"用例正好可以给 R19 当变异对照。**两条必须在同一批里做完再开 M3，不要分两次动 `dpos/`** | 闸门：①**必须先造红**——新增一条跨 tick 的用例（至少等 2 个 `viewTick`，且能让 `ChangeView` 的 `offset > 0` 分支真的执行），`-race` 下必须报出竞争；②加锁后转绿，并做**变异验证**（去掉锁必须变红）；③`go test -race -count=10 ./dpos/ ./consensus/pbft/`；④**devnet 必须用 `-race` 构建的二进制跑一段**——#13 的"107 块 0 fatal"不能当无竞争的证明 |

---

## 八、测试策略

### 8.1 golden 向量双向自洽（最重要）

| 测试 | 方向 | 断言 |
|---|---|---|
| `TestGoldenConfirmDecodesRealMainnetExtra`（已存在） | 主网块 → pgp 验证 | 12 条全绿（回归基线，**不得改动**） |
| `TestGoldenConfirmSerializesBackToTheSameBytes` ✅ | 主网 Extra → `payload.Confirm` → `Serialize` | 输出逐字节等于原 `extra`；再解一次仍是同一 confirm。**这是出块侧可被旧节点接受的全部依据** |
| `TestConfirmWireLengthIs1199For8Votes` ✅ | 常量 ↔ 主网数据 | `ConfirmMinLen` 自洽 + 8 票 = 1199，且 12 条真实块都符合该公式 |
| `TestSealProducesABlockTheReadOnlyVerifierAccepts` ✅ | pbft 出块 → pgp 验证 | 12 生产者 / 阈值 8 / 真实投票 / Extra 1199B / `VerifyHeader` 通过 / `GetIrreversibleHeight()==1` |
| `TestSignProposalVerifiesWithElastosCrypto` ✅ | 签名 → `crypto.Verify` | 通过，含翻位负样例（R15 闸门） |

### 8.2 迁来的测试

| 源 | 处理 |
|---|---|
| `dpos/{proposal,proposalvote,message,producers,blockpool}_test.go`、`dpos/msg/*_test.go` | **直接跑**，只改 import 路径 |
| `dpos/dispatcher_test.go`、`dpos/routes_test.go` | 小改（去 chainbridge 初始化） |
| `consensus/pbft/pbft_test.go` | **重写**为 `Prepare/Seal` 窄测（fake `ChainHeaderReader`，可复用 `pgp_test.go` 同类） |
| producer_stats 相关 | 不迁移 |

### 8.3 devnet 验收清单

```
[1 节点 / 1 生产者 / 无 P2P]
  - 100 块间隔 ∈[3.0,3.3]s；extra 可解；8 票 = 1199B；重启续块
[4 节点 / 4 生产者 / DPoS TCP + devp2p]
  - 200 块 hash 一致；kill -9 后 ≤2 view 恢复；iptables DROP 后自动重连追平
[混跑 1 新 + 3 旧]
  - 500 块双向互认；新节点 --pbft.enable=false 时纯跟随不报错
[负向]
  - 改签名字节 → ErrInvalidConfirm
  - 同高度同 viewOffset 不同块 → ErrDoubleSignBlock
  - 同高度更小 viewOffset → ErrChainForkBlock
  - 未来时间戳 +30s → ErrFutureBlock
[race —— 只由 R20 触发，见 #17]
  - 用 -race 构建的二进制，单节点跑 ≥30 连续高度（目标 100）
  - 判据只有一条：日志里无 `WARNING: DATA RACE`
  - 不判出块间隔、不判 seal timed out、不判 view churn（-race 会拖慢，
    这三项在非 race 的那轮里判）
  - 跑之前必须先确认二进制真是 -race 构建的（`go version -m ./geth | grep -- -race`
    或有对应的构建记录）：不是的话这条会**静默通过**，而它正是 R20 唯一
    能暴露的场合——这条验收的价值全在"红灯可能真的亮"，不能靠"跑了没报"
  - 日志里必须同时出现正常的 PBFT 出块行，否则是空跑（节点没起来，"无
    DATA RACE" 同样成立）
  - 这条 smoke **不能替代** R20 的定向单元用例（#17 验收第 1 条：修复前
    必须被 -race 判红）——smoke 只证明"真实路径上没撞上"，不证明"不存在"
```

---

## 九、待确认输入

| # | 事项 | 当前默认 |
|---|---|---|
| 1 | PBFT 出块默认关闭（`--pbft.enable=false`） | 认同：什么都没配的节点仍是只读回放行为 |
| 2 | DPoS `Magic` 与 `NodeVersion` 的主网取值 | 待提供（可从运行中的旧节点 `--verbosity` 日志抓） |
| 3 | PBFT 节点是否强制设 miner address | 建议强制（`header.Coinbase` 参与 `SealHash`；无 etherbase 时 `commitWork` 直接拒绝出块） |
| 4 | 滚动升级窗口 M5 需要一个长期运行的旧 1.9.7 节点作对端 | 待提供 |
| 5 | legacy 链 `Recommit` 压到 3s 会略增低负载时的空块频率 | 建议接受 |
