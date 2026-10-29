# PBFT 出块侧移植：让 bsc 成为可出块的 pgp 节点

| 项 | 值 |
|---|---|
| 状态 | 设计已定，M1 实施中 |
| 日期 | 2026-10-29 |
| 适用仓库 | `bsc`（链） |
| 上游参照 | `pgp-chain`（geth 1.9.7 改造版，PBFT/DPoS 现网实现）。**本文所有 `pgp-chain/...:NNN` 行号都钉在 `a0cecae32fabf40b826cd10a0fe4d6156f4e3833`**（分支 `release_v0.0.6`，`git describe` = `v0.0.3.2-101-ga0cecae`；以上三项均由 `git rev-parse HEAD` / `git describe --tags --always` / `git branch --show-current` 打印）。上游 HEAD 一动行号全废，引用前先对一次 `rev-parse`，对不上就按函数名重定位，别照数字跳。**另：同一处机制常在两三层各有一个"看起来是判定点"的函数，找错层比找错数字更常见。**以 on-duty 为例，1.9.7 上从 `Prepare` 到"这一轮是不是我出块"是**四跳、终点是读缓存**：`Pbft.IsOnduty()`（`pgp-chain/consensus/pbft/pbft.go:1046-1051`，转调 `p.dispatcher.ProducerIsOnDuty()`）→ `Dispatcher.ProducerIsOnDuty()`（`dpos/dispatcher.go:410-412`，再转调 `consensusView.IsOnduty()`）→ `ConsensusView.IsOnduty()`（`dpos/consensusview.go:256-258`，**函数体只有 `return v.isDposOnDuty`**）。所以这一族 `IsOnduty` **没有一个是判定点**：真正写 `isDposOnDuty` 的只有 `ChangeView` 的 `offset > 0` 分支（`:195-200`）与 `UpdateDutyIndex`（`:175-180`）。**还有一个同名近邻容易混**：`ConsensusView.ProducerIsOnDuty(account)`（`:260-262`）是**现算**版（`producers.GetNextOnDutyProducer(v.viewOffset)`），只被 `RecoverFromConsensusStatus` 与 `DumpInfo` 调，不在这条链上。本仓结构逐字相同，**但下面这一串只给函数名与接收者、一个行号都不给**（这六个数字里五个已经漂过，其中 `:564-566` 现在落在 `CreateUnconfirm` 里——`sed` 出来是**真代码**，只是不属于那个函数）：`(*Engine).IsOnduty` → `(*Dispatcher).ProducerIsOnDuty` → `(*ConsensusView).IsOnduty`（函数体只有 `return v.isDposOnDuty`），写缓存是 `(*ConsensusView).ChangeView` 的 `offset > 0` 分支与 `(*ConsensusView).updateDutyIndexLocked`。**接收者必须写全**：`bsc` 这边 `IsOnduty` 挂在 `*Engine` 上，不是上游的 `*Pbft`——名字里少了这一层，读者按 `Pbft.IsOnduty` 去 grep 会空手而归。现算版是 `(*ConsensusView).ProducerIsOnDuty(account)`，只被 `RecoverFromConsensusStatus` 与 `DumpInfo` 调，不在这条链上。**还有一类比"行号漂移"更贵：层找错，它不会被打回。****行号错会被"读"打回，不会 `grep` 打回**——`grep -n "564"` 什么都不打回，`sed -n 564p` 打出来的也是一行真代码，只是它属于另一个函数；**只有把内容和你的断言对一遍，才知道自己错了**（我此前写成"`grep` 会当场打回"，是错的，而这个错恰好藏在一段专讲行号不可靠的话里）；层错不会——你要么在另一个文件里找到一个**看起来合理**的东西（误报），要么什么都没找到、然后被解释成"没有"（漏报）。**漏报比误报贵**：误报会有人来吵，漏报会安静地成为结论。本月已有三次：一次是我（`pbft.go` 的 `Prepare` 与 `consensusview.go` 的 getter，三个人锚在三个不同的层）；一次在代码注释里——`dpos/dispatcher.go` 曾写 "upstream calls this from `network.go:597` and `:924`"，但 `pgp-chain/dpos/network.go` 只有 451 行，真实位置是 **`pgp-chain/consensus/pbft/network.go`**（`:597` 在 `OnRequestConsensus`、`:924` 在 `doRecoverLocked`）——**这条已修 `737bf1e5b`**：那段注释现在把目录写全，并自己说明了"`dpos/network.go` 是另一个短得多的文件、同样的数字在那边已经超出文件末尾"，把失败模式写在了注释里而不是留给读者踩。**同一个文件名在两个包下都存在时，只写 `network.go:NNN` 就等于没写。****第三种形态是"同一份代码的两个位置"：把 A 处注释的意思安给 B 处。**本轮我自己犯了一次——我写"与 `RecoverFromStatus` 注释自己说的 `a pair that never existed` 同族"，实测这句**不在**那个函数的注释里：`grep -rniF "never existed" dpos/ consensus/` 打出三处，`ConsensusView.DumpInfo` 里的 `snapshots that never existed at one instant`，以及 `Dispatcher.RecoverFromConsensusStatus` 里调 `RecoverFromStatus` 之前那段的 `a deadline computed from a pair that never existed`，而 `(*ConsensusView).RecoverFromStatus` 自己的函数头注释是另一句（`...the pre-recovery start time`）。**三种形态的共同点：都不靠"数字看起来合理"来打回。**前两种靠文件名/副本分辨（`dpos/` 还是 `consensus/pbft/`、模块版还是 vendored），第三种**只能靠引文原文**——所以锚点必须是能 `grep` 到的原句，不能是转述的意思；转述一旦错位，读者按它去 grep 会空手而归，然后怀疑整段。 |
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

**但"wire 之外"的包并不相同——本项目里同时存在两份 `Elastos.ELA` 源码，引它们内部的行号必须写明是哪一份。**依据就是上面那两行 `go.mod`，两份路径分别由 `go list -m -f '{{.Dir}}' github.com/elastos/Elastos.ELA`（模块缓存，大写被转义成 `!elastos.!e!l!a@…`）与 `third_party/Elastos.ELA` 给出，实测差异：

| 位置 | 模块版（`bsc` 编译） | vendored（`pgp-chain` 编译） |
|---|---|---|
| `dpos/manager/proposaldispatcher.go` | `CleanProposals` 定义 `:205`；传 `true` 在 `:822`；传 `false` 在 `:403`、`:415` | 定义 `:213`；`true` 在 `:841`；`false` 在 `:422`、`:434` |
| `dpos/manager/dposnormalhandler.go` | `:89` | `:89`（数字相同纯属巧合） |
| `dpos/manager/dposondutyhandler.go` | `:57`（3 个 tab 缩进） | `:57`（2 个 tab） |

**"行号相同"不代表"同一份文件"，"行号不同"也不代表"有人写错了"**：`proposaldispatcher.go` 两份的规模是：总行数 1085 对 1108（差 **23**），`diff` 出 **73** 行不同（行首 `<` **25**、行首 `>` **48**），同一条调用从 `:822` 移到 `:841`（偏移 **19** 行）——**这几个量没有一个等于旧文写的"18 行"，那个数我复现不出来，删掉**。**这三个数只有一个自由度：增 − 删 = 行数差（48 − 25 = 23），所以它们不是三份独立证据，而是一条可以自校验的等式**——我第一版报的是"75 行 / 27 删"，而 48 − 27 = 21 ≠ 23，**三个数当场互斥，只是当时没人做这个减法**。同一条等式还顺手指出了量法本身错在哪：`grep -cF '<'` 数的是"**含** `<` 的行"，而两条**新增**的 Go 行里本来就有小于号（`if v.Proposal.ViewOffset < currentOffset`、`if … p.finishedHeight < height`），于是 25 被数成 27——**错法仍是"匹配子串而不是锚点"**；锚在行首才是指令：`diff A B | awk '/^</ {d++} /^>/ {a++} END{print d, a}'`。谁按另一份去核都会得出"文档造假"。本文的约定是——**引 `Elastos.ELA` 内部行号一律带"模块版/vendored"标注**；讲"上游怎么调我们"取 vendored（移植物来自 `pgp-chain` 编译的那份），讲"`bsc` 运行时实际链接哪份"取模块版。所以 `proposaldispatcher.go` 的 `:822`（模块版）与 `:841`（vendored）**两个数都对**，只是指的不是同一个文件。**路径也一样：裸写的 `dpos/manager/*` 一律指模块缓存下的那份。**本仓没有 `dpos/manager/` 这个目录（`ls dpos/manager` → `No such file or directory`，`bsc` 的 dpos 是平铺的 `dpos/*.go`），所以这类裸路径误读的方向是"找不到"，不是"找错到本仓"；但"找不到"会被读成"这处不存在"。**因此 `pgp-chain/third_party/Elastos.ELA/` 这个前缀只能配 vendored 的数**——配到模块版的数上会**精确地指错一处**：vendored 的 `:822` 是 `log.Info("Collect majority signs, finish proposal.")`，模块版的 `:841` 是一个 `}`。两个号在对面的副本里都不指向这件事，这就是"必须写清哪一份"的实证。

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
| `consensus/router/router.go` | `:311-317`（注释；`router_test.go:131-133` 有同一份拷贝） | **`Delay` 不返回 nil 是 M2 的必要条件，不是顺手清理**。事实链：pre-fork 时链上是 `pgp.Engine`，其 `Delay` 无条件 `return nil`（`consensus/pgp/pgp.go:416`：`func (e *Engine) Delay(...) *time.Duration { return nil }`）；而 `miner/worker.go:1547` 取到 delay 后，`:1548-1550` 的 `log.Debug(..., "delay", delay.String(), ...)` **没有 nil 检查** ⇒ 改前 pre-fork 只要有交易到达就是 **nil 解引用 panic**（`time.Duration.String()` 是值接收者，nil 指针解引用即崩）。**注意这与 `--verbosity` 无关**：`log.Debug` 的实参在调用前求值，Debug 被关掉也照样崩——别把它当"只在调试时炸"。**注释本身两处错，须同一提交改掉（两份拷贝都改）**：① 它引的 `miner/worker.go:1418` 实际是 `if w.isRunning()`，nil 分支在 `:1464-1467`；② "warns and stops collecting transactions until the next block" 半真半假——`:1466` 确实 `log.Warn("commitWork delay is nil, something is wrong")`，但随后只 `stopTimer = nil`、**没有 `break` 也没有 `return`**，循环继续收交易，所以"停收交易"是假的，真正致命的是上面那条 panic（把它写成"饿死交易"会把一条 panic 降级成一条性能问题）。**实际修复在 `91b25e901`**（"four comments that claimed things the code does not do"，纯注释；可机械复验：`git show 91b25e901 --numstat` 显示 4 个 `.go` 文件 43+/16−，逐 hunk 看增删行**全部**是 `//` 注释或空行，非注释改动 0 行）。**不单独成 commit 的理由仍然成立**：改写 25 个提交会让全部验收锚点失效，所以它折进了一个"四处同类错注释一起改"的提交。**这一格自己有过一次失实，留在这里当反面样本**：上上版写着"这两处改动已在 `ab424d622` 内与 M2 主体同提交"，并锚定 blob 哈希 `9e31f240…`/`14106c33…`"（与 HEAD 相同，即此后未再改动）"——**那句是错的**：`git show --stat ab424d622` 里没有这两处改动，真正改动出现在 `91b25e901`；而承载这条记录的 `ab2457d65` 自己只碰了文档（`1 file changed, 1 insertion(+)`）。**所以别再把"blob 与 HEAD 相同"当"已修正"的证据**：它只能证明**没人动过**，不能证明**它是对的**；同一份证据可以支持两个相反的结论，取决于你把"相同"读成"没动"还是"已经好了"。**取证工具仍必须写出来**（这条独立成立）：同一文件 `shasum` 给 `b6e20b50…`、`git rev-parse` 给 `9e31f240…`（均为 `ab424d622` 时点），不写工具的哈希等于没有哈希 |
| `eth/ethconfig/config.go` | `:287-313` | `pgp.New(config.Legacy)` → `pbft.New(config.Legacy, pbftCfg)`；`CreateConsensusEngine` 加 `*pbft.Config` 参数。`router.New` 签名不变 |
| `eth/backend.go` | `:333`、`:509`、`:575-600` | ① 传 `pbftCfg`；② `Legacy != nil` 时**跳过** `miner.SetExtra(makeExtraData(...))`：legacy 期的 `header.Extra` 被 confirm 独占，任何 vanity 预填都是纯脚枪（`Prepare` 本来也会置 nil，见 D6），跳过是为了不留一个容易被读错的假入口。**`SetExtra` 的 32 字节上限（`params.MaximumExtraDataSize`，`miner/miner.go:202`）不构成本处的依据**——它收到的是 `--miner.extradata` 的 vanity 串，永远不会是那个 1199 字节的 confirm，所以 confirm 长度与它无关；③ 装配 `pbft.AttachChain` / `spv.PbftEngine`，并由装配层自建 `pbftLoop`。**`pbft.Engine` 不提供 `Start` 方法**：它只有 `AttachChain`（`consensus/pbft/engine.go:236`）和 `Close`（`engine.go:319`）；出块驱动循环在 `eth/backend.go` 的 `pbftLoop`（订阅链头 → `OnBlockImported` → `StartMine`，见 `eth/backend.go:930`） |
| `cmd/utils/flags.go` | `:2954` + 新 flags | 第二个调用点同步改签名；新增 `--pbft.enable / .keystore / .keystore.password / .net.address / .net.port / .net.magic / .maxnodeperhost / .miner.address` |
| `miner/worker.go` | `:1093` | `Prepare` 的良性错误（`ErrSignerNotOnduty`/`ErrWaitRecoverStatus`/`ErrEpochTransitionPending`/`ErrAlreadyConfirmedBlock`）降为 Debug 并**中止本次生成**（不能继续用该 header） |
| `miner/worker.go` | `:534-536` | 周期 resubmit 条件加 `\|\| chainConfig.Legacy != nil`（**已完成**，源码注释在 `:526-533`）。**不动 `Recommit`**：它是事务收集窗口，与 3s 块周期绑死只会多出空块，见 §2.2 末段 |
| `miner/worker.go` | `:1560` | MEV 门控加 `&& w.chainConfig.Legacy == nil` |
| `eth/protocols/eth64/handler.go` | `dispatch` switch + `Backend` 接口 | 新增 `case ELAMSG:`（当前落 `default` 被 Discard）；新增 `BroadcastElaMessage`；`Backend` 加 `ElaMessageSink()`。**`protocolLengths` 恒 17 不动** |
| `eth/protocols/eth64/peer.go` | — | 新增 `SendElaMessage`；`run()` 里包一层 `elaPeer{p, pp}` 以提供 `Disconnect()` |
| `eth/handler.go`、`eth/handler_eth64.go` | — | `handler` 加 `dposBridge eth64.ElaMessageSink` 字段并注入，避免 `eth` 反向依赖 `dpos` |
| `spv/blocklistener.go` | `:358-402` | 补 `ETNextProducers` 等价回调（当前是 TODO）→ 驱动 `(*Producers).UpdateNextProducers`，**并写回 `spv.SetCurrentProducers`**。**旧文档在本格写的 `dpos.SetNextProducers` 是幻名，本仓与上游都零命中，别照抄**（真名分属两包，见 R13 对策格）。**M5 才做** |

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
- **M3 前置缺口：SPV 动态生产者集在本仓未接线（架构级；任务表里有对应票，请按这个标题找，别按票号找——号会被回收）**：`ConsensusView.ChangeCurrentProducers` / `UpdateNextProducers` / `UpdateProducers` 在本仓的实际可达性**三个函数要分开说，合成一句会把修饰语安到错的那两个身上**（我上一版就是合并写的"只有内部转发、`NewProducers` 初始化与测试"）：`ChangeCurrentProducers` 与 `UpdateNextProducers` **只有 `ConsensusView` 内的两个转发，连测试都没有**（`grep -rlF '<函数名>' --include=*_test.go .` 两个都空）；`UpdateProducers` 只被 `NewProducers` 初始化调一次，外加三个测试文件（`consensusview_race_test.go` / `producers_race_test.go` / `consensusview_deadlock_test.go`）——**它是三个里唯一真被执行过的那个，但只有启动时那一次：由 `NewProducers` 用同一份列表装初值，之后没有任何人再调它**（它本身是换整个集合的，不是"只装初值"的函数）；`Dispatcher` 上只暴露 `IsProducer`/`ProducerIsOnDuty`，没有生产者更新的转发；`ETUpdateProducers`（`dpos/event.go`）零订阅者；`dpos/msg/producersmsg.go` 的 `ProducersMsg` 没有任何生产代码解出来应用到 `ConsensusView`。**后果：生产者集合自 `NewProducers` 之后在运行期永不变化。**定性还要再收一格——**不是"上游设计未完成"，也不是"移植漏了个函数"，而是上游的调用点没跟着一起搬过来：方法搬了，调用点没搬。**上游四处是 `pgp-chain/consensus/pbft/network.go:505`（`ChangeCurrentProducers`）、`pbft.go:304` 与 `:312`（`UpdateNextProducers`）、`pgp-chain/consensus/pbft/network.go:174`（`UpdateProducers`，在 `Pbft.UpdateCurrentProducers` 里，同一函数还调 `spv.SetCurrentProducers` 回写 SPV），**在本仓一个都没有**；查法也就变成机械的：拿这四个上游行号去本仓找对应位置，找不到就是缺口，不用理解语义。**"三处"是我上一版的数，漏掉的正好是第三个函数名**——我只 `grep` 了 `ChangeCurrentProducers` 与 `UpdateNextProducers` 就下了结论，`UpdateProducers` 那个名字从来没进过我的命令。**按"我记得该查哪几个名字"去数，和按记忆去数一样不可靠**；数法要么把三个函数名一次列全再 `grep`，要么换个口径数"上游对 `GetConsensusView()` 的写调用"。这条定性比"没接线"好用的地方在于它可被证伪——只要在本仓找到任何一个真正的生产调用点，整条就作废。**缺口的物理位置已经写在代码注释里，接线就是填那两个 TODO**：`spv/blocklistener.go` 两处 `TODO(spv-porter): pgp-chain notified dpos.ETNextProducers`（锚点用 `grep -nF 'ETNextProducers' spv/blocklistener.go`，同一命令同时就是这条缺口的验收命令——填完之后它应该归零）。**这两处 TODO 不是这一轮才出现的**：`git log --oneline -S 'ETNextProducers' -- spv/blocklistener.go` 只有一条 `a7cbae102`（把本仓改造为 pgp-chain 节点那次移植），**所以标记从移植第一天就在那儿**——缺的从来不是标记，是"这个标记会决定 R20 的窗口什么时候变成活的"这个连接。（我上一稿写它是 `06ec5dbac` 带出来的，两处都错：那两次提交只碰了 `dpos/` 两个文件，而且那一笔不是我落的。**这正是本节要防的那种话：一句听上去合理的因果，`git log -S` 一跑就散。**）**它与 R20 (a′) 那个 `p.mtx` 窗口直接相关**：窗口在结构上存在，本树内无人可达；**接线那天它立刻可达，同一批必须一起验**——不要接线一次、验一次窗口，那会让一个已经被记录过的潜在交错重新变成"没验过的"缺陷。**"那天"能定到一个下界，但不能定成一个日子**：R13 把 **SPV 事件那一端**排在 M5，而另外两处上游调用点（`network.go:505` 的 `ChangeCurrentProducers`、`network.go:174` 的 `UpdateProducers`）属于网络层移植，**本表没有排期**。所以准确说法是"**不早于 M5，且以这三个调用点逐个搬来为准**"，不是"M5 就到齐了"——**两个端点、两条独立的线，谁先接上谁就先把窗口变活**。能确定的是反向那半句：**M3 的交付范围只有 eth/64 `ELAMSG` 与 DAddr 路由，三个调用点一个都不在里面，所以 M3 的互通验收与 devnet 长跑不可能覆盖这个窗口，别把 M3 的绿当成它的证明**；"同一批一起验"这条约束挂到**每个端点各自接线的那一批**上，不是一个统一的 M5 验收清单。**越界那条已改（team-lead 指定 R13 的对策格归我改；只换名字、保留错误痕迹，见该格）**：`dpos.SetNextProducers` 在本仓与上游都零命中（`grep -rnF 'SetNextProducers' dpos/ consensus/`，`grep_exit=1`），真名分属两包——`dpos` 侧是 `(*Producers).UpdateNextProducers`，**`spv` 侧是 `spv.SetCurrentProducers`**。**顺带记一个口径盲区，它是本轮第四个"作用域即分母"的实例**：`spv.SetCurrentProducers`（本仓 `spv/nextturn_dposinfo.go` 有定义、**零调用者**）是这条链在 spv 包里的写回口，上游在 `network.go:175` 与 `:511` 调它——**而我一直拿 `dpos/ consensus/` 当搜索范围，永远看不见它**，是 kou-dou-ma 核 `SetNextProducers` 时才发现的。所以"本仓零调用者"这个结论必须带范围：**说"D+A 范围内零调用者"是对的，说"零调用者"是错的**——`spv/`、`eth/`、`miner/` 从来没进过这个范围。

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

