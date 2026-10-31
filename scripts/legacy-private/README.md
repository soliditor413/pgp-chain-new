# legacy-private — 把 pgp-chain 的创世配置映射成 bsc 的 `config.legacy`

本目录是你那份 **pgp-chain（PBFT/DPoS）私链创世配置** 在 **bsc 客户端** 里的对应物。
pgp-chain 把共识参数放在 `config.pbft`，bsc 把它们整体搬到了 `config.legacy`
（`params.LegacyConfig`，`bsc/params/pgp_config.go:37-136`），所以这不是改几个字段，
而是一次**语义搬迁**：有对口的原样搬，没有对口的必须丢，还有几个字段在两边同名但含义不同。

渲染后的完整文件（由 `run.sh init` 生成）：

```json
{
  "config": {
    "chainId": 83,
    "homesteadBlock": 1,
    "eip150Block": 2,
    "eip155Block": 3,
    "eip158Block": 3,
    "byzantiumBlock": 4,
    "constantinopleBlock": 4,
    "petersburgBlock": 4,
    "istanbulBlock": 4,
    "berlinBlock": 4,
    "londonBlock": 4,
    "shanghaiTime": 1569381864,
    "legacy": {
      "forkHeight": 18446744073709551615,
      "gasFeeRecipient": "0x53781e106a2e3378083bdcede1874e5c2a7225f8",
      "gasFeeRecipientRatio": 65,
      "passBalance": 0,
      "verifySeal": true,
      "dynamicArbiterHeight": 3,
      "producers": ["0x… 你自己的 33 字节压缩公钥"]
    }
  },
  "nonce": "0x0",
  "timestamp": 1569381864,
  "extraData": "0x",
  "gasLimit": "0x2068F7700",
  "difficulty": "0x1",
  "mixHash": "0x000…000",
  "coinbase": "0x000…000",
  "alloc": {
    "0x53781e106a2e3378083bdcede1874e5c2a7225f8": { "balance": "9999999999999999999999" }
  },
  "number": "0x0",
  "gasUsed": "0x0",
  "parentHash": "0x000…000"
}
```

`genesis.json` 里 `producers` 保留 `__PRODUCERS__` 占位符，由 `run.sh init` 用
JSON 解析器（不是字符串替换）写进去，避免手改出错。

## 1. 字段映射表

### 1.1 `config.pbft.*` → `config.legacy.*`

| pgp-chain 原字段（值） | bsc 落点 | 处理 | 依据 |
|---|---|---|---|
| `pbft.producers`（6 个压缩公钥） | `legacy.producers` | **换成自己 keygen 的公钥** | `pbft.DecodeProducers(legacy.Producers)` 在启动时解析（`consensus/pbft/engine.go:231`）；你没有那 6 个私钥，沿用则链永远不出块 |
| `pbft.magic`（202000） | 无 | 丢弃 | bsc 里发现/握手走 devp2p 与 forkid，没有 magic 这个概念 |
| `pbft.ip`（127.0.0.1） | 无 | 丢弃 | bsc 用 `--pbft.net.address` 命令行；`Pbft.IPAddress` 在 pgp-chain 里也只是覆盖命令行（`eth/backend.go:185-235`） |
| `pbft.dposport`（0） | 无 | 丢弃 | 同上，`--pbft.net.port` |
| `pbft.maxnodeperhost`（100） | 无 | 丢弃 | DPoS 直连网络限流参数，bsc 侧无对应 |
| `pbft.dposv2startheight`（0） | 无 | 丢弃 | 未移植 |
| `pbft.printlevel`（0）、`maxlogssize`、`maxperlogsize` | 无 | 丢弃 | pgp 的日志旋钮，bsc 走 `--verbosity` / `--log.*` |
| `dynamicArbiterHeight`（3，原在 `config` 顶层） | `legacy.dynamicArbiterHeight` | **原样搬 3** | 同名同义，只是从 `ChainConfig` 下沉进了 `LegacyConfig` |

> `dynamicArbiterHeight` 是 **ELA 主链高度**（从该高度起 SPV 监听切换到“下一轮 DPoS 出块人”）。
> 私链没有 SPV 数据库（`spvDataDir` 为空），这个值不参与任何执行路径。
> **坑：写 0 不等于 0** —— `ChainConfig.DynamicArbiterHeight()`（`pgp_config.go:299-307`）
> 会把 0 兜底成主网默认值 `2075778`。要么写真实高度，要么写 `18446744073709551615`。

