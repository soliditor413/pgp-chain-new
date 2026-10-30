# M4 网络层移植拆解（A 方案）

> 输入约束（用户 2026-10-30）：① `pgp-chain` 的 chainbridge 部分**不移植**；② producer 间走自建直连 TCP；③ 签名私钥用 ELA 主链账户格式。  
> 本文是**编码前的判定书**：逐文件给「保留 / 改写 / 丢弃」，按 T 编号排任务，开放问题单列待批。  
> 基线：`bsc` `HEAD=9b6aca40d`、工作树干净、`go build ./dpos/... ./consensus/pbft/... ./spv/...` → `rc=0`。

---

## 零、一句话结论

M4 = **1096 行移植**（`network.go` 451 + `routes.go` 645）+ **24 个 listener 适配方法** + **4 处接线**。  
chainbridge 的**业务**整块丢弃，但它的**消息 codec** 一个都不能少——这不是风格问题，少注册一个 CMD 就直接断连（§1.2）。

| 项                      | 现状                       | M4 后                            |
| ---------------------- | ------------------------ | ------------------------------- |
| `dpos/network.go`      | 不存在                      | 移植 + 裁剪（删 8 个 case、1 个接口项）      |
| `dpos/routes.go`       | 不存在                      | 645 行**全量**移植，零裁剪               |
| `ETDirectPeersChanged` | 不存在（全仓 0 命中）             | 新增 1 个常量                        |
| `Transport` 实现         | `SetTransport` **零调用者**  | 装配层注入，M4 第一个能跑的东西               |
| `NetworkEventListener` | `Engine` 只有 2 个 `On*` 方法 | 24 个方法，新增 `network_listener.go` |
| chainbridge            | 已按 D10 只留 codec          | 不动                              |

---

## 一、chainbridge 边界：能丢什么，不能丢什么

### 1.1 三个"寄生点"——不是 chainbridge 的业务，却住在 chainbridge 的包里

用户说"chainbridge 没用"，这句话在**业务层**成立，但 chainbridge 的包里寄生着三样 DPoS 共识自己要用的东西。切的时候必须逐个搬，不能整目录删。

| 寄生点                          | 位置                                         | 谁在用                                                                   | 处置                                             |
| ---------------------------- | ------------------------------------------ | --------------------------------------------------------------------- | ---------------------------------------------- |
| `CmdProducers = "producers"` | `chainbridge-core/dpos_msg/message.go:8`   | `dpos/msg/producersmsg.go:35`                                         | ✅ **已搬到** `bsc/dpos/msg/commands.go:41`        |
| `ETESCStateChanged = 2005`   | `chainbridge-core/dpos_msg/event.go:11`    | `pgp-chain/spv/blocklistener.go:121/126`（SPV 链状态切 DPOS/POW）           | ⚠️ bsc 侧两半都在、缺中间那根线，见 O6                       |
| `crypto.Keypair` 接口          | `chainbridge-core/crypto/keypair.go:23-36` | `dpos/account.go:23` `GetBridgeAccount`、`pbft.go:112` `bridgeAccount` | ❌ **整个丢弃**，唯一消费者是 `GetBridgeArbiters()`，见 §1.3 |

`CmdProducers` 那一行是关键证据：`ProducersMsg` 本体（89 行，`SpvHeight`/`ChangeHeight`/`Producers`）**完全不含跨链语义**，它只是恰好把 CMD 字符串常量放在了 chainbridge 的包里。所以它是**共识消息**，必须留。

### 1.2 为什么 4 个仲裁人 codec 一个都不能删（D10 的硬理由）

`dpos/network.go:447-449`：

```go
default:
    return nil, errors.New("Received unsupported message, CMD " + hdr.GetCMD())
```

这个 `createMessage` 是 `p2p.Config.CreateMessage`（`network.go:380`），在 **DPoS 连接握手时**逐条消息调用。返回 error 的后果是**这次 DPoS 连接直接断**。

所以只要对端是 1.9.7 节点、只要它可能发 `arbiter` / `req_arbiter` / `req_a_sig` / `ret_a_sig` 中任意一条，bsc 就必须能**解出这条消息**——哪怕解出来后什么都不做。

**数字（实施后修正过一次，这是修正版）**：`createMessage` 的 case 全集是 **24 条**，其中 4 条是仲裁人消息。移植后 **`createMessage` 一条都不删，仍是 24 条**——那 4 条改用本仓的 `dmsg.CmdXxx` 常量与 `dpos/msg/layer2.go` 的类型注册，于是既不依赖 chainbridge 包，CMD 又还在表里。真正删掉的是 `processMessage` 里对应的 4 个**分发分支**。

> 一句话：**24 → 20 只发生在 `processMessage`，不发生在 `createMessage`。**
> 本文第一版写成"移植后应在 `createMessage` 里降到 20 条"，那是错的——按那个做，每个 1.9.7 节点都会在第一次发仲裁人消息时被断连。T1 的表驱动测试现在两边都锁：`messageForCMD` 必须 24 条全非 nil，`processMessage` 必须对那 4 条不分发。

`bsc/dpos/msg/layer2.go` 已经把这个决定落地了，`:20-26` 的注释就是理由原文。这与你这次给的约束完全一致：**业务不移植，codec 保留**。

> 因此 `layer2.go` 不是"chainbridge 残留"，它是 `network.go` 的**前置依赖**。砍它 = M4 第一分钟就断连。

### 1.3 可整块丢弃清单（26 个 chainbridge 引用文件的分类）

`grep -rl chainbridge --include=*.go . | grep -v third_party | wc -l` → **26**，分类如下（16 + 3 + 3 + 2 + 1 + 1 = 26，可复核）：

| 类别           | 数量 | 文件                                                                                                                                                                       | 处置                                                                                         |
| ------------ | -- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------ |
| **业务实现**     | 16 | `chainbridge-core/` 下全部：`chainbridge.go`、`api.go`、`relayer/`、`engine/`、`config/`、`blockstore/`、`keystore/`(5)、`bridgelog/`、`chains/evm/{listener,voter,chain,evmclient}` | ❌ 全丢                                                                                       |
| **消息 codec** | 6  | `chainbridge-core/dpos_msg/{darbiter,requirearbiter,reqarbiterssignature,feedbackArbitersSignature,message,event}.go`（**不在那 26 个里**，因为包内不 import 自己的路径）                  | 🔀 4 个 codec 已搬进 `bsc/dpos/msg/layer2.go`；`message.go` 常量已搬进 `commands.go`；`event.go` 见 O6 |
| **入口装配**     | 3  | `cmd/pgp/main.go`、`eth/backend.go`、`core/state_transition.go`                                                                                                            | ❌ 丢（bsc 有自己的装配层）                                                                           |
| **PBFT 侧泄漏** | 3  | `consensus/pbft/pbft.go:112/169/178/197`（`bridgeAccount`）、`consensus/pbft/layer2Listener.go`、`dpos/account.go:23`                                                        | 🔀 **裁剪**，见 §2                                                                             |
| **待处理**      | 2  | `dpos/network.go`（`processMessage` 删 4 个分支）、`spv/blocklistener.go`（`ETESCStateChanged` 的 TODO）                                                                              | 🔀 `network.go` 已在 T3 完成；`spv` 见 O6                                                         |
| **已解决**      | 1  | `dpos/msg/producersmsg.go`                                                                                                                                               | ✅ `CmdProducers` 已本地化                                                                      |
| **ABI 工具**   | 1  | `chainbridge_abi/utils.go`                                                                                                                                               | ❌ 丢                                                                                        |