**行号基线（读这一章前必看）**：三个文件各自走动，所以基线**按文件给**——`dpos/dispatcher.go` = `06ec5dbac`、`dpos/consensusview.go` = `363d260ff`、`dpos/producers.go` = `34a2f1410`（三条都是 `git log --oneline -1 -- <file>` 现取；**这张基线表本身也是引用，也要现取**——它上一版把 `consensusview.go` 写成 `06ec5dbac`，而 `363d260ff` 改的正是那个文件，于是"定了要现取"的规矩没对自己执行一次。**引用前必须自己再取一次**，本章写完到读者打开它之间，`dpos/` 可能已经动过两轮）。**本章保留的行号分两类，读的时候必须分清**：①**指向现存代码的**（要被人拿去执行，如 `grep -nA3` 的判据、调用点位置）——以基线为准，**漂了就重取、不要沿用**；②**指向已被改掉的那段代码的**（R19/R20 里描述"修复前长什么样"的那些）——它们**按定义不可复现**，只能当历史记录读，别拿去 `grep`，找不到是正常的。**这不是一次性对齐**：`dpos/dispatcher.go` 在这批改动里已位移过多次——**次数不复述**，它每落一次就 +1，写下来的那一刻就开始错（`RecoverAbnormal` 早先写作 `:624`，`737bf1e5b` 之后是 `:662`，`981818400` 之后仍是 `:662`，三次数完也就明白了：值得记的是命令不是次数），每次旧号都会把读者送到无辜代码上、然后他得出"这处不存在"的结论——所以旧号一律删除、不留样。**`dpos/` 仍在演进，这一秒准确的行号下一秒就会漂**，本章的锚点约定是：**函数名与代码引用为主，数字只作定位辅助**。**余量这个数，本节决定不再记。** 它先后被记成 **20 / 31 / 38 / 48 / 60**，**五个值全部可复现**——差别只在三件事：正则匹不匹配目录前缀、范围切在哪一行、数的是行 / 出现次数 / 去重后的引用。**"处"在这个量上不是单位，是歧义**；而一个值取决于"用哪条正则"的指标，写下来只会让下一个复核的人得出"文档过时了"，其实它从来没准过。§8.3 第二条要求"否定性结论必须附可复现的命令"，这里补它的后半句：**同一个量能被五条不同命令数出五个都对的值时，它不该被写成一个数，该被写成一条规则。** **而且那个 38 自己也是混的**：它的"本仓"那一格里躺着 **4 条 `dpos/manager/*`**，其中 3 条是**裸写**的——在 `908dfd92a` 的 §七 里逐条可数：`dpos/manager/dposnormalhandler.go:89`、`dpos/manager/dposondutyhandler.go:57`、`dpos/manager/proposaldispatcher.go:822`，第 4 条带 `pgp-chain/third_party/` 前缀（`:225`）——于是"本仓"的真值是 **24，不是 28**（我量的是那 4 条的存在与形态，"24"这个数与 team-lead 的独立计数一致）。而它们全是 `Elastos.ELA` 的代码：前三条是模块版副本、第 4 条是 vendored。机制是 `grep -oE '(dpos|consensus)/…'` 抓的是**子串**，`pgp-chain/dpos/…` 的前缀被吃掉、于是**上游一律记成本仓**。一个把别人家算成自己的指标比没有指标更坏：它虚高，而虚高的数看起来像"还有很多活要干"，于是既没人去干、也没人怀疑。**最要紧的不是"这个量没法定义"，而是它连定义清了还是错的**：副本只要存在，**"文件:行号"就永远不是能指向唯一对象的标识**——它指哪个取决于读者从哪个根开始找。`38` 这个总数**按同一条命令**我复跑不出来（换个正则就是另一个数，这正是上一段那句"规则而非数"的意思），所以这里只留下能单独核的几条：那 3 条裸路径的真实存在、以及 `dpos/manager` 在本仓**不存在**（`ls dpos/manager` → `No such file or directory`）。

规则本身很短：**本章新写的引用一律"函数名 + 可 grep 的原句"，行号只作定位辅助；只有"要被人拿去执行"的东西（`grep -nF` 判据、调用点位置）才允许带行号，且必须写明取号时的基线提交。** 已经写下的行号不追着改——它们要么指现存代码（漂了就现取），要么指已被改掉的代码（按定义不可复现、`grep` 不到是正常的），这两类的处置方式不同，见上一段。

**那条旧正则是个真缺陷，与数字无关，一并留档**：前一版把 `(?:dpos|consensus)/[\w/]*\.go:\d+` 写成"可复查"的命令——toybox 既不认 `(?:...)` 也不认 `[\w]`，当场报 `repetition-operator operand invalid`，而 **`grep -c` 在语法错时 stdout 一个字都不输出**，`wc -l` 于是得到 0；**0 恰好是"已经全部转完"的读数**。这是甲族在本文件里最难看的一次：**用来检查"我有没有做完"的那条命令，本身就是那条不会跑的命令。**

**编号约定：R 号（本文）与任务表 `#` 号必须成对出现**，新票在标题处就写成 "R21 / `#24`" 这种双号形式——只写一个，另一侧就会有一个看起来完整、其实查不到的东西。**但非 R 号的缺口与残留（如本轮那两条）一律按标题引用、不写票号**：任务表的号会被回收——删掉一张票，下一张新建的票会拿到同一个号，`#27` 就先后指过两件完全不同的事。**号能查到东西、查到的是另一件事，这比查不到更坏**：查不到你会去问人，查到了你会直接拿去用。这就是"标题做锚点、别用序号"从"第 N 条"扩展到"票 N 号"；配套纪律是**以后不删票**，作废的票置 `completed`、正文写"作废，并入 X"——删除是号被回收的唯一原因。**同理，提交信息不是证据**：`981818400` 的信息声称改掉了两个方向词，`git grep` 到那个提交上一查，两处都还在（详见 R20 行的残留③）。**任何"已修 X"的断言都要落在 `git grep -nF "<落地后的原句>" <commit> -- <路径>` 上，不能落在 `git log --format=%B` 上**——前者问代码，后者问写提交信息的人，而写提交信息的人可能是在描述自己的草稿。要重取本章任何行号，跑这三条（都打印 `NR`）：

```
grep -nE "consensusView\.[a-z]" dpos/dispatcher.go
grep -nE "CleanProposals\((false|true)\)" dpos/dispatcher.go
grep -nE "\.mtx\.|producers\." dpos/producers.go dpos/consensusview.go
```