### 1.2 原 `config` 顶层的其它字段

| pgp-chain 原字段 | bsc 落点 | 处理 |
|---|---|---|
| `chainId` / `oldChainID`（83） | `chainId` | 保留 83；`oldChainID` 丢弃（bsc 无此字段） |
| `homestead/eip150/eip155/eip158/byzantium/constantinople/petersburg/istanbul/berlin/london` | 同名 | 原样保留，值不变（1,2,3,3,4,4,4,4,4,4） |
| `eip150Hash`（全 0） | 无 | 丢弃（bsc 的 `ChainConfig` 没有这个 json tag） |
| `chainIdBlock`（4） | 无 | 丢弃（pgp 专用） |
| `pbftBlock`（0） | 无 | 丢弃；bsc 用 `legacy.forkHeight` 表达“何时切 Parlia” |
| `developerFeeBlock`（1）/ `developerFeeTime` | 无 | 丢弃；费用分账在 bsc 是 `legacy.gasFeeRecipient` + `gasFeeRecipientRatio` |
| `preConnectOffset`（5） | 无 | 丢弃（pgp 的预连接提前量） |
| `layer2block`（4）/ `customizeDIDHeight`（1）/ `maxExpiredHeight`（2）/ `nodeVersionHeight`（150） | 无 | 丢弃：这四个是 pgp-chain 自己的链上开关，bsc 里没有对应实现 |
| `clique { period: 3, epoch: 30000 }` | 无 | **必须删**：bsc 里 `Clique != nil` 会去建 clique 引擎（`ethconfig/config.go:331`），而且 `core/genesis.go:671` 会因 `extraData` 太短直接报 `can't start clique chain without signers` |
| `shanghaiTime`（1569381864） | 同名 | 保留原值 |
| `pbft` 整个对象 | — | 见 1.1 |

### 1.3 创世头字段

| 字段 | 原值 | 现值 | 说明 |
|---|---|---|---|
| `extraData` | clique 格式 217 字节（32 vanity + 6×20 signer + 65 seal） | `"0x"` | 那 6 个 signer 是 clique 地址，与 `legacy.producers` 无关；两个仓的 PBFT 都不读创世 `extraData` 做校验 |
| `timestamp` | `"1569381864"`（字符串） | `1569381864`（数字） | 去掉引号：`Genesis.Timestamp` 是 `uint64` |
| `gasLimit` | `0x2068F7700` | 不变 | 与 bsc 里 pgp 主网创世一致（`core/genesis.go:735`） |
| `difficulty` | `0x1` | 不变 | 同上（`core/genesis.go:736`）；出块后由引擎写成 `0x2` |
| `nonce` / `mixHash` / `coinbase` / `number` / `gasUsed` / `parentHash` | 全 0 | 不变 | |
| `alloc` | 1 个地址 9999999999999999999999 wei | 不变 | 见下面“要改的三处” |

## 2. 相对原配置必须改的三处（私有链才需要）

### 2.1 `producers`：换成自己的公钥，数量决定能不能出块

确认阈值是 `int(N*2/3)` 向下取整：

| producers 数量 | 需要几票 | 单节点能出块吗 |
|---|---|---|
| 1 | 0 | ✅ |
| 4 | 2 | ❌ 至少 2 个节点 |
| 6（= 你原配置） | 4 | ❌ 至少 4 个节点 |

所以默认 `PRODUCERS=1`。要用 4 个就先 `PRODUCERS=4 ./run.sh init`，再同时起 2 个以上节点。

### 2.2 `forkHeight`：保持 `MaxUint64`

`[0, forkHeight)` 用 legacy（PBFT/DPoS）规则，`forkHeight` 起切 Parlia。
私链没有 Parlia 配置（`config.parlia` 为空），所以必须 `18446744073709551615` 全程 legacy。

### 2.3 `alloc`：换成你**有私钥**的地址

原配置里的 `0x53781e106a2e3378083bdcede1874e5c2a7225f8` 是 goerli 创世里的样例地址
（`pgp-chain/core/genesis.go` 的 `DefaultGoerliGenesisBlock`），**它的私钥不在任何人手上**。

