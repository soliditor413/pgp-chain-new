# PBFT devnet 启动件（运维 runbook）

这个目录是给 `--pbft.enable` 用的**可执行启动件**：Elastos 格式的 arbiter keystore、
`legacy.producers` 指向自己公钥的 genesis、以及一键 init/start/stop/status 的脚本。

> **当前状态：单节点 devnet 能稳定出块（每 3 秒一个，已实测到 tip 130）。**
> 曾经挡住出块的那处 `seal.go` 顺序问题已经修好，证据与验收方法见后面的
> 「已验证：单节点 devnet 能连续出块（原「已知阻塞」已修复）」一节。
> 那个旧的"出不了块"叙述保留在该节的"历史形状"里作为排查样本，不再代表现状。

---

## 1. 目录

| 文件 | 作用 |
|---|---|
| `keygen/main.go` | 独立 Go 程序（`go run` 执行，不进 geth 二进制）。生成 N 个 Elastos 格式 arbiter 钱包、打印 33 字节压缩公钥、并把每个钱包用生产代码路径 `dpos.GetDposAccount` 读回来校验 |
| `genesis.json` | genesis 模板。`config.legacy.producers` 里是一个占位字符串，`run.sh init` 用 JSON 解析器替换掉（不做文本拼接），再交给 `geth init` |
| `run.sh` | 一键启动：`init` / `start` / `start-follower` / `status` / `stop` / `verify-keys` / `verify-secrets` / `clean` |
| 本文件 | runbook |

**所有运行期产物（私钥、genesis、datadir、日志、临时配置）都在 `$PGP_DEVNET_HOME`
（默认 `~/.pgp-devnet`）里，不在仓库内。** 仓库里只有上面三个文件。

## 2. 快速开始

```bash
# 1) 密码只走环境变量，不落仓库、不进 shell history
export PGP_DEVNET_PASSWORD='换成你自己的密码'

# 2) 生成 keystore + 渲染 genesis + geth init
./scripts/pbft-devnet/run.sh init

# 3) 起生产节点（前台，Ctrl-C 停），日志同时 tee 到 ~/.pgp-devnet/logs/geth.log
./scripts/pbft-devnet/run.sh start

# 4) 另一个终端看状态
./scripts/pbft-devnet/run.sh status

# 5) 停（可以在另一个终端执行，也可以直接 Ctrl-C）
./scripts/pbft-devnet/run.sh stop
```

`geth` 二进制不存在时会自动 `go build -o build/bin/geth ./cmd/geth`。
**不会**动仓库根目录那个历史遗留的 `geth`。

⚠️ `clean` 删的是**整个 `$PGP_DEVNET_HOME`**，包括 `keys/` 里的 arbiter keystore ——
不是只清链数据。密钥删了只能重新 `init` 生成一套新的（`legacy.producers` 会跟着变，
旧链数据也就对不上了）。它需要交互确认；非交互环境（CI、脚本）里必须先给
`CONFIRM_CLEAN=yes`，否则它会 `aborted; nothing was deleted`。

**顺序陷阱（一定会踩，写死在这里）**：`clean` 之后正确的顺序是

```
clean → init → verify-keys → start
```

`verify-keys` 必须在 `init` **之后**。反过来（`clean` 完直接 `verify-keys`）必然失败：

```
ERROR: no keystores at /Users/you/.pgp-devnet/keys
```

因为 `verify-keys` 做的事是"用生产代码路径 `dpos.GetDposAccount` 把磁盘上已有的 keystore
读回来校验"，它**不生成**密钥。`init` 才生成。所以验证顺序错不会证明密钥坏了，
只会证明还没生成——看到这条报错先看是不是顺序问题，别急着怀疑密码或文件权限。