**`pgp-chain` 侧的行号不受此约定影响**：那边钉死在上游 commit `a0cecae32…`（见第 8 行），不会漂；会漂的只有本仓。同理，`Elastos.ELA` 的行号还必须带"模块版/vendored"（见 §2.3）。

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
| R12 | `Seal` 阻塞导致关停挂死（`taskLoop` 同步调用，`exitCh` 期间 `interrupt()` 执行不到）。**阻塞上限不是 6s**：`viewChangeTime = parent.Time + signTolerance`，而父块时间戳允许超前 `allowedFutureBlockTime = 15s`（`consensus/pgp/pgp.go:56`，校验在 `:227`），于是"等到 `header.Time`"这一段（`seal.go:168-179`）可以吃满 18s——**实测父块 +14s 时 `Seal` 阻塞 20s**（pre-delay 17s + deadline 3s），所以 `seal.go:47-51` 与 `TestSealExitsWhenTheMinerStops` 里"nothing here may wait longer than one signTolerance window"这句**曾经是假的**（**状态更新（代码锚点 `b5861d7b4`）**：`seal.go:46-62` 的头部注释已按分段改真——pre-delay ≤ `allowedFutureBlockTime + period` = 18s、`deadline` ≤ 3s（re-anchor ≤ 6s）、total ≈21s，并写明"不做 clamp、不在 `header.Time` 之前释放"是互通要求；**这条待办已撤回——`ccb40029` 上它已经改对了，不要再去"修"**：现在 `TestSealExitsWhenTheMinerStops` 的 doc comment 原文是 "…with the view deadline as the backstop. **Here the backstop really is one window, because this round has no pre-delay to wait out** (the header time is already in the past); a round that does have one can block for ~21s, which is the shutdown debt recorded as R12 and is not what this test measures." ——它把"一个窗口"限定在本用例的形状上、并点名了 21s 这笔债，这就是正确写法。**留着一条"残留"待办是反向危害**：M3 的人会去把一段正确的注释改成更笼统的说法。错误的待办比没有待办更危险——这条是本轮方法论的一部分，见 §8.3 末。断言本身是 1s（`seal_test.go:678-681`，合理），不用动）。**根因不在 `Seal` 等太久，而在 `Prepare` 肯为"时间戳在未来的父块"开工**：`header.Time = parent.Time + period`（`prepare.go:116`），且不能往下压——`pgp` 拒 `parent.Time + period > header.Time`，`prepare.go:117-123` 的 re-anchor 只在 `parent.Time + period < now` 时触发。设 D = `parent.Time − now_producer`（`verifyHeader` 允许 D ≤ 15），则 T = now+D+3、`Seal` 必等 D+3 秒；**我们验证方**的接受条件是 `now_v ≥ now+D−12`（15s 窗口是我们自己的，1.9.7 是零容忍，两者不可代入——见 R18），即**容忍的时钟落后 = 12−D**，D=15 时为 −3——子块生下来就没有在线验证方能收。**这个损失与 `Seal` 怎么等无关**：`verifyHeader` 只看 `header.Time`（`pgp.go:227`），改等待不改变任何人的判定 | 换杠杆，夹 `Seal` 是死路：**在 `Prepare` 里禁止为"未来父块"开工**——位置是**在 header 改写之前**（`prepare.go:74-84` 的 Difficulty/Extra/MixDigest/UncleHash/Nonce 改写之前；`nowTime` 在 `:59-62` 已就绪，但 `parent` 现在在 `:86` 才解析，**必须把这段解析连同其 `ErrUnknownAncestor` 检查一起上移**），加一道 `parent.Time > nowTime` 即返回错误（新错误如 `ErrParentInTheFuture`）。**写成"`startView`（`:114`）之前"是不够的**——`:114` 在改写之后，门禁放那里会让"被拒"的 `Prepare` 已经改过 header，直接破掉 `prepare.go:42-48` 自己声明的 D6 不变量（"refused Prepare 必须不动 header"）和守着它的 `TestPrepareIsSideEffectFreeWhenRefused`（实测：父块 +5s 时 difficulty/extra/mixdigest/nonce 四个哨兵全被改写）。`startView` 只是**第二个**理由：`:146` 会 `SetRunning()`，在它之后返回错误会留下 Running 却无轮次的永久停摆（B1）。顺带收益：`parent` 上移后，`ErrUnknownAncestor` 这条拒绝路径也变成零改写。**同一提交还必须改掉 `prepare.go:91-92` 的那句注释**——原文 `// The remaining gates cannot move above the rewrite: they depend on the parent header or on the view this round is about to open.` 本身就是一条假的约束：`parent == nil` 这道门禁只依赖 `header.ParentHash` 与 `header.Number`（miner 在调 `Prepare` 之前就设好了），既不依赖改写、也不依赖视图；它今天之所以在改写之后返回 `ErrUnknownAncestor`（`:86-88`），唯一原因是 `parent` 的解析被放在了改写之后——而这正是本节要挪开的东西。留着这句话的后果很具体：实现者读到它就会心安理得地把门禁放在改写之后，也就是本节刚修掉的那个 D6 违例。（这条由 team-lead 核 `Prepare` 实际语句顺序时指出，属本轮在清的同一族"注释声称强于代码"。）门禁形如 `D ≤ K`，K 拉动三个轴：①阻塞上界 ≤ `K + period + signTolerance`（保守值，两项都取上界；名义值 `K + period + period` = K+6，因为 `deadline` 是在 pre-delay 之后取的、恒 ≈3，两个上界不可能同时吃满）⇒ K=0 时为 6~9s；②验证方容忍的时钟落后 `≥ 12 − K`（`K ≤ 12` 才保证 ≥0——所以把 K 压到 0 不是为了余量，余量在 K=12 时就够）；③**拒绝率随 K 单调下降**：`D ∈ (0,12]` 的子块时间戳 `now+D+3 ≤ now+15`，在线验证方本来全收，`K=0` 把它们全部拒掉——而 `D>0` 恰恰是"上一个出块人时钟比网络中位时间快"的常态（12 个生产者里有一个快，它出的每一块都让下一块落在 `D ∈ (0,12]`），不是极端场景。所以 **K 是三轴取舍，`K=0` 只是"最耐时钟偏移 + 最短阻塞 + 拒绝最多"的那个角，不是唯一解**；上一版写成"唯一解"是把 K 当成无代价参数、只算了两轴，属过度声明。**裁决（team-lead）：M2 不合入。** 理由不是方案不好，而是生产行为非对称（1.9.7 节点没有这道门禁、会为未来父块开工，我们单方面拒绝会在互通验证阶段变成噪音源），且 `D ∈ (0,12]` 的实际分布零运行时证据；立项与准入条件见 §六 M3+。仍然成立的两条否决路径：①夹 `Seal` 的 pre-delay——**这条路不是"效果差"，是"不能走"**：1.9.7 对未来块零容忍（`pgp-chain/consensus/pbft/pbft.go:395`/`:421`；其正常入链路径恒 `verifySeals=true`，`pgp-chain/core/blockchain.go:1535`/`:1992`/`:2006`），所以这段等待**不是缺陷，但它保的不是"对方能不能收下"，而是"对方能不能当场入链"**——两层要分开：**验证器层**零容忍（1.9.7 `pbft.go:395`/`:421`）只决定"收到这一刻收不收"；**节点层**双方都有未来块队列（我们 `core/blockchain.go:2404`/`:2561` → `addFutureBlock:2224-2235`、`updateFutureBlocks:3356-3357`；1.9.7 `pgp-chain/core/blockchain.go:1664` → `addFutureBlock:1491-1498`、5s ticker `:2223-2224`），所以提前广播**不丢块**，代价是它在 peer 的队列里躺到 `header.Time` 之后（最坏 `D+3−B` 再加 ≤5s 的 ticker 粒度），而这段延迟里**没有任何节点能在该高度之上出块**。**但停等只发生在 1.9.7 那半个网络**：`header.Time ≤ now+15` 的块在我们这侧根本不进队列——`consensus/pgp/pgp.go:227` 直接收下（`now+15 < header.Time` 才拒），所以我们自己的节点不会为它等。M5 滚动升级期间网络上两种规则并存，验"停等多久"必须**按节点类型分开记**，不能把 1.9.7 的队列延迟记到我们这半个网络上——真实代价是这段停等，不是"打回"。**真风险另有其处**：`header.Time > peer_now + maxTimeFutureBlocks(30)` 时 `addFutureBlock` 直接返回错误、**不入队**（1.9.7 `:1493-1495`、我们 `:2225-2228`），那块对该 peer 才是真丢——这条见 R18。即使不考虑互通，夹也换不到余量（不改变 `header.Time`）。**所以 21s 只能从源头收窄**：靠 Prepare 门禁把 D 压到 ≤K，不要在 `Seal` 里夹。**另须记住：`allowedFutureBlockTime = 15s`（`pgp.go:56`，判定在 `:227`）是我们验证器的宽容度，不是 1.9.7 网络的宽容度，两者不可互相代入**（见 R18）；②在 `Prepare` 里把时间戳往下压——被 period 校验判死。另：`seal.go:195-197` 那个 `if deadline > signTolerance` **不可达**——两条独立证据：①**解析**：`deadline` 在 pre-delay 之后取，future-parent 分支恒 ≈3s，re-anchor 分支 ≤6s（= `signTolerance`），故 `> signTolerance` 永不成立；②**实测（来源 QA，非本文档作者，也非工程师）**：parent 偏移 −30s…+15s 全表，`deadline ∈ [2s, 5s]`，该分支一次都不成立（5s 那档属 re-anchor 分支，与解析上界 6s 相符）。**署名须保留 QA**：工程师没跑过这组数、并拒绝把它写进自己的 commit message，这个拒绝是对的，别把实测记到他名下。其注释 "this clamp is what makes it true" 与事实相反，不要把它当作预算实现——**已于 `b5861d7b4` 删除（M2 出厂前）**（`seal.go` 里不再有这个分支，也不再有任何 `deadline` 上限夹）；`seal.go` 的头注释同时按本节要求改写成真实上界（pre-delay ≤ 18s、`deadline` ≤ 3s、re-anchor 分支 ≤ 6s、总 ≈ 21s），并写明 pre-delay 是 1.9.7 互通的前提、不得夹；`TestSealExitsWhenTheMinerStops` 的注释补了"本用例无 pre-delay"这一前提 | **M2 出厂状态**：不合入门禁，阻塞上界仍是实测的 21s——所以 `seal.go:47-51` 那句"nothing here may wait longer than one signTolerance window"已改成真实上界（`b5861d7b4`），**理由不是"我们没做好"，而是这段 pre-delay 本身是 1.9.7 互通的必要条件，注释必须说明它为什么必须存在**（M2 不能带着假注释出厂），`TestSealExitsWhenTheMinerStops` 也已注明"本用例无 pre-delay"这一前提（`b5861d7b4`），且不得断言"一窗口内返回"。**M3+ 准入通过后才加闸门**：`TestPrepareRefusesFutureParent`（父块 +1s → `Prepare` 返回错误**且** `view.IsRunning() == false`，后半句锁住"门禁必须在 `startView` 之前"，否则 B1 复发）——放 `Prepare` 侧是为了确定性：producer 用 `GetNowTime()`（`dtime.MedianTimeSource.AdjustedTime`，`dpos/dispatcher.go:576`）、验证方用 `time.Now()`，在 `Seal` 侧断言"慢 4s 的验证方"必然 flaky。**门禁未做之前，先放一条"扳机式特征测试"，不许用 `t.Skip`**：用例名为当时的事实（如 `TestPrepareAcceptsFutureParentToday`），断言**今天 `Prepare` 不拒绝未来父块**（父块 +5s 时拿到的是实际结果，而不是 `ErrParentInTheFuture`），文件头写明它是 R12 的开工扳机——**门禁一旦落地，这条必然变红**，实现者必须在同一提交里把它翻成拒绝形态并改名。用 `t.Skip` 的写法是错的：skip 让 CI 永远绿、没人会注意；写成"断言当前还没有门禁"之后，门禁一落地就红，实现者没法忘。**M3 实现者的第一步是固定的三步**：①落门禁（在 header 改写之前，`parent` 解析上移）→ ②**同一提交**把 `TestPrepareAcceptsFutureParentToday` 翻成拒绝形态并改名、把 `prepare.go:91-92` 那句错注释改掉 → ③再跑包。缺①是没做，缺②③是留下"用例名说的是设计意图"和"注释说的是假约束"两个反向误导。另：`TestSealHonoursStopDuringPreDelay`（`consensus/pbft/seal_stop_delay_test.go`，QA 出）覆盖的是**pre-delay 期间对 `stop` 的响应**，不是阻塞上界——它把 `header.Time` 直接推到 now+4s 而不用 parent+15s（后者会被 M3+ 门禁拒掉，用例得改一行才能跑），与本节闸门不重叠、不要合并。**本节事实锚点**：以上"已删/已改"对应代码 `b5861d7b4`（删 clamp + `seal.go` 头注释改真 + `seal_test.go` 注释补前提）；**`ab424d622` 及更早的提交里 clamp 仍在**——只 checkout 到那里时，本节"已删"的陈述不成立，勿据旧提交认定文档与代码矛盾 |
| R13 | SPV 事件链缺失 → 生产者集永不更新 | M5 在 `onBlockHandled` 的 `isNexturnBlock` 分支回调 **`(*Producers).UpdateNextProducers`**（本仓经 `ConsensusView.UpdateNextProducers` 转发），并且**同一个事件还要写回 SPV 侧**：`spv.SetCurrentProducers`——上游这两下在**同一个函数里、隔五行**（`pgp-chain/consensus/pbft/network.go:505` 的 `ChangeCurrentProducers` → `:511` 的 `spv.SetCurrentProducers`，中间是 `go func{AnnounceDAddr, Recover}` 与一行 `DumpInfo`——**不是相邻两行**，这个细节值得写准，否则有人会照着"相邻"去找一个不存在的形态），**本仓两下都没有**（`grep -rnF 'SetCurrentProducers' --include=*.go .` 只剩 `spv/nextturn_dposinfo.go` 里的定义与注释，零调用者）。**本格原写的 `dpos.SetNextProducers` 在本仓与上游都零命中，是一个不存在的名字——留档在此，以免下一个人从旧文档里再抄一次**（来源：kou-dou-ma 核 `SetNextProducers` 时发现 spv 侧那个写回口，此前 #27 只数了 `dpos/` 侧三个函数，漏了它） | ELA 高度过 `dynamicArbiterHeight` 后集合切换 |
| R14 | Elastos `hub.New` 注册全局 SIGINT、`ETDirectPeersChanged` 走全局事件总线 | 只创建一个 `dpos.Network`；`ETDirectPeersChanged` 必须继续走 Elastos events（上游代码改不动） | devnet 多节点观察 |
| R15 | **签名必崩**：`Elastos.ELA/crypto.Sign` 只填 `ecdsa.PrivateKey.D`，不填 `PublicKey.X/Y`。Go 1.20（pgp-chain）下无害；Go 1.24+ 起 P-256 走 FIPS 模块，`privateKeyToFIPS` 解引用 nil X → SIGSEGV。bsc 是 `go 1.25.0` | `dpos/signer.go` 自建 `p256Account`：从 D 补全公钥后调标准库 `ecdsa.Sign`，输出仍是同样的 `r‖s` 64 字节 P-256 签名，旧节点可验；`GetDposAccount` 改为返回该实现 | `TestSignProposalVerifiesWithElastosCrypto`（含翻位负样例） |
| R16 | 出块侧漏登 proposal：`dpos.StartProposal` 只签名不登记，投票全部被拒 → 每轮必然超时（静默，只打 debug 日志） | `Seal` 里 `StartProposal` 之后必须 `dispatcher.ProcessProposal(self, proposal)` | `TestSealProducesABlockTheReadOnlyVerifierAccepts` 走真实投票路径 |
| R17 | **阈值 ≤1 的生产者集把自己的 confirm 清掉 → 该高度永久卡死**（M2 devnet 唯一一次卡死就是它）。边界是 `legacyMajorityCount(n) = int(float64(n)*2/3) <= 1`（`consensus/pgp/confirm.go:254-256`），即 **n=1（阈值 0）和 n=2（阈值 1）都挂**，n=3 起正常（n=12 阈值 8，实测无影响）——不要只测 n=1 就以为覆盖了。在这种集合里提案者的自投票在 `dispatcher.ProcessVote`（`seal.go:161`）里**同步**走到 `onConfirm` → `trySendConfirm` 写入 `confirmCh`。若轮次 drain 放在这次自投票之后，`drainRoundChannels` 就会把刚产生的 confirm 吃掉 → 每轮等满 deadline 超时；更糟的是 `blockPool.AppendConfirm` 已记录该块，重试会撞 `ErrAlreadyConfirmedBlock`（`seal.go:74` 起）→ 链永久停在该高度 | drain（`drainRoundChannels` + `isSealOver=0`）必须在 `Seal` 内**任何可能产生 confirm 的动作之前**，即早于 `ProcessProposal`（`seal.go:112`）和自投票（`:156`/`:161`）。**语义上**："清上一轮遗留"与"本轮开始"是两个时刻，"本轮"的起点应由 `isSealing=1` 界分（这正是 `onConfirm` 已有的那道闸），所以依次做 `drainRoundChannels()` → `isSealOver=0` → `isSealing=1` 最紧。修复前它位于 `seal.go:165-166`，这正是本 bug；工作区已按上序落地（`drainRoundChannels` 在 `:149-150`，早于自投票 `:156`），不要重复"修"它。附带一条同源教训：轮次通道只能 **drain，不能 close**——`trySendConfirm` 会先取字段再发送，close 会让在途发送 panic | 闸门（M2 必须落地）：单生产者单轮出块用例（命名待定，`TestSingleProducerSealsInOneRound` 在树上**不存在**），**必须覆盖 n=1 与 n=2，且每个 n 连做高度 1/2/3**。只测高度 1 会假通过——"卡在高度 1"正是本 bug 的症状，只跑高度 1 的用例无法把它和"正常出完一个块"区分开。**测试前提**：现有 `sealTestChain`（`seal_test.go:49-80`）只服务创世，高度 2 直接 `ErrUnknownAncestor`，所以这条用例要求先把 harness 改成会记住自己封出的块（或显式喂 `OnBlockImported`），否则写不出来 |
| R18 | **未来块判定是规则级分歧：我们比 1.9.7 宽松 15s**。1.9.7（互通目标）`header.Time > now` 即 `ErrFutureBlock`，**零容忍**：`pgp-chain/consensus/pbft/pbft.go:395`（`VerifyHeaders`，`seals[i]=true` 时无条件生效，且位于 block pool 短路 `:399` **之前**，pool 救不了）、`:421`（`verifyHeader`）；其正常入链路径恒 `verifySeals=true`（`pgp-chain/core/blockchain.go:1535`/`:1992`/`:2006`），老节点没有任何豁免通道。本仓 `consensus/pgp/pgp.go:56` `allowedFutureBlockTime = 15s`，判定在 `:227`（`now+15s < header.Time` 才拒）。⇒ **`header.Time ∈ (now, now+15]` 的块：我们当场接受、老节点当场拒收**（"当场"两个字很要紧，见下）。**分三档写，不要写成"不是分叉"一句话**（"不是分叉"这个结论过强，team-lead 驳回了我上一版）：**①`header.Time ∈ (now_v, now_v+30]`（当场拒、可入队）**：老节点拒收后同样入队重试（`pgp-chain/core/blockchain.go:1664` → `addFutureBlock:1491`，5s ticker `:2223-2224`），时钟走过 `header.Time` 后能入链 ⇒ **不构成永久分叉**。**补一句精度**：这一档上下两段的性质不同——`(now_v, now_v+15]` 才是"我们当场收、1.9.7 当场拒"的真分歧带；`(now_v+15, now_v+30]` 里**我们自己也**返回 `ErrFutureBlock` 并入队（`core/blockchain.go:2404`/`:2561`），两边行为一致，那段没有规则分歧。也就是说 R18 这个 15s 差异的**有效作用带只有 15s 宽**，不要按 30s 去估它的影响面。**②但同一档里"短暂落后"不等于"无害"**：`updateFutureBlocks` 的 ticker 粒度是 **5s**，而块周期是 **3s**——未来块在队列里躺的最坏时长**超过一个出块槽位**。对端落后期间，如果它自己正好是下一个高度的 on-duty 生产者，它在队列排空前就**已经能基于 N−1 产出自己的 N 块**（这不是推测，是 `Prepare` 的形状决定的。**引代码原文，行号只作辅助**——本轮已经有三个问题出在"锚点指错了东西"上：1.9.7 与我方都只按 `header.ParentHash` 取父块，`parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)`（1.9.7 `pbft.go:560`、我方 `prepare.go:86`），对端手里有 N−1 就够了。**但我上一版写的"它算出的 on-duty 与全网一致、所以正确地认为自己该出块"是过度声明，team-lead 驳回正确**：对端**链数据落后而墙钟正常**时 `parent.Time` 是旧的，于是 `if header.Time < nowTime { header.Time = nowTime; p.dispatcher.ResetView(nowTime) }`（1.9.7 `:582-585`、我方 `prepare.go:117-123`）会**把视图重锚到它自己的 now**，紧接着的 `if !p.IsOnduty()`（1.9.7 `:586`）判的是**重锚之后**的视图，而全网的视图锚在 `parent.Time` 上。准确说法是：**它的 duty 判定建立在一个与全网不同的视图上，因此可能合法地判出不同的生产者**；当这个差异恰好让它也认为轮到自己时，同一高度就出现两个块。这比"恰好轮到它"**更容易**发生，因为重锚到的是它自己的 now、视角与全网完全不同步。**M2 单生产者证伪不了这一条**——单生产者时根本没有第二个视图，别拿 devnet 没见过当反证。**这也不是"对方的缺陷、我们兼容一下"**：我方 `prepare.go:117-123` 与 1.9.7 `:582-585` 都有这道重锚，所以它是**协议里两边共有的不确定性**——按"必须在 M3 互通台架上验"排，不按"兼容即可"排）⇒ **可能出现同高度竞争块**，其消解取决于 confirm/vote 与 reorg 逻辑。**这一档的验收不能默认自愈**，必须单独验。**③`header.Time > peer_now + maxTimeFutureBlocks(30)`**：`addFutureBlock` 返回错误、**不入队**（`:1493-1495`，我们这侧同理 `core/blockchain.go:2225-2228`），该块对该 peer 永久丢弃，后续块因父块缺失也一并被丢——**只有这一档才是"分叉"**，等价于"对端时钟落后 > `30 − (D+3)`"，在我们 15s 宽容下最坏是落后 > 12s。这个 15s 在本仓的唯一先例是 ethash（`consensus/ethash/consensus.go:45` `allowedFutureBlockTimeSeconds = 15`，PoW），而 BSC 自己的 Parlia 同样零容忍（`consensus/parlia/parlia.go:585-587`）——我们这条 legacy 规则比**两边**都宽松 | **M2 不改**，交 M3 互通实测决策。理由：诚实出块人不会产出这种块（都等到 `header.Time` 才广播，这正是 `Seal` 的 pre-delay，与 1.9.7 `pbft.go:692` 同一个理由），所以是理论场景；但"我们的规则比被替代的共识更宽松"在线级互通这个目标下必须显式记录，不能靠"不会发生"。**收紧到 0 的代价**（也是现代实现普遍保留宽容窗的原因）：出块人时钟略快时，其块会被略慢的验证方判为未来块拒收 → 变成出块失败与 view 轮转；宽容窗就是用来吸收真实时钟抖动与传播延迟的，收到 0 等于要求全网时钟严格一致。**另记一条边界：切 Parlia 时规则收紧**——Parlia 零容忍（`parlia.go:585-587`），已入链的块不会被回溯校验，但分叉点之后的第一批块要求 `header.Time ≤ 各验证方 now`，15s 的宽容不延续 | 两侧行号可静态核对。M3 devnet 须**分三档**验，**不要笼统问"是否分叉"**：①构造 `header.Time ∈ (now_v, now_v+30]`（如 now+10）→ 确认我们 `VerifyHeader` 通过、1.9.7 返回 `ErrFutureBlock`，且对端**在队列里**（不是丢弃）——这一档判的是"有没有永久丢"，判据是队列排空后追平；②同一档的**竞争块**必须单独验：让那个落后对端在落后窗口内正好轮到 on-duty，看它是否产出同高度块、以及 confirm/vote 如何消解——**不许用"ticker 内自愈"把这一档跳过**；③构造 `header.Time > now_v+30`（等价对端时钟落后 >12s）→ 确认对端**永久丢弃**，只有这一档才预期停滞/分叉。三档用的时钟偏移要记进 M3 报告，收不收紧由实测的时钟偏差与 `D` 分布决定——与 §六 M3+ 准入条件 1 共用同一份数据 |
| R19 | **票数 map 无锁重建 → `fatal error: concurrent map read and map write`，进程直接死**（不是 race 警告，Go 运行时不走 panic 恢复、可能完全不留栈）。`CleanProposals`（`dpos/dispatcher.go:439-472`）在 `FinishedProposal` 释放 `finishedMu` **之后**把 `acceptVotes`/`rejectedVotes`/`pendingVotes`/`precociousProposals` 整体重新 `make`（修复前无锁，`grep -n` 在旧版上是 `:361-363`/`:366`/`:370-373`；**现在这几行已在 `d.mu` 之内，见对策格**），而读写这些 map 的路径**并不都持有同一把锁**。**两条独立交错**：①`FinishedProposal` 的两个调用者同时进入——`Seal → addConfirmToBlock → FinishedProposal`（miner 协程）与 `pbftLoop → OnBlockImported → FinishedProposal`（链头协程）；②**不需要两个调用者**——`CleanProposals` 的整体替换 vs `processVote` 的 `acceptVotes[hash]=vote`（`:206`）/`rejectedVotes[hash]=vote`（`:219`），只要有对端投票落在清理窗口内就撞。**另有三处同样无锁，团队原稿未列**：`ProcessProposal` 写 `precociousProposals`（`:85`）；`ResetAcceptVotes`（`:384-386`）与 `UpdatePrecociousProposals`（`:388-395`）——后两个在本仓**零调用者**，但上游在 `pgp-chain/consensus/pbft/network.go:1080` 与 `pgp-chain/consensus/pbft/pbft.go:1182` 调用它们，M3 接 p2p 时会被复活成测试覆盖不到的竞态路径。**可达性（决定排期；写机理，不写结论）**：**M2（无 transport）= 不可达**，三条机理叠加：①`ProcessVote` 全仓唯一非测试调用点是 `seal.go:193` 的自投票，与 `FinishedProposal` 同协程；②链头那次 `FinishedProposal`（`engine.go:364`，来自 `OnBlockImported`）被水位守卫 `height <= finishedHeight`（`dispatcher.go:296-300`）短路 `return`，**根本到不了 `CleanProposals`**——因为 `addConfirmToBlock`（`seal.go:315`）在块交给 miner **之前**就把水位推到了 h；③无外部块、无对端投票。**这条不再只是代码推导，它在生产日志里有签名（team-lead 亲自数的；出处以整份日志为准）**：`~/.pgp-devnet/logs/geth.log` 全份 **6389 行、8 次启动、164 次封块**，配 **164 条 `WARN FinishedProposal received fork`**——**严格 1:1**；同一份日志里 `fatal error` / `concurrent map` / `panic:` / `SIGSEGV` 全为 **0**（分段也 1:1，但**这三组数的口径不同、引用必须带人次与复现方法**：team-lead 数的是 13:51:58→14:08:50 那一段、收到高度 107 时 141:141；an-quan-shi 独立数出 164:164 与 187:187 两组，后者用"全部 WARN 行的高度之和 = 封块高度之和 = 12845"对账。**141:141 的分段边界是口头给的、我手上没有可复现的行号清单**，只作旁证；要复现请用 187:187 的高度和对账法，它不依赖你怎么切分段。）。**引用这行日志时注意格式**：本仓实际输出是 `WARN [...] FinishedProposal received fork blockheight1` —— `height` 与值粘连、没有空格也没有 `=`（`dpos/log.go:40` 用 `fmt.Sprint`），所以任何按 `height=NNN` 抓字段的脚本都会静默匹配 0 条；也不能拿本仓日志与 1.9.7 逐行对照。详见 §8.3"日志判据怎么读"。这一比一同时钉住两件事：链头那次 `FinishedProposal` **每块都真的来了**（否则 WARN 会少于出块数），而且**每次都真的被 `:257-261` 挡在 `CleanProposals` 之外**（否则要么 WARN 数对不上，要么出现 `seal timed out`）。两个方向的漏都能被它抓出来：若"来了但没被挡住"，就是 R19 已经在跑；若"挡住了但没来"，是我上面第②条机理写错了、M3 不该按"必然可达"排。**M3（接 DPoS p2p）= 必然可达**：①对端投票来自网络协程（上游 `network.go:756`/`:1082` 即此路）→ `processVote` 出现多协程调用者；②follower/追赶/重启补 confirm 会让链头那次 `FinishedProposal` **真的越过水位守卫**、走到 `CleanProposals`。**必须写机理而不是只写结论**——只写"M2 不可达 / M3 可达"会让读者推出两个相反的错误："单机 devnet 可能撞上"（不会，别拿长跑没撞当证据）、"接了 p2p 也安全"（会死）。**现状须诚实记录**：竞争**仍然存在**；`-race` 现在绿，是因为没有用例把这两条路径交叉驱动，不是因为不存在 | **不要新增 `votesMu`——用已有的 `mu`**。`dpos/dispatcher.go:34-46` 的锁序注释**是按边列的**：首行 `mu -> proposalMu   allowed, and required.`，其后依次 `proposalMu -> mu   forbidden.`、`mu -> viewMu       allowed …`、`viewMu -> mu   forbidden.`，末行 `proposalMu stays a leaf and must not be taken under viewMu.`；且 `AddPendingVote`（`:203`）、`ProcessVote`（`:209`）、`setProcessingProposal`（`:181`）都已在用它。**本行上一版引的是旧注释原文 `` `mu` guards the vote maps ``——那句话在 `e1089e728` 把锁序表改成"按边列"时已被替换掉，引文也跟着换了**：引一句已经不在文件里的话，和编一句没有区别，读者 `grep` 不到时会先怀疑整行；给同一批字段再添一把锁，正是 `:47-52` 自己点名的缺陷（"giving one field two locks is the defect, not the fix"）。做法：`CleanProposals` 在 `setProcessingProsalNil()`（取 `proposalMu`，内部自闭合）**返回之后**再取 `mu`，把四张 map 的替换包住，两把锁**不嵌套**（与 `setProcessingProposal` 已有的"先 proposalMu 放掉、再 mu"模式一致，锁序天然满足"`mu` 从不在持有 `proposalMu` 时获取"）。**已按此落地（`f7c38120c`）**，现状可从代码直接核：`setProcessingProsalNil()` 在 `:441`，`d.mu.Lock()` 在 `:443`、`defer d.mu.Unlock()` 在 `:444`，四张表的 `make` 与 `precociousProposals` 剪枝都在锁内（`:446`-`:471`）——**即"先 proposalMu 放掉、再取 mu"，与本行要求的顺序逐字一致**。**另三处无锁点的处置已定，不留"或"**：①`ProcessProposal` 写 `precociousProposals` **已纳入 `mu`**（`f7c38120c`；现为 `:114-116` 的 `d.mu.Lock()`/`d.mu.Unlock()`，注释写明"M3 起 proposals 从 p2p handler 协程进来"）；②`ResetAcceptVotes` **已删除**；③`UpdatePrecociousProposals` **已删除**——删而不是留着加锁，因为它们的风险正是"今天死、M3 复活"，留着加锁只是把看不见的问题变成看得见但没人测的问题，git 历史能找回（与本项目删 `ErrNilChain` 同一判断）；M3 需要时带锁重引入。**别跟着删错一样东西**：`precociousProposals` **字段本身**和 `CleanProposals` 里 `changeView=true` 分支的**遍历剪枝**（`:449`-`:471`）都必须留着，**但两者的理由不同，别混**：①**字段今天是活的**——被创建（`:412` 的 `!changeView` 分支整体换新）、被 `ProcessProposal` 写入（`:114-116`）、被 `CleanProposals` 清空，删字段就是另一个 bug；②**`changeView=true` 那个分支今天零调用**（三个生产调用点 `:309`/`:324`/`:359` 全传 `false`），它必须留的理由不是"活着"，而是**上游四处调 `true`**（`pgp-chain/consensus/pbft/pbft.go:1189`、`dpos/manager/dposnormalhandler.go:89`、`dposondutyhandler.go:57`、`proposaldispatcher.go:822`——**均为模块版** `github.com/elastos/Elastos.ELA@v0.9.8-0.20230822071649-363670e32ee4`；`pgp-chain` 经 `go.mod:107` 的 `replace` 编译的是 `third_party/Elastos.ELA`，同名函数在 `:213`、该调用在 `:841`——两份源码内容不同，**行号不可互换**，见 §2.3），M3 接线时会走它。**上一版我写成"它是活状态…被这条分支读并剪枝"，结论对、理由错**——正是"说明声称强于代码"那一族：把"接线后会活"说成了"现在活着"。（更正来源 yan-guo-guan，他复验 `#13` 时逐点 grep 过调用参数。）④`proposalProcessFinished` **已连字段一起删（`f7c38120c`）**：四处同时消失——字段声明、getter `GetProposalProcessFinished`、setter `SetProposalProcessFinished`、`CleanProposals` 里的写入（`grep -n proposalProcessFinished dpos/dispatcher.go` 现在**零命中**）。依据是 kou-dou-ma 复核过的调用面：**含测试文件在内全仓 0 调用者**；上游 `network.go:712`/`:714` 会调，M3 复活，故按②③的同一判断删掉、**M3 重引入时必须带 `mu`**。**⑧ 第五个写入者：本行首版没覆盖它，现已补修（`3f6c2d188`）**。`RecoverFromConsensusStatus` 原来在**不持 `mu`** 的情况下整体重换三张票数表（`acceptVotes`/`rejectedVotes`/`pendingVotes`），中间夹的 `setProcessingProsalNil()` 只短暂取 `proposalMu` 就放掉——三张表一行都没进 `mu`。**它与本行同型**（整表换新 vs `processVote` 逐键写），但**不在 `f7c38120c` 的覆盖面里**：那个提交只改了 `CleanProposals` 与 `ProcessProposal`。**补修的形态值得记，因为它同时解决了"两处会再次漂开"的问题**：抽出 `resetVoteMapsLocked`（`dispatcher.go:433`，注释写明"caller must hold `d.mu`"），`CleanProposals`（`:446`，在自己的 `mu` 临界段里）与 `RecoverFromConsensusStatus`（`:663`-`:665`，自取 `mu` 包住）**共用同一个实现**——同一批字段只留一个重换点，下一次改就不会只改一半。两处细节：①`setProcessingProsalNil()` 必须在取 `mu` **之前**（`:645`），否则就是 `proposalMu → mu` 那条禁边；②恢复路径**故意不用 `defer`** 放锁（`:654`-`:656` 的注释），因为 R20 之后 `DumpInfo` 要取 `viewMu`，`defer` 会让 `mu` 跨过一把视图锁、还跨过日志写盘。**可达性不变**：本仓 0 调用者（`grep -rn -e RecoverAbnormal -e HelpToRecoverAbnormal --include=*.go .` 除定义与自身日志外零命中；这里特意用两个 `-e` 而不是 BRE 的交替转义——既避开实现差异，也免得在表格里制造列分隔），上游 `pgp-chain/consensus/pbft/network.go:597`、`:924` 调用——**"今天死、M3 复活"**，所以 devnet 长跑与 `-race` 都不会红，只能靠读代码发现（an-quan-shi 在核 on-duty 四跳链时顺带挖出）。代码里也把这一层写明了（`:658`-`:662`："Unreachable in this tree today … Ported-but-not-wired, not dead"）。**"只锁写入点"是明确排除的中庸解**——写点和两个存取器同样无锁，只锁一处等于留了个没人测的竞态。**不能扩 `finishedMu`**：`:38-52` 明确它是最外层、从不与 `mu`/`proposalMu` 嵌套；扩过去就是破坏已确立的锁序。**不能把锁跨过 `:269-272`**：尾巴经 `ChangeView → listener.OnViewChanged → miner.RequestNewWork` 回调外部包，持锁调外部未知代码，死锁面无法静态排除。**取 `mu` 后必须守住的不变量**：`onConfirm`/`unConfirm` 回调链上不得同步调用 `CleanProposals`（Go mutex 不可重入，而 `ProcessVote` 持 `mu` 时会同步走到 `createConfirm → onConfirm`；今天 `engine.onConfirm`（`seal.go:388`）只做 `trySendConfirm` 的 channel send，所以安全——这条要写进注释，不能只靠今天的调用图）。**锁序登记——按现有边图写，不要用"永不嵌套"这种粗口径**：现存**唯一嵌套边是 `mu → proposalMu`**（`processVote:179`、`createConfirm:440`、`createUnConfirm:463` 在持 `mu` 时经 `GetProcessingProposal:487-491` 取 `proposalMu`），这条**允许且必须保留**；**禁止的只有反向边 `proposalMu → mu`**（取 `mu` 时不得持 `proposalMu`）——文件 `:34-38` 的 "never taken while `proposalMu` is held" 就是这个意思。`finishedMu` 在 `FinishedProposal` 里于 `:267` 先释放、`:270` 才调 `CleanProposals`，不产生 `finishedMu → mu`；三个调用点（`:270`/`:285`/`:300`）进入时都不持 `mu`。所以本补丁是**把 `CleanProposals` 拉回文件已声明的锁序，不是改锁序**。**"把 `CleanProposals` 里两段合成一段锁"不是风格问题，是 ABBA 死锁**：那样 `CleanProposals` 会持 `proposalMu` 求 `mu`，而 `ProcessVote` 正持 `mu` 经 `GetProcessingProposal` 求 `proposalMu`，双方各等对方 | 闸门：①**先把竞争写成两条，不要用"4 个并发 writer 打 `FinishedProposal`"这种构造**——`FinishedProposal` 的尾巴每次都 `SetReady` + `UpdateDutyIndex` + `ChangeView`（`dispatcher.go:308-314`），那条用例会**先红在 `ConsensusView` 的裸字段上（R20），红点不在 map 上**；加了 `mu` 也不会转绿，等于拿一个错误的红灯去验一个正确的补丁，最后只能把用例删掉或降级。（这个坑由 kou-dou-ma 在核 `GetViewOffset` 时发现，他据此撤掉了原来那版并发用例。**注意那条构造没有废弃，它归 R20**——#17 的验收第 4 条把它当作"恢复 4 写者"的判据，那里它是对的：R20 修的就是视图锁本身。同一个用例在两个票里判的是两件不同的事，不要因为本条不用就把它删掉。）正确的两条：①a `CleanProposals(false)` × `ProcessVote` 交叉——`changeView=false` 分支（`:366`）只重建 map、**完全不碰 `consensusView`**，红灯只可能落在本条；①b 若要覆盖多 writer，用 `ResetConsensus`/`ResetConsensusForEpochTransition` 之外另构造，不许经过 `ChangeView`；②新增专门打第二条交错（无需第二个调用者）的用例——现有 `TestFinishedHeightRace` 只覆盖 `FinishedProposal` 的多个 writer，打不到这条；③**变异验证**：去掉 `mu` 后新用例必须变红，否则只是加了一把没人用的锁、`-race` 绿是假绿；④`go test -race -count=10 ./dpos/` 绿。**以上四条已全部落地并实测（`f7c38120c`）**，是变异结果不是"跑绿了"：新用例 `TestCleanProposalsDoesNotRaceVotes`；**摘掉 `mu` → 报 6 条 `DATA RACE`**（读 `processVote` 侧 vs 重建 `CleanProposals` 侧），红；**"把两段锁合并成一段"→ 真死锁**，而且**只靠 go test 的超时才红**：kou-dou-ma 那次是 `panic: test timed out after 1m30s`，yan-guo-guan 独立复现一次（`-timeout` 设 25s，于是 `panic: test timed out after 25s`）——堆栈两侧清楚：一侧持 `mu` 等 `proposalMu`（`ProcessVote → GetProcessingProposal`，`dispatcher.go:518-522`），一侧持 `proposalMu` 等 `mu`（`CleanProposals:421`）；两次的 `WARNING: DATA RACE` 计数都是 **0**。**两种超时给出同一结论，说明与时间设置无关**；也正因如此，**死锁判据不能只写"跑一遍没报 race"**——超时值不同、红的时间就不同，必须给显式 `-timeout` 或用例内计时器（R20 闸门 ②b 就是这个原因）。**所以本行那句"合并 = ABBA"现在不是论证、是可执行断言**：谁"顺手整理"成一段锁，这个用例直接挂死，不需要靠读文档。**排期**：原定 M2 验收通过之后、M3 开工之前——理由是 **M2 由机理保证不可达、M3 必然可达**（见上方可达性三条机理）；**实际已提前落地（`f7c38120c`，M2 验收前）**，不改变 M3 的推理，只是暴露面提前归零。措辞上两个方向都不能错：写成"M2 也可能撞"会让人拿长跑没出事件当反证；写成"只是并发面变宽"会低估 M3 的必然性。**M3 的观测判据（必须写进 M3 验收，否则运维会误判）**：M3 上线后那行 `WARN FinishedProposal received fork` 应逐块降到接近 0。**它归零的那一刻，正是 `CleanProposals` 开始跑在链头协程上、本条暴露面从"零"变成"每块一次"的时刻**——WARN 消失不是"系统变干净了"，是风险从 0 变成 1。所以顺序是硬的：**本条的 `mu` 必须先落地，再让 M3 去打通那条路径**。反向也要写清楚：如果 M3 上线后 WARN **仍逐块打印**，说明链头 `FinishedProposal` 仍被水位守卫短路，即 M3 的入链/confirm 路径没真正打通——那是另一个问题（比 R19 更严重），不要记成"R19 还没触发"。**现场指纹（给运维）**：`eth_blockNumber` 停住 + 进程消失 + 日志尾部只有正常 PBFT 行、无 Warn/Error、退出码非 0 → 先怀疑本条，不要先怀疑出块逻辑；撞上时保全现场（日志、退出码、datadir 清单），别重启覆盖 |
| R20 | （**已修 `0841cba3e`，逐条核对见对策格"排期"那段**）**`ConsensusView` 全部状态字段无锁，却是三个协程的共享写目标**（R19 的邻缺陷。**两处独立发现，须并列署名**：我在核 `Seal` 的 deadline 与签名域读链时认定，kou-dou-ma 在核 `GetViewOffset`（`:247-249`）裸读 `viewOffset` 时认定，结论一致。分工要说清以免后人误判覆盖面——**他只核了 `dispatcher.go` 内的调用面**，`prepare.go` / `seal.go` / view loop 这三条读写链是我读代码得出的，**`dpos/` 全仓的读写点没有人做过穷尽核对**；动手加锁前必须按访问器名逐个 `grep` 一遍，不要拿本条当清单用）。`dpos/consensusview.go:35-46` 的 `consensusStatus`/`viewOffset`/`viewStartTime`/`viewChangeTime`/`isDposOnDuty` 都是裸字段，全部访问器无一取锁：`resetViewOffset:48`、`SetRunning:52`、`SetReady:56`、`IsRunning:60`、`UpdateDutyIndex:184`、`ChangeView:191-216`、`GetChangeViewTime:239`、`GetViewOffset:247`、`ResetView:251`、`SetChangViewTime:255-259`。**三个并发写方**：①view loop 协程（`consensus/pbft/view.go:80` 起 `viewTick = 1s` 的 ticker，`view.go:113` 与 `TryChangeView` → `ChangeView`）；②miner 协程（`prepare.go:135-147 startView` → `SetChangViewTime`/`ResetView`/`SetRunning`）；③链头协程（`dispatcher.go:268-272` `FinishedProposal` 尾巴 → `SetReady`/`UpdateDutyIndex`/`ChangeView`）。读方包括 `Seal`（`seal.go:224` 用 `GetChangeViewTime()` 算 deadline、`:119` 用 `GetViewOffset()` 定签名域）与 `CleanProposals`（`dispatcher.go:465`）。**后果比 R19 轻但同样落在共识判定路径上**：`viewChangeTime` 是 `time.Time`（多字），撕裂读会拿到垃圾时间 → `deadline` 变负或极大 → `Seal` 立刻 abort 或长时间阻塞；`viewOffset` 撕裂 → 轮次归属错乱、双提案风险。**不是 fatal**（不像 map 会杀进程），所以不会在 devnet 上自动暴露。**这不是"运气不好没撞上"，是运行时有/没有兜底的差别（实测，来源 an-quan-shi，我独立复现过）**：map 的并发写**不带 `-race` 也会** `fatal error: concurrent map writes`（Go 运行时自带检查，4 goroutine × 20000 次写，10/10 抓到）；而**普通字段的竞态不带 `-race` 是 10/10 静默**（同一份写成 `_test.go` 连跑 5 次全 exit 0，`-race` 下立刻 `WARNING: DATA RACE`）。所以 R19 能被 devnet 抓到、R20 不能——**判据必须不同**，R19 可以靠长跑计数，R20 只能靠 `-race`。**当前 `-race` 绿不构成证据**：实测 `go test -race -run TestFailedRoundDoesNotStallTheNode ./consensus/pbft/`（3.4s）无报告，原因是 `viewTick = 1s` 且 `ChangeView` 只在 `offset > 0` 时写，**现有用例没有稳定跨过这个窗口**——这是覆盖缺口 | **需独立立项**（#13 明确划在 R19 之外；**已立项为 #17，owner kou-dou-ma，requirement 见票**；票里已把本节的三条前置抄进去：#13 先落、开工前自行穷举访问面、`ChangeView` 的 `OnViewChanged` 必须放到放锁之后）。推荐 **A：给 `ConsensusView` 加一把 `sync.RWMutex`，只护这 5 个裸字段**，`producers`/`listener` 不动；`ChangeView` 持锁时**只更新字段，`OnViewChanged` 回调必须放到放锁之后**——否则又变成"持锁调外部"（`OnViewChanged → miner.RequestNewWork`），与 R19 那条同型。**A 有八个前置，缺一条就变成"加了一把没盖住现场的锁"（与 R19 的"只锁 `:364`"同型）**：**(a) 同包直接访问必须一起改，不能只锁访问器。**`dpos/` 包内绕过访问器直接读写字段的地方已逐条枚举（`grep "consensusView\.[a-z]"`，10 处）：**修复前该清单共 10 处，现已全部改道（逐条核对见"排期"段）；这里只留名字，因为行号在这批改动里漂了三次。**写方是 `RecoverFromConsensusStatus` 里对 `consensusStatus`/`viewOffset`/`isDposOnDuty` 的三处直写；读方是 `HelpToRecoverAbnormal` 里对 `consensusStatus`/`viewOffset`/`producers.workingHeight` 的三处直读；外加 `RecoverAbnormal` 的 `calculateOffsetTime` 调用、`ResetConsensus`/`ResetConsensusForEpochTransition` 的 `resetViewOffset` 调用、以及 `GetCurrentNeedConnectArbiters` 的 `getCurrentNeedConnectArbiters` 调用。**修复后 `grep -nE "consensusView\.[a-z]" dpos/dispatcher.go` 只剩 2 处**（`:579` 转调 `Producers` 的锁内版本；`:631` 的 `calculateOffsetTime` 是纯函数、只吃参数），**直写归零**；读方改成了复合 getter（`GetViewState()` 一次取三样、`GetWorkingHeight()` 自己取锁）——这正是 (a′) 要的形状，比"逐个 setter"更不容易被读者看成一致快照——后三个本身是访问器，进锁即可，但清单里要有，别漏。**行号已按 `f7c38120c` 之后的文件重取**（`grep -n "consensusView\.[a-z]" dpos/dispatcher.go` 现在正好 10 处，以上逐条列全）；上一版那组 `:556`/`:589`/`:595`/`:597` 是 `f7c38120c` 之前的，**已作废**。这里与 §5.2 `:170` 的处理**故意相反**：那条错的是"读法"，留着能让人看见同一份证据曾支持过相反结论；这条错的是**行号**，读者按陈旧行号跳过去会落在无辜代码上（现在 `:589` 是 `if status == nil {`），据此得出"不存在这处直写"——**误读会导出一个具体动作，所以按 team-lead 的判别标准删除、不留反面样本**。另：上一版还列过一处 `dumpConsensusStatus`，现全仓 `grep -rn dumpConsensusStatus --include=*.go .` **零命中**，那个名字是旧名或是我记错，真实调用点是 `RecoverAbnormal`——**这里上一版写的 `:594` 是错号，别照抄**：在 `737bf1e5b` 和 `356ace37f` 上 `sed -n '594p' dpos/dispatcher.go` 打出来的都是 `func (d *Dispatcher) GetNextNeedConnectArbiters`，一个连 arbiters 的函数，而这句话正在讲裸字段写入——**行号错不等于"基线漂了"，这一条在它自己声明的基线上就是错的**。按 `grep -nF 'func (d *Dispatcher) RecoverAbnormal'` 现取：`737bf1e5b` 是 662、`06ec5dbac` 是 663，**所以这里只留名字、不留数字**。**`RecoverAbnormal`/`HelpToRecoverAbnormal` 在本仓 0 调用者、上游 `network.go:597`/`:924` 调用**——又是"今天死、M3 复活"，而它们直接写裸字段，只改访问器完全盖不住。**(a′) 有一个看着更简单的替代，是陷阱，明确排除**：不要"在 `RecoverFromConsensusStatus` 入口加一次大锁"。它内部要调 `ResetView` 与 `ProducerIsOnDuty`（旧版就是这两处调用，现已并入 `RecoverFromStatus`；这里只留函数名，因为那两行已经不存在了，写行号只会把读者送到别处），而 R20 落地后这两个方法**自己就要取锁**，`sync.RWMutex` 不可重入 ⇒ **同一个函数内自死锁**；**反过来，(a′) 的正确产物也必须诚实标注边界**——`RecoverFromStatus` 内要取 **两次** `p.mtx`（`finishedHeight > 0` 时 **四次**；对应的可插入窗口是**一个**，或 `finishedHeight > 0` 时的**三个**），所以它是"一次持 view 锁的调用"，不是"一次原子操作"（详见本行"排期"段的 (a′) 那句）**这里曾写「三次」——恰好落在两个真值中间。三次比两次或四次更坏**：读的人无论照哪个分支数一遍，都会觉得"只差一个、差不多"，然后不再核；而 2 或 4 一数就打回。**夹在真值之间的错数最像对的。**；更糟的是死锁**不是竞争**，`-race` 抓不到、CI 全绿、只在运行时挂住。所以三处直写**一律改成走 setter/accessor**，不在入口加大锁。另：`publicKey` 是构造后不再变的字段，**不参与加锁**，注释里要写明"这是不可变字段，不是漏了锁"——否则后来人会顺手把它加进去，造出一把永不释放的锁。**还有一个新出现的入口要盯**：`ProcessProposal` 已因 R19 变成"内部取 `mu`"的入口（`dpos/dispatcher.go:130-132`，写 `precociousProposals`），前提是**调用方不得持 `mu`**；R20 之后任何持锁路径去调它或 `ProcessVote` 都是自死锁，而这一类同样 `-race` 抓不到。**(b) epoch 切换有两套语义，不能混用。**`(*Dispatcher).ResetConsensusForEpochTransition`（本仓 0 调用者）是**复用同一个 view 对象**；而 `(*Engine).attachAccount` 会**重建 `dispatcher`**，从而在 `NewDispatcher` 的 `consensusView: NewConsensusView(...)` 处**造出一个新 view**、丢掉旧的视图状态。两条路径混用 ⇒ dispatcher 持旧 view、链头持新 view 的分裂。**这条要写进代码注释，不能只写在本表或票里**（实现者只看代码）；我方不改 `.go`，由 #17 的实现者在同一提交落下。**(c) 修法的形状是"Locked 内部函数 + 导出包装"，不是"给每个访问器都加锁"**——后者会在**编译期就撞上**的地方当场挂死：`ChangeView` 内部调 `ResetView`（`:195`、`:201`），`ResetView`（`func` 在 `:251`、体只有 `:252` 一行转发、`}` 在 `:253`）又只是转调 `SetChangViewTime`（`func` 在 `:255`、体 `:256-259` 写 `viewChangeTime`/`viewStartTime`），`ChangeView` 还调 `resetViewOffset`（`:200` → `:48-50` 写 `viewOffset`）——**三层嵌套**。`sync.RWMutex` 不可重入，所以"每个访问器各自取锁"的实现方式会在 `ChangeView` 第一行之后自死锁；同样地 `ChangeView:210` 调 `DumpInfo`（`:218-229`）、后者调 `ProducerIsOnDuty`（`:269-272` 读 `viewOffset`），`UpdateDutyIndex`（`:184-189`）写 `isDposOnDuty` 并在同函数里读 `viewOffset`。正确形状：**内部一律 `xxxLocked` 且假设已持锁，导出函数取锁后只调 Locked 版**；`ChangeView` / `UpdateDutyIndex` / `RecoverFromConsensusStatus` 这三个复合操作**全程持一次锁、内部只调 Locked 版**，`OnViewChanged`（`:212-214`）放到放锁之后。**这条比"逐个 grep 访问器"重要**：访问器覆盖不全要到 M3 才暴露，而自死锁是**当场挂**——但两者同属"`-race` 看不见"那一族，别指望 CI。**(d) 锁序——这里有一条已经存在的真边，方向不能写反**：`CleanProposals` 的 `changeView=true` 分支在**持有 `mu` 时**读 view——`d.mu.Lock()`（`:405`）/`defer d.mu.Unlock()`（`:406`）之后，`:415` `currentOffset := d.consensusView.GetViewOffset()`。今天它安全只因为 `GetViewOffset` 是裸读（`consensusview.go:247-249` `return v.viewOffset`）；R20 一加锁，**`mu → viewMu` 就成了一条真实边**。所以允许的嵌套是 **`mu` 在外、`viewMu` 在内**，**禁止的是反向边 `viewMu → mu`**（view 内部回调 dispatcher 取 `mu`）；`proposalMu → viewMu` 同样禁止。边表合起来是一句可执行的话：`mu` 可以先于 `proposalMu`/`viewMu` 获取（`ProcessVote → GetProcessingProposal`，`func` 在 `:568`、锁在 `:569`，已经是这么用的），而 `proposalMu` 与 `viewMu` 之间、以及任何"从 view 回到 dispatcher"的方向都不许。**完整链是三段，不是三个平级节点：`mu → viewMu → producers.mtx`。**依据是 `viewMu` **不是叶子**——`ConsensusView.UpdateDutyIndex`（`consensusview.go:184-189`）在 `:185` 调 `v.producers.UpdateDutyIndex(height)`、在 `:187` 调 `v.producers.GetNextOnDutyProducer(...)`，`ChangeView` 在 `:208` 也调 `GetNextOnDutyProducer`，这三个都要取 `producers.mtx`（`producers.go:151`/`:218`）。由此：`proposalMu` 必须**留在叶子位**，**不许在 `viewMu` 之下被取**（否则 `proposalMu` 与 `producers.mtx` 的先后将取决于调用顺序，等于再造一条未声明的边）；`producers.mtx` 自身是真叶子（见 (e) 末尾的核验）。这条链的登记位置是 `dispatcher.go:34-53` 的锁序注释，现只有 `mu → proposalMu`（允许、必须）与 `proposalMu → mu`（禁止）两条，**第三条 `mu → viewMu` 由 #17 的纯注释提交补入**，措辞照抄我们定下的那句：这条边今天不可达，但它**由函数体定义、不由调用者定义**。这也顺手解释了 (g) 为什么必须存在：`viewMu → producers.mtx` 一旦成边，`UpdateProducers` 若"为一致性"反向取 `viewMu` 就直接成环。**反方向那条边 `viewMu → mu` 禁止——但它的成立理由不在 `dpos/` 里，必须一起写下来，否则等于把一条有外部前提的约束伪装成不变量**：`ChangeView` 放锁之后调 `OnViewChanged` → `miner.RequestNewWork` → `worker.requestNewWork`（`miner/worker.go:423-430`），那里是 `select { case w.startCh <- struct{}{}: default: }` 的**非阻塞发送**，`startCh` 只有一格缓冲，注释原文写着 "Non-blocking on purpose … A blocking send here would let a busy miner stall the view clock, which is how a view change turns into a stalled chain"（`:419-422`）。所以这条回调链不会绕回来取 `mu`。**一旦有人把 `requestNewWork` 改回阻塞发送，这条边立刻存在，而 `-race` 看不到死锁**——能抓它的只有闸门 ②b 那个用例内 2 秒计时器。**而且那段注释本身不是断言**：`miner/worker.go:419-422` 只是文字，没有任何用例守着"发送必须非阻塞"，改回阻塞发送测试不会红。**所以 ②b 的 2 秒计时器不是锦上添花，它是这条边唯一的看守**——把上面这段引用读成"已有保证"，等于把注释当测试；这一节引用它的目的，是让下一个人知道**地板是谁家的**，不是让他以为地板焊死了。**同一个 `ChangeView` 里有一对方向相反的边，性质不同，别写成同一种**：`mu → viewMu` 是**由函数体定义的边**（`CleanProposals` 的 `true` 分支在 `mu` 内读 view，读代码即可确认；今天不可达，但不可达只说明没被执行、不说明不存在）；`viewMu → mu` 是**由外部条件维持的非边**（依赖 `miner` 侧的实现细节）。前者可以只写"禁止/允许"，后者必须写"为什么现在成立"，否则下一个人只会看到一条禁令、看不到它脚下的地板是别人家的。**这条边必须按「函数能做什么」写，不能按「今天谁调它」写**：`CleanProposals(true)` 在本仓**零调用**（三个生产调用点 `:309`/`:324`/`:359` 全传 `false`，测试也全传 `false`），但**上游在四处调 `true`**（`pgp-chain/consensus/pbft/pbft.go:1189`、`dpos/manager/dposnormalhandler.go:89`、`dpos/manager/dposondutyhandler.go:57`、`dpos/manager/proposaldispatcher.go:822`——**均为模块版** `Elastos.ELA@v0.9.8-...-363670e32ee4`；`pgp-chain` 经 `replace` 编译的是 `third_party/Elastos.ELA`，同名函数 `:213`、该调用 `:841`，两份不同、行号不可互换，见 §2.3）——它是**移植了但没接线**，不是死代码；不可达的分支照样会在被接线那天决定谁能嵌套谁，而锁序是它复活前就已经欠下的债。**这条边是我上一版写反的**：原句是"取 view 锁时不持 `mu`"，与 `:415` 直接矛盾。纠正来自 yan-guo-guan 复验 `#13` 时的静态审查，team-lead 独立指了同一条。**(e) `view.mu` 盖不住 `Producers` 的裸字段读——这是 A 方案的一个真实盲区，必须一起修。**`Producers` 自带 `mtx`（`dpos/producers.go:46`），但**不是所有访问都走它的方法**：`GetTotalArbiterCount`（`consensusview.go:81`）、`GetSpvHeight`（`:85`）、`GetTotalProducersCount`（`:89`）直接 `return v.producers.totalProducers/spvHeight`；`ChangeView` 的日志行读 `v.producers.dutyIndex`（`:207`）；`DumpInfo` 直接遍历 `v.producers.producers` 这个裸切片（`:220`）。写侧**三处，不是两处**：`UpdateProducers`（`producers.go:62-70` 持 `p.mtx` 写三个字段）、`UpdateDutyIndex`（`:150-158`，`:157` 写 `dutyIndex`）、以及**完全绕开 `p.mtx` 的第三处** `SetWorkingHeight`（`:90-92`，函数体只有 `p.workingHeight = changeHeight` 一行，**不取锁**），唯一入口是 `ConsensusView.SetWorkingHeight`（`consensusview.go:110-111`），生产路径上只在 `RecoverFromConsensusStatus` 里被调（现已经由 `ConsensusView.RecoverFromStatus` 一次持锁完成）——又是"今天死、M3 复活"，与 (a) 里那两个恢复函数同一批。**这一处与 (g) 同型，但失败方式不同、要分开记**：`SetWorkingHeight` **必须保持无锁**，因为 `ChangeCurrentProducers` 已经在持 `p.mtx`（`p.mtx.Lock()` 在 `:73`）时于 `:84` 调它；谁"为一致性"给它补一行 `p.mtx.Lock()`，死的不是锁序（`viewMu → producers.mtx` 这条边本来就合法）、而是**普通 `sync.Mutex` 的不可重入**——当场自死锁，`-race` 一个字节都不会报。**所以"五处"要补到七处**：第六处是 `dispatcher.go:598` 的 `status.WorkingHeight = d.consensusView.producers.workingHeight`（见 (a)）；**第七处是 `ConsensusView.GetDutyIndex`（`consensusview.go:243-245`，函数体只有 `return v.producers.dutyIndex`）**，它在 `RecoverFromConsensusStatus` 的收尾日志里被调（`dispatcher.go:673`），而那条正是"今天死、M3 复活"的恢复链——`viewMu` 同样盖不住，修法同上。**另有一族不是竞态、是同族，但"零调用者"这个说法要打补丁——而且它有 两个实例，只修一个会以为整族清了。**两者**都取了 `v.producers.mtx`**，所以取字段那一下没问题；问题在紧接着的 `sort.Slice` **原地排序共享切片**：锁保住了"读的时候没人替换字段"，保不住"别人正在 `range` 同一块底层数组"，更要紧的是 `dutyIndex` 是**这个切片的下标**，排序一改，此后每次 `GetNextOnDutyProducer` 选出来的人就变了。**实例一 `IsCurrentProducers`**（排的是 `v.producers.producers`——当前集合）：排序只影响这一次查询之后的顺序。**实例二 `IsSameProducers`**（取的是 `v.producers.nextProducers`，见函数体前几行）**比实例一危险一个量级**：`nextProducers` 是**将来要生效的集合**，`ChangeCurrentProducers` 会把它采纳成 `producers`——所以这里不是"这次查询选错人"，而是**把改过的顺序留在了那个将来会被采纳的切片上**。两者在 `bsc` 全仓都零调用者，**上游都是活的**：`IsCurrentProducers` 有 6 处（`network.go:222`/`:253`/`:463`/`:483`/`:1132`、`eth/backend.go:476`，经 `pbft.go:346`/`:349` 转发），`IsSameProducers` 有 5 处（`pbft.go:318`、`:346` 转发定义、`network.go:426`/`:520`/`:1136`）——M3 接 `network.go` 时两处一起复活。所以"今天无害"必须写成"M3 起必须保证每个节点排出同一个顺序"，而不是"没有调用者所以不用管"（这正是 §8.3"死代码不是没有语义"的现场）。#17 的处置：**别删、别加锁、留一行注释，并在注释里点名有两个实例**，因为删或改语义会把这一票从"加锁"扩成"改行为"，而这批票的证据链只支持前者。**改法上有一条现成的安全出口**：`GetProducers()`（`producers.go:161-167`）返回的是 `make`+`copy` 出来的**拷贝**，所以前面那句"改走 `GetProducers()`"不会引入别名、可以放心用；但 `DumpInfo`（`consensusview.go:218-229`）现在是在 `:220` 直接 `range v.producers.producers`，必须换成它（换掉之后每次迭代也不再各取一次 `p.mtx`，`:221` 的 `ProducerIsOnDuty` 就改为读快照）。**顺便记下 `producers.mtx` 的叶子性是有代价维持的**：`:247` 的 `HasArbitersMinorityCount` 在 `:246` **先 `Unlock()` 再** `GetMajorityCount()`（后者自己取锁 `:192`），`GetCRMajorityCount`（`:199-201`）选择不取锁只转调——这两处都是在手工规避不可重入，不是巧合；谁把"先解锁再调"整理成"一起锁着"，得到的是死锁而不是更安全。**所以这六处与三个写点是真竞态，而 `view.mu` 加得再对也不解决——它们根本没碰 view 的字段。**`DumpInfo` 尤其危险：它在 `ChangeView:210` 被调，形态上像"已经被 view 锁保护了"，实际是绕过另一把锁去读一个正在被替换的切片。修法：这五处改走 `GetProducers()` / 新增 Locked 访问器；`GetNextOnDutyProducer`（`producers.go:217-219`）本身取 `p.mtx`，所以 `view.mu → producers.mtx` 这条边**成立、单向、必须保留**——这也正说明同一个 struct 里两种访问方式并存，方法走锁、字段裸读。**(f) 日志不得在锁内发，且 `DumpInfo` 在 `ChangeView` 内必须走 Locked 版。**加锁后 `ChangeView` 的 `Info`（`:205`、`:206-207`、`:211`）与 `DumpInfo` 里的 `Info(str)`（`:228`）全落在锁内；锁内做阻塞 I/O 是另一类问题（日志写盘/轮转可能阻塞），应改成"锁内 snapshot → 锁外拼串与打印"。更硬的一条：**`DumpInfo` 在 `ChangeView` 内若调导出（自己取锁）版，就是"持写锁再 RLock 同一把 `RWMutex`"——比 ABBA 更直接的自死锁。**(g) `UpdateProducers` / `ChangeCurrentProducers` / `UpdateNextProducers` **不许加 `view.mu`**。它们只动 `producers.*`、自带 `p.mtx`；"为一致性也加上"看起来很自然，但那会造出 `view.mu ↔ producers.mtx` 的反向边，直接违反 (d)。**这条要写进代码注释**，否则下一个人一定会加。B：不改 `ConsensusView`，把 view loop 的 tick 与 `Seal`/`Prepare` 的读串行到同一协程——架构改动大、且会把 `Seal` 绑到 tick 上，不推荐。**排期**：**已落地（`0841cba3e`；变异与时序用例在 `a89492c3a`、`e964242f1`）**。本条曾长期是这批里**唯一没修的**，也是唯一一条"不 fatal、devnet 永远不报、只能靠 `-race` 抓"的缺陷（M3 会再加进网络协程这个**第四写方**）。落地后我按 HEAD 逐条复核了上面八条前置——**七条齐、一条只落了一半**，不足的那半见残留③：①`ConsensusView` 加 `mu sync.RWMutex`（`consensusview.go:72`），五个裸字段全部走 `xxxLocked` 内层（8 个 Locked 函数，`:90`-`:139`），导出层取锁后只调 Locked；**(a)** `dispatcher.go` 里绕过访问器的直访从 10 处降到 **2 处**，且两处都安全——`getCurrentNeedConnectArbiters` 转调 `Producers` 的锁内版本，`calculateOffsetTime` 是纯函数、只吃参数不读字段；**(a′)** 没有采用"入口大锁"，而是新增一个复合操作 `RecoverFromStatus`（`ConsensusView.RecoverFromStatus`）**持锁期间做完全部五件事**，注释写明了理由：分两次调 setter 会让读者看到"恢复后的 offset 配恢复前的 startTime"这种从未存在过的组合。**但"一次调用"不等于"一次原子操作"，这一点必须跟着记，否则下一版注释会再被读成保证**：该函数在 `v.mu` 内**顶层三个语句；`finishedHeight > 0` 时展开为四次获取 `p.mtx`、三个可插入窗口，`finishedHeight == 0` 时只有两次获取、一个窗口**（`updateDutyIndexLocked` 整个坐落在 `if finishedHeight > 0` 里）：`Producers.UpdateWorkingHeight`（第 1 次）、`producerIsOnDutyLocked` → `GetNextOnDutyProducer`（第 2 次）、`updateDutyIndexLocked`（第 3 次 `UpdateDutyIndex`、第 4 次 `GetNextOnDutyProducer`——**这个内层函数自己取两次**），每次取完即放；而 `ChangeCurrentProducers` 只取 `p.mtx`、不取 `v.mu`，**一旦有调用者，就能插在任何两个窗口之间改掉 `workingHeight`**（可达性见下）。**"三处"是我上一版的写法，量的是语句位置；决定交错面大小的是窗口数。而"四次"同样必须带条件**——我上一版写成了无条件四次，实际 `finishedHeight == 0` 时只有两次，**所以"第 3、4 次之间"这个窗口也只在 `finishedHeight > 0` 时存在**（`#26` 落地的代码注释用的就是 "two when finishedHeight is 0, four when it is positive"）。最该记的就是（`finishedHeight > 0` 时的）第 3、4 次之间那一个：`UpdateDutyIndex` 已经改了 `dutyIndex`、`GetNextOnDutyProducer` 还没算 `isDposOnDuty`，此刻若 `ChangeCurrentProducers` 插进来，`updateDutyIndexLocked` 里写 `isDposOnDuty` 那行就是**新 `dutyIndex` 配另一套 `producers`** 算出来的——**这不是数据竞争**（每一步都持 `p.mtx`），是一次**语义上从未存在过的组合**，与那对"从未存在的组合"同族。**但必须紧接着说清可达性，否则这段会被读成"现在就发生"：这个窗口在本树内目前没人能走进去。**`ChangeCurrentProducers`/`UpdateNextProducers` 在本仓只有 `ConsensusView` 内部的两个转发，**而且连测试都没有**（`grep -rlF '<函数名>' --include=*_test.go .` 两个都空——"没有生产调用者"与"没有调用者"是两个结论，这里成立的是后者）；`Dispatcher` 上只暴露 `IsProducer`/`ProducerIsOnDuty`，没有任何生产者更新的转发方法；`ETUpdateProducers`（`dpos/event.go`）定义后**没有订阅者**，`dpos/msg/producersmsg.go` 的 `ProducersMsg` 也没有任何生产代码把它解出来应用到 `ConsensusView`。**结论：生产者集合自 `NewProducers` 之后在运行期永不变化。**上游是活的——`pgp-chain/consensus/pbft/network.go:505` 调 `ChangeCurrentProducers`、`pbft.go:304`/`:312` 调 `UpdateNextProducers`、`pgp-chain/consensus/pbft/network.go:174` 调 `UpdateProducers`（**四处，不是三处**；任务表那张票的标题写的就是"4 个上游调用点"，而本文上一版只列了三处，漏的正是 `UpdateProducers`），**又一次"本仓零调用者 + 上游有调用者 = 移植了但没接线"**；更准确的说法是**方法搬了、调用点没搬**（这个定性可证伪：在本仓找到任何一个真正的生产调用点，整条作废）。所以准确写法是：**窗口在结构上存在，本树内无人可达；哪个端点先接上，它当天就可达——下界是 M5**（此处原写"M3 接上"，与 §六 M3 缺口段冲突：R13 的 SPV 事件端排在 M5，`network.go` 那两处尚未排期；**M3 范围内它仍不可达**，所以 M3 的互通验收与 devnet 长跑都不覆盖它）。这也解释了单节点 devnet 跑一百多块为什么一次没触发——**不是"可能没触发"，是不可能触发**。**"不可达"三个字必须带证据，否则它和"我没测到"长得一模一样**——否定性结论在本轮反复出事的正是这一点。这里的证据按强度分两级，别混着引：**①证据（可复跑、可逐条反驳）**：`grep -rnF 'ChangeCurrentProducers' dpos/ consensus/` 的每一条命中都能归到转发 / 初始化 / 测试；`Dispatcher` 上没有生产者更新的转发方法、`ETUpdateProducers` 没有订阅者、`ProducersMsg` 没有生产解码方。**②旁证（不是证据）**：devnet 两轮日志里各有 `SPV reported an ELA block it never committed ... elaHeight=0`、`spvHeight` 停在启动值（67500 / 50000）不再前进——这只说明"这次运行里那条链没走动"，不说明"代码里没有使能路径"：**一个从没启动过的 goroutine 和一条走不到的分支，在日志上留下的是同一个空白。把观察当证据，就是拿"没发生"当"不可能"。**把它写成"现在就会发生的交错"强度过了，属于同一族里的"文档声称强于代码"。原文两处分别在：`RecoverFromStatus` 自己的函数头注释（原句 "another goroutine could read the recovered offset with the pre-recovery start time"）与调用点注释（`RecoverFromConsensusStatus` 里调 `RecoverFromStatus` 之前那段，原句 "a deadline computed from a pair that never existed"）。**这两处只给原句、不给行号**：调用点那段在我写它到现在，同一个文件里已经下移过两次（`737bf1e5b` 与 `92562efdd`），每次都是 `grep -nF` 一量才发现——**行号会漂，原句不会；锚点是能 grep 到的那句话。**——**别把这两句混成一句**，它们分属两个文件；这里要说的是同一族故障在 `p.mtx` 这一层又出现了一次。所以准确的分法不是"view 原子 / 派生不原子"，而是**把存储与取值分开**：view 的**五个**字段（`consensusStatus`/`viewOffset`/`viewStartTime`/`viewChangeTime`/`isDposOnDuty`）全部在**同一次 `v.mu` 持有内**写完——**注意"五个"只是"受 `mu` 保护的那一子集"，结构体一共十个字段**：`mu`、`consensusStatus`、`viewOffset`、`publicKey`、`signTolerance`、`viewStartTime`、`viewChangeTime`、`isDposOnDuty`、`producers`、`listener`。按 §8.3 第一条老实去数的人会数出 **10**，所以这里必须把五个点名、把总数也写出来，否则**一个正确的数字会被读者判成错误**——那比写错数字更贵，因为从此这段里别的数字也不被信了。剩下五个各有不归 `mu` 管的理由：`publicKey`/`signTolerance`/`listener` 只在 `NewConsensusView` 的复合字面量里设定、构造后不再写（`grep -nF "signTolerance:" dpos/consensusview.go` 等各命中构造函数那三行），`producers` 是 `*Producers`、自带 `p.mtx`，`mu` 自己与 `listener` 都不是"被保护的数据"，所以任何取 `v.mu` 的读者**永远看不到半份 view**——这部分是真原子；但 `isDposOnDuty` 的**取值来源**可能是一个从未存在过的 `Producers` 组合（第 3–4 次获取之间那个窗口），**它的存储是原子的，它的推导不是**；`dutyIndex` 则整个属于 `Producers`，由 `p.mtx` 保护存储、跨锁的组合不保护。一句话：**锁保护字段，不保护"这个字段凭什么算出来"。**（这条分法是 kou-dou-ma 纠正的：我上一版写"四个字段原子 / 派生的不原子"，会把 `isDposOnDuty` 整体划进非原子那一侧——而它恰恰是这次交错里最要紧的那个；**从"过度保证"跳到"过度否定"是同一个错误的镜像**，注释改数字时最容易踩。）**判据一句话：当一个函数内部要取第二把锁时，"one critical section" 就已经不成立了，无论它在源码上是不是一段连续代码**（**注意这个短语在 `dpos/dispatcher.go` 里有两处**：一处在 import 高度并发的注释里、一处在 `RecoverFromConsensusStatus` 的调用点——按 `one critical section` 直接 `grep -F` 会同时命中两处，**锚点必须用更长的一截 `One call, one critical section`**；代码里那句现在（**已落 `981818400`**）是 **`One call, one view-lock critical section`**，同段还补了 "No annotation here should say atomic, because the code is not"；这条经历过"定稿 → 落地"两态，而**引用一个还没落地的措辞，和引用一个已被改掉的措辞，读者看到的是同一个空结果**——所以本轮的做法是：落地前按现文引并标注定稿，落地后同一批切过去（`981818400` 之后我切了））；同族再补一句：**"每次访问都加锁"不等于"整段语义一致"——前者是逐点正确，后者要一次临界区；锁的粒度能解决撕裂，解决不了组合。**——旧版是数据竞争，新版是合法交错，这是改善，但不是原子；**(d)** 链序成立——`RecoverFromStatus` 持 `v.mu` 时调 `Producers.UpdateWorkingHeight`（取 `p.mtx`），是允许方向 `viewMu → producers.mtx`，`proposalMu` 仍是叶子；**(e)** 七处裸读全部改道：`GetDutyIndex` 现在转调 `Producers.GetDutyIndex`（`:132-136`，锁内），`workingHeight` 有锁内 `GetWorkingHeight`（`:114-118`），`DumpInfo` 改用 `GetProducers()` 的拷贝；**(f)** `ChangeView` 锁内只做 snapshot（`dumpInfoLocked`），`Info` 与 `listener.OnViewChanged` 都在解锁之后（解锁在 `:377`、回调在 `:392`）；**(g)** 直接写成了代码注释（`consensusview.go:217-221`："taking mu here would create the reverse edge `producers.mtx -> viewMu`, and an ABBA cycle against `ChangeView`"）。**②b** 也有实物：`consensusview_deadlock_test.go:54-55` 是用例内 `time.After(2 * time.Second)` + `t.Fatal`，失败消息直接点名"同一协程重复取锁"，不依赖 suite 的 `-timeout`。**一处残留 + 一处已处置，别读成"R20 修完就没事了"**。**已处置（不计入残留）**：`ConsensusView.SetWorkingHeight`（`consensusview.go:244-249`）本仓零调用者，但**上游 `pgp-chain/dpos/dispatcher.go:522` 调用它，故保留**；实现已改为取锁转发 `Producers.UpdateWorkingHeight`。删掉它不是"从 git 历史找回"，而是要在上游**已经调用它的位置**凭空复原一个函数——所以它从来不是"可删的包装"，我上一版判错过一次，依据是"本仓零调用者"这个**没有主语**的结论（见 §8.3 第五条）。**唯一残留**：**(b) 只落了一半**：`attachAccount`（`engine.go:206-212`）的注释写了 "rebuilds the dispatcher around it"，但**没写它与 `ResetConsensusForEpochTransition`（`dispatcher.go:334`，复用同一个 view 对象）是两套语义、混用会让 dispatcher 与链头各持一个 view**——而这正是 (b) 点名要进代码的那一半，理由是**实现者只看代码、不看本文**；**两边都要写，不能只写这边**（已转 kou-dou-ma 落代码注释）。**第二处残留（`92562efdd` 引入，已在 `981818400` 修掉，留档）：代码注释里的取锁次数比实际少一半。****两处，不是一个**（这个形态和 `IsCurrentProducers`/`IsSameProducers` 一模一样：只点名一个实例，后来人以为整族清了）：`(*ConsensusView).RecoverFromStatus` 的 doc 写 "p.mtx is taken **twice** with a release in between"，`Dispatcher.RecoverFromConsensusStatus` 的调用点又写了一遍 "RecoverFromStatus takes p.mtx twice with a release in between"——**同一句错话在调用点和被调用方各写一次**，改完一处另一处会原样留着。实测 **四次**：`UpdateWorkingHeight`、`producerIsOnDutyLocked` → `GetNextOnDutyProducer`、`updateDutyIndexLocked` 里的 `UpdateDutyIndex` 与 `GetNextOnDutyProducer`——**第②次被漏掉，第③④次被当成一次**。它错的方向很典型：**把"几个语句"当成"几次获取"**（正是本行开头那句"顶层三个语句、展开四次获取"要区分的东西）。kou-dou-ma 认下并立 **`#26`**，**由 an-quan-shi 落 `981818400`**（**归属要写清：这次纠正是代码贡献的，不是草稿贡献的**——kou-dou-ma 的草稿本身写的就是无条件的 `four times`，是落地版把"`finishedHeight == 0` 时两次"写了进去。**"注释纠正了文档"这句话一旦署名错，后面所有引用都跟着错**，而这是本小时第二次归属错），且落地时全仓 `grep` 了 `twice` 这类计数词（正控：同一条命令喂 `p.mtx` 必须非空）；**验收判据不是"改完了"，而是"剩余命中逐条说明为什么不是同一件事"**。**分母必须由命令生成，不能由共识生成**：`grep -rniF 'twice' dpos/ consensus/ \| wc -l` 在 `356ace37f` 上是 **7**（修复前 **8**，算式是 `8 − 2 + 1`：删掉两处错的，同时新写出一处对的）。**而"四处"这个数今天是能复现的——只不过它是另一个分母**：`grep -rniF 'twice' dpos/` 恰好 **4**，`consensus/` **3**，合计 7。所以三份表都对得上"四"不是三方独立验证，是三方**都在 `dpos/` 里数**；票据命令的范围是 `dpos/ consensus/`，范围一放大就多出三处，而没人重跑过那条命令。**这是"分母"那一族在本轮第三次出现，形态是"数是对的，作用域是错的"——它比数错更难发现，因为数字能被复现。**七条逐条（只给原句与文件、不给行号：树外那张方向词补丁落地后，同文件内的行号会整体位移，而原句不会）：①`asking twice would be two p.mtx acquisitions`（`dpos/consensusview.go`；`GetProducers` 与 `GetDutyIndex` 确实是两次独立获取，而且这句恰恰是在说"别再问一次"）；②`the part worth reading twice`（同文件；修辞，没有主语）；③`isDposOnDuty is assigned twice`（同文件；真的两次赋值）；④`once -- twice, or four times when finishedHeight is positive`（`dpos/dispatcher.go`；**这条是修复自己新引入的**，它没改错，但任何"命中归零"式判据都会把它判成没改干净——修复会以自己的产物形态留在 `grep` 结果里）；⑤`a round can be started twice`（`consensus/pbft/view.go`；主语是 goroutine 生命周期，与 `p.mtx` 无关）；⑥`created per round can be created twice`（`consensus/pbft/prepare.go`；同族，后果是两个循环把 `signTolerance` 减半）；⑦`finalised twice at one height`（`consensus/pgp/confirm_test.go`；业务语义，同高度两个块都被最终化）。**"8 → 7"这个算式本身比七条清单更该记**：一次修复可以同时删掉错的、又写出一处对的，所以判据只能是"每条命中都解释得清"，不是"数变小了"。**在它落完之前，任何人都不要引用 "twice" 这个数**，也不要把它当"注释已经说过"的依据——它是同一族里最新鲜的一个例子：**注释声称的次数比实际弱，读者会据此以为交错面已经最坏。****残留③（方向词指错，已落 `06ec5dbac`，留档）**：与 `#26` 同一族、不同票，**别当成 `#26` 没改干净**。修复前那四处的原句（现在 `grep` 不到了，找不到是正常的）：①`dpos/consensusview.go` 的 `the second one, below`——"第二个"确实在下，但读者不知道是哪一个，要直接点名 `updateDutyIndexLocked`；②同文件 `isDposOnDuty, which was written above unconditionally`——**赋值在它下一行**，方向词指反了；③`dpos/dispatcher.go` 的 `One read of the view offset, not two: the two reads below ...`——**下面只有一次读**（这句自己正在说"合成了一次"，却用"下面的两次"指过去），追到 `f7c38120c`（来源 kou-dou-ma，`git log -L 120,122`），不是 `#26` 引入的；④`RecoverFromStatus` 里那段"取模不算错位"的解释**根本不存在**——它只活在提交信息里，而三处直写改道后这段解释是防止下一个人把它"优化"回取模的唯一屏障。**现文锚点（可直接 `grep -nF` 到）**：`inside updateDutyIndexLocked`、`One read of the view offset, not two.`、`each reduce their index modulo`。**验收判据是两条，缺一条都不算**——修复前原句 `grep -F` 归零、现文锚点各有命中；只做前一条，"删掉了"和"换成另一句坏话"在输出上分不开。四处是同一个动作：**用方向词或数量词去指一个已经变过形的东西**，与 `twice` 数错是同一个动作的又一族实例。**这里同时是本轮"提交信息不是证据"的现场，而且出现了两次、方向相反**：`981818400` 的信息写着"It now names updateDutyIndexLocked"与"It now points at that assignment"，`git grep -F` 到那个提交上一查，①②原样都在、`modulo` 在 `dpos/` 零命中；而 `06ec5dbac` 的信息与代码一致。**所以"已修 X"要落在 `git grep -nF "<落地后的原句>" <commit>` 上，不能落在 `git log --format=%B` 上**——同一句断言既可能夸大也可能属实，只有代码能判：**读代码、读提交、读文档是三个来源，两个来源一致不构成验证。****（本节上一版列过的第三处残留——R19 第 ⑧ 条，恢复路径无 `mu` 重换三张票数表——已在 `3f6c2d188` 补修：抽成 `resetVoteMapsLocked` 一个实现、`CleanProposals` 与 `RecoverFromConsensusStatus` 两个调用点，所以两处不会再各自漂。详见 R19 行第 ⑧ 条。）****与 R19 的闸门①互为硬约束**：R19 要求"先造红"，但任何经过 `FinishedProposal` 尾巴的并发构造都会先红在本条上，所以 R19 的用例必须走 `CleanProposals(false)` × `ProcessVote`；反过来，本条的"跨 tick 造红"用例正好可以给 R19 当变异对照。**两条必须在同一批里做完再开 M3，不要分两次动 `dpos/`** | 闸门：①**必须先造红**——新增一条跨 tick 的用例（至少等 2 个 `viewTick`，且能让 `ChangeView` 的 `offset > 0` 分支真的执行），`-race` 下必须报出竞争；②加锁后转绿，并做**变异验证**（去掉锁必须变红）；②b **死锁类变异用"限时 FAIL"，不要靠 suite 级 timeout**：用例在 goroutine 里跑 `ChangeView`，`select { case <-done: case <-time.After(2*time.Second): t.Fatal("ChangeView did not return -- a mutex was re-entered") }`。靠 suite 的 `-timeout` 默认是 10 分钟（R19 那次实测是 `panic: test timed out after 1m30s` 才红），90 秒的判据会退化成"太久没人跑、跑了也懒得等"；用例内 2 秒 FAIL 能把原因直接打在人脸上。变异是一次性动作、不随用例出厂，输出（改了哪个包装 + FAIL 行）记进提交信息；③`go test -race -count=10 ./dpos/ ./consensus/pbft/`；④**devnet 必须用 `-race` 构建的二进制跑一段**——#13 的"107 块 0 fatal"不能当无竞争的证明 |