而且注意：出块人的 `minerAddress` 是由 **P-256（Elastos）** 公钥推导的，不是 secp256k1，
你同样签不了 EVM 交易。所以想让私链能转账/部署合约，必须往 `alloc` 里加一个你自己
secp256k1 地址（`geth account new` 生成），然后重新 `init`。

## 3. 怎么跑

```bash
cd bsc
export PGP_DEVNET_PASSWORD='<启动口令>'      # 只用于 keystore，不落盘到仓库

./scripts/legacy-private/run.sh init       # 生成 arbiter keystore + 渲染 genesis + geth init
./scripts/legacy-private/run.sh start      # 出块节点（前台，Ctrl-C 停）
./scripts/legacy-private/run.sh status     # 查块高
./scripts/legacy-private/run.sh stop
./scripts/legacy-private/run.sh clean
```

产物全部落在仓库外：`~/.pgp-legacy-private/{keys,chain,logs,genesis.json}`。

可用的环境变量：`PRODUCERS`（默认 1）、`CHAIN_ID`（83）、`HTTP_PORT`（8577）、
`P2P_PORT`（30401）、`GENESIS_TEMPLATE`、`PGP_DEVNET_HOME`。

实现上本目录只是一个薄壳：真正的 keygen/启停/校验复用 `scripts/pbft-devnet/`，
仅通过 `GENESIS_TEMPLATE` 换掉创世模板（该变量是本轮为复用而加的）。

## 4. 实测记录（2026-10-31，本机 macOS Intel，go1.23.12）

```
genesis hash : 0x1d95be943a8b9be61b069f175ceea775b5c1deb9d43d06f5f415452a62490d90
chainId      : 0x53 (83)
出块         : 1 → 166 连续，间隔 ~3s，无 ERROR
创世余额     : 9999999999999999999999 wei 全额可查（0x21e19e0c9bab23fffff）
区块头       : difficulty 0x2，无 baseFee，extraData 为「33 字节公钥 + 确认签名」
               —— 确认走的是 legacy 布局，不是 clique 的 32+65
```

`shanghaiTime` 保留 2019 的值不影响出块：`core/genesis.go:611` 对 PGP legacy 区块
不写 `WithdrawalsHash`，所以“上海已激活但没有 withdrawals 字段”是自洽的。

## 5. 两个已确认的阻塞（做交易测试前必读）

### 5.1 交易进池 → 出块停住（严重）

同一份 genesis、同一个二进制，两台节点对照：

| 节点 | 交易池 | 结果 |
|---|---|---|
| A（8577） | 空 | 连续出到 166 块 |
| B（8588） | 2 笔 pending | 停在 17 块，15 分钟不动 |

B 的日志顺序（`--verbosity 5`）：

```
DEBUG commitWork stopTimer                     block=18 ... commit delay=2.98s
DEBUG Tx tracker status                        need-resubmit=2
TRACE Pooled new future transaction            hash=a344df..7fad28
TRACE Promoting queued transactions            count=2
DEBUG commitWork txsCh arrived                 work.tcount=0 newTxsNum=0 len(ev.Txs)=2
DEBUG Not preparing a header for sealing       err="pbft: consensus is already running"
（此后再无任何 commitWork / Sealing block）
```

机制：交易事件让 `commitWork` 提前 return（`miner/worker.go:1132-1136`，`Prepare` 返回
`pbft.ErrConsensusIsRunning`，被 `pbft.IsBenignPrepareError` 判为“良性”只打 Debug）；
提前 return 使 `interruptCh` 里留着一个未取走的值，`newWorkLoop` 下一次定时
`commit()` 往里写时阻塞（`miner/worker.go:464-477`），工作循环就此停摆，而 `mainLoop`
只在收到新 work 时才动，于是链头冻结。

即：**当前 bsc 的 legacy 引擎只能出空块**。要做真正的交易测试，得先修这条路径
（`commit()` 里改成非阻塞发送，或让提前 return 前把 `interruptCh` 排空）。

### 5.2 `eth_sendTransaction` 必 panic

```
panic: runtime error: invalid memory address or nil pointer dereference
math/big.(*Int).Mul
internal/ethapi.(*TransactionArgs).setLondonFeeDefaults  transaction_args.go:273
```

`transaction_args.go:273` 无条件 `new(big.Int).Mul(head.BaseFee, 2)`，而 legacy 引擎产出的
区块头 `BaseFee` 恒为 `nil`（`consensus/pgp/pgp.go:114` 明确写着 legacy 走 `BaseFee = 0`）。
只要 `IsLondon(head.Number)` 为真就会走到这里。