主要环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `PGP_DEVNET_PASSWORD` | 无（必需） | keystore 密码 |
| `PGP_DEVNET_PASSWORD_FILE` | 无 | 上一个的替代：从文件读（自身建议 0400） |
| `PGP_DEVNET_HOME` | `~/.pgp-devnet` | 运行期根目录 |
| `PRODUCERS` | `1` | `init` 时生成几个 arbiter（见 §5） |
| `CHAIN_ID` | `8606211` | **不能**是 pgp 主网的 `860621`，见 §6 |
| `HTTP_PORT` / `P2P_PORT` | `8555` / `30399` | |
| `VERBOSITY` | `3` | 排错时用 `4` |
| `ARGV_PASSWORD=1` | 关 | 即使二进制支持环境变量也强制把密码放命令行（仅排错用，会被 `ps` 看到） |

## 3. 怎么判断成功

成功的唯一硬指标是 **`eth_blockNumber` 在涨**，且块间隔约 3 秒
（`consensus/pbft/consts.go` 的 `blockPeriod` 是常量 3，不是配置项）。

`./run.sh status` 会打印每个块的 `extra` 长度并反算票数：

```
block 1  ts=1761  difficulty=2  extra=275B  votes=1
          coinbase=0xe2af...
          interval=3s (expected 3)
PASS: the chain is advancing; extra length must be 143 + 132*votes
```

`extra` 的长度公式（来自 `consensus/pgp/confirm.go`）：

```
proposal = 33(公钥)+1 + 32(BlockHash) + 4(ViewOffset) + 64(签名)+1 = 135
确认头   = 135 + 8(票数, 固定 uint64 LE)                        = 143
每张票   = 32 + 33+1 + 1 + 64+1                                 = 132
extra    = 143 + 132 * 票数
```

- 12 个生产者 × 8 票 = 1199 字节，这是 pgp 主网老块的固定长度。
- 1 个生产者 × 1 票 = 275 字节。
- 如果你看到 extra 长度不是 `143 + 132k`，说明块里的 confirm 不是这条路产生的，直接判失败。

## 4. 怎么判断失败

| 现象 | 含义 |
|---|---|
| `status` 打印 `no block has been produced` 并给出 `diagnosis:` | 见 §7 的对照说明，脚本已经替你 grep 了日志 |
| 日志 `PBFT: seal timed out waiting for a confirm height=N` | 一轮 6 秒（`signTolerance`）内没凑够票 |
| 日志 `PBFT: height already has a confirm, refusing to propose` | 该高度的 confirm 已在 block pool 里，节点拒绝再提一次（`ErrAlreadyConfirmedBlock`） |
| 日志 `PBFT: cannot open arbiter keystore` | keystore 打不开：密码错 / 文件不是 Elastos 格式 / 权限不对。用 `./run.sh verify-keys` 单独验证 |
| 日志 `Not preparing a header for sealing ... pbft: signer is not on duty` | 当前 view 轮不到这个生产者（多生产者时正常） |
| 日志 `pgp: legacy ELA precompile set incomplete` | **预期内**。12 个 ELA 跨链预编译里 11 个要等 SPV 注册；空块不触发任何预编译，可以忽略 |
| 启动即 `Fatal: database was initialised for a different chain` | chain id 用了 `860621`（pgp 主网）但 genesis 不是主网的。改用别的 chain id 并 `clean` 重来 |

## 5. 单生产者还是多生产者（阈值算给你看）

`consensus/pgp/confirm.go` 的阈值是

```go
func legacyMajorityCount(totalSigner int) int { return int(float64(totalSigner) * 2 / 3) }
```

所以：

| 生产者数 | `legacyMajorityCount(n)` | 后果 |
|---|---|---|
| **1** | **0** | 阈值 0：**没有票的 confirm 也能通过校验**。一个节点自己那一票就够（1 ≥ 0） |
| 2 | 1 | 自己那一票就够 |
| 3 | 2 | 单节点凑不到（只有自己 1 票） |
| 4 | 2 | 单节点凑不到，需要第二个节点投票 |
| 12 | 8 | 主网形状 |

**结论：单节点 devnet 只有 `PRODUCERS=1` 有可能出块。**
`PRODUCERS=4` 在单节点上永远不可能达到 2 票，而 DPoS 网络层（`--pbft.net.*`、
`pbft.Transport` 的实现类）在本树里还没有实现——`SetTransport` 除定义外零调用点——
所以今天不存在"多节点互相投票"的运行形态。