### 7.1 移植继承的缺陷（非锁）：R21 / `#24`（已修 `737bf1e5b`）

这一条**故意不放进上表**，理由是索引而不是体量：R1–R20 那张表的边界是"锁与并发"，把一条 nil 解引用塞进去，会让"R20 = 加锁那一批"这个索引失效——下一个人按 R20 查锁的问题时，会读到一条与锁无关的东西，然后开始怀疑整张表的边界。**编号：文档里是 R21，任务表里是 `#24`——同一件事的两个坐标，不是两件事。**下一轮有人按 R21 查不到票、或按 `#24` 查不到文档，两个编号各自看起来都完整；所以新票一律双号登记（`#13`/R19、`#17`/R20 能对上纯属运气——每次对话里都恰好同时提到两个号）。

**`RecoverAbnormal` 的 nil 检查曾在解引用之后 → 传 nil 必 panic。下面是它的历史形态，现状见下一段。**当时是 `dpos/dispatcher.go:625` 先用 `status.ConsensusStatus`/`status.ViewOffset`/`status.ViewStartTime` 拼日志，`:626` 才 `if status == nil`。**上游同序——这是一条可核的证据，不是"原样"两个字**：`pgp-chain/dpos/dispatcher.go:481` 用 `fmt.Println` 打的就是同一组三个字段（`status.ConsensusStatus`/`ViewOffset`/`ViewStartTime`），`:482` 才 `if status == nil`（这两行我在 `pgp-chain` 检出 `a0cecae32fab…` 上用 `awk 'NR>=478 && NR<=484'` 重跑过）；我们的 `:625`/`:626` 与它**逐字对应**（我们的 `Info(...)` 就是那行 `fmt.Println` 的移植）。所以可以排除"移植时把两行顺序弄反了"。**"原样"本身是需要证据的断言，和"已修"同一类**——所以这里给的是两对行号的对应关系，而不是一句结论。结论是**继承来的、不是移植改坏**；但"上游如此"不是保留理由：我们要对旧节点保真的是**线格式**，不是缺陷本身。