### 1.4 `layer2Listener.go` 的 91 行里，只有 7 个方法该活下来

这个文件名骗人——它跟 Arbitrum/OP 的 "L2" 无关，指的是**跨链桥的仲裁人层**。91 行里：

| 方法                         | 行        | 判定    | 理由                                                                                        |
| -------------------------- | -------- | ----- | ----------------------------------------------------------------------------------------- |
| `SendMsgProposal`          | `:12-17` | 🔀 改名 | bsc 走 `Transport.BroadcastProposal`                                                       |
| `SendMsgToPeer`            | `:19-23` | ❌ 删   | 全仓唯一调用者是 `chainbridge.go:347/575`，零共识调用者                                                  |
| `SignData`                 | `:25-27` | ✅ 留   | `e.account.Sign`                                                                          |
| `DecryptArbiter`           | `:29-36` | ❌ 删   | 唯一调用者 `chainbridge.go:464`                                                                |
| `GetProducer`              | `:38-43` | ✅ 留   | 网络层要认自己                                                                                   |
| `HasProducerMajorityCount` | `:45-50` | ✅ 留   | 转 `dispatcher.GetConsensusView()`                                                         |
| `GetBridgeArbiters`        | `:52-54` | ❌ 删   | 8 个调用者**全在** `chainbridge-core/` 内                                                        |
| `GetTotalProducerCount`    | `:56-58` | ✅ 留   | 转 `consensusView.GetTotalProducersCount`                                                  |
| `GetTotalArbitersCount`    | `:60-62` | ❌ 删   | 调用者：`chainbridge.go`(4 处) + `chains/evm/chain.go:64` + `eth/bpos_network.go:58`，全是跨链/BPoS |
| `IsSyncFinished`           | `:64-66` | ✅ 留   | 转 `IsRecovered()`                                                                         |
| `OnLayer2Msg`              | `:68-91` | ❌ 删   | 4 个 case 全是 `dpos_msg` 仲裁人消息，纯 `events.Notify` 转发                                         |

**这张表是 1.3 里"PBFT 侧泄漏"那一行的展开**，也是 M4 唯一能靠删代码省工的地方：91 → ~30 行。

---

## 二、逐文件判定表

| 源（pgp-chain）                       | 行数   | 判定          | 落点                                      | 必须改动                                                                                                                                                                                                                                                                                            |
| ---------------------------------- | ---- | ----------- | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dpos/routes.go`                   | 645  | **全量移植**    | `bsc/dpos/routes.go`                    | 无 chainbridge 依赖（imports 只有 6 个 Elastos 包，已核）。改 3 处：`fmt.Println(:153)` → `Debug`；`routes.go:276` 空 `if` 块加注释（O7）；`New()` 里的全局 `events.Subscribe` 不返回句柄，见 O8                                                                                                                                    |
| `dpos/network.go`                  | 451  | **移植 + 裁剪** | `bsc/dpos/network.go`                   | ①删 `:261-280` 4 个 `dpos_msg` case（`processMessage`）；②删 `:435-442` 4 个 `dpos_msg` case（`createMessage`）——**保留 `:286` 与 `:445` 的 `CmdProducers`**；③删接口方法 `:94 OnLayer2Msg`；④合并重复 import（`:23 peer` 与 `:24 dpeer` 是同一个包）；⑤改 `Stop()`（见 T4，不是开放问题，是已定缺陷修复）；⑥删 `:368` 的 `fmt.Println(">>>>>>> ...")` |
| `dpos/message.go`                  | 53   | ✅ 已完成       | `bsc/dpos/message.go`(40)               | —                                                                                                                                                                                                                                                                                               |
| `dpos/msg/producersmsg.go`         | 89   | ✅ 已完成       | `bsc/dpos/msg/producersmsg.go`          | `CmdProducers` 已本地化                                                                                                                                                                                                                                                                             |
| `consensus/pbft/layer2Listener.go` | 91   | 🔀 只留 7 个方法 | 并入 `consensus/pbft/network.go`（新）       | 见 §1.4                                                                                                                                                                                                                                                                                          |
| `consensus/pbft/network.go`        | 1148 | 🔀 只搬 3 段   | 部分已在 `seal.go`/`view.go`/`transport.go` | 待搬：`AnnounceDAddr`(`:160-171`)、`AddDirectLinkPeer`(`:953-958`)、`StartServer`/`StopServer`(`:973-985`)、`GetActivePeersCount`(`:959-971`)                                                                                                                                                         |
| `consensus/pbft/pbft.go`           | —    | 🔀 裁 3 处    | `engine.go`/`config.go`                 | 删 `bridgeAccount`(`:112/:169/:197`)、删 `GetBridgeAccount` 调用(`:178`)                                                                                                                                                                                                                             |
| `chainbridge-core/**`              | 288+ | ❌ **不移植**   | —                                       | 见 §1.3                                                                                                                                                                                                                                                                                          |
| `eth/bpos_network.go`              | —    | ❌ 不移植       | —                                       | BPoS 验证人网络，与 PBFT 无关                                                                                                                                                                                                                                                                            |

**总待搬：1096 行**（`network.go` 451 + `routes.go` 645），其中 `network.go` 裁掉约 20 行 → **净约 1076 行**。

---

## 三、T 编号任务清单

依赖顺序：T1 → T2 → T3 → (T4 可与 T5 并行) → T6 → T7 → T8 → T9。  
每个 T 都给了**可机械复验的判据**，不接受"看起来对"。

### T1 — 固化 chainbridge 边界（零编码，纯断言）✅ 已完成

- **动作**：写 `dpos/network_cmd_table_test.go`，把 §1.2 的结论变成测试。为了让判据可机械复验，`createMessage` 的 switch 被抽成纯函数 `messageForCMD(cmd string) elap2p.Message`（`createMessage` 变成"查表 + 校验"两步，校验仍归 `CheckAndCreateMessage`）——这样测试不必伪造 header、checksum 和 `net.Conn`。
- **内容**：三件事，缺一不可
  1. `wireCommands` 手写 24 条表（**不**从被测代码派生），逐条断言 `messageForCMD` 非 nil，并回读 `.CMD()` 等于输入（防两个 case 返回同一类型）；
  2. `messageForCMD` 对 `""` / `unknown_cmd` / 大小写变体返回 nil；
  3. 4 个仲裁人命令能被解码，但 `processMessage` **不得**把它们分发给 listener——用嵌入 `NetworkEventListener` 接口的 `undispatchableListener` 实现：任何方法一被调用就 nil 解引用 panic，测试捕获后报"chainbridge business is not ported"。
- **判据**：`go test ./dpos/ -run 'TestWireCommand|TestMessageForCMD|TestChainbridgeMessages'` 绿，且两个负向验证各红一次（见 §八 实施记录）。
- **为什么先做**：这是唯一能防"以后有人手滑删 layer2.go"的护栏，而它是 O(1) 成本。

### T2 — 补 `ETDirectPeersChanged` 与 `peer.PeersInfo` ✅ 已完成

- **动作**：`bsc/dpos/event.go` 加常量（ET1012，接在 `ETNextValidators=1011` 后）；确认 `peer.PeersInfo` 来自 `Elastos.ELA/dpos/p2p/peer` 而非自建。
- **证据**：`pgp-chain/consensus/pbft/network.go:168` 发的 payload 就是 `&peer.PeersInfo{CurrentPeers, NextPeers}`，`routes.go:147` 直接断言 `e.Data.(*peer.PeersInfo)`。类型必须逐字一致，否则断言 panic。
- **一个必须知道的细节**：`peer.PeersInfo` 有**三个**字段（模块版 `dpos/p2p/peer/peer.go:63-67`）：`CurrentPeers`、`NextPeers`、**`CRPeers`**。上游 `AnnounceDAddr` 只填前两个，`CRPeers` 留 `nil`，`routes.go:148-152` 也只读前两个。移植时**照抄这个不对称**，不要"补全" `CRPeers`——它是跨链仲裁人候选集，属于被丢弃的 chainbridge 域。
- **一个坑（已避开）**：`Elastos.ELA/events` **也**有 `ETDirectPeersChanged`（`events.go:47`，值 6）。不要用它，否则 `AnnounceDAddr` 的 Notify 到不了 `routes.go` 的订阅者——两边编号空间不同。本仓一律用 `dpos.ETDirectPeersChanged`（1012）。
- **判据**：`go build ./dpos/...` 绿；`grep -c ETDirectPeersChanged dpos/event.go` → 1。

### T3 — 移植 `dpos/network.go` ✅ 已完成

- **动作**：按 §2 的改动项搬。**先搬不带裁剪的版本让它编译过，再逐条删**——直接搬裁剪版会把"删错了"和"搬错了"两种错误混在一起。
- **判据**：
  1. `go build ./dpos/...` 绿
  2. `grep -c "dpos_msg" dpos/network.go` → **0**
  3. `OnLayer2Msg` 只允许出现在注释里：`grep -n "OnLayer2Msg" dpos/network.go | grep -vE ":[0-9]+:\s*//" | wc -l` → **0**（当前命中 1 条，是 `:91` 的说明注释"OnLayer2Msg is NOT here"，刻意保留）
  4. `messageForCMD` 的 case 数 = **24**（4 个仲裁人的 codec 保留）；`processMessage` 的 case 数 = **20**（4 个仲裁人不分发）。两个数都要，只查一个会把 §1.2 的错误再做一遍
  5. `go vet ./dpos/` 无 "duplicate import"

