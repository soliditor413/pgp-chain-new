# PBFT devnet 启动件（运维 runbook）

这个目录是给 `--pbft.enable` 用的**可执行启动件**：Elastos 格式的 arbiter keystore、
`legacy.producers` 指向自己公钥的 genesis、以及一键 init/start/stop/status 的脚本。

> **当前状态：能启动、能进到"出块轮次"，但出不了块。**
> 原因不是配置，是 `consensus/pbft/seal.go` 里的一处顺序问题，见下面
> [已知阻塞](#已知阻塞必须改代码不是配置问题)。本目录的所有件都已实测跑通到
> "PBFT 轮次真的跑起来"这一步，卡的最后一环在代码里。

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

## 已知阻塞（必须改代码，不是配置问题）

**症状**：节点能起、能进轮次、日志里出现

```
INFO  Collect majority signs. Proposal confirmed.       <- confirm 造出来了
INFO  Block confirmed.error<nil>
WARN  PBFT: seal timed out waiting for a confirm height=1   <- 6 秒后
WARN  PBFT: height already has a confirm, refusing to propose height=1
```

然后 `eth_blockNumber` 永远是 0。

重启没有用：block pool 是内存态，重启后节点会重新尝试高度 1，仍然在同一处超时。
44 秒、66 秒的两次观测都是 `seal timed out`，而 4 生产者形状下更糟——6 秒后 view
轮转到下一个（不存在的）生产者，本节点连提议权都没了。

**根因**（`consensus/pbft/seal.go`，行号对当前工作树）：

1. `:123` `Seal` 先投出自己那一票，并同步走 `dispatcher.ProcessVote`；
   单/双生产者时阈值 ≤ 1，这一票当场就触发 `onConfirm` → `trySendConfirm`
   → 把 confirm 放进 `confirmCh`（缓冲 1）。
2. `:165` 的 `e.drainRoundChannels()` 在**这之后**才执行，它的职责是清掉上一轮的残留，
   但它会**把刚放进去的、属于这一轮的 confirm 一起倒掉**。
3. `:168` 的 `select` 于是永远等不到 confirm，6 秒后超时。
4. 而 `onConfirm` 早先已经 `blockPool.AppendConfirm` 把这个高度的 confirm 记下了，
   所以重试时 `:74` 的 `GetConfirmByHeight` 命中，返回 `ErrAlreadyConfirmedBlock`——
   高度 1 被永久卡住。

**最小修复方向（本次没有改，留给代码负责人）**：把 `:165` 的
`drainRoundChannels()` + `atomic.StoreInt32(&e.isSealOver, 0)` 提到本轮
**任何投票之前**（例如 `:88` `blockPool.AddToBlockMap` 之前，或 `:59` 进门处），
语义不变（跨轮残留照样被清；`select` 前还有 `:177` 的 seal hash 比对兜底），
但不会再吞掉本轮的 confirm。

**修复后怎么验收**：`PRODUCERS=1 ./run.sh init && ./run.sh start`，
`./run.sh status` 应在 3 秒内看到 `block 1 ... extra=275B votes=1`，
之后每 3 秒一个块。建议同时加一个 `consensus/pbft` 内的单生产者单测——现有
`seal_test.go` 全部用 12 生产者 + 对端投票，绕开了这条路径，所以这个 bug 才没被测出来。

## 关停可能要多等几秒（不是挂死）

`miner.Close()` 在 `engine.Close()` 之前，所以一个**正卡在一轮里**的生产节点收到 SIGTERM
后，要等这一轮收尾（最长一个 `signTolerance` = 2 × 3s = 6s）才退出。

实测：出一轮中 `stop` → `stopped in 5s`；不在轮次里 → `stopped in 2s`。
`./run.sh stop` 默认最多等 30 秒（`STOP_TIMEOUT`）再报"还活着"。
**不要**在这 6 秒内 `kill -9`，那会让 leveldb 少一次干净落盘。

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

PBFT 那个字段是本移植新建的、没有存量配置文件，所以能直接关。Metrics 这两个不行：
正确的做法是一次迁移（保留 TOML 读取并打 deprecated 警告，同时让 `dumpconfig` 不输出），
gencodec 的单个 tag 表达不了这件事，得手写 `MarshalTOML` 或改成输出侧过滤。那是独立工作量。

**本轮规避（用户侧可执行）**：

- **不要把 InfluxDB 凭据写进 `--config` 配置文件。** 用 flag 或环境变量传：
  `--metrics.influxdb.password` / `--metrics.influxdb.token`，
  或 `GETH_METRICS_INFLUXDB_PASSWORD` / `GETH_METRICS_INFLUXDB_TOKEN`。
  这不能阻止 `dumpconfig` 打印（它打印的是**合并后**的配置），但至少凭据不落在磁盘文件里。
- **`geth dumpconfig` 的输出本身就是要保护的文件。** 配了真实 InfluxDB 凭据的节点，
  它的 `dumpconfig` 输出等同于一份凭据副本，别贴进 issue、别上传到共享目录。
- 没开 `--metrics.influxdb*` 的节点泄漏的只是占位符 `"test"`，没有真实风险。

## 未验证 / 已知缺口

- `run.sh status` 的"≥1 个块"分支（extra 长度反算、间隔计算）**没有在真实链上跑过**，
  因为当前代码出不了块。代码路径已写好，修复后请重点看这一段。
- 多节点（4 生产者 + 相互投票）：`pbft.Transport` 无实现，本树做不到。
- ELA SPV 服务会尝试连 ELA 主网（`startSpv` 对 legacy 链是强制的），devnet 上连不上
  任何 ELA peer，只会在日志里留下连接尝试，不影响空块出块。