**现状（`737bf1e5b`，"dpos: the recovery entry points check nil before they dereference it (#24/R21)"）**：`RecoverAbnormal`（现在在 `:662`）的第一条语句就是 `if status == nil { return }`（`:663`），日志在其后（`:666`）。判据可原样执行：`grep -nA3 'func (d \*Dispatcher) RecoverAbnormal' dpos/dispatcher.go` → `:662` 函数行、`:663` nil 检查、`:664` `return`。**同一提交里还修了同族的第二处，而这一处原票没写、必须记账**：`RecoverFromConsensusStatus`（`:691`）原先**完全没有 nil 检查**，现在有（`:692-694`）并返回 error；理由不是洁癖——它是导出函数，上游存在一个**不经 `RecoverAbnormal`** 的调用者：`pgp-chain/third_party/Elastos.ELA/dpos/manager/dposhandlerswitch.go:225`/`:230`，两处都已经是 `if err := …; err != nil` 的形状（我核过）。**由此带出第三条同族缺陷，比前两条更隐蔽**：`RecoverAbnormal` 调它却把 error 丢了（Go 静默丢弃返回值），那等于把"panic"换成"静默跳过"——同一个缺陷往前挪一步、更难发现。现在那个唯一的树内调用点会打错误日志。

**为什么它比看起来重要**：它所在的函数正是"本仓零调用者、上游 `pgp-chain/consensus/pbft/network.go:597`/`:924` 活、M3 必然接线"的那一条；接线当天一个 nil status 就 panic，而**现场指纹恰好抹掉我们要看的东西**——日志停在 `RecoverAbnormal` 第一行之前，可那一行本身就是排查的起点（`dpos/log.go` 的格式问题已让日志难读，再少一行等于瞎一半）。