`PRODUCERS=4 ./run.sh init` 仍然可以做：它产出 4 个 keystore 和 4 公钥的 genesis，
留给将来多节点 / 单测验证用。默认值取 1，是因为它是**唯一**能在单机上走完一轮的形状
（`consensus/pbft/transport.go` 的注释也把"单生产者测试链"列为受支持形态）。

⚠️ 阈值 0 是**上游 pgp-chain 的原样行为**，不是本移植引入的（`confirm.go` 的注释写得很清楚：
"one producer needs zero, which means a one-member producer set accepts a vote-less confirm.
That looks like a bug and is the reference behaviour"）。所以 `PRODUCERS=1` 的链只适合
devnet / 集成测试，**不要**拿它当生产配置。

## 6. chain id 为什么必须是 8606211

`core/genesis.go` 的 `checkPGPDatabaseMatch` 会在**启动时**（不是 `geth init` 时）检查：
如果 chain id 等于 pgp 主网的 `860621`，而 datadir 里的 genesis 不是主网 genesis，就
`Fatal` 拒绝启动。这是防止"把 pgp 配置指到一个 BSC datadir"的安全网。

实测报错：

```
Fatal: Failed to register the Ethereum service: database was initialised for a different chain:
stored genesis is 0x3ff99d0a…, pgp-chain mainnet genesis is 0xaab1ef44…;
point --datadir at a pgp-chain data directory or wipe this one with 'geth removedb'
```

所以模板里写 `8606211`。它只影响交易签名域，不改变任何 legacy 规则
（`IsInBSC()` 看的是 `Parlia != nil`，不看 chain id）。

## 已验证：单节点 devnet 能连续出块（原「已知阻塞」已修复）

**结论：能出块，已实测。** 曾经挡住高度 1 的那处 `seal.go` 顺序问题已经修好，
本文这一段从"已知阻塞"改写为"已验证"，下面同时保留它的历史形状，因为它是最好的排查样本。

### 实测数据

采证基线：`seal.go = 50912286a2ca25e05f6164508f276262439778b1`
（= `b5861d7b4` 的版本）；二进制单独构建在 `build/bin/geth-aqs`，构建前后源码哈希逐字节一致。

| 判据 | 实测 |
|---|---|
| 高度连涨 | 同一进程内 1→6 连续，之后每次采样都在涨；本次采证最高 **tip 130** |
| 块间隔 | 相邻块 `ts` 差全为 **3.000s**（`status` 同步打印 `interval=3s (expected 3)`） |
| `extra` 长度 | **0x113 = 275 字节** = `143 + 132×1`，全样本一致，`votes=1` |
| `seal timed out waiting for a confirm` | **0** |
| `height already has a confirm`（`ErrAlreadyConfirmedBlock`） | **0** |
| `confirm channel busy` | **0** |
| 封块总数 | 164 次（跨 8 次启动、6389 行日志），无一次超时 |
| 单进程最长连续出块 | **45 块** |
| `status` 分支 | 走到 "≥1 block" 并打印 `PASS: the chain is advancing` |
| 优雅关停 | 日志 `Blockchain stopped`，`stopped in 4~5s` |

### 修复位（现在的代码长这样）

`consensus/pbft/seal.go` 里 `drainRoundChannels()` + `atomic.StoreInt32(&e.isSealOver, 0)`
被提到了本轮**任何能产出 confirm 的语句之前**：`ProcessProposal` 之后、`AcceptProposal`
（自投票）之前。要守住的不变量一句话：

> drain 必须在所有能为本轮产出 confirm 的语句之上，`BroadcastProposal` 必须在它之下。

把广播提到 drain 之前就会复现原 bug：对端可以在 `isSealing=1` 到 drain 之间确认，
那份 confirm 会被 drain 吃掉。`Seal` 全程只有两处 drain（这里和 `finishRound`）。

### 历史形状（排查样本，不是现状）

当时的现象是节点能起、能进轮次，日志里出现