### T4 — 修 `Network.Stop()` 的阻塞（与 T3 同一提交）✅ 已完成

`pgp-chain/dpos/network.go:155-158`：

```go
func (n *Network) Stop() error {
	n.quit <- true          // quit = make(chan bool)，无缓冲
	return n.p2pServer.Stop()
}
```

问题：`quit` 是 `make(chan bool)`（`:361`），无缓冲。若事件循环已退出（`:148-149` 收到过 quit 后 `break out`），第二次 `Stop()` 会**永久阻塞**；`Start()` 之前调 `Stop()` 同样阻塞。同一文件的兄弟实现 `routes.go:218-223` 用的是 `atomic.CompareAndSwapInt32` + `close(r.quit)`，是对的——`network.go` 这处是上游遗留。

- **动作**：改 `sync.Once` + `close(n.quit)`，`Start()` 的 select 用 `<-n.quit`（已是）。
- **判据**：新增 `TestNetworkStopTwiceDoesNotBlock`——`Stop()` 调两次，第二次必须在 100ms 内返回；`go test -race` 绿。

### T5 — 移植 `dpos/routes.go` ✅ 已完成

- **动作**：全量搬，只改 §2 列的三处。
- **特别处理 `routes.go:274-280`**：

```go
//TODO Temporary cancellation
// Do not announce address if connected peers not enough.
if len(state.peerCache) < minPeersToAnnounce {
    // Retry announce after the retry duration.
    //scheduleAnnounce(retryAnnounceDuration)
    //continue
}
```

`if` 体里两行**全被注释**，所以这是**空块**——不 `continue`、不重试，直接往下走。`minPeersToAnnounce = 5`（`:32`）因此是**死常量**（`grep -rnF minPeersToAnnounce dpos/` 只命中 `:30/:32/:276` 三处，全是这个空块）。

这是个**地雷**：4 节点台架里每个节点的 `peerCache` 最多 3 个，`3 < 5` 恒真。谁把注释解开，DAddr 公告就永不触发，M4 的 `pbft_getArbiterPeersInfo` 判据直接卡死，而且**不报错、只是静默不动**。

- **动作**：保留空块，但把注释改成显式警告（"intentionally empty: upstream disabled this gate; re-enabling it deadlocks any cluster with < 6 nodes, see m4 O7"），并加一条单测：4 个 peer 时 `announce` 仍然触发。
- **判据**：`go build ./dpos/` 绿 + `TestAnnounceWithFewPeers` 绿 + `grep -n "intentionally empty" dpos/routes.go` → 1。

### T6 — `Engine` 实现 `NetworkEventListener`（最大工作量）

`bsc/consensus/pbft/engine.go` 现在只有 `OnBlockImported`(:365) 和 `OnViewChanged`(:428) 两个 `On*`。而 `network.go:76-97` 的接口要求 **24 个**（原 25 个减去 `OnLayer2Msg`）。

**新增文件 `consensus/pbft/network_listener.go`**，逐方法映射：