**改法与验收**：把 nil 检查提到函数第一行、日志移到检查之后；零风险、纯排序改动。**不要**顺手加 `recover()`、也不要改签名——那会把"调用者传了 nil"这件事藏起来，而它恰恰是该暴露的。**三条都已满足**：①函数第一行是 nil 检查（上面那条 `grep -nA3` 的输出）；②`dpos/dispatcher_recovery_nil_test.go` **故意不带 build tag**——它断言的是确定性事实（panic 与否、error 与否），所以必须在普通 `go test ./dpos/` 下也跑；只在 `//go:build race` 文件里存在，等于这条守卫在常规运行里不存在（这两个函数 devnet 到不了、`-race` 也只能经测试到达）。三个用例：`TestRecoverAbnormalWithNilStatusDoesNotPanic`、`TestRecoverAbnormalAppliesStatusWithMedianTime`、`TestRecoverFromConsensusStatusRejectsNil`。③**变异四条全部变红（`EXIT=1`）**，由 kou-dou-ma 在 `/tmp` 的 rsync 副本里跑、变异体未提交、还原后 `shasum -c` 全 OK。**其中两条值得单独记，因为它们是"看起来在测、其实没在测"的活样本**：其一，"把检查移回日志之后"会让 `medianTime=0` 与 `medianTime=1600000000` **两行同时**红——说明第二行**并不是**靠 medianTime 的取值在判，他最初给的理由是错的（测试注释已改成真理由）；其二，`TestRecoverAbnormalAppliesStatusWithMedianTime` 是必需的——**"没有 panic"这个断言对一个什么都不做的函数同样成立**，所以必须另有一条"状态真的传到了 view"。**这两点正是 §8.4 第二行说的：阴性判据单独用，会把空函数判成通过。**

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

**读这一节之前先看一条，因为它解释下面这些规则为什么反复出现：每当一条规则需要别人配合才能成立，就先问「能不能改成不需要配合」。** 本轮同一形状出现三次，**三次都在指挥侧**（给规则的那一侧），不在实现侧——这一条值得单独写出来，因为它说明这个错法不挑人，挑的是"谁在写规则"：

| 我给出的规则 | 被什么推翻 | 更好的解 |
|---|---|---|
| 否定性结论必须用 `grep -E` | ERE 里 `+` 是量词，不是加号 | 默认 `-F`，多字面量用两个 `-e` |
| 二进制必须贴 `vcs.revision`/`vcs.modified` 才算数 | worktree 下 buildvcs 静默放弃 | `git clone` 到别处再构建 |
| 等共享树干净窗口再构建 | `vcs.modified` 是**全仓**判断，任何人的在飞改动都算 | 同上：clone 不需要窗口 |

**三次都是在「约束别人行为」，而更好的解都在「改变获取方式」。** 约束行为的方案有三个坏处：要求所有人配合、把失败归因成"谁没配合"、**看起来像规则其实是一份协调请求**；改变获取方式只有一个坏处——**要想到它**。这套协调在今晚花了三轮消息在等一个本来不需要等的窗口。

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
  - 理由必须写在验收记录里：-race 把执行拖慢数倍，而我们是 3s 周期 +
    1s viewTick 的真实定时器，抖动被放大后"时序类失败"必然大量出现。
    把它们当判据 ⇒ 这条 smoke 永远红 ⇒ 最后没人看它，那比不跑更糟
  - 为什么这条不能省（判据按缺陷类型分）：map 的并发写有运行时兜底，
    不带 -race 也会 "fatal error: concurrent map writes"，所以 R19 那类
    能靠 devnet 长跑计数抓到；而普通字段的竞态不带 -race 完全静默，
    R20 那类**只有** -race 能看见。两类缺陷的判据不同，不能互相顶替
  - 主闸门永远是 `go test -race` 的定向用例（#17 验收第 1 条要先造红）；
    本条只是端到端补一层，不是硬闸门，失败要分诊（真竞争 vs -race 下
    定时器失真）
  - 跑之前必须先确认二进制真是 -race 构建的（`go version -m ./geth | grep -- -race`
    或有对应的构建记录）：不是的话这条会**静默通过**，而它正是 R20 唯一
    能暴露的场合——这条验收的价值全在"红灯可能真的亮"，不能靠"跑了没报"
  - 日志里必须同时出现正常的 PBFT 出块行，否则是空跑（节点没起来，"无
    DATA RACE" 同样成立）
  - 这条 smoke **不能替代** R20 的定向单元用例（#17 验收第 1 条：修复前
    必须被 -race 判红）——smoke 只证明"真实路径上没撞上"，不证明"不存在"
```

**日志判据怎么读（所有按日志验收的条目共用，含 R18/R19）**：本仓 `dpos/log.go:36`/`:38`/`:40`/`:42` 四个不带 format 的 helper 都是 `glog.Xxx(fmt.Sprint(a...))`，而 `fmt.Sprint` 只在"两个操作数都不是字符串"时才插空格 ⇒ `Warn("FinishedProposal received fork block", "height", 1)` 实际输出是
`WARN [10-29|13:52:03.379] FinishedProposal received fork blockheight1`——**`height` 与值粘连、没有空格也没有 `=`**。原版走 `Elastos.ELA/common/log` 的 `fmt.Sprintln`，是有分隔的。

两个直接后果，验收时都要算进去：

- **任何按 `height=NNN` 抓字段的脚本/文档/rg 表达式都会静默匹配 0 条**——不是"没发生"，是"抓不到"。写判据时要按当前实际形态写（`fork blockheight`），或者改成抓 message 前缀 + 数字后缀。**而且这份格式还会再变**：`9e6880e42` 之后是 `fork block height 1`（有空格、无 `=`），此前是 `fork blockheight1`。所以在**跨修复前后两份日志做对账**时，别写死任何一种，用容错式 `fork block ?height ?[0-9]+`（这一条已在修复前后两份日志上各验过一遍，分别 14/14 与 187/187）。
- **计数判据要跨"规模"验，不是跨"一次运行"验。**同一份日志给出 164:164（全份）与 187:187（另一份全份、含高度和对账 12845），再补一个短样本 17:17（逐元素相同、和 153）——**三种规模比值全等**才算证据。只报一次"看到 1:1"等于没说：它可能只是那一段没触发。**每个样本要报三样，缺一样就不算"同口径"：逐元素相同、高度和、以及和能否被算术自证**——若样本是从 1 开始的连续块，高度和必须恰好等于 `n(n+1)/2`：17 → 153、115 → 6670、113 → 6441、42 → 903（四个样本全部对得上）。**对不上就说明"这段不是从 1 开始的连续段"，而那正是这个判据要排除的东西**；高度和本身不是自证，能被算出来才是。 同一个方法用在**计数**上就是**闭合校验**：把一个计数按来源拆开（本仓 / 上游 / vendored / 夹具），**各分部之和必须等于总数**；不等说明有东西没被归类，而没被归类的那一类通常正是你要找的。本轮就有一例（team-lead 实测）：按**写出来的路径前缀**分部，`27 + 10 = 37`，离总数 38 差 1——差的正是那条 `pgp-chain/third_party/…`，它前缀里多一层目录，于是没落进任何一个已列出的部。**先分部、再求和，比盯着一个总数猜它对不对要快。** **同一个道理管"有自由度的几个数"**：`diff` 的增、删、行数差满足 `增 − 删 = 行数差`，三个量只有两个自由度——**"三个我都量了"不提供三份保证，只提供三次犯错的机会**；正确用法是先量两个、算出第三个、再与实测的第三个对。本轮它一次就抓出 `48 − 27 = 21 ≠ 23`（详见 §2.3）。
- **验收判据必须落在"被改的东西"上，不能落在"我加的东西"上。** 前者是**锚点归零**、**记录存在且能读回原始值**、**两向转移**（改之前不存在 → 改之后存在且内容与输入一致）；后者是 `> 0` 的计数、没有报错、getter 的返回值。**后一类在"工作量为零"时也能通过——这是最贵的一种绿，因为它连"要不要怀疑"都不会触发。** 三个实例，都发生在本仓：
  - **计数 `> 0` 会被修复动作自己满足。** `changeNextTurnProduces` 在提交前 0 命中、提交后 1 命中，而那 1 条是注释里写的函数名，不是函数。同族：`twice` 从 8 降到 7 而不是 6，因为修复句自己贡献了一条。
  - **"调用了、没报错、没 panic"三件事同时成立，磁盘上可能一个字节都没写。** `spv/nextturn_dposinfo.go` 的 `SetCurrentProducers` 首行是 `if spvTransactiondb == nil { return }`——静默返回、无日志；而 `spvTransactiondb` 的唯一赋值点打不开库时只 `log.Error` 后 return。于是在库坏掉的节点上，每次调用都是一句无声的 return。
  - **最狠的一种：读回端自己兜底。** `GetCurrentProducers` 在键不存在时**回落到 live 头现算一个集合**，于是"把写进去的东西读回来、非空，就算通过"在零写入的节点上同样通过。**这条判据的失效方式不是偶尔漏，是结构性永远绿。**
  **所以判据要写成两向转移，不写单点状态**：接线前 `current_producers` 键不存在 → 接线后存在、且解出的条数与输入长度一致。单向"存在"会在库已损坏的节点上永远绿，单向"不存在"接线后必然假红。
- **锚点判据必须限文件、限行；分母选大了判据永远不绿，选小了永远绿。** `spv/blocklistener.go` 那 5 条 `TODO(spv-porter)`（两条 `ETNextProducers`、三条 `InitCurrentProducers`）在接线成功时**必然消失**，不成功时必然还在——这是好判据。但 `spv/` 全目录的 `TODO(spv-porter)` 是 **14 条**（`blocklistener.go` 7 条 + `spv_module.go` 7 条），只有 5 条属这一笔；写成"`spv/` 下这一族降为 0"会把 smallcrosstx、initLog、listener 注册一起算进验收，**那个判据永远不绿**。这是"分母"那条在锚点判据上的对偶，两边都错得很安静：**选大了永远红（于是被跳过），选小了永远绿（于是被相信）。**
- **合起来一句：要数的是"上游每一次变更的完整语句组"，不是函数名，也不是包。** 本轮的落点表被两个人各补了一半，原因不是谁不仔细，是两种口径各有盲区：按**包**数会漏跨包的（上游每次集合变更都是两条语句、两个包：`network.go:174` dpos + `:175` spv，`:505` dpos + `:511` spv），按**名字**数会漏"名字本身是错的"（`dpos.SetNextProducers` 本仓与上游两侧都零命中，是幻名）。**幻名这个盲区最贵：它 grep 不到任何东西，而"找不到"最容易被读成"还没实现"，那一格就会永远挂着。**
- **本仓日志不能与 1.9.7 逐行对照**：约 300 个调用点的输出格式在移植时被静默改了，名字保住、语义没保住（`dpos/log.go:32-34` 的注释恰恰声称"helper 名字保持不变，好让约 300 个调用点逐行对照原版 review"——又一条"注释声称强于代码"）。M3 与老节点互通排障时，同一条日志在两边长得不一样，别据此判定"事件没发生"。

**方法（本轮付过学费的一条，写给下一个扫注释的人）**：查"注释/文档声称的事实"要**按位置类型扫，不按文件扫**——①函数头注释；②测试的 doc comment；③断言文案/失败消息；④脚本里的探测与闸门。四类位置各自会长出同一种错误（把当时的形状写成永久结论），按文件名扫必然漏：本轮就有人只 grep 了 `seal.go`、没扫 `seal_test.go`，于是漏掉测试侧两处。同理，**错误的待办比没有待办更危险**——一条已经不成立的"残留"会让下一个人去把正确的注释改坏（见 R12 里撤回的那条）。**还有一条同族但更隐蔽的**：锚点的行号**只能来自会打印 `NR` 的输出**（`awk`、`grep -n`、`git blame`），**从 `sed -n '555,590p'` 这类范围转储里用眼睛数的行号一律不采信**——它跟"凭记忆写行号"是同一个错误的两个变体，而且更危险，因为它披着"我查过源码"的外衣。本轮这个坑连着绊了三个人（我、team-lead、QA 各一次），每次都是差 1–2 行，而差 1–2 行恰好会把"重锚"和"duty 判定"这两行说成同一行、直接推翻结论。**写锚点的口径统一为：贴 `grep -n`/`awk` 的原始输出，不写"`file:line` + 转述"。**

**判据的最上位一条（比下面每一条都高；`-F`、"正控"、比率分母、fail-closed 都是它的子规则）**：**任何"检查有没有通过"的动作，都必须同时能回答「这个检查是不是真的执行了」；回答不了的，一律只算观测，不算闸门。** 本轮样本**按失效来源分族**（下面逐族一段，行首标 A/B/C），越往后越难抓：越靠后的族，证据越完整、错的越不是仪器。

**族 A：检查没执行，但看起来像通过。** 下面是**形状的样本，不是这一类别的清单**——它的边界我们还没走到头，别把这个表当穷举：

| # | 现象 | 真实情况 |
|---|---|---|
| 1 | 用反斜杠竖线写"或"关系，`grep` 返回 0 | BRE 里它不是交替（本机 `grep` 是 `toybox 0.8.13`） |
| 2 | `grep -E "… +="` 返回 0 | `+` 在 ERE 里是量词，不是加号 |
| 3 | 表格单元格里的 `"A\|B"` 被切成两列 | Markdown 吞字符 |
| 4 | `go test ./dpos/` 打出 `ok` | `//go:build race` 的文件这次根本没被编译进去 |
| 5 | `go version -m <bin> \| grep vcs.revision` 返回空 | buildvcs 在 worktree 下放弃，不报错 |
| 6 | 判定脚本收到一个整数，量级也不荒谬 | `awk -v exp=…`：`exp` 是**内置函数名**，绑定被静默丢弃，`exp` 按 `exp($0)` 求值 → `-inf`，于是**每一行**都被判成异常（实测 3，正确答案 1） |