绕法：**本地签名 + `eth_sendRawTransaction`**（钱包本来就这么做，pgp 主网也是靠这个规避的）。
根治：那一行加 nil 判断。

### 5.3 附带观察（不影响出块）

启动时 `pgp: legacy ELA precompile set incomplete ... missing=11 total=12`：
SPV 未注册，`0x…3e8`~`0x…3f3` 这 11 个 ELA 预编译调用会失败；纯 EVM 转账/合约不受影响。

### 5.4 多节点（N>1）：视图栅格只能锚在区块时间上，锚错了 N=6 会永久活锁

> **本条曾经写错过，错误结论留档。** 旧版本把下面这套行为写成"能出块，只是冷启动
> 会抖动"：`OnBlockImported` 对"自己 confirm 池里还没有对应 confirm"的块单方面
> `ResetConsensus(height)`、按**本节点自己的时钟**重锚视图，各节点导入同一块的时刻差
> 几百毫秒，于是"`viewStartTime` 各锚各的、proposal 互相拒收；`precociousProposals`
> 暂存 + 每 6s 的视图轮转最终会把票凑齐，所以是收敛慢、不是永久停摆"。
>
> **这个结论是错的。** 把"按本机时钟锚定"当成已知设计写进文档，正是下面这次 6 节点
> 事故被误诊的根源：它把一个结构性分叉描述成了一次会自愈的抖动。

**正确契约。** `viewOffset` 不是投票数出来的量，它是**本节点自上次重锚以来走过了多少
个 `signTolerance`（6s）的计数**。两个节点只有用**同一个锚点**重锚，才会在同一个高度
上认为自己在同一个视图里。全流程里唯一"每个节点都能算出同一个值"的锚点，是被导入
区块的 `header.Time`——整秒、随区块广播、人人相同。据此：

- `Dispatcher.ResetConsensus(height, anchor)`（`dpos/dispatcher.go:395`）的锚点由调用方
  给，函数不再读本机时钟（`:406` 用的是 `anchor`）；
- `Engine.OnBlockImported`（`consensus/pbft/engine.go:439`）对**每一个**被导入的块都用
  该块自己的 `header.Time` 重锚（`:453`）。这与"推进 finality 水位"是两件事，必须分开：
  水位仍只在"本节点有 confirm 且 sealHash 匹配"时推进（D2/R7），**但视图重锚不能因此
  被跳过**；
- 只有启动恢复路径（`network_listener.go:636` 的 `OnResponseResetViewReceived`，重锚在
  `:662`）在取不到 head header 时才降级用本机时钟，而且降级会打进日志——首个区块导入
  就会把它纠正回来。

上游对应位置是 `pgp-chain/consensus/pbft/pbft.go:591-598`（`Finalize` →
`FinishedProposal(..., header.Time)`）：上游把"重锚"和"推进水位"捆在一起，所以它的常规
路径永远不会分叉栅格。本移植按 D2 把两者拆开，拆的时候把锚点一起丢了，才有了这次事故。

**症状**（6 producers，链停在高度 6~8，`eth_blockNumber` 半小时不动）：

| 读法 | 含义 |
|---|---|
| 六台 `viewoffset` 分成两组（本次 9 9 9 / 7 7 7） | 不是抖动，是两组栅格 |
| 日志刷 `have different view offset` | 两端视图号不同，`ProcessProposal` 直接拒收，**且不回追**（上游同样只丢弃，所以"加回追"不是修法） |
| 每隔几秒六台同时出现 `force: true … offset: 1 offsetTime: 3.007s viewOffset 0` | 有人在按 ResetView 票数强推，推完立刻又分开 |
| 3 台对 3 台，`acceptVotes count` 到 3 就上不去 | 6 producers → `majoritycount = int(6*2/3) = 4`，3 票永远不够 |

**判据（怎么一眼定性，按这个顺序）**：

1. **量六台锚点的整秒差，不是量子秒相位。** 任取一条
   `--------------------Change View---------------------`，用它的 `viewStartTime` /
   `offsetTime` / `viewOffset` 反算 `anchor = now − offsetTime − viewOffset × 6s`。六台算出
   两个不同的**整秒**（本次 21:59:49 与 21:59:39，差 10 秒 ≈ 两个视图），就是本条。