```
INFO  Collect majority signs. Proposal confirmed.       <- confirm 造出来了
INFO  Block confirmed.error<nil>
WARN  PBFT: seal timed out waiting for a confirm height=1   <- 6 秒后
WARN  PBFT: height already has a confirm, refusing to propose height=1
```

然后 `eth_blockNumber` 永远是 0。重启没用：block pool 是内存态，重启后仍卡在同一处。

旧根因（行号是**修复前**的，只作对照）：

1. `:123` `Seal` 先投出自己那一票并同步走 `dispatcher.ProcessVote`；单/双生产者阈值 ≤ 1，
   这一票当场触发 `onConfirm` → `trySendConfirm` → confirm 进 `confirmCh`（缓冲 1）。
2. `:165` 的 `drainRoundChannels()` 在这之后才执行，把刚放进来的、属于本轮的 confirm 一起倒掉。
3. `:168` 的 `select` 永远等不到，6 秒后超时。
4. 而 `onConfirm` 早已 `blockPool.AppendConfirm` 记下该高度，重试时 `GetConfirmByHeight`
   命中并返回 `ErrAlreadyConfirmedBlock`，高度 1 永久卡死。

这个 bug 之所以没被测出来：`seal_test.go` 原有用例全是 12 生产者 + 对端投票，
绕开了"自己那一票就够"的路径。修完之后补的 `TestSingleProducerSealsInOneRound`
（`producers=1` / `producers=2` 两个子用例）就是钉这条路径的。

### 但有一件事要跟"链是不是好的"分开看：SIGKILL 之后的回退

我采证时所在的执行环境会在几十秒后**硬杀进程（SIGKILL）**，于是观测到一个现象：
重启后 head 回退到上一个落盘高度，同一高度被**重新产出、哈希不同**。实测片段：

```
每次启动加载的 head：0 → 13 → 13 → 13 → 45 → 62 → 62 → 107
高度 14 的 4 次 seal，4 个不同 hash（ca82f3/103466/d50391/717daf）
```

**这不是共识分叉，也不是出块能力有问题**，是 R12 那条"关机不能中断进行中的 Seal"
在本 devnet 上的可观测形态：进程被 SIGKILL 时，最后几个块还没落盘，重启就从未落盘处继续，
于是同样的高度会被合法地再产一次。

由此得出两条使用纪律：

1. **验"连涨 ≥3 高度"必须在同一次不间断运行里采。** 跨重启把数字拼起来会看到重复高度，
   很容易误判成"链在回滚"。
2. **要停就 `./run.sh stop`（SIGTERM），别 `kill -9`。** 优雅关停会干净落盘，
   `stop` 在 4~5 秒内返回；硬杀则会丢最后几个块并触发上面的重复高度。

## 关停可能要多等几秒（不是挂死）

`miner.Close()` 在 `engine.Close()` 之前，所以一个**正卡在一轮里**的生产节点收到 SIGTERM
后，要等这一轮收尾（最长一个 `signTolerance` = 2 × 3s = 6s）才退出。

实测：出一轮中 `stop` → `stopped in 5s`；不在轮次里 → `stopped in 2s`。
`./run.sh stop` 默认最多等 30 秒（`STOP_TIMEOUT`）再报"还活着"。
**不要**在这 6 秒内 `kill -9`，那会让 leveldb 少一次干净落盘。

## 日志里会看到、但属于正常工作的几行

排查时先把这几行认出来，别当成故障。

### `WARN FinishedProposal received fork block height N`

**它是我们自己的出块路径在正常工作，不是错误。**

机理在 `dpos/dispatcher.go:256-261` 的水位守卫：`if height <= d.finishedHeight { return }`。
链头追平之后，同一个高度再被喂进来就会被这里短路，并打这一行。

所以计数会比高度数大：实测 164 次封块时这条 WARN 出现 140 次左右（数量随重启/回退变化）。
`WARN 数 > 高度数` 是因为快照重放与回退窗口会让同一高度被重复喂几次，**不是分叉**。