| #  | 方法                            | 转发到                                              | 备注                                           |
| -- | ----------------------------- | ------------------------------------------------ | -------------------------------------------- |
| 1  | `OnPing`                      | `dtime` 时间偏移 / 忽略                                | 上游也基本空转                                      |
| 2  | `OnPong`                      | 同上                                               |                                              |
| 3  | `OnBlock`                     | `blockPool.AddBlock` + `chain.InsertChain`       | 需 `confirmed=false`                          |
| 4  | `OnInv`                       | 回 `GetBlock`                                     |                                              |
| 5  | `OnGetBlock`                  | `blockPool.GetBlock` → 回块                        |                                              |
| 6  | `OnGetBlocks`                 | `blockPool` 范围查询                                 |                                              |
| 7  | `OnResponseBlocks`            | 空实现                                              | 上游 `network.go:216-220` 的 case **整段注释**，天然空转 |
| 8  | `OnRequestConsensus`          | 回 `ConsensusStatus`                              |                                              |
| 9  | `OnResponseConsensus`         | `dispatcher.RecoverFromConsensusStatus`          | `dispatcher.go:746` 已有                       |
| 10 | `OnRequestProposal`           | `blockPool` / 已提提案查询                             |                                              |
| 11 | `OnIllegalProposalReceived`   | 黑名单（M6）                                          | 暂记日志                                         |
| 12 | `OnIllegalVotesReceived`      | 同上                                               |                                              |
| 13 | `OnProposalReceived`          | → `ProcessProposal`                              | **已有**，见 `bsc/consensus/pbft/prepare.go`     |
| 14 | `OnVoteAccepted`              | → `ProcessVote`                                  | **已有**                                       |
| 15 | `OnVoteRejected`              | → `ProcessVote`                                  | **已有**                                       |
| 16 | `OnChangeView`                | `e.onChangeView()`(:101)                         | **已有**                                       |
| 17 | `OnBadNetwork`                | 记日志 + 触发 re-announce                             |                                              |
| 18 | `OnRecover`                   | `e.MarkRecovered()`(:109)                        | **已有**，R9 的关口                                |
| 19 | `OnResponseResetViewReceived` | `e` 的 resetview 处理                               |                                              |
| 20 | `OnBlockReceived`             | `chain.InsertChain`，`confirmed` 决定是否跳 confirm 校验 | **语义最重的一个**                                  |
| 21 | `OnConfirmReceived`           | `e.onConfirm(confirm)`(:437)                     | **已有**                                       |
| 22 | `OnSmallCroTxReceived`        | no-op + `Debug`                                  | D10：业务不移植                                    |
| 23 | `OnFailedWithdrawTxReceived`  | no-op + `Debug`                                  | 同上                                           |
| 24 | `OnProducersMsg`              | `dispatcher` + `spv.SetCurrentProducers`         | 连 `#27` 的 `ETUpdateProducers` 线              |


- **判据**：`var _ dpos.NetworkEventListener = (*Engine)(nil)` 编译期断言通过；`go build ./...` 绿。
- **风险**：13/14/15/16/18/21 标"已有"的 6 个，签名**未必**和接口一致（接口用 `dpeer.PID` + `*payload.*`，`Engine` 现有方法可能是 `*types.Header` 等）。逐个对签名是这一步的主要时间成本，不要假设能直接复用。

### T7 — 装配：`SetTransport` + `AnnounceDAddr` + `routeCfg`

四处接线，都是"写对了才动"的：

| 接线              | 位置                                                  | 内容                                                                                                                                                                                     |
| --------------- | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `SetTransport`  | `eth/ethconfig/config.go:312` 附近 / `eth/backend.go` | 构造 `dposTransport` 实现 `Transport` 5 方法（`BroadcastBlock/Proposal/Vote/Confirm/ResetView`），内部 `network.BroadcastMessage(emsg)`。**必须在 miner 启动前**（`transport.go:68-71` 的注释说明晚了会静默丢当前轮的提案） |
| `AnnounceDAddr` | 新 `consensus/pbft/network.go`                       | 照 `pgp-chain:160-171`，发 `ETDirectPeersChanged` + `peer.PeersInfo`                                                                                                                      |
| `routeCfg`      | `eth/backend.go`（bsc 侧对应装配点）                        | `Config{PID, Addr, TimeSource, Sign, IsCurrent, RelayAddr, OnCipherAddr}`，见 O2/O3                                                                                                      |
| `AnnounceAddr`  | `pbft.New` 的 `NetworkConfig`                        | `func() { events.Notify(dpos.ETAnnounceAddr, nil) }`                                                                                                                                   |

- **判据**：启动日志里出现 `[UpdatePeers] self account:` 与 `received ETDirectPeersChanged peers` 两行（上游 `network.go:299` / `routes.go:153` 就打印这两行——它们是最好的"网络活着"探针）。

### T8 — `DPoSV2StartHeight` 与线格式（O1 的落地）

- **动作**：见 O1 建议。核心是**一行**：`emsg.SetPayloadVersion(emsg.DPoSV2Version)`，放在 `eth/backend.go` 的 `Legacy != nil` 分支里，与上游 `:190` 对齐。
- **判据**：新增 `TestPayloadVersionIsV2`——断言启动后 `emsg.GetPayloadVersion() == emsg.DPoSV2Version`。

### T9 — 4 节点台架验收

判据照 `pbft-port-design.md:243-250`，逐条：

| # | 判据                                                 | 复验方式                                             |
| - | -------------------------------------------------- | ------------------------------------------------ |
| 1 | 4 节点 4 生产者、阈值 `int(4*2/3)=2`                       | 启动日志里的 `producers` 数与 `GetMajorityCount()`       |
| 2 | 200 块无分叉                                           | 4 节点 `eth_blockNumber` 差 ≤ 1，且无 reorg 日志         |
| 3 | `kill -9` 一个生产者后 ≤ 2 个 view 补上                     | 计时：kill 时刻到链高恢复增长                                |
| 4 | `pbft_getArbiterPeersInfo` 返回 3 个 `2WayConnection` | 每个存活节点都查一遍                                       |
| 5 | 回滚 `--pbft.enable=false` 后节点退化为 follower           | 不 panic、`Prepare` 返回 `ErrProductionNotSupported` |

---

## 四、接线图：当前 vs M4 后

**当前**（`SetTransport` 零调用者）：

```
miner ──Seal──> pbft.Engine ──getTransport()──> nil
                                               └─ "sealed locally, told nobody"
```

**M4 后**：

```
                                              ┌─ BroadcastXxx（出站）
miner ──Seal──> pbft.Engine ──Transport──> dposTransport ──> dpos.Network ──> Elastos dpos/p2p TCP :20338
                     │                                            ▲
                     │ 24 个 On* 回调                              │ UpdatePeers
                     └── NetworkEventListener ─────────────────────┘
                                                                  ▲
                            ETDirectPeersChanged ─────────────────┘
                                    ▲
                     pbft.AnnounceDAddr（新增）
                                    ▲
                     dispatcher.GetCurrentNeedConnectArbiters()
                                    │
  eth64 ──ELAMSG 0x08──> dpos.Routes（DAddr 公告 / Inv / GetData）
                                    │
                            OnCipherAddr → Network.AddDirectLinkAddr
```

两条通道的分工再强调一次：**共识消息只走 TCP :20338**，devp2p 的 ELAMSG(0x08) **只做地址发现**。M3 已完成后者（`bsc/dpos/elamsg/`、`dpos/message.go`），M4 补前者。

---

## 五、开放问题（按需你定，不定我不开工）

### O1 — `DPoSV2StartHeight` 放哪、传什么 ⚠️ 阻断级

**证据**：`pgp-chain/params/config.go:503-521` 的 `PbftConfig` 里有 `DPoSV2StartHeight uint32`（即**在 ChainConfig 里**，走 JSON 落盘）。它最终喂给 `p2p.Config`（`network.go:386`），在 `dpos/p2p/peer/peer.go:814` 被这样用：

```go
if uint32(p.cfg.PingNonce(p.pid)) > p.cfg.DPoSV2StartHeight {
    if msg.GetPayloadVersion() < msg.DPoSV2Version {
        msg.SetPayloadVersion(msg.DPoSV2Version)
    }
}
```