2. 子秒相位跨度（本次 786ms，`.890` 与 `.104`）**不是**根因，只是"窗口损耗"：相位靠后的
   节点会晚 ≤1 个 tick 跟上，偏差有界且自愈。把 ticker 相位对齐既造不出、也修不好 10 秒
   的锚点差。
3. `dutyIndex` 卡在某个值**不代表**出块人被锁死：`GetNextOnDutyProducer`
   （`dpos/producers.go:261`）会把 `viewOffset` 加进索引，职责是转的。

**为什么 4 producers 测不出来**：`GetMajorityCount()` 是 `int(n*2/3)` 截断，n=4 → **2 票**，
比 `f=1` 需要的 3 票还低。4 节点台架跑的是"低于 BFT 阈值"的配置，一组已经分叉的节点
靠 2 票也能出块，把同样的分叉完全掩盖。所以 4 节点台架跑到 189 块、相位差 7ms 的"健康"
记录，是**配置假象 + 恰好同时在 force 重锚**的产物，不能当作"栅格已收敛"的证据。

同一台架（`scripts/pbft-devnet/multinode.sh`，4 producers，跑到 67 块）当初被我们读成
"冷启动抖动"的计数，按本条重新解读，那**就是同一条 bug 在 n=4 下的泄漏量**：

| 节点 | `have different view offset` | `seal timed out` | `resetConsensus` |
|---|---|---|---|
| node0 | 24 | 2 | 5 |
| node1 | 8 | 2 | 6 |
| node2 | 40 | 1 | 3 |
| node3 | 42 | 4 | 0 |

四台机器上拒收次数差 5 倍，说明分叉程度每台都不同——"抖动"这个词掩盖了这一点。

**回归判据（改完怎么确认）**：

- `go test ./dpos/` 的 `TestResetConsensusAnchorDecidesTheViewGrid`：喂本机时钟锚点 → 两台
  `viewOffset` 差 2、五轮超时后仍是 2（红态固化成断言）；喂同一个 `header.Time` →
  `viewStartTime` / `viewChangeTime` 完全相等、亚秒为 0，五轮后 `viewOffset` 仍相等。
- `TestSharedAnchorIsStableUnderPhaseSkew`：`.890` / `.104` 相位差下，窗口内偏差恒为 1 且每轮
  自愈，不累积。
- 集成侧：6 节点**错峰启动（每台间隔 1.5s）**仍能出块。错峰启动才是能暴露这个 bug 的
  启动序——齐步启动会让所有节点在同一秒内被重锚，把问题藏起来。

判定：停在高高度不动**超过 3-5 分钟**且 `pbft_status.isrecovered=true`、`activedpospeers`
≥ `majoritycount`，才是真的有问题；刚启动 30 秒内停在低高度属正常。但**只要出现
`viewoffset` 分成两组且组间差 ≥2，就不用等了**，那是本条描述的活锁，不会自愈。

## 6. pbft 共识 RPC（v0.9.8 起新增）

`net_peerCount` 只数 devp2p peer，而共识走的是**另一条 TCP**（`--pbft.net.port`），
所以"peer 都连上了却不出块"这个经典症状，原来的 RPC 无法区分。新增 `pbft` 命名空间
（`consensus/pbft/api.go`）：

```bash
# 必须显式开启命名空间，否则 method not found
geth ... --http.api eth,net,web3,pbft
```

| 方法 | 用途 |
|---|---|
| `pbft_status` | 一张表回答"为什么不出块" |
| `pbft_getArbiterPeersInfo` | 只列 voting set 内的直连 peer（`2WayConnection` 才算双向） |
| `pbft_getAllPeersInfo` | 全表，含 `NoneConnection` = "该连它但连不上" |
| `pbft_getActivePeers` | 活连接层，带方向（`InboundOnly` / `OutboundOnly`） |
| `pbft_announceDAddr` | 手动重播 DPoS 地址公告（N≤3 时自动重播条件恒假，只能手踢） |

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"pbft_status","params":[]}' \
  http://127.0.0.1:8577
```

```json
{"isproducer":true,"isonduty":false,"isrecovered":true,"isproductionenabled":true,
 "isrunning":true,"viewoffset":0,"producercount":4,"majoritycount":2,
 "activedpospeers":6,"finishedheight":66,"dposnetworkrunning":true,
 "dutyindex":2,"viewstarttime":"2026-10-31T11:34:31Z",
 "changeviewtime":"2026-10-31T11:34:37Z","headheight":67}