**这条同时是 M3 的判据，请一并记下来：**

- **M3 上线后这行 WARN 应当降到接近 0。** 那时会有别的生产者的块进来，水位守卫不再短路。
- 但"WARN 消失"**不等于**"一切变好"：它恰好是 R19 暴露面从 0 变成"每块一次"的信号
  （`CleanProposals` 重建票数 map 与 `processVote` 无锁写同一批 map 的竞态）。
  所以 **`#13` 必须在 M3 之前落地**。
- 看到这里的人不要因为 WARN 没了就以为问题解决了；先确认 `#13` 的状态。

### `WARN pgp: legacy ELA precompile set incomplete`

12 个 ELA 跨链预编译里 11 个要等 SPV 注册。空块不触发任何预编译，**devnet 上可以忽略**。

### `WARN Unknown config environment variable envvar=...`

geth 会把环境里**所有 `GETH_*`** 当作配置覆盖项。`run.sh` 早先自己导出的 `GETH_BIN`
就撞上过这条（已在 `start` 里 `unset`）。如果你在自己的环境里看到它指向别的变量，
先 `env | grep ^GETH_` 看看是不是有杂散变量会影响节点配置。

## 安全注意

1. **keystore 就是私钥**。`keygen` 写文件用 `0600`，目录 `0700`，`run.sh` 每次 `init`
   都会再 `chmod` 一遍。别把它 `cp` 到共享目录、别塞进 git。
2. **密码永远不进仓库、不进 keygen 产出的文件**。用 `PGP_DEVNET_PASSWORD`
   或 `PGP_DEVNET_PASSWORD_FILE`（后者建议 `chmod 400`）。
   - 别写成 `export PGP_DEVNET_PASSWORD=xxx` 放在仓库里的脚本中。
   - 想让它不进 shell history：`read -rs PGP_DEVNET_PASSWORD && export PGP_DEVNET_PASSWORD`。
3. **密码怎么进 geth**。`pbft.Config.KeyStorePassword` 被标成 `toml:"-"`（见
   `consensus/pbft/config.go`），也就是**故意**排除在 `--config` 和 `geth dumpconfig`
   之外——能被从磁盘文件里读回来的密码不算秘密。所以只有两条路：
   - `GETH_PBFT_KEYSTORE_PASSWORD` 环境变量（**默认走这条**）；
   - `--pbft.keystore.password` 命令行参数（本机任何用户都能 `ps` 看到）。
   `run.sh` 会先 `"$GETH_BIN" --help | grep GETH_PBFT_KEYSTORE_PASSWORD` 判断这个二进制
   认不认环境变量：认就用环境变量（**密码既不进 argv 也不落盘**），不认才退回命令行并
   打警告（否则节点会静默降级成只验证不出块）。`ARGV_PASSWORD=1` 可以强制用命令行。
   实测：走环境变量启动后 `ps` 里 grep 不到密码，日志有
   `Config environment variable found envvar=GETH_PBFT_KEYSTORE_PASSWORD`，
   且仍能签名出 confirm（说明 keystore 真的被解开了）。
4. **`PRODUCERS=1` 的链没有拜占庭容错**（阈值 0，见 §5）。它只是跑通链路的脚手架。
5. **测试账户的私钥是公开的**：genesis 给 `0x71562b71999873db5b286df957af199ec94617f7`
   打了余额，这个地址对应 geth 测试里公开的私钥
   `b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291`。
   任何知道它的人都能花掉这些币——**只用于 devnet**。
6. genesis 里 `legacy.gasFeeRecipient` 指向上面的测试地址、`gasFeeRecipientRatio=65`
   （对齐主网费率形状）。空块不产生手续费，所以现在没有任何资金流动。
7. **`[Metrics]` 段的 InfluxDB 凭据会被 `geth dumpconfig` 原样打印出来 —— 这是已知缺陷，本轮并未修复。**
   细节与规避见下面一节。不要因为 `verify-secrets` 通过就以为 `dumpconfig` 里没有秘密了
   —— 那道闸门**只管 PBFT keystore 密码**，不管 Metrics。