而 `SetPayloadVersion` 是**进程级全局**（`dpos/p2p/msg/version.go:35`），它决定 `version.go:60/67/74/97` 四处 `GetPayloadVersion() >= DPoSV2Version` 的**字段布局分支**——也就是 `version` / `ping` / `pong` 的**线格式**。

**但上游还有第二处**，`pgp-chain/eth/backend.go:190`：

```go
if chainConfig.Pbft != nil {
    if chainConfig.Pbft.DPoSV2StartHeight <= 0 {
        chainConfig.Pbft.DPoSV2StartHeight = config.DPoSV2StartHeight
    }
    msg2.SetPayloadVersion(msg2.DPoSV2Version)   // ← 无条件
    ...
}
```

**无条件设 V2**。所以 `peer.go:814` 那个 `if` 做的事已经被上一行做过了——`DPoSV2StartHeight` 是个**死开关**（它唯一的作用是"可能提前设 V2"，而 V2 早就设了）。

**与 D9 的冲突**：D9 明写"节点运维参数不进 `ChainConfig`"。但 `DPoSV2StartHeight` 是**链属性**不是运维参数——它决定线格式，全网必须一致。

**影响**：漏了 `SetPayloadVersion` → 与 1.9.7 节点的 `version` 消息字段数不同 → 握手失败 → **M5 互通直接归零**，而 M4 台架（bsc↔bsc）**测不出来**。

**建议**：

1. `eth/backend.go` 的 `Legacy != nil` 分支里无条件 `emsg.SetPayloadVersion(emsg.DPoSV2Version)`，与上游 `:190` 逐字对齐；
2. `pbft.Config` 增 `DPoSV2StartHeight uint32`（纯透传，默认 0），**不放 `params.ChainConfig`**，理由：它是死开关，放 ChainConfig 会触发 D9 的 "stored config mismatch"；
3. T8 的测试锁死版本值，防回退。

**待你确认**：接受"进 `pbft.Config` 而非 `ChainConfig`"这个偏离上游的选择，还是宁可冒 D9 的风险也照上游放 ChainConfig？

选建议1: `eth/backend.go` 的 `Legacy != nil` 分支里无条件 `emsg.SetPayloadVersion(emsg.DPoSV2Version)`，与上游 `:190` 逐字对齐；

### O2 — `routes.Config.IsCurrent` 在 bsc 绑什么

**证据**：上游绑的是 **devp2p Downloader 进度**，不是 SPV 高度（`pgp-chain/eth/backend.go:315-324`）：

```go
engine.IsCurrent = func() bool {
    progress := eth.Downloader().Progress()
    curHeight := eth.blockchain.CurrentHeader().Number.Uint64()
    if engine.IsBadBlock(progress.HighestBlock) && curHeight+1 == progress.HighestBlock { return true }
    return progress.CurrentBlock >= progress.HighestBlock
}
```

`routes.go:408` 拿它做 `announceAddr()` 的门（"链没同步完就不公告地址"，防止公告了连不上）。

**问题**：bsc 有 `eth/downloader`，但 `engine.IsBadBlock` 是 pgp-chain 特有。而且 bsc 的 `pbft.Engine` 没有 `IsCurrent` 字段。

**建议**：`dpos.Config.IsCurrent = func() bool { return e.chain.SyncProgress()... }` 或直接绑 `downloader.Progress()` 的 `CurrentBlock >= HighestBlock`，**去掉 `IsBadBlock` 分支**（bsc 没有这个概念，且它是"最高块是坏块"的兜底，属于 recover 场景，M4 不追求）。

**待你确认**：M4 阶段是否可以简化成"`CurrentBlock >= HighestBlock`"，把 bad-block 兜底推到 M6？

确认：可以

### O3 — `NetworkConfig.GetCurrentHeight`（即 `PingNonce`/`PongNonce`）绑什么

**证据**：上游是 `pbft.go:272-274` → `spv.GetSpvHeight()`。bsc 已有同名函数（`spv/nextturn_dposinfo.go:237`）。

- **建议**：直接绑 `spv.GetSpvHeight`，无需决策。列在这里只为**留证**：它是 ELA 高度不是 pgp 高度，两者在 recharge 场景下会差很多，绑错了 ping 值会异常（但不致命，因为 `DPoSV2StartHeight` 是死开关）。

### O4 — `NetworkConfig.DataPath` 与目录创建

**证据**：`pgp-chain/consensus/pbft/pbft.go:53-56` 拼 `filepath.Join(dataDir, "network/dpos")`，然后 `NewNetwork` 传 `DataDir: cfg.DataPath`（`network.go:372`）。

**问题**：`p2p.NewServer` 是否会自己 `MkdirAll`？未验证。若不会，首次启动会在"写 peer 地址库"时失败，而错误未必显眼。

- **建议**：装配时显式 `os.MkdirAll(dposPath, 0700)`，并在 T9 判据 1 之前加一条"目录存在"检查。
- **待你确认**：目录权限用 `0700`（上游是默认 umask）是否可接受。

### O5 — 24 个 listener 方法的签名适配成本（不是决策，是提醒）

T6 表里标"已有"的 6 个方法（`ProcessProposal`/`ProcessVote`/`onChangeView`/`MarkRecovered`/`onConfirm`），现有签名大概率**不是** `(dpeer.PID, *payload.X)` 形状——bsc 是 geth 风格 `Engine`，入站方法被设计成"peer 直接调"（`transport.go:36-39` 注释）。要包一层 adapter，**不能**假设接口能直接满足。

- **需要你拍板的只有一点**：这 24 个方法放在 `Engine` 上（`Engine` 直接实现接口），还是单独一个 `dposListener` 结构体持 `*Engine` 引用？
  - 放 `Engine`：少一层，但 `engine.go` 会更臃肿（已 460 行）
  - 单独 listener：干净，但 `Engine` 要让出 `network`/`dispatcher` 的访问权
- **我倾向单独 `dposListener`**，理由：`Engine` 已经背了 geth 的 `consensus.Engine` 接口，再背一个 24 方法的 DPoS 接口，两个接口的语义会互相污染。

好的。单独dposListener

### O6 — `ETESCStateChanged`（2005）在 bsc 的落点（**已查清，不需你决策，只需点头**）

**证据**：`chainbridge-core/dpos_msg/event.go:11` 定义 `ETESCStateChanged events.EventType = 2005`，消费者是 `pgp-chain/spv/blocklistener.go:121/126`（`events.Notify(dpos_msg.ETESCStateChanged, ChainState_DPOS / ChainState_POW)`）。

bsc 侧的两半**都已经在了**，只差中间那根线：

| 半          | 位置                                                                          | 现状                                                                                       |
| ---------- | --------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| 状态常量       | `bsc/spv/spv_module.go:123-124` `ChainState_DPOS = iota` / `ChainState_POW` | ✅ 已有                                                                                     |
| notify 调用点 | `bsc/spv/blocklistener.go:390`、`:395`                                       | ⚠️ 是 `// TODO(spv-porter): notify dpos_msg.ETESCStateChanged with ChainState_DPOS.` 两行注释 |
| 事件常量       | —                                                                           | ❌ 缺                                                                                      |