**族 B：检查执行了，但测的不是被测对象。** 本轮三个样本：`naiveGrep` 扫的是"我的搜索模式能不能命中"、`counted == 0` 判的是"这一轮有没有撞上并发"、还有下面那条 `1:1 OK (5:5)`——**比率成立但没有分母**。族 B 的共同判据只有一句：**"这个判据失败时，失败的原因会不会与被测的缺陷无关？会——那它测的不是被测对象。"** 三个样本用它都能一次判出来（它单独成条，见本清单末尾"判据要能被正确的理由打红"）。

**族 C：证据齐了，读的人替它编了一个无害的解释。** 本轮一例，由 team-lead 自己报出：同一屏上同时出现 `awk: illegal statement at source line 3` 与 `gates accounted for: 25 of 25`，他判前者是"这是他在改的中间帧，不是 bug"，还专门告诉对方"不是让你去查"。**这一族与 A、B 的根本差别：A、B 的证据是缺的，这一族证据完整地摆在屏幕上**——缺的是"两条互相矛盾的行必须被当成一个矛盾"这个动作。**"他在改"不是证明，是对别人工作状态的猜测**，而猜测不能被日志证伪；能证伪的说法必须指向这两行本身（例如把该 awk 的 stdout/stderr 分别落盘，再看那次崩溃有没有产出任何字节）。**判据：错误输出与正常结论同时出现时，默认是 bug，除非能用一条命令证明那次崩溃与这个结论无关。谁给出"无害"的解释，谁负责给出那条命令。** 这一族也解释了为什么它比 A、B 贵：**前面的错都能靠换命令兜住，这一族的错必须靠不替别人省事兜住**——而"省事"在主观上长得像体谅。

**为什么这条必须上位**：`-F` 只解掉 1、2、3，解不掉 4、5。而 4、5 的共同点是**执行与否不体现在输出里**——`go test` 的 `ok` 与 `grep` 的空输出，对"没跑"和"跑了且通过"给出的是**同样的字节**。所以判断依据只能来自**输出之外**：`-race` 与不带 `-race` 各跑一次、`git rev-parse HEAD` 与二进制里的 `vcs.revision` 对比、同一条管道喂一条已知非空的输入（见下条"正控"）。（自查这张表的列数时，要先把 `\|` 这种转义竖线剔掉再数，否则这个自查本身会误报——判据的判据也得先能回答"它执行对了吗"。）

**样本 5 单独展开，因为它的"看起来正常"最像正常：构建产物的出处证明依赖构建上下文，不是普遍可用的。** worktree 的 `.git` 是一个 `gitdir:` 指针**文件**而不是目录；`git archive` 解出来的树、`GIT_DIR` 被重定向的场景同理。这些场景下 `go build` 的 buildvcs **少写几行而不是报错**：`go version -m` 里根本没有 `vcs.*`，`grep vcs.revision` 返回空，与"这个二进制本来就没有版本信息"完全一样。**我自己复现过**（隔离探针：`/tmp` 下一个两文件仓库 + 它的一个 worktree，同一提交，`go1.23.12`，输出节选）：

```
主仓构建      →  vcs.revision=043ec6c1db93…   vcs.modified=false
worktree 构建 →  (无 vcs.* 条目)                ← 静默，无警告、无 exit code 变化
```

**指针文件的字节数不能当判据**：我探针里那个 `.git` 是 **53** 字节，本仓 `/private/tmp/pgp-race/.git` 是 **81** 字节（`gitdir:` 后面跟的是绝对路径，长度随路径变）。写"81 字节的 `.git`"会让下一个人拿 `wc -c` 去验、因为数字不等得出"这里没问题"。判据是**它是文件、且首行以 `gitdir:` 开头**。**这条探针本身有保质期**：跑它的是 `go1.23.12`，而本仓 `go.mod` 要求 1.25——工具链升级后 buildvcs 的行为可能变（它一直是 `cmd/go` 里被修的地方），**换工具链就要重跑这条探针再看结论**。这也是"判据要能回答它执行对了吗"的另一面：**判据也会过期，而过期不报错**。

**可执行替代（已在用）**：在**共享树里**构建、产物 `-o` 到 `/tmp`（`go build` 只写 Go 缓存与 `-o` 目标，不动工作树）。于是 `vcs.modified=false` 本身就是"这个二进制来自这棵树、且构建那一刻树是干净的"。**`vcs.modified` 比 `git status` 空更强**：它是**构建时**写进二进制的快照，`git status` 是**检查时**的状态——中间可以有人改一行再改回来，二进制里的字段不会变。**代价必须一起记住**：`vcs.modified` 是**全仓**判断，一个未提交的 `.md` 与未提交的 `dpos/dispatcher.go` 在它眼里是同一件事。所以共享树里**文档提交也会影响别人的证据**：docs 挂着改动，别人构建出来的 `-race` 产物就是 `vcs.modified=true`。

**`#21` 的后续，一句话：忽略不是治理。** `scripts/pbft-devnet/run.sh:45` 是 `GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}`，`:173-175` 是 `if [ -x "$GETH_BIN" ]; then return 0; fi`——**只要那个路径上有一个可执行文件，`run.sh` 就直接用，不看它是哪个提交、是不是脏树构建的**。实测（我核的）：`build/bin/geth` 的 `vcs.revision=60981594b12f…`、`vcs.modified=true`、`vcs.time=2026-10-29T05:09:19Z`（= 本机 13:09），同目录还有 `geth-aqs`（13:42）。拿它跑 devnet 得到"当前代码没问题"，验的是几小时前的脏树。与样本 5 同族：**"文件存在"被当成"工具正确"**。

**更重的一类：编造标识符。** 本轮出现过一次"哈希是造的"——转述者给了两个不存在的 `shasum`（`d20fdb9c…`/`488a18cd…`），而真值当时就在他眼前。**这比行号偏 1 严重一个量级**：行号错是"读错"，编哈希是**生产了一个看起来可核验的标识符**，它伪装成证据、把"待核实"变成"已核实"。三条硬规则：

- **结构属性只能测量，不能记忆。** 行号、表格列数、条目数、函数个数、字数——全是**可数**的东西，而人倾向于记住它们，然后**记住的那个数与测量的那个数混在同一段文字里**，读者无法区分。判据统一：**这个数字背后有没有一条会打印它的命令？** 没有，它就不是证据，只是印象。**同一个问句在"改文件之前"再问一遍，就是另一道闸：这个数在文件的哪一行？答不上来就不动文件。** 它不问"谁给的"——对方是 team-lead 也一样：本轮那三组"我复核过"的旧计数在全仓 `*.md` 里 0 命中，而文件一个字没被改，靠的正是"先定位、答不上来所以不动"，不是靠执行方更谨慎。**"要小心"是态度，"先定位"是一个能被违反的条件**——文档只收后者，态度留给各自。**"记了"也必须带地址。** 说"我记下了"时，下一句要能说出它在哪个文件、哪一行；说不出就得把话说全——**"我写在自己的日志里了，公共文档里还没有"**。本轮实例：一句话被"记了"，对 `docs/` 做 `grep` 是 0 命中，实际写在项目记忆日志里——**两个地址、同一句话，而找不到就等于没记**。**"记了"比数字更危险的地方在于它听起来永远是对的**：数字还有量级和格式可挑，"记了"没有任何可核的外形，只有地址能证伪它。本轮两笔：①**行号**——`f7c38120c`、`e1089e728` 两次改动后整批清单失效（§七 开头那段"行号基线"就是为此而写），根因就是那些数字来自记忆；②**表格列数**——我报"风险表 20 行 × 5 列"，实际是 **4 列**（`#`/风险/对策/验证，5 条竖线），谁按"6 条竖线"去校验都会把整张表"修"成碎片。**同一张表里不许混两种取数来源。**一张表一半是"目数"、一半是"从 `grep -n` 复制"，排在一起就像同一套坐标，读者无法分辨——**表格会掩盖取数方法的差异**，而错数字会被 `grep` 打回、混来源不会。实例：同一张逐字段表里，前三行是目数（各偏 +1，因为把空行数错一格），后两行是复制的（全对）；表看起来完全自洽，只有逐行 `awk` 才分得清。**规矩：一张表只能有一个取数来源；两个来源就在表头注明哪几行是哪个。****这笔账是我自己抓到的，但功劳不在记性，在那条 `awk`**：`awk '/^\| R[0-9]+ \|/ {n=gsub(/\|/,"|"); if(n!=5) print NR, n}'`——**能抓到它的不是回想，是一条会打印的检查**。同族的还有"某函数有几处调用者"：压 `grep -c`，不写"大概三处"。（新加的判据强度表里也有 `\|` 转义竖线，数之前要先剔掉，否则这个自查本身会误报。）
- **任何字符串型标识符（哈希、路径、函数名、包名）只能从工具输出里复制，不许从记忆流到文档/消息里。** 真值和假值长得一模一样，读者无法从形态上分辨。
- **哈希必须带取证工具**：`git rev-parse <commit>:<path>` 给的是 blob 哈希，`shasum` 给的是文件内容哈希，同一个文件两组值。不写工具的哈希等于没有哈希——复核者拿错工具会得出"文档是假的"，而那会把一条真结论连带作废。
- **接到的锚点一律视为未验证**：文档的维护者必须自己重跑一遍命令再落笔，不能因为"这是某个人给的"就直接抄进来。本轮唯一挡住那个假哈希的动作，就是"落文档之前自己核了一遍"——流程上要让这个动作成为默认，而不是靠某个人恰好谨慎。
- **打字错和编造，在读者那里是同一种故障，但成因完全不同——所以规则不能只写"不许编"。** 本轮两种都出现过：一次是在发给同伴的消息里把真哈希少打一位（`ab245d65`，正确是 `ab2457d65`），一次是完全编出来的两个哈希。对复核者来说两者都是"解析失败"，而它长得和真哈希一模一样，所以没有任何外部特征能让人分辨；但成因不同意味着**不同的补救**——打字错是流程粗糙（差一个可执行动作），编造是证据链坏了（差一次真正的核对）。**可执行的措辞是"落笔前必须能对着终端里的那一行指出来"**：能指出来，就少打不了那一位；指不出来，就说明那是记忆不是证据。这条同时说明为什么"我核过了"不算免责——核过的是那个提交，不等于核过那串字符。本轮挡住后者的动作正是前者：重跑一遍命令，比自己"再想一遍"可靠。
- **转引的数字和编造的数字，在读者那里是同一个东西。** 本轮多了一次：`pgp-chain/dpos/network.go` 的行数被转述为 467，实测 **451**（`wc -l pgp-chain/dpos/network.go pgp-chain/consensus/pbft/network.go` → `451` / `1148`），转述的人没跑 `wc`、只是把别人量的值原样搬了过来。对复核者来说 `467` 与 `451` 都是"解析失败"——按 467 去核会得出"文件没那么长"，于是**这条真结论被连带作废**。**转抄比编造更危险，因为它带着"有人量过"的信用**：编造至少一眼可疑，转抄会被读成已复核。规则：引用别人量的值时，要么自己重量一次，要么在文中注明"未经我复核"。
- **死代码不是没有语义，它只没有调用者。** 锁序、可达性、复活性这类断言要按「函数能做什么」写，不能按「今天谁调它」写：`CleanProposals(true)` 在本仓零调用，但函数体里已经定义了 `mu → viewMu` 这条边；两个恢复函数（`RecoverAbnormal`/`RecoverFromConsensusStatus`）本仓零调用者，但它们写的是三个裸字段。把"今天没人调"当成"不存在"，就会在接线那天收到一条自己欠下的锁序债。
- **否定性结论必须附可复现的命令原文，而且默认用 `grep -F`（或 Grep 工具）；只在确实需要模式匹配时才用 `-E`。** 这是**选择规范**而不是操作规范，所以默认值必须是对的：日常搜索（"某函数名有没有调用者""某字段有没有写赋值点"）全是字面量，没有一处需要正则交替。"某函数无调用者""某字段无写入者""零命中""删干净了"全属此类。**它们失效的方式是静默的**：命中会打印，而**未命中有三种原因——语法错、路径错、实现差异——输出完全一样，都是空**。本机 `grep` 是 `toybox 0.8.13`（`grep --version` 自己打印 "is not GNU grep 9.0"），BRE 的 `\|` 不生效：`grep -c "sprintArgs\|fmt.Sprint" dpos/log.go` → **0**，而 `grep -cE "sprintArgs|fmt.Sprint" dpos/log.go` → **11**（其中 `sprintArgs` 实际 6 次）。我自己也栽在同一个坑：`grep -n "resetViewMu\|resetViewRequests" dpos/dispatcher.go` 返回空、`-E` 版本 15 行——那个锁一直存在。**但 `-E` 只解掉了 `\|` 一个坑，同时把 `+ ? * ( ) |` 五个变成了坑**：an-quan-shi 用"扫全部字段赋值点"来证明"`viewMu` 盖不到的直写为 0 处"时就撞上了——`grep -rnE "\.viewOffset +=" --include=*.go dpos/` 返回空，而 `grep -rnF "viewOffset +=" --include=*.go dpos/` 打出 `dpos/consensusview.go:368: v.viewOffset += offset`，**因为 `+` 在 ERE 里是量词**。（那处结论没变，它在 `ChangeView` 的锁内；但那次对是运气，不是方法对。）**三个案例是同一条族的三个面**：①`grep "sprintArgs\|fmt.Sprint"` 返回 0（本机 `grep` 是 toybox，BRE 的 `\|` 不生效）；②`grep -E "\.viewOffset +="` 返回 0（ERE 元字符当字面量用）；③写进表格的证据命令被 `|` 切成两截（Markdown 吞字符）。共同点是**你以为在写一个字面量，其实在写另一层的语法**。所以正向表述不能是"记住这些陷阱"——列不全所有层的所有元字符——而是：**当一个字符在多个语法层里都有含义时，把它移出歧义区**；`-F` 代替 `-E`、两个 `-e` 代替 `|`、代码块代替表格单元格，都是同一个动作。**立论要精确：不是"grep 害了五次"，而是"空输出这种证据形态本身不可靠"，grep 语法只是它最常见的一种成因。**本轮因证据失真返工五次（三批行号失效、两份 `Elastos.ELA` 副本导致的行号互换、`git commit -a` 扫走别人 staged 的文件、上面那个 `sprintArgs`），**其中三件与 grep 无关**（分别是提交推移、路径歧义、索引共享）。**这个区分不能省**：写成"grep 有问题"，读者会以为换个 grep 就没事；写成"空输出不可靠"，他才会去贴命令原文。**还有一层比语法更隐蔽：否定性结论必须写清"在哪棵树里"零。**"某函数无调用者"缺主语时，读者会默认是你刚 grep 的那棵树。实例：`ConsensusView.SetWorkingHeight` 在本仓零调用者，但上游 `pgp-chain/dpos/dispatcher.go:522` 调用它——我据此判过"可删"，被同一棵树里的五条 `grep` 命中纠正。**命令对、输出对，结论仍然错，错在缺主语**；而"本仓零调用者 + 上游有调用者 = ported-but-not-wired"正是本项目最常见的一种状态，它在单个仓库的 grep 输出里与"死代码"完全无法区分。**贴进表格的证据命令必须能原样复制执行**：单元格里的 `|` 会被当列分隔符吞掉（`grep -E "A|B"` 变成两截），读者复制走的是被切过的版本、跑出来是语法错，然后他会认为你贴错了命令——**被截断的命令和空输出是同一族：看起来能用，实际不能用**。改用两个 `-e`，或把命令放进代码块。本条由 an-quan-shi 提出、team-lead 定形，两人各给了一份可复现数据。**而"默认 `-F`"这个说法本身不完整，必须分三档**，因为 `-F` 只能匹配**单个**字面量：①匹配**一个**字面量 → `grep -F "foo"`（默认走这条）；②匹配**多个字面量之一** → `grep -F -e foo -e bar`，**不要退回 `-E`**（退回就把 `\|` 一个坑换成 `+ ? * ( ) |` 五个坑）；③确实需要正则（`^`、`$`、`.*`、字符类）→ 才用 `grep -E`，但**待匹配串里含 `+ ? * ( ) \| [ ] { } ^ $ . \` 时不许用**。第三格的判据是关键一句：**"从代码里复制出来"这个动作本身就意味着它可能含元字符，所以"复制"和"`-E`"不该同时出现**——本轮两次事故（`\|`、`+`）都是同一个动作：从代码里抄一个串、塞进 `-E`。**而"不用 `-F` 就得用正则"这个二分里还藏着第三个坑，而且它不是几个字符的事：本机 `grep -E` 根本没有反斜杠转义类。**`toybox 0.8.13` 上实测三条：`printf 'a b\na\tb\n' | grep -cE 'a\sb'` → **0**（换成 `a[[:space:]]b` → **2**）；`printf 'foo_bar\nfoo bar\n' | grep -cE 'foo\bbar'` → **0**；`printf '\tfoo\n tbar\ntbaz\n' | grep -cE '^\t'` → **1**，与 `grep -cE '^t'` **完全相同**。所以 `\s`、`\b`、`\t`、`\w` 一律退化成**那几个字母本身**。**规则只写一条、不列清单（清单一定会漏）：本机 `grep -E` 没有反斜杠转义类，只认 POSIX 字符类（`[[:space:]]`/`[[:blank:]]`/`[[:alpha:]]`…）；`-F` 之外出现任何 `\` 加字母，都等于在匹配那个字母。**这条比 `\|` 那次更危险：它不报错，只是**什么也不匹配**，于是"过滤"变成一个空操作——**你以为滤掉了注释行，其实一条都没滤**，而计数看起来仍然合理。
- **任何"空结果"的结论，必须附一条正控：同一台机器、同一条管道、换一个已知非空的输入，结果非空。** 只贴命令原文不够——**命令原文可以是对的，管道仍然能静默吞掉一切**。本轮实测（被测：`f19e9a6b0` 是不是纯注释提交。管道由 an-quan-shi 提出，**数字我自己重跑过**，见下）：

```
# 被测：滤掉 diff 头、注释行与空行后，还剩多少新增代码行
$ git diff -U0 34a2f1410 f19e9a6b0 -- '*.go' | grep -E "^\+" | grep -vE "^\+\+\+" | grep -vE "^\+[[:space:]]*//" | grep -vE "^\+[[:space:]]*$" | wc -l
0
# 正控：同一条管道，换成 R20 加锁那批（真代码提交）
$ git diff -U0 3f6c2d188^ 3f6c2d188 -- '*.go' | grep -E "^\+" | grep -vE "^\+\+\+" | grep -vE "^\+[[:space:]]*//" | grep -vE "^\+[[:space:]]*$" | wc -l
81
```

四个要点，缺一条这条规则就退化成装饰：①**同一条管道**，不是"类似的管道"；②**同类输入**（真代码提交对纯注释提交，不是拿个 txt 去比）；③**已知非空、且量能解释**（**81** 行 ≈ 那次加锁改动的规模；若正控只打出 3 行，说明管道本身吃掉了东西）**这组数字我自己踩过一次，值得当作第五条要点**：第一版管道里写的是 `^\+\s*//`（当时还不知道 `\s` 在本机失效），正控跑出 **112**，由于它与别人报的数字一致，我就写进文档并声明"我独立重跑过"。换成 `[[:space:]]` 之后**同一个正控是 81**——112 里含着二十多条带 tab 缩进的注释行与空行，它们**根本没被滤掉**。**两个人"各自复现"出同一个错数，不构成独立证据**：只要两处用的是同一个坏模式，一致性只说明**这个错法很常见**。所以"我重跑过"必须连同**命令原文**一起给出，而命令原文本身要先被正控过。；④**正控的输入必须点名**（写 `3f6c2d188`，不写"一个已知非空的输入"），否则哪天被换成随便什么非空输入，正控就没人能复核。**为什么它值得单独立一条**：上面表里的样本多数能被正控当场抓住（`\|`、`+` 量词、`//go:build race` 没编译，以及后面那条 `2>/dev/null` 吃掉"路径不存在"），而"贴命令原文"一个都抓不住——那四条的原文全都看起来正常。**它和上面 `-F` 的分档、以及 `//go:build race`／`-race` 没开机，是同一个问题的三个层面**：阴性结果有三种成因——**真的没有、检测器没开机、检测器开了但没覆盖到**——而三者输出完全一样。前两种由 `-F` 分档与正控覆盖；第三种只能靠"先证明检测器开着"（`go test -race` 与不带 `-race` 各跑一次、比对输出差异）。判据能撑起多强的结论，见 §8.4。
- **"管道没报错"几乎永远为真，所以它不能当证据。** 退出码只看**最后一级**：`… | head`、`… | tail`、`… | wc -l` 永远返回 0，前面某一级当场死掉也看不出来。本轮两个实例，形状相反、同一件事：①**`cat -A`**（想看 tab）在 BSD 上没有 `-A`，它死了、屏幕上**什么都没有**——和"证明通过"长得一模一样，唯一救命线索是 stderr；若那天顺手写了 `2>/dev/null`，它就是又一个静默样本——**这里不写序号**：那个"第 N 个"依附的清单只存在于对话里，文件里从来只有表里那几条，第八和第九谁都没数过；序号本身是记忆产物，而记忆产物混进证据正是本节在治的病。②**`grep … 2>/dev/null | head`**：看到的 `exit=0` 是 `head` 的、不是 `grep` 的，重定向还把 stderr 吞了。**判据一句话：要判管道里某一级，就单独跑那一级，或用 `set -o pipefail` 把它的退出码显式带出来——"整条管道 exit 0"只等于"最后一级成功了"。**
- **守卫写在了不会被执行的上下文里，等于这个守卫不存在。** 判 build tag 时先问"这段代码在哪个构建下会跑"（§7.1 里 `dispatcher_recovery_nil_test.go` 故意不带 tag，正是这条的另一半），判 `exit` 时先问"这个 `exit` 在哪个进程里会跑"——**两次都是先把执行上下文定下来，再看那句代码**。本轮实例（`27f9b2452`）：`scripts/pbft-devnet/gate.sh:108` 的 `count()` 原本在自己内部 `exit 2` 表示"这个数读不出来"，而它**永远**被调用成 `n=$(count …)` ⇒ 那个 `exit` 只结束**子 shell**，调用方拿着空值继续往下跑，于是"读不出数就停"这条守卫变成了 fail-open。修法是**让函数返回 2、把判断放在调用点**（`n=$(count …) || cannot …`），而不是在函数里退出——**能退出的只有"知道自己在哪一层"的那一方**。判据一句话：**"这句拒绝执行，最终落在谁的退出码上？"** 答不出、或答成"不是我这一层的"，它守的范围就不是你以为的那个。**这条故意不给清单**：`$()`、`$?`、`pipefail`、`xargs sh -c`、`su -c`、`ssh` 的名单永远不全，而"先定执行上下文"一句能盖住所有还没列出来的形态——**给一条规则它管一大片，给一张清单它只管那几条**。
- **作用域在这份文档里反复出现，共同点是"东西是对的，它管辖的范围不是它以为的那个"**（下面按见到的先后排，不声称穷举）：①`grep` 的搜索根（`dpos/ consensus/` 还是全仓）；②`grep -oE '(dpos|consensus)/…'` 把 `pgp-chain/` 前缀吃掉，于是**上游被记成本仓**；③build tag 与 `$( )` 里的 `exit`——守卫写在不执行的上下文里；④**一条通则接在某个例子的末尾，它就继承了那个例子的辖域**（本次实例：一段"文档自述计数"的规则接在 `cat -A` 例子后面、夹在 ①② 之间，读起来像 ① 的一部分）。**前几种都能被"先把上下文定下来"治住，最后一种不能**——它**没有不变量**：反引号配对、fence 偶数、表格列数、行数**全部通过**，只有按顺序读才知道这一段在讲什么。所以分工是硬的：**有不变量的交给命令，没有不变量的只能交给"回读"，而回读必须是一个固定动作**——本轮两次抓到这类错（嵌套反引号、本条的辖域跑位）都发生在"本来只想核引号"的那次回读里，**它值钱的地方正是这个溢出**。
- **文档描述自身结构的计数，是唯一一类会自动变假的句子。** "五个形态各异的样本""第九个静默样本""本轮样本分成两族"，这三句在本文件里各过期过一次；它们不含量级、格式、单位可挑，**一加一减就错，错的时候还长得像结论**。**这三句会留在 `grep` 结果里，那不是残留，是反例本身**——按 R20 对策格那条「验收判据不是"改完了"，而是"剩余命中逐条说明为什么不是同一件事"」，判据是能说明每一条命中为什么不是同一件事；**当修复以它自己的产物形态留在结果里（`twice` 那个 `8 − 2 + 1` 同族），"命中归零"就永远不是可用判据**。**处置与 §七 那个余量同一条：凡"每次编辑都要改的数"，不写成数，写成指针**（"见下表""按 A/B/C 分族"）——写"三族"，下一个加族 D 的人得先发现这句话，**而他不是来改这句话的，所以他会漏；写指针，他加一行就完事**。
- **参数传进去了，不等于参数生效了——判定脚本的每个入参都要能做一次"换值必变"对照。** 实测（an-quan-shi 提出，我在本机复现并**纠正了机制**）：间隔统计写成 `awk -v exp=$((EXPECT_INTERVAL*1000)) '{ d=$1-exp; … }'`，而 `exp` 是 awk 的**内置函数名** ⇒ 绑定被静默丢弃、`exit=0`、无警告。**机制要写准：它减掉的不是 0**——`exp` 被当成 `exp($0)` 求值（`$0` 是整行 `2999`），得 `+inf`，`$1-inf` 是 `-inf`，取绝对值后仍然 `>30` ⇒ **每一行都被判成"偏离"**。这也正好解释为什么真实数据上是 **40/40** 而不是某个常数：错的值是**数据的函数**。对照两条，两次都 `exit=0`、只在输出上不同：