### 已知同类隐患：`[Metrics]` 的 InfluxDB 密码/令牌会落进 `dumpconfig`（**当前未修**）

这是和 `KeyStorePassword` 同一族的缺陷，但没有修，原因见下。

**现状（实测，当前二进制）**：`metrics/config.go` 里有两个承载凭据的字段带着
`toml:",omitempty"`：

```go
InfluxDBPassword string `toml:",omitempty"`   // :31
InfluxDBToken    string `toml:",omitempty"`   // :36
```

`omitempty` 只在**空值**时省略，而两者默认值都是非空占位符 `"test"`
（`metrics/config.go:51` / `:57`），所以它们**总是**出现在输出里；一旦用户填了真凭据，
真凭据就被原样打出来：

```console
$ geth --metrics.influxdb --metrics.influxdb.password='influx-admin-hunter2' dumpconfig | grep -A1 InfluxDBUsername
InfluxDBUsername = "test"
InfluxDBPassword = "influx-admin-hunter2"      # ← 明文

$ geth --metrics.influxdb.token='SENTINEL-token-9f3a' dumpconfig | grep InfluxDBToken
InfluxDBToken = "SENTINEL-token-9f3a"          # ← 明文，v2 里这个等于完整认证凭据
```

更严重的是**从配置文件读进来、再从 dumpconfig 漏出去**这条往返通路，它是现成的：

```console
$ cat /tmp/influx-compat.toml
[Eth]
NetworkId = 8606211

[Metrics]
EnableInfluxDB = true
InfluxDBUsername = "alice"
InfluxDBPassword = "SENTINEL-config-file-pass-7c2d"
InfluxDBToken    = "SENTINEL-config-file-token-7c2d"

$ geth --config /tmp/influx-compat.toml dumpconfig; echo "EXIT=$?"
EXIT=0
...
InfluxDBUsername = "alice"                                  # ← 证明配置真的被读进去了
InfluxDBPassword = "SENTINEL-config-file-pass-7c2d"         # ← 又原样打出来
InfluxDBToken    = "SENTINEL-config-file-token-7c2d"        # ← 同上
```

也就是说：**配置里的凭据没有变安全，只是被搬到了 `dumpconfig` 的输出里。**
运维动作里 `geth dumpconfig > somewhere` 这种"备份配置"习惯，等于在备份明文凭据。

> 顺带记一个坑：`[Metrics]` 是 `gethConfig` 的**顶层**段，不是 `[Eth.Metrics]`。
> 写成 `[Eth.Metrics]` 会报
> `Fatal: /path/x.toml, line N: field 'Metrics' is not defined in ethconfig.Config`
> （`eth/ethconfig.Config` 里根本没有 `Metrics` 字段，它挂在 `cmd/geth` 的 `gethConfig` 上）。

受影响面比 PBFT 那个大：任何开了 `--metrics.influxdb*` 的用户。
**但本仓库里 PBFT 的 `KeyStorePassword` 已经修了，Metrics 这两个没有。**

**为什么不顺手加 `toml:"-"`**：`toml:"-"` 是**双向**的——连 `--config` 读取也一起关掉。
而上面的实测已经证明 `InfluxDBPassword` / `InfluxDBToken` 的 `--config` 用法**今天是可以用的**，
加了 tag 之后所有现存配置文件里写了这两个键的用户**升级即启动失败**。报错形态与下面这段同类
（**这一条是按同一机制推演的，未在 Metrics 字段上实测**，因为本轮没有改那个字段；
PBFT 那条是实测原文）：

```
Fatal: field corresponding to 'InfluxDBPassword' in metrics.Config cannot be set through TOML
```

PBFT 那个字段是本移植新建的、没有存量配置文件，所以能直接关。Metrics 这两个不行。

### 想修的话，能走的路只有两条（**都尚未实施**）