- **建议**：`bsc/dpos/event.go` 加 `ETESCStateChanged = 1013`，把 `spv/blocklistener.go:390/395` 的 TODO 换成真实 `events.Notify`。**注意这会引入 `spv → dpos` 的依赖边**——`dpos/event.go:14-19` 的注释说这条边已经存在（SPV 是 `ETNextProducers` 的生产者），所以不新增环。
- **待你确认**：只确认"这条依赖边可以加一个常量"，不需要更多判断。 可以

### O7 — `routes.go:276` 空 `if` 块（见 T5，这里只留证）

不构成决策，只是提醒：**别"顺手清理"这个空块**。删掉它和保留它行为相同，但删掉会让人以为门禁从来不存在；保留 + 注释能传递"这里曾经有个 5 peer 门禁、被上游临时废掉了、4 节点台架依赖它保持废除"这三层信息。

### O8 — `dpos.Routes.New()` 里的全局订阅无法注销

**证据**：`routes.go:144-166`，`New()` 里直接 `events.Subscribe(func...)`，不返回任何句柄。

**问题**：一条进程里只能建一个 `Routes`（建两个就有两个订阅者同时响应 `ETDirectPeersChanged`，DAddr 会被公告两遍）。台架重启/测试里反复 `New()` 会累积订阅者。

- **建议**：M4 接受这个限制（一进程一 Routes），但加一条包级 `sync.Once` 或 `var routesCreated atomic.Bool` 断言，`New()` 第二次调用时 `panic` 或返回 error——**让错误在启动时暴露，而不是在台架上表现为"地址公告了两份"**。
- **待你确认**：`New()` 二次调用是 panic 还是返回 error。

---

## 六、本轮没有解决、但必须记账的

| 项                     | 状态    | 为什么现在不解决                                                                                                                                                                        |
| --------------------- | ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| R12（未来父块门禁）           | 冻结    | 等 devnet 的 `D` 分布数据，与网络层无关                                                                                                                                                      |
| R18（M3 互通台架）          | 冻结    | 等 M3 的台架结果；M4 依赖 M3 的 ELAMSG 通路先通                                                                                                                                               |
| `#27` 六条落点线           | 未结    | `network.go:173-176`、`:500-512`、`pbft.go:304/312`、`network.go:1122`、`ETUpdateProducers`、`spv/nextturn_dposinfo.go:261` —— 其中 `ETUpdateProducers` 那条由本拆解 T6 #24 收口，其余待 M4 编码时逐条核 |
| `eth/bpos_network.go` | 判为不移植 | BPoS 验证人网络，与 PBFT 生产者集无关；若 M5 互通需要再评估                                                                                                                                           |

---

## 七、批不批

按 T1→T9 的顺序，**T1–T5 不需要任何开放问题结论就能开工**（纯移植 + 边界断言），第一次需要你拍板的是 **O1**（T8 之前）和 **O2**（T7 之前）。

如果你想尽快看到代码，我建议：**先批 T1–T5**（约 1100 行移植 + 边界测试，不碰任何开放问题），O1/O2 在 T5 完成后再定。

---

## 八、实施记录：T1–T5（已交付）

### 8.1 产出清单

| 文件 | 行数 | 内容 |
|---|---|---|
| `dpos/network.go` | 484 | 移植 + 裁剪。`NetworkConfig`/`Network`/`createMessage`/`processMessage`/`UpdatePeers` 等 |
| `dpos/routes.go` | 671 | **全量**移植（零 chainbridge 依赖，无裁剪） |
| `dpos/event.go` | +20 | `ETDirectPeersChanged = 1012` |
| `dpos/network_test.go` | 115 | T4 的 3 条 Stop 回归 |
| `dpos/network_cmd_table_test.go` | 145 | T1 的 3 条 wire 表断言 |
| `dpos/routes_test.go` | 152 | T5 的 2 条 announce 断言 |

### 8.2 一个与原计划相反的结论（重要）

原文 §1.2 第一版写"移植后 `createMessage` 应降到 20 条"。**实施时发现那是错的**：按那个做，每个 1.9.7 节点第一次发 `arbiter` 就会把 DPoS 连接断掉——正是 D10 要防的事。

正确的落地是：

| 位置 | case 数 | 说明 |
|---|---|---|
| `messageForCMD`（旧 `createMessage` 的 switch） | **24** | 4 个仲裁人用 `dmsg.CmdXxx` + `dpos/msg/layer2.go` 的类型注册，**不依赖 chainbridge 包**，但 CMD 留在表里 |
| `processMessage` | **20** | 4 个仲裁人**不分发**，解出来即丢 |

也就是"24 → 20 只发生在 `processMessage`"。§1.2 已按此改写，T3 判据 4 同时查两个数。

### 8.3 判据与负向验证（每条都真跑过）

| 判据 | 结果 |
|---|---|
| `build_rc`（`go build ./...`） | 0 |
| `go vet ./dpos/` | 0 |
| `go test ./dpos/...` | 3 个包全绿（dpos 1.01s） |
| `go test ./dpos/ -race -run '...'` | ok 2.21s |
| `grep -c dpos_msg dpos/network.go` | **0** |
| `OnLayer2Msg` 非注释命中 | **0**（总计 1，是 `:91` 的解释性注释） |
| `messageForCMD` case 数 | **24** |
| `processMessage` case 数 | **20** |
| `grep -c 'dpos/p2p/peer"' dpos/network.go` | **1**（`peer` 与 `dpeer` 已合并） |

三条负向验证，确认断言不是摆设：

1. **删 codec**：从 `messageForCMD` 移除 `case dmsg.CmdDArbiter` → `TestWireCommandTableIsComplete` FAIL，报错精确到 `messageForCMD("arbiter") = nil; a 1.9.7 peer can send this and the DPoS connection would be dropped`。
2. **加回分发**：给 `processMessage` 加一个 `CmdDArbiter` 分支 → `TestChainbridgeMessagesAreDecodedButNotDispatched` FAIL，报 `processMessage dispatched "arbiter" to the listener (...nil pointer dereference); chainbridge business is not ported, so the case must be absent`。
3. **修好门禁**：把 `routes.go` 的空 `if` 块填上 `continue` → `TestAnnounceWithFewPeers` FAIL，报 `only 0 of 2 DAddr relays observed with 3 peers; the announce gate at routes.go must stay empty (m4 O7)`。
4. **恢复旧 Stop**：把 `stopOnce + close` 换回 `n.quit <- struct{}{}` → `TestNetworkStopIsIdempotent` 卡在 `chan send`（`network.go:185`），40s 超时 FAIL。

四次改动都已还原并复跑通过。

### 8.4 实施中新增的两处结构改动

1. **`createMessage` 拆成 `messageForCMD` + 校验**。原函数是"switch 然后 `CheckAndCreateMessage`"，没法在单测里调（需要 header、checksum、`net.Conn`）。拆开后 `createMessage` = 查表 + 校验，行为不变，但 wire 表变成可断言对象。这是为 T1 判据服务的，不是风格偏好。
2. **`Network.quit` 从 `chan bool` 改成 `chan struct{}` + `sync.Once`**。见 T4。顺带把 `fmt.Println(">>>>>>> cfg.DPoSV2StartHeight <<<<<<<<")` 这类 stdout 调试输出全部去掉。