```

按输出定位问题：

| 现象 | 结论 |
|---|---|
| `dposnetworkrunning:false` | DPoS 服务器对象根本不存在（`e.network == nil`）。见 6.1，**绝大多数是 keystore 没打开**，不是端口问题 |
| `isproducer:false` | 两种可能：keystore 没打开（`account == nil`），或打开了但公钥不在 `legacy.producers` 里。用 `dposnetworkrunning` 区分 |
| `isrecovered:false` | 还在等状态恢复，`Prepare` 全部被拒（只打 Debug，日志里看不见） |
| `activedpospeers < majoritycount` | 直连 peer 不够 → `pbft_getAllPeersInfo` 里挑 `NoneConnection` 的条目查 |
| `majoritycount` 大于"在跑节点数" | 票数物理凑不齐。N=6 → 需 4 票，只起 2 个节点永远不会出块 |
| 全部正常但 `viewoffset` 持续增长 | 视图在空转，见 5.4 |

⚠️ 两个读数口径：
- `activedpospeers` 数的是**连接数**，不是 peer 数。4 producer 全互连时是 6（每个对端一条入向 + 一条出向），不是 4。它用的是恢复门的同一个 `GetActivePeers()`，故意不与 `pbft_getArbiterPeersInfo`（按 PID 折叠）对齐，避免两套口径。
- `finishedheight` 落后 `headheight` 1-3 块是 confirm 池的正常迟滞。

### 6.1 典型误诊：`dposnetworkrunning:false` + `isproducer:false` + `producercount:6`

这份输出（`net.peerCount` 有值、`headheight` 恒 0）不是网络问题，是**一个字段的连带反应**：

```
isproductionenabled:true   →  --pbft.enable 给了
producercount:6 majoritycount:4  →  genesis 的 legacy.producers 解出了 6 个合法 P-256 公钥
isproducer:false           →  engine.go:455  `e.account == nil || e.dispatcher == nil`
                              dispatcher 在 New() 里无条件创建（engine.go:239），不可能 nil
                              ⇒ 只剩 e.account == nil：keystore 没打开
dposnetworkrunning:false   →  network.go:130 就是 `e.network != nil`，
                              只在 pbft_network.go:175 的 AttachNetwork 里被赋值。
                              而 pbft_network.go:71-75 恰好是 `account == nil` 就 return
isrecovered:true           →  假的好消息。backend.go:934 的逻辑是
                              `pbftEngine != nil && !HasTransport() → MarkRecovered()`，
                              没有 transport 的节点被**直接标记为已恢复**，不构成任何证据