```
$ printf '2999\n3001\n3102\n' | awk -v exp=3000  '{d=$1-exp;  if(d<0)d=-d; if(d>30)b++} END{print b+0}'
3
$ printf '2999\n3001\n3102\n' | awk -v want=3000 '{d=$1-want; if(d<0)d=-d; if(d>30)b++} END{print b+0}'
1
```

  能静默的恰好是"零参也能成表达式"的那几个内置名：`exp`/`log`/`length`/`int` 都作用于 `$0`、返回一个**看起来完全正常**的数（`log` 给 `log($0)`、`length` 给 `length($0)`、`int` 给 `int($0)`）；而 `index` 直接语法错——**报错的那几个自己保护了自己，静默的那几个才是坑**。**`NR` 是这一族的正控**：`-v NR=3000` 正常生效 ⇒ **坏的从来不是 `-v`，是名字和内置函数撞车**（`int` 我已量过，同样静默、同样 `int($0)`）。**而改名只降低概率，不构成判据**：an-quan-shi 在 `gate.sh` 的"闸门自数"里把 awk 数组名写成 `exp`，awk 当场语法错 → 输出文件为空 → `[ -s "$TMP/acct.txt" ]` 为假 → **整个闸门打印 `25 of 25 / PASS`**。**一个只会在出错时才有输出的检查，它的"没有输出"必须能区分"没发现问题"和"没跑起来"**——所以检查器自身的退出状态要单独判（非零就记一条 `bad`）。**名字挡住你想到的那几个，判执行状态挡住你没想到的那个。**所以**最优解不是加对照，是移出歧义区**：变量名一律加前缀（`want`、`expect_ms`、`v_exp`），这与"`-F` 代替 `-E`"是同一个动作——**一个名字在两层里都有含义时，换掉它，而不是记住它**；对照法留给换不掉的外部入参（`--interval` 这类命令行开关）。**而本轮真正救回来的不是检查，是"我事先独立算过期望值 4"**：40 与 4 差一个数量级，才逼出第二条路径（不经脚本、直接对日志算 `n=40 mean=3000.0 min=2898 max=3102 dev>30=4`）。**若先跑脚本再照它写报告，报告里就是一个错数，而且每一处看起来都对。** 自检一句：**"这个参数换个值，输出会不会跟着变？"不变就等于没传进去。** `gate.sh` 现状：**两个开关的两侧对照都做完了**——`--expect-race`（race 二进制 → 通过、非 race → 失败）、`--interval`（`--interval 5` 下 G5 报 `3s, expected 5s`、G6 从 4 个偏离变 **40 个**；默认 3 下仍是 4 个）。**其中 `--interval` 的运行时实测由 an-quan-shi 做，我核的是代码链而不是他的数字**：`EXPECT_INTERVAL` 在 `gate.sh:274` 判周期、在 `:294` 变成 `-v want=$((EXPECT_INTERVAL * 1000))`——**一个开关带两条判据，所以"那两个数是各自独立的"是错觉；运行时数值未经我复核**。
- **查询者本身会被算进结果，这和"判据要排除自己的夹具"是同一条。** `pgrep -fl geth` 用来判断"有没有残留 geth 进程"会**假阳**：它命中的常常是发起查询的那条命令行（`zsh -c … geth …`），不是 geth。换成 `pgrep -f "cmd/geth"` 能消掉这个假阳，但那时"0 命中"又说不清是真没有还是模式写错。**所以这类判据要换成不依赖查询者形态的东西**：本轮用的是端口侧证（`lsof -nP -iTCP:8555/30399 -sTCP:LISTEN` 为空）加脚本自报（`run.sh stop` 输出 `stopped in 3s`），两条相互独立。**"我在查它"和"它在"在 `pgrep` 的输出里是同一个样子。**
- **比率类判据必须同时报出起点与终点的绝对值。** 任何"平均 / 比率 / 每单位时间 / 成功率"的判据，都要让读者能自己算出分母——**只报比率等于把分母藏起来**。本轮实例：采证脚本报 `sealed blocks = 5`、`1:1 OK (5:5)`、`interval = 3000ms`、失败标记全 0，**每个数都是对的**，但那 5 块是第 **20–24** 块（该 home 里已有 19 块）——比率成立、间隔精准、标记全零，结论却是错的。**可执行的形式**：取日志里第一条 seal 的块号 `FIRST`，`FIRST != 1` 就判 `INVALID RUN`——**它不依赖任何外部时机、只看日志头**，把"读者能自己算分母"变成"工具自己先算一遍"。这条与正控是一对：**正控证明管道不是死的，分母证明这次跑的是全程。****但它有一个更优解，优先用它**：**能收敛成"分母恒为 1"的判据，就收敛，而不是每次去报分母**——例如"每次只 `-run` 一个用例"，分母从此恒为 1、连证明都不需要。**报分母的成本随运行形状变化，收敛是一次性的**；凡是同一条判据要重复报分母，说明运行形状还没被收住。
- **守卫在拿不到判据时必须拒绝（fail-closed）。** `unknown` 不是"没问题"，是"判断不了"。**fail-open 的守卫比没有守卫更糟**——它提供了一个"已验证"的假象。还有一个更毒的变体：**用默认值伪造判据**——`H0=${H0:-0}` 这类兜底会把 `unknown` 填成 `0`，于是"起始高度未知"看起来成了"从创世块开始"。这与 `//go:build race` 同族：**让一个不存在的检查看起来存在**。判据一句话：**守卫里出现 `:-` 默认值时，问一句"这个默认值会不会正好落在'通过'那一侧？"**
- **锁序要按「这个函数体能传递地取到哪些锁」写，不按调用图写。** 前者读一个函数体就不会过期，后者要跟踪调用图、必然过期；而两者分叉的那天**没有任何测试会红**——所以锁序必须写在代码旁边，设计文档只记"当前接线下的锁序"，函数体记"任何接线下的锁序"。推论：像 `viewMu → mu` 这种靠**别的包的实现细节**维持的"非边"，不能只写"禁止"，必须写"为什么现在成立"（见 R20 (d)：`miner/worker.go` 的非阻塞发送）。只写禁令，等于把一个有前提的假设当成了不变量。
- **哈希只能回答"是否被改动过"，回答不了"是否正确"。** 本节曾经踩过：一份 blob 哈希（`9e31f240…`）确实能在 `ab424d622` 上复现，于是被写成"改动已包含在内"的证据；实际上那个提交里没有这处改动——哈希只证明**没人动过**，不证明**已经改好**。**同一份证据可以支持两个相反的结论**，取决于你把"相同"读成"没动"还是"已经好了"。状态类断言（已修/已删/已加）只能指向**做出那个动作的提交**，不能用"某文件自某提交后未变"来代替。
- **同名同号不等于同内容。** 两份 `Elastos.ELA` 上 `dposnormalhandler.go` 的调用点**都是 `:89`**、`dposondutyhandler.go` **都是 `:57`**（只差一个 tab 缩进，见 §2.3 的表）——`grep` 会命中、数字会对上，只有 `git blame` 或逐字节 `diff` 才看得出不是同一份。这比行号漂移更难发现，因为**没有异常数字提示你**。判据是 blob 哈希或 `diff`，不是"行号相等"。它与 row 8 记的"层找错"同族：层找错是**到不了**那一层，同名同号是**到了另一份**，两者都不会被"数字看起来合理"打回去。
- **判据要能被"正确的理由"打红，否则它测的是工具。** 本轮我自己写过一条自办：一个叫 `naiveGrep` 的临时脚本（一次性、未入库，别去仓库里找它——找不到不代表这份文档在编，但**正因为找不到，它不能当证据用**），它只验"我的搜索模式能不能命中那个字符串"。绿的时候它证明的是"grep 命中了"，不是"那处写入是安全的"——**它在测它自己**。同形状的另一种表现是拿一个不确定的量当判据（第一版 `clean_proposals_race_test.go` 拿 `counted` 的取值方向）。两者都满足"有断言、会变红、看起来在验证"，但被测对象都不是那个缺陷。自检一句话：**"这个判据失败时，失败的原因会不会与被测的缺陷无关？"** 会——那就是在测工具。

### 8.4 判据强度表（写任何断言之前先查这张表）

§8.3 讲**怎么写判据**，这一节讲**一个判据能撑起多强的结论**。两者不重叠：同样一句结论，换个判据强度可以差一个数量级，而在文字上它们长得一样——`counted == 0` 与某脚本打印 `PASS` 并排读，看不出前者可能只说明"这次没数到"。**没有这张表，写判据的人不知道自己手上的判据能撑起什么。**

| 行 | 判据类型 | 能推出什么 | 写法 |
|---|---|---|---|
| 一 | **构造性质** vs **调度性质** | 构造性质（同一输入必得同一输出：编码字节、签名域、哈希、排序结果）**可强制** → 可断言 | `if got != want { t.Fatal }` |
| 一 | 同上 | 调度性质（谁先拿到锁、哪个协程先跑）**不可强制** → 只能记录 | `t.Logf` 人工判读，或用"限时 FAIL"而不是"必须挂死" |
| 二 | **检测器的阳性** vs **阴性** | **阳性可信** | **阴性只说明"本次条件下没检出"** |
| 三 | **判据与被测缺陷有无因果关系** | **无因果关系 → 红和绿都不支撑结论** | 先问：它变红/变绿的原因，与被测的缺陷是同一件事吗 |

**第三行是唯一一行"红绿都不算数"的**，前两行至少有一个方向有效。它的样本见 §8.3 的族 B：`naiveGrep`（红只说明模式不匹配）、`counted == 0`（红只说明这轮没撞上并发）、`1:1 OK (5:5)`（绿只说明封块数与 WARN 数相等，**不说明从第一个块开始跑**）。**判据有没有因果关系，不看它跑没跑、也不看它红不红，只看"失败原因"与"被测缺陷"是不是同一件事**——这也是为什么族 B 比族 A 难抓：族 A 的检查根本没跑，而族 B 的检查真的跑了、也真的会红。

第二行的具体形态（这一节最有用的部分）：

| 判据 | 阳性 | 阴性 |
|---|---|---|
| `shasum` 相同 | 产物相同 | — |
| `shasum` 不同 | — | 只说明至少一字节不同 |
| `-race` 报 `DATA RACE` | 确实有竞争 | — |
| `-race` 无报告 | — | 只说明本次没检出 |
| `counted == 0` | — | 只说明本次没数到 |
| `//go:build race` 的文件 × `go test` 打出 `ok` | — | 只说明没报错，**不说明它被编译了** |

**阴性结果有三种成因，三者输出完全一样：真的没有、检测器没开机、检测器开了但没覆盖到。** §8.3 的"默认 `-F`"与"正控"解决前两种；`//go:build race` 与 `-race` 没开机解决第三种。**这三条不是三条并列的经验，是同一个问题的三个层面**——读者要能看出这一点，否则会以为"照做三条"就安全了。

**`//go:build race` 这一条必须配一份文件清单，否则下一个人会把它当成漏项。** 它是"假绿"里**最干净**的形态：三个 race 文件在 `go test ./dpos/`（不带 `-race`）下**整个不编译**，而 `go test` **仍然输出 `ok`，没有任何提示**；build tag 在输出里也不出现——**要发现它，必须事先知道 build tag 存在**。这与甲族前六种形态不同：那些都是判据写错或提前退出（`pipefail` 141、空断言、写死根因、`2>/dev/null` 路径不存在、`grep -E` 元字符、`vcs.*` 缺失），**都有痕迹可查**；这一个连痕迹都没有——文件在、内容对、输出 `ok`，只是没被编译。

本仓 `dpos/` 测试文件的 tag 分布（一条命令复核：`for f in dpos/*_test.go; do printf '%-46s ' "$f"; head -5 "$f" | grep -F '//go:build' || echo '(no tag)'; done`）：

| 带 tag（只在 `-race` 下编译） | 不带 tag（默认就编译） |
|---|---|
| `consensusview_race_test.go`、`dispatcher_recovery_race_test.go`、`producers_race_test.go` | `clean_proposals_race_test.go`、`consensusview_deadlock_test.go`、`dispatcher_recovery_nil_test.go`、`finished_height_test.go`、`log_test.go`、`signer_test.go` |

**注意这张清单是 3 + 6 = 9 个文件，不是 3 + 5 = 8**：口口相传的那份清单漏了 `dispatcher_recovery_nil_test.go`。**"按清单数"和"按命令数"的区别在这里是 8 与 9**，而 8 看起来完全合理——这正是本节第一条要人测量的东西，只不过这次被测的是清单本身。

**不带 tag 的六格里有一格是"故意不带"，不是漏**：`consensusview_deadlock_test.go` 的判据是"用例内 2 秒 `t.Fatal`"，而 `-race` **不检测死锁**——给它加 tag 只会让它从默认闸门里消失。**所以"no tag"有两个含义（没有需要 / 漏了），清单是唯一能区分它们的东西；没有清单时，唯一安全的读法是"不知道"。**

**闸门因此是两条命令，不是一条**：`go test ./dpos/` 与 `go test -race ./dpos/` 必须同时在闸门里，**任一单独跑出来的 `ok` 都不能当结论**——前者根本不编译那三个文件、也不提示，后者会编译它们，但**不会让非 race 文件的断言变强**（`-race` 是检测器，不是断言的替代品）。

**用反方向最贵：它会造出一个假发现，而假发现会被当成证据传下去。**本轮实例（an-quan-shi 提出并测了，我在隔离探针上把机制独立复现了）：他想省一次重建，理由是"`f19e9a6b0` 相对 `34a2f1410` 只加注释，产物应该逐字节相同"。实测 `shasum -a 256` 不同、`cmp -s` → 不同。原因是 Go 二进制带 DWARF 行号表，**多 20 行注释会让其后所有代码的行号整体偏移**。**如果当时没测**，结论就会是"注释改了行为"——而那是假的。**弱判据用对方向仍然有用；用反方向会造出假发现。**机制复现（同一份 `main.go`，只在最前面加一行 `// just a comment`）：

```
$ shasum -a 256 b1 b2
01baff2f…  b1        (原样)
1c345585…  b2        (只加一行注释)
$ cmp -s b1 b2 && echo IDENTICAL || echo 不同
不同
$ wc -c b1 b2
1506560 b1      1506560 b2        ← 大小完全相同，字节不同
```

**顺带一条：大小相同不能当"内容相同"的代理判据**——这一对的字节数一模一样。谁用"体积没变"去推"行为没变"，拿到的是同一个假发现。**同理，"只改了注释所以产物不变"这类推理不属于构造性质，它连调度性质都不是，它是一条没有判据支撑的断言。**

---

## 九、待确认输入

| # | 事项 | 当前默认 |
|---|---|---|
| 1 | PBFT 出块默认关闭（`--pbft.enable=false`） | 认同：什么都没配的节点仍是只读回放行为 |
| 2 | DPoS `Magic` 与 `NodeVersion` 的主网取值 | 待提供（可从运行中的旧节点 `--verbosity` 日志抓） |
| 3 | PBFT 节点是否强制设 miner address | 建议强制（`header.Coinbase` 参与 `SealHash`；无 etherbase 时 `commitWork` 直接拒绝出块） |
| 4 | 滚动升级窗口 M5 需要一个长期运行的旧 1.9.7 节点作对端 | 待提供 |
| 5 | legacy 链 `Recommit` 压到 3s 会略增低负载时的空块频率 | 建议接受 |