### 8.5 还没做的

| 任务 | 卡在哪 | 现状 |
|---|---|---|
| T7（`SetTransport` + `AnnounceDAddr` + `routeCfg`） | **O2 已定**（绑 downloader 进度，放弃 bad-block 兜底），O3、O4 未定 | 未开工 |
| T9（4 节点台架） | 依赖 T7 | 未开工 |

`dpos.NetworkEventListener` 现在有实现者了（`*pbft.Engine`），见 §十。T8 见 §九。

---

## 九、实施记录：T8（O1 落地，已交付）

O1 的决定是「`DPoSV2StartHeight` 无条件视为已过」，即 DPoS 线格式**恒定 V2**。落地方式：

| 文件 | 内容 |
|---|---|
| `consensus/pbft/wireversion.go` | `SetWireVersionV2()` / `WireVersion()` / `DPoSWireVersionV2` 三个薄封装 |
| `consensus/pbft/config.go` | 新增 `DPoSV2StartHeight uint32` 字段，注释写清它是**死开关**、不进 `ChainConfig`（D9） |
| `eth/backend.go` | `chainConfig.Legacy != nil` 时调用 `pbft.SetWireVersionV2()`，并打一条 `DPoS wire version pinned` 日志 |
| `consensus/pbft/wireversion_test.go` | 2 条断言：设完读回来是 V2；本地常量 == `dposmsg.DPoSV2Version`（防上游常量漂移） |

要点：`SetPayloadVersion` 影响的是 `dpos/msg/version.go:60/67/74/97` 的**字段布局**，不是可选优化。上游 `pgp-chain/eth/backend.go:190` 本来就是无条件 `SetPayloadVersion(DPoSV2Version)`，`peer.go:814` 的 `if` 只是重复一次。所以这里「无条件设 V2」不是简化，是**对齐上游唯一真实行为**。

漏掉它的后果是 M5 与真链互通时字段错位、全量解不出消息 —— 而 M4 台架是 bsc↔bsc，两边都漏，测不出来。

---

## 十、实施记录：T6（24 个 listener 方法，已交付）

### 10.1 产出

| 文件 | 内容 |
|---|---|
| `consensus/pbft/network_listener.go` | 24 个回调 + 恢复状态机 + 链读写窄接口 + 辅助函数 |
| `consensus/pbft/network_listener_test.go` | 8 条测试（含 2 条同步路径、1 条恢复状态机、1 条纯函数算术） |
| `consensus/pbft/transport.go` | +3 方法：`BroadcastRaw` / `BroadcastRawExcept` / `SendToPeer` |
| `consensus/pbft/engine.go` | +字段：`reqMu` / `requestedBlocks` / `requestedProposals` / `notHandledProposal` / `statusMap` / `recoverStarted` / `blocks` / `inserter` / `peerCount` / `announce` |

形态选择（O5）：**放在 `*Engine` 上**，不另建 `dposListener` 类型。理由是上游的监听器本来就是 `*Pbft` 自己，而恢复路径要同时读 dispatcher、blockPool 和链头，独立类型只能把它们全部转发一遍，多一层而没有任何隔离收益。

### 10.2 三处必须新增的接缝

| 接缝 | 为什么必须新增 |
|---|---|
| `Transport` 从 5 方法扩到 8 | 同步路径是**请求/响应**：`OnInv` 要单播一个 `GetBlock` 给发方，`OnGetBlock` 要单播回去。广播会让 N 个 producer 同时答同一个请求 |
| `blockReader` / `blockInserter` 两个窄接口 | `consensus.ChainHeaderReader` 只读，而同步路径要 `GetBlockByHash` / `InsertChain`。`core` imports `consensus`，所以不能直接命名 `*core.BlockChain`；这和 D3 是同一手法 |
| `SetPeerCounter` | 恢复与块同步都要判「peers 够不够多数」，而 peer 数只有网络对象知道 |

### 10.3 两处刻意留空

1. **`OnGetBlocks` / `OnResponseBlocks` 空实现**。上游也是空的（`network.go:579-585`，「范围同步从来没写完」），保留空函数是为了让这两个 CMD 能解码、连接不断。
2. **`OnSmallCroTxReceived` / `OnFailedWithdrawTxReceived` 只打日志**。chainbridge 业务不移植（D10）；消息解码后直接丢。

### 10.4 判据与负向验证

| 判据 | 结果 |
|---|---|
| `go build ./eth/ ./consensus/... ./dpos/...` | rc=0 |
| `go vet ./consensus/pbft/ ./dpos/...` | rc=0 |
| `gofmt -l consensus/pbft/ dpos/` | 空 |
| `go test ./consensus/pbft/ ./dpos/...` | 全绿 |
| `go test -race ./consensus/pbft/ ./dpos/` | ok（pbft 3.31s / dpos 2.53s） |
| `grep -c 'func (e *Engine) On' network_listener.go` | **24** |
| `var _ dpos.NetworkEventListener = (*Engine)(nil)` | 编译期成立 |

负向验证 1 条：

- **去掉 `OnInv` 的 producer 守卫** → `TestOnInvIgnoresNonProducers` FAIL，报 `a node outside the producer set answered OnInv (1 messages)`。已还原。

另有一处**不需要负向验证**：`OnGetBlock` 里把池里的 `dpos.DBlock` 断言成 `*types.Block` 这种写法，编译器直接拒绝（`impossible type assertion: *types.Block does not implement dpos.DBlock (missing method GetHash)`），不需要靠测试兜。

### 10.5 编译期踩到的一个坑（记下来免得重犯）

`network_listener.go` 一开始同时写了：

```go
"github.com/elastos/Elastos.ELA/common"          // 裸名 → 占用 common
elacom "github.com/elastos/Elastos.ELA/common"  // 别名
```

两个 import 指向同一个包，裸名那个赢下了 `common` 这个标识符，于是 geth 的 `common.Hash` / `common.BytesToHash` / `common.Bytes2Hex` 全部不可达，报错是 `common.Hash (value of type func(data []byte) Uint256) is not a type` —— 看起来像 geth 的问题，实际是自己遮蔽了自己。

修法：**只给 Elastos 的 common 起别名**（`elacom`），geth 的保持裸名 `common`，因为本文件里 geth 侧全是 `common.Hash` 这种写法，反过来别名会让文件读起来是反的。

### 10.6 与原计划的一处偏差

原文把「`blockPool.GetBlock` 返回的 DBlock 需要一层适配」列为待办，实际不需要新代码：`legacy.go` 的 `dBlock` 就是那层适配，`OnGetBlock` 只需 `block.(*dBlock)` 再取 `.Block`。真正需要新写的是 `blockReader` / `blockInserter` 两个接口 —— 因为链那一侧（`*core.BlockChain`）还没有任何东西包过它。

---

## 十一、实施记录：T7 引擎侧（已交付）＋ 一个前置缺口（未解决）

### 11.1 T7 拆成了两半