activedpospeers:0          →  DPoS 服务器不存在，自然是 0
```

判定顺序：`majoritycount` 有值 ⇒ dispatcher 与 view 都活着 ⇒ 只可能是 `account == nil`。
N=6 要 4 票，0 条直连物理凑不齐。

**去日志里搜这三行，按命中的那行分诊：**

```bash
grep -F 'PBFT: production enabled without a keystore' node.log   # 压根没给 --pbft.keystore
grep -F 'PBFT: cannot open arbiter keystore'          node.log   # 给了但打不开（路径/密码/格式）
grep -F 'PBFT: no producer key, DPoS network not started' node.log  # 上面两种的下游，必现
grep -F 'DPoS network started' node.log                          # 反向确认：没有这行就是没起来
```

第二种的 `err` 还要再分一次：

* `password wrong` / JSON unmarshal 错 → 文件是 `geth account new` 生成的 **secp256k1 EVM
  keystore**，不是 ELA wallet。`dpos.GetDposAccount` 走 `elastos/Elastos.ELA/account.Open`，
  读的是 AES 主密钥 + IV + 32 字节加密 blob 的 ELA 格式，secp256k1 文件在它眼里是垃圾。
* `no such file` → 相对路径是按 **datadir** 解析的（`backend.go:365` `stack.ResolvePath`），
  不是按当前工作目录。

**修法（30 秒）：**

```bash
export PGP_DEVNET_PASSWORD='devnetpass'            # 空密码会被 keygen 拒绝
go run ./scripts/pbft-devnet/keygen -out ./devkeys -n 6
# stderr 会直接打印可粘贴的 producers 列表；manifest 在 ./devkeys/producers.json
go run ./scripts/pbft-devnet/keygen -out ./devkeys -n 6 -verify   # 用生产代码路径复检
```

拿到 6 个公钥后写回 genesis 的 `config.legacy.producers`，**必须重新 `geth init`**
（producers 在创世里 ⇒ genesis hash 变了 ⇒ 旧 datadir 全部作废），然后每节点：

```bash
--pbft.enable --mine \
--pbft.keystore ./devkeys/arbiter-<i>.dat \
--pbft.keystore.password <(printf '%s' "$PGP_DEVNET_PASSWORD") \
--pbft.miner.address <producers.json 里的 minerAddress> \
--pbft.net.address 127.0.0.1 \
--pbft.net.port $((20679+i)) \
--pbft.net.maxnodeperhost 6
```

> **`--pbft.keystore.password` 是文件路径，不是字面密码。** 与 pgp-chain
> （`cmd/utils/flags.go` 的 `MakeDposPasswordList`）一致：flag 给的是一个"装密码的文件"，
> geth 启动时把文件内容读出来当密码用。所以上面用 `<(printf ...)` 进程替换造一个只有本进程
> 能读的临时 fd 递进去，argv 里出现的只是 `/dev/fd/63` 这种路径，`ps` 看不到密码。
>
> **把字面密码直接写在这个 flag 上是本节最容易踩的坑**：进程照常启动，但 geth 会拿这个
> 字符串当路径去 `os.ReadFile`，读不到 → 启动即失败并打印
> `Failed to open the file named by --pbft.keystore.password ...`。若换个存在的路径但内容不对，
> 则是静默降级成 verifier（`isproducer:false`、链高度永远是 0），因为 `Prepare` 的失败只在
> Debug 级打日志。
>
> 文件格式：**单行**，末尾换行会被去掉（LF 和 CRLF 都行）。以下四种都会被**明确拒绝**
> 而不是变成 `password wrong`：
>
> | 文件内容 | 报错 |
> |---|---|
> | 多行 | `contains more than one line` |
> | 空文件（0 字节 / 只有换行） | `is empty` |
> | 含 NUL 字节（PowerShell `>` 写出的 UTF-16LE 就是这种） | `contains NUL bytes; ... save it as UTF-8` |
> | 只有 BOM | `is empty`（BOM 先被去掉，剩下空） |
>
> 开头是 UTF-8 BOM 的文件会被**自动去掉 BOM** 再用——记事本等编辑器会加，而带上 BOM 的
> 密码看起来完全正确却解不开，正是本坑最典型的形态。

三处容易漏：

* `--pbft.net.port` 每节点必须不同，同端口只有先起的那个能 bind，其余在建 network 时直接
  返回错误、进程起不来（这个不是静默的，和本节的症状不一样）。
* `--pbft.net.magic` 全组必须一致，默认 `202511`（`config.go:41`），与 genesis 里的
  `magic: 202000` **没有关系** —— 本移植没有任何代码读 genesis 的 magic。不一致的症状是
  "连接被握手拒绝，日志只在 Debug"，也就是本节这个"看起来健康"的形状，所以显式写上更好。
* 如果 producers 沿用了原 pgp-chain genesis 里的那 6 个 PID，而你手里没有它们的私钥，
  就永远修不好 —— 必须生成自己的 6 把 key 换掉（这也是本节最常见的情形）。

修好后的对照读数：`isproducer:true`、`dposnetworkrunning:true`、`activedpospeers` 至少
`majoritycount`，日志里出现 `DPoS network started` 带 `pid` / `magic` / `producers` 三列。

## 7. 安全注意

* keystore 与密码不进仓库：`~/.pgp-legacy-private/keys`（0700）。密码只从
  `PGP_DEVNET_PASSWORD` / `PGP_DEVNET_PASSWORD_FILE` 取，不写盘。`run.sh` 用一个
  `<(printf ...)` 进程替换把密码递进 `--pbft.keystore.password`（该 flag 收的是文件路径），
  密码只存在于管道里，argv 里只有一个 `/dev/fd/N` 路径，因此 `ps` 看不到、磁盘上也没有。
* 不要把这份 genesis（含你自己的测试私钥对应地址）用于任何有真实资产的链。
* `verifySeal: true` 保持不变：它会真的校验 legacy 区块的 confirm 签名。关掉只用于调试。
