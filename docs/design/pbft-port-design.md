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
| R12 | `Seal` 阻塞导致关停挂死（`taskLoop` 同步调用，`exitCh` 期间 `interrupt()` 执行不到） | Seal 必须有硬超时（≤ 2×period），`epochPending` 时快速返回 | `Close()` 在 Seal 等待中 < 7s 返回 |
| R13 | SPV 事件链缺失 → 生产者集永不更新 | M5 在 `onBlockHandled` 的 `isNexturnBlock` 分支回调 `dpos.SetNextProducers` | ELA 高度过 `dynamicArbiterHeight` 后集合切换 |
| R14 | Elastos `hub.New` 注册全局 SIGINT、`ETDirectPeersChanged` 走全局事件总线 | 只创建一个 `dpos.Network`；`ETDirectPeersChanged` 必须继续走 Elastos events（上游代码改不动） | devnet 多节点观察 |
| R15 | **签名必崩**：`Elastos.ELA/crypto.Sign` 只填 `ecdsa.PrivateKey.D`，不填 `PublicKey.X/Y`。Go 1.20（pgp-chain）下无害；Go 1.24+ 起 P-256 走 FIPS 模块，`privateKeyToFIPS` 解引用 nil X → SIGSEGV。bsc 是 `go 1.25.0` | `dpos/signer.go` 自建 `p256Account`：从 D 补全公钥后调标准库 `ecdsa.Sign`，输出仍是同样的 `r‖s` 64 字节 P-256 签名，旧节点可验；`GetDposAccount` 改为返回该实现 | `TestSignProposalVerifiesWithElastosCrypto`（含翻位负样例） |
| R16 | 出块侧漏登 proposal：`dpos.StartProposal` 只签名不登记，投票全部被拒 → 每轮必然超时（静默，只打 debug 日志） | `Seal` 里 `StartProposal` 之后必须 `dispatcher.ProcessProposal(self, proposal)` | `TestSealProducesABlockTheReadOnlyVerifierAccepts` 走真实投票路径 |
| R17 | **单生产者把自己的 confirm 清掉 → 高度 1 永久卡死**（M2 devnet 唯一一次卡死就是它）。`PRODUCERS=1` 时阈值 `int(float64(1)*2/3)=0`，提案者的自投票在 `dispatcher.ProcessVote`（`seal.go:128`）里**同步**走到 `onConfirm` → `trySendConfirm` 写入 `confirmCh`。若轮次 drain 放在这次自投票之后，`drainRoundChannels` 就会把刚产生的 confirm 吃掉 → 每轮等满 deadline 超时；更糟的是 `blockPool.AppendConfirm` 已记录该块，重试会撞 `ErrAlreadyConfirmedBlock` → 链永久停在该高度 | drain（`drainRoundChannels` + `isSealOver=0`）必须在 `Seal` 内**任何可能产生 confirm 的动作之前**，即早于 `ProcessProposal`（`seal.go:112`）和自投票（`:123`/`:128`）。修复前它位于 `seal.go:165-166`，这正是本 bug。附带一条同源教训：轮次通道只能 **drain，不能 close**——`trySendConfirm` 会先取字段再发送，close 会让在途发送 panic | 闸门（M2 必须落地）：`TestSingleProducerSealsInOneRound`，**必须连做高度 1/2/3**。只测高度 1 会假通过——"卡在高度 1"正是本 bug 的症状，只跑高度 1 的用例无法把它和"正常出完一个块"区分开 |

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