T7 原文是"四处接线"。实际做下来，其中三处是**引擎侧**、不依赖任何 eth 层决策，已经做完；第四处（`routeCfg` 的 `RelayAddr`）卡在一个 T7 原文没提到的前置缺口上。

已交付（本节）：

| 文件 | 内容 |
|---|---|
| `consensus/pbft/dpos_transport.go` | `dposTransport`（8 方法全部落地）+ `Engine.AttachNetwork` 一次装配三个钩子 |
| `consensus/pbft/network.go` | `AnnounceDAddr` / `GetTimeSource` / `SetIsCurrent` / `IsCurrent` |
| `consensus/pbft/transport.go` | `BroadcastConfirm` 增加 `height` 参数 |
| `consensus/pbft/engine.go` | `isCurrent func() bool` 字段 |
| `consensus/pbft/dpos_transport_test.go` | 7 条测试 |

### 11.2 一个必须纠正的实现细节：broadcast 不是 network 广播

上游 `Pbft.BroadMessage` **不是** `network.BroadcastMessage`，而是遍历 `GetCurrentProducers()` 逐个 `SendMessageToPeer`。

这不是风格差异。`dpos.Network.UpdatePeers` 把 **current + next 两批** PID 都交给 `p2pServer.ConnectPeers`，所以"已连接的 peer"⊋"当前生产者"。用 network 级广播会把投票发给下一轮才上任的节点，甚至发给不在集合里的连接——既浪费也可能泄露投票收集状态。

`dposTransport` 因此每次都遍历 `dispatcher.GetConsensusView().GetProducers()`（而不是构造时捕获的 `e.producers`，否则跨 epoch 会把消息发给上一届）。

测试手法：`stubDPOSNetwork.BroadcastMessage` **直接 panic**。一旦有人把实现改成 network 广播，测试立刻炸而不是静默通过。

### 11.3 `BroadcastConfirm` 为什么要加 `height` 参数

`payload.Confirm` 里**没有高度字段**（它靠 seal hash 提交到一个区块），而线上 `ConfirmMsg` 是 `(confirm, height)` 两段。上游 `broadConfirmMsg(confirm, height)` 也是显式传的——实际上上游那个函数没有任何调用者。

本仓 `seal.go:332` 是调用者，那里 `block.NumberU64()` 就在手边，所以改成显式传参是零成本的；让 transport 自己从 blockPool 反查高度则需要它持有一个它本来不该拥有的池。

### 11.4 判据与负向验证

| 判据 | 结果 |
|---|---|
| `go build ./consensus/... ./dpos/... ./eth/` | rc=0 |
| `go vet ./consensus/pbft/ ./dpos/...` | rc=0 |
| `gofmt -l consensus/pbft/ dpos/` | 空 |
| `go test ./consensus/pbft/ ./dpos/...` | 全绿 |
| `go test -race ./consensus/pbft/ ./dpos/` | ok（pbft 2.40s） |

负向验证 2 条，都真跑过并还原：

1. **事件常量用错**：把 `events.Notify(dpos.ETDirectPeersChanged, …)` 换成立字面值 `events.EventType(6)`（即 `Elastos.ELA/events` 里同名常量的编号）→ `TestAnnounceDAddrReachesSubscribers` 2s 超时 FAIL，报 `ETDirectPeersChanged never reached a subscriber`。这正是 `dpos/event.go` 注释里写的那个坑，现在有测试兜住。
2. **改用 network 广播**：`broadcast` 里加一条 `t.network.BroadcastMessage(msg)` 早退分支 → `TestDPOSTransportBroadcastsToProducerSetOnly` panic FAIL，栈顶是 `stubDPOSNetwork.BroadcastMessage`。

### 11.5 前置缺口：ELAMSG 中继在 eth 层**完全不存在**

这是本轮最重要的发现，也是 T7 剩下那一半（`routeCfg`）真正卡住的地方。

**证据链**：

1. `dpos/routes.go:514` **无守卫**调用 `r.cfg.RelayAddr(iv, m)`。`RelayAddr` 为 nil 直接 panic。
2. `dpos.Routes` 只从 `ETElaMsg` 事件拿数据（`routes.go:172-173`）。
3. 全仓 `grep`：`events.Notify(dpos.ETElaMsg, …)` **零调用者**；`dpos.IPeer` **零实现者**；`SendELAMessage` 只在 `routes.go` 内部被调、在 `routes_test.go` 里有个假实现。
4. `eth/protocols/eth64/handler.go:198-206` 对入站 0x08 的处理是"打 Trace + Discard"，注释明写 `0x08 is ELAMSG on pgp-chain (dpos traffic)`。
5. 上游对应位置在 `pgp-chain/eth/handler.go:748-756`：解 `dpos.ElaMsg` → `events.Notify(dpos.ETElaMsg, &dpos.MsgEvent{ElaMsg, Peer})`；出站靠 `pgp-chain/eth/peer.go:155` 的 `SendELAMessage`。

**为什么这条不能绕过**：DAddr 中继是 4 个节点互相知道**对方 TCP 地址**的唯一途径。

- `dpos/p2p.Config` 里**没有任何静态 seed 字段**（只有 `DataDir` 存地址簿）。
- `server.ConnectPeers` 只记 PID 集合，真正拨号靠 `addrManager`；`addrManager` 只有 `server.AddAddr` 能填。
- `AddAddr` 的唯一调用链是 `OnCipherAddr` → 解密 → `Network.AddDirectLinkAddr` → `server.AddAddr`。
- 也就是说：**没有 ELAMSG 中继 → 没有地址 → 4 节点台架里 producer 之间永远连不上 TCP → 共识一条消息都发不出去**。

所以 T9 的判据 1/2/3 全都过不了，而现象是"`[UpdatePeers] self account:` 打出来了，但一个 peer 都连不上"——很容易被误判成防火墙或端口问题。

### 11.6 建议的下一步（需要你选）

| 方案 | 内容 | 代价 |
|---|---|---|
| A | 先补 ELAMSG 中继（`eth/peer.go` 加 `SendELAMessage`、`eth/handler.go` 或 eth64 handler 加入站解码 → `ETElaMsg`、加一个 `BroadcastDAddr`），再做 `routeCfg` 装配 | 要动 `eth/` 与 `eth/protocols/eth64` 两层，是本轮最大的一块，但这是上游真实路径 |
| B | M4 台架先跳过 DAddr：给 `dpos/p2p` 加一个"静态地址表"配置项（`--dpos.peer-addr=pid@host:port`），直接 `AddAddr` | 小，但**多出一个上游没有的运维面**，且它只服务自测，M5 与真链互通时还得回头做 A |
| C | A + B 都做：B 用于 T9 先把共识跑起来，A 并行补齐 | 最稳，工量最大 |

我的建议是 **C，但顺序反过来**：先 B 把 T9 台架跑通（证明共识层在 4 节点下真的能出块，这是 M4 的验收目标），再补 A（证明与真链的发现机制一致）。理由：B 只有几十行且可测；而如果先做 A，一旦台架仍不通，就会分不清是共识层的问题还是中继层的问题。