一个常见的误判值得先写清：**"加 `toml:"-"` + 把 `metrics.Config.InfluxDBPassword` 加进
`cmd/geth/config.go` 的 `deprecatedConfigFields`"这样组合是不成立的。**
`deprecatedConfigFields` 只对"结构体里已经没有这个字段"生效。naoina/toml 的
`fieldCache.findField`（`util.go:42-56`）先在字段表里查名字：

- 名字**查不到** → 调 `MissingField` → `deprecatedConfigFields` 可以把错误降级成
  `Config field '...' is deprecated and won't have any effect.` 的 warn；
- 名字**查到了、但 tag 是 `toml:"-"`**（`info.ignored`）→ 直接返回
  `field corresponding to ... cannot be set through TOML`，**`MissingField` 根本不会被调用**。

实测（`naoina/toml v0.1.2-0.20170918210437`，本仓库 go.sum 里的那个版本，
最小复现程序放在仓库外）：

```
== 字段仍在、tag 为 toml:"-" ==
    ERROR: line 1: field corresponding to `A' in main.Tagged cannot be set through TOML
    （MissingField 未被调用）

== 字段已从结构体删除 ==
    MissingField called: id=main.Removed.A
    ok, value=&{B:}
```

所以：

| 路径 | 做法 | 代价 | 评价 |
|---|---|---|---|
| A. 删字段 + 登记 | 从 `metrics.Config` 里**删掉**两个字段，同时把 `"metrics.Config.InfluxDBPassword"` / `"metrics.Config.InfluxDBToken"` 加进 `deprecatedConfigFields` | 老配置不再 Fatal，只 warn，但**这两个值从此读不进来**（配置静默失效，只剩一条 warn）。要还让用户能用，得给一个不带 TOML 的入口（flag / 环境变量），否则等于撤掉功能 | 可行但降级 |
| B. 只挡输出 | 保留字段与 `--config` 读取，给 `metrics.Config` 加一个 `MarshalTOML`（或包一层只用于输出的结构），序列化时丢掉这两个键 | 一个方法，读取路径零改动，`dumpconfig` 干净，**无升级中断**，功能不丢 | **推荐** |

B 之所以是正解：泄漏发生在**输出侧**，而 `toml:"-"` 是**输入侧**的开关，用输入侧的开关去堵输出侧，
就必然要牺牲兼容性。改输出侧不动输入侧，才既止漏又不破坏存量配置。

两条都**没有做**，本轮按 team-lead 的决定留给独立迁移窗口。

**本轮规避（用户侧可执行）**：

- **不要把 InfluxDB 凭据写进 `--config` 配置文件。** 用 flag 或环境变量传：
  `--metrics.influxdb.password` / `--metrics.influxdb.token`，
  或 `GETH_METRICS_INFLUXDB_PASSWORD` / `GETH_METRICS_INFLUXDB_TOKEN`。
  这不能阻止 `dumpconfig` 打印（它打印的是**合并后**的配置），但至少凭据不落在磁盘文件里。
- **`geth dumpconfig` 的输出本身就是要保护的文件。** 配了真实 InfluxDB 凭据的节点，
  它的 `dumpconfig` 输出等同于一份凭据副本，别贴进 issue、别上传到共享目录。
- 没开 `--metrics.influxdb*` 的节点泄漏的只是占位符 `"test"`，没有真实风险。

## 未验证 / 已知缺口

- `run.sh status` 的"≥1 个块"分支已经在真实链上跑通了（1..6、46→47、70→71 都采过），
  但**它是先在真实链上崩了三次才修通的**：`rpc()` 用 `"$*"` 拼参数导致
  `["0x1" false]` 不是合法 JSON、`json_get` 在 jq 下把缺失键打印成字面量 `null`
  再进算术展开、`cmd_status` 不校验字段就做算术。修法见脚本内注释。教训：
  "分支写好了"和"分支跑过"是两件事，只有出块之后才可能知道。
- 多节点（4 生产者 + 相互投票）：`pbft.Transport` 无实现，本树做不到。
- ELA SPV 服务会尝试连 ELA 主网（`startSpv` 对 legacy 链是强制的），devnet 上连不上
  任何 ELA peer，只会在日志里留下连接尝试，不影响空块出块。
