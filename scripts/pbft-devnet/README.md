# PBFT devnet 启动件（运维 runbook）

这个目录是给 `--pbft.enable` 用的**可执行启动件**：Elastos 格式的 arbiter keystore、
`legacy.producers` 指向自己公钥的 genesis、以及一键 init/start/stop/status 的脚本。

> **当前状态：单节点 devnet 能稳定出块，块间隔恰为 3.000 秒（已实测 187 次封块、0 次超时）。**
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
| `gate.sh` | 判定一次**已经跑完**的 run：只读 `$PGP_DEVNET_HOME` 里的 `logs/geth.log` 与 `status-before-stop.txt`，逐条打判定。**它不启动、不停止节点** |
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

## `gate.sh`：把「怎么判断成功」和「怎么判断失败」变成一条命令

> **`gate.sh` 上线之前的每一次 devnet 运行，追溯性地全部不可作为证据。**
>
> 包括 M2 那次"42 块跑通"。那次运行确实发生过——但"发生过"和"可举证"不是一件事。
> 那次没有任何闸门看着它：没有人当时就断言首块是 1、块号连号、`FinishedProposal` 与封块
> 逐元素相同、失败标记全 0，也没有人把当时读到的数固定下来。现在回头去读那份日志，
> 读到的是**今天**的判据对**昨天**的产物的解释，而中间任何人都可以改成另一个结论。
>
> 所以引用一次 run 时的正确说法是"用 `gate.sh` 判过、退出码 0"，而不是"跑通过"。
> 这个区别在下面「为什么要有它」那四个实例里已经出现过四次：前三次是**绿的**，
> 第四次红了、但红错了原因（报了一条并不存在的故障，见第 4 条）。没有闸门看着的时候，
> 错的结论和错的诊断都不会有人当场拦住。
>
> **还有一层更细的：`gate.sh` 自己的语义变过。** 从 `fde34ff2b` 到 `27f9b2452`
> 之间，它把"读不到数"从**给出判定**改成了**停止判定**。所以凡是在 `27f9b2452` 之前
> 引用过 `gate.sh` 输出的结论，都要**重新跑一次**才能继续引用——
> 包括本节立项实测里的第 1 条。它现在多半仍然成立，但"仍然成立"也要跑一遍才成立，
> 不能由"当时是对的"推出来。**判据的小数点后一位改了，之前所有的小数都作废。**

```bash
# 跑完之后（run.sh stop 已经退出），拿这个 home 下判定
./scripts/pbft-devnet/gate.sh ~/.pgp-devnet

# 顺带判定产出这次 run 的二进制（-race 那轮必须带 --expect-race）
./scripts/pbft-devnet/gate.sh /tmp/some-home \
    --binary /tmp/bin/geth-abc123-race --expect-race

# 退出码：0 全部通过 / 1 有闸门失败或账目故障 / 2 读不到输入（没有产生判定）
```

**它不起节点。** 起/停归 `run.sh`，`gate.sh` 只读一个已经结束的 run 目录，下判定。
这条分工是硬的：一个既起节点又判定的脚本，会在"判定失败"和"跑不起来"之间分不清，
而这两件事的处置完全不同。

### 一份可以复跑的 `exit 0` 夹具（在仓库里，但不进版本库）

`.tmp-gate-run/` 是一份**真的跑过的** run 的两个文件，逐字节与原始 run 相同：

```
$ shasum -a 256 .tmp-gate-run/logs/geth.log
f1cc87eca2dc74b027f1a337c9a0df0041b9c031771f7d78f50cd7ac79c08b53
$ shasum -a 256 .tmp-gate-run/status-before-stop.txt
2263878c18acaf082449ba009b4a6a56326bb781fbd36f444b83fc5bbab8f7d6
$ ./scripts/pbft-devnet/gate.sh .tmp-gate-run ; echo "rc=$?"
   gates passed: 23   failed: 0   not applicable: 2
   gates accounted for: 25 of 25
   PASS: this run is usable as evidence
rc=0
```

**为什么只留两个文件。** 那是 `gate.sh` 唯一读的两个（137 KiB）。目录里原来还有
`chain`（64 MiB 的 leveldb）和 `keys` + `pw`（keystore 与它的密码文件）。
验证 `exit 0` 不需要它们中的任何一个，而把 keystore 和密码文件放进源码树这件事，
比它要检查的那个 bug 更糟。

**为什么它必须在 `.gitignore` 里。** 一个未跟踪目录会让 `git status` 变脏，
而那会让此后构建的每个二进制带 `vcs.modified=true`——**正好是 G8 要读的那个字段**。
夹具污染判据，这个陷阱比它看起来近。

**为什么不做一份假日志。** 造假日志去验"日志检查器"等于判据自己给自己发通行证，
比不验更坏。所以这份夹具的用途是**唯一**的：让别人能亲手把 `exit 0` 那条跑到。

### 为什么要有它，而不是照着 §3/§4 人工看一眼

因为"人工看一眼"这一步在这套代码上**已经被证伪过三次**，而且三次都是绿的：

1. **分母错，但每个数都对。** 一次 run 报出"封了 5 块 / 1:1 OK(5:5) / 间隔 3000ms / 失败标记全 0"——
   每一项都成立，错的是那个 5 **不是从块 1 起算的**（home 里已有 19 块，从第 20 块接着跑）。
   比率、间隔、失败标记全部对得上，所以它**绿得有理有据**，比一片红难对付得多。
   对策是 G2 里那条 `first sealed block != 1 → INVALID RUN`：它只看日志头，
   不依赖 RPC、不依赖墙钟，也不依赖"起始高度"这种节点还没起来时读不到的量。
2. **守卫拿不到判据就放行。** 上一版守卫的形状是"起始高度不是 0、也不是 `unknown` 才拒绝"，
   于是 `status` 解析失败和"还没有节点"合流成同一个 `unknown`，**两者都放行**。
   而后者安全、前者意味着这条守卫已经什么都看不见了。对策是 G0：读不到就 `exit 2`，
   不给 0。**读不到 ≠ 通过。**
3. **`grep -c ... | wc -l` 在语法错时读成 0。** 见下面「边界」第 1 条。
4. **`exit` 在 `$( )` 里只结束子 shell。** `count()` 早期版本用 `exit 2` 表达"读不到"，
   而它总是被写成 `n=$(count ...)`——于是退出只终止了子 shell，主流程拿着**空值**继续跑，
   最后报出来的是一个**错误的诊断**。这不是推演：一次瞬时的外部命令失败（`rc=2`）被报成
   `no 'Blockchain stopped': the log is a fragment ...`，于是读报告的人会去查日志截断，
   而日志根本没被截断。现在 `count()` 用 `return 2`，调用方一律写
   `n=$(count ...) || cannot ...`，另外 `need_num()` 在空值/非数字进入算术比较之前把它拦住。
   **判据读不到时的正确行为是"停止判定"，不是"按最坏情况猜一个原因"。**

所以 `gate.sh` 的两条硬规矩是：**每道闸门旁边印出它自己那条命令**（读者可以只信那一行，
不信这个脚本），以及**任何一步读不到数就非零退出**。

### 九道闸门、25 个判定位，各自守什么

| 闸门 | 判定条数 | 守的错误 |
|---|---|---|
| G0 输入 | 2 | 目录不存在 / 日志为空 / 快照缺失 → 直接 `exit 2`，不进判定 |
| G1 这是一份完整的 geth 日志 | 2 | 日志是片段或被 `head` 截过：要求既有 `INFO [` 行、又有 `Blockchain stopped` |
| G2 封块记账（分母） | 5 | 半路起步（首块不是 1）、块号断号、最高块 ≠ 计数、日志停在快照记录的链头之前 |
| G3 失败标记 | 6 | `DATA RACE` / `fatal error` / `panic:` / `concurrent map` / `seal timed out` / `ERROR` 必须为 0 |
| G4 `FinishedProposal` 1:1 | 3 | fork 行数与封块数不等、**或两者高度序列不是逐元素相同**、或高度和不等 |
| G5 链头快照 | 2 | `extra` 长度不是 `143 + 132*votes`、`votes < 1`、块周期与预期不符 |
| G6 间隔 | 1 | 只报告（首间隔单列 + 稳态 mean/min/max + 偏离 >30ms 的个数），**不下阈值判定** |
| G7 WARN 记账 | 2 | 每条 WARN 都落进一个打印出来的模板，且各模板计数之和等于总行数 |
| G8 二进制出处 | 2 | `--binary` 时打印 sha256 / `vcs.*` / 是否带竞争检测器；`--expect-race` 时没有检测器就失败 |
| **合计** | **25** | 这个数在脚本里是 `GATE_TOTAL`，末尾会断言；对不上就红 |

"判定条数"这一列是**契约**，不是描述：某一节少吐一条、或多吐一条，`gate.sh` 都会红。
所以这张表和脚本里的声明必须同时改——只改一边，闸门自己会发现。

G4 用的是**逐元素相同 + 高度和相等**，不是只比值。比值相等是必要条件：
两个不同的高度序列可以碰巧有相同的个数。`17` 那个样本里 `153 = 17·18/2` 这条算术
就是它唯一的交叉检验，所以这里三条都要。

### 立项实测（这就是这些闸门长这样的原因）

这一节的条目会一直加，所以标题不写数——写数就一定会过期，而过期的数字混进证据，
正是这一整节在治的病。

1. **H0 三态。** 用桩替换 `status`，把守卫那几行原样 `eval` 出来跑：
   无节点（退出 1、无输出）→ 无警告、放行；解析失败（退出 0、读不懂）→ 打 `PARSE FAILURE` 警告、
   **仍然放行**（按设计不改判定）；节点在跑（`eth_blockNumber = 0x13`）→ `REFUSING ... block 19.`
   且**退出码 4**。第三态是正控：它证明这份夹具跑的是真逻辑，不是一段什么都不做的空壳——
   否则前两态的"正确"毫无意义。**一个不能失败的闸门，看不出它是否在跑。**
2. **合成夹具那次。** 见上面「为什么要有它」第 1 条：那次报表述完整、比率 1:1、间隔精准，
   唯一的错是分母。它证明"绿得有理有据"确实会发生，而且发生时没有任何一处看起来可疑。
3. **`grep -c ... | wc -l` 的 0 读数。** 这是 `gate.sh` 里 `count()` 非要把 `rc=1` 和 `rc>1`
   分开的原因：

   ```
   $ printf 'alpha\nbeta\n' > /tmp/g.txt
   $ grep -c -- 'foo[' /tmp/g.txt ; echo "rc=$?"
   grep: bad regex 'foo[': brackets ([ ]) not balanced
   rc=2                                  ← 非法模式：什么都不打印
   $ grep -c -- 'foo[' /tmp/g.txt 2>/dev/null | wc -l
   0                                     ← 读成"零命中"
   ```

   同一个字符串换个 flag 结论就变：`'foo('` 在 BRE 下是合法字面量（`rc=1`，打印 `0`），
   在 `-E` 下是非法模式（`rc=2`，什么都不打印）。**所以"零命中"这个结论必须能区分
   "真的没有"和"这个模式根本没跑起来"**，否则它和"跑通了"看起来一模一样。
4. **敌意输入：把 `grep` 换成一个恒返回 5 的假货。** 这条比第 1 条更硬：第 1 条证明闸门**会**红，
   这一条证明闸门在"世界崩了"的时候**不会绿**。

   ```
   $ mkdir -p /tmp/fakebin && printf '#!/bin/sh\nexit 5\n' > /tmp/fakebin/grep && chmod +x /tmp/fakebin/grep
   $ ./scripts/pbft-devnet/gate.sh /tmp/fixture            ; echo "rc=$?"
   rc=1        ← 读到了数，结论是"证据不足"
   $ PATH=/tmp/fakebin:$PATH ./scripts/pbft-devnet/gate.sh /tmp/fixture ; echo "rc=$?"
   rc=2        ← 读不到数，所以"没有结论"
   ```

   **这两个退出码的区别就是这次修的东西。** 修之前假 `grep` 给的是 **0**——最坏的那一档，
   比"判错"更坏，因为它是"判了，而且通过了"。注意同一个工具要能同时给出三个答案
   （`0` 通过 / `1` 不合格 / `2` 无结论），只测前两个是测不完的。
   （这一条由 team-lead 独立执行，我在 `27f9b2452` 上复现过同样的两个退出码。）
   `count()` 的做法是：`rc=1` 当 0，`rc>1` 直接 `exit 2`。

### 闸门要数自己，而且要**按节**数

`gate.sh` 末尾会断言：每一节吐出的判定条数，等于这一节声明的条数；总数必须等于 25。
不等就红。这看起来像在给一个只在文档里成立的不变量找替身，但它不是——
**一个只在文档里成立的不变量等于没有**，而且下面这个实测说明"只断言总数"也不够。

**先说为什么不是 24。** 原来有两节的判定条数**不是固定的**：

- G5 的违规是 `while read line; do bad "$line"; done`，违一条吐一条。五十块违规会读成
  "五十道闸门失败"，而不是"一道闸门失败"。
- G6 在正常 run 上**一条判定都不吐**（只有 `info`），只有间隔有问题时才吐一到两条 `bad`。

一条走少、一条走多，**能互相抵消**。两处都改成恒定一条（明细退到 `info`），声明总数因此从 24 变成 25。

**再说为什么必须按节。** 这是量出来的，不是推的。往 `gate.sh` 里注入两个互相抵消的错误
（删掉 G7 一条判定、在 G6 加一条）：

```
$ sh /tmp/g_injB.sh /tmp/pgp-head-home ; echo "rc=$?"
   FAIL  gate accounting: section G6 emitted 2 verdict(s), 1 declared
   FAIL  gate accounting: section G7 emitted 1 verdict(s), 2 declared
   gates accounted for: 25 of 25
rc=1
```

**最后一行仍然是 `25 of 25`。** 只断言总数的话，这一行就是它会说的全部内容——
而它是人唯一会看的那一行，也是唯一还对的那一行。所以两条都留着：

| 注入 | 总数闸门 | 按节闸门 |
|---|---|---|
| 删掉 G7 一条 | 红（`24 of 25`） | 红（G7 emitted 1, 2 declared） |
| 删一条 + 加一条 | **绿（`25 of 25`）** | 红（两节各报一条） |

**账目故障不记在"闸门失败"里。** 这两件事必须分开放，否则报告会说
"2 道闸门失败"，而真实情况是**零道闸门失败、坏的是检查器**：

```
   gates passed: 23   failed: 0   not applicable: 2
   gates accounted for: 25 of 25
   accounting problems: 2 -- these are not failed gates; they mean this
   report cannot be trusted at all, which is worse than a failed gate
```

`failed: 0` 和 `accounting problems: 2` 同时出现不是矛盾，是**诊断**：它说"我查过的每一项都过了，
但我数不清我查了几项"。这时候正确动作是去修声明表，不是去查某道闸门。

所以声明表是**两份，故意不合并**：`GATE_TOTAL` 是一个字面量，`DECL_SPEC` 是每节的清单。
从清单推出总数看着更干净，但那样"清单里被删掉一行"就永远发现不了——
两份互相核对才能同时抓住"某节吐错了"和"两张表对不上"，而且后者会先报，并明说
"下面的节不是问题所在"。三例实测：

| 改了什么 | 报出来的第一句 |
|---|---|
| 把 `G0` 的计数 2 改成 3 | `the two declaration tables disagree: GATE_TOTAL says 25, the per-section list sums to 26` |
| 整行删掉 `G7` | `... the per-section list sums to 23` |
| `GATE_TOTAL` 改成 26 | `... GATE_TOTAL says 26, the per-section list sums to 25` |

### 记账检查器自己也会坏，而且坏起来像"没问题"

上面那个检查器是一个 awk 程序，它的输出写进文件、再按行读出来打 `bad`。
第一版里数组叫 `exp`——那是 awk 的**内置函数名**，程序当场语法错，
而 `acct.txt` 是空的，`if [ -s "$TMP/acct.txt" ]` 于是什么都没做。
**一次失败的检查，读起来和一次没有发现问题的检查，完全一样**，脚本照样打印
`gates accounted for: 25 of 25 / PASS`。这跟 `count()` 那条（`rc>1` 不能读成 0）
是同一条规矩，只是换了一层：现在检查器非零退出会直接变成一条 `bad`；
数组改名 `want_n`，避开内置名（详见 `docs/design/` 里 §8.3 那一节，
"一个名字在两层里都有含义时，换掉它，而不是记住它"）。

### 边界（这些它守不住，别当成守住）

1. **G3 是单边判据。** 每条标记读 0 时，既可能是"确实没有"，也可能是"字面量写错了"——
   两者输出完全一样。所以 G3 打印出每一行命令，并明确告诉你它单边；
   把字面量钉住的是**植入式夹具**：往一份正常日志里插一行真的
   `WARNING: DATA RACE`，闸门必须失败（实测退出码 1）。`strings -a <bin> | grep -cF 'WARNING: DATA RACE'`
   的阳性/阴性两侧同理：race 二进制得 1、非 race 得 0。
2. **G6 不下阈值判定。** 抖动是**用来比两轮的**，不是一条线。给一个阈值会让"慢 40ms 但没出错"
   和"慢 40ms 且开始超时"混成一档。
3. **它看不到死锁。** `-race` 只覆盖竞争，不覆盖死锁；而"`-race` 绿 + 进程不推进"恰恰是
   死锁的指纹之一。所以 `gate.sh` 判的是**这一份日志**能不能当证据，不是"这套代码没有并发问题"。
4. **它看不到没接线的路径。** 生产者集变更（epoch 边界）在本树里没有调用者，
   再跑一百轮也覆盖不到。这不是量不够，是那条路根本没接上。
5. **被 SIGKILL 时临时目录会残留——而且不在 `/tmp`。**

   ```
   $ echo "$TMPDIR"
   /var/folders/h2/.../T
   $ ls -d "${TMPDIR:-/tmp}"/pgp-gate.*
   .../T/pgp-gate.gq02XM      ← 20:48  我 kill -9 掉的
   .../T/pgp-gate.kkNRwu      ← 20:11  另一次运行被外部 SIGKILL 打断（exit 137，有记录）
   $ ls .../T/pgp-gate.gq02XM/
   c  e  seal.txt  seal_h.txt  slots.txt
   ```

   临时目录是 `mktemp -d "${TMPDIR:-/tmp}/pgp-gate.XXXXXX"`，所以**在设了 `TMPDIR` 的机器上
   它不在 `/tmp`**（macOS 默认就设）。`trap 'rm -rf "$TMP"' EXIT` 对 `SIGKILL` 无效
   （信号无法捕获），这是设计边界不是 bug。

   **数这个数有两条前置条件，缺哪一条都有一个方向会错。** 两条都在同一个傍晚真实发生过：

   | 错法 | 做法 | 得到 | 为什么错 |
   |---|---|---|---|
   | 假阴性 | 用 `/tmp/pgp-gate.*` 查 | 0 | 路径错：`TMPDIR` 已设，它不在这儿 |
   | 假阳性 | 用 `${TMPDIR:-/tmp}/pgp-gate.*` 查，但**有实例正在跑** | 1 | 时点错：那是活着的实例自己的临时目录 |

   所以查之前先确认没有实例在跑：

   ```
   $ pgrep -f 'pbft-devnet/gate\.sh' || echo "(none running)"
   (none running)
   $ ls -d "${TMPDIR:-/tmp}"/pgp-gate.* 2>/dev/null | wc -l
   0
   ```

   `pgrep -f` 匹配**完整命令行**，所以它也可能命中一个命令行里带着这个字符串的外层 shell
   （比如一条 `sh -c '... gate.sh ...'`）。看到命中先看 pid 是不是真在跑的那个，
   别把包装者当成实例。实测两侧：没有实例时 0 命中，人为起一个立刻抓到同一个 pid。

   两条前置条件合起来才说明白这件事：**一个瞬态资源的计数，两个方向都会给出一个
   "看起来很确定的整数"，而其中只有一个是结论。**"0"和"1"都是整数。

   残留里是这次 run 的中间产物（`seal.txt`、`warn.txt` 之类），所以它既是"上次崩溃现场的符文"，
   也是**一份日志片段**：下次排查看到它，先确认它属于哪个 run，别当成这次 run 的产物。

   **四条退出路径各测一次，只有第四条留东西**——这张表之前不存在，
   下面那句"只有 SIGKILL 会留"当时是读 `trap` 读出来的：

   | 情形 | 退出码 | 残留 |
   |---|---|---|
   | 正常跑（`exit 0`） | 0 | 0 → 0 干净 |
   | 闸门失败（`exit 1`） | 1 | 0 → 0 干净 |
   | 读不到输入（`exit 2`） | 2 | 0 → 0 干净 |
   | **运行中被 `kill -9`** | **137** | **0 → 1 残留** |

   判据是**跑之前数一次、跑之后数一次**，不是"跑完没看到"。前者能区分
   "这条路径干净"和"这条路径没被执行到"，后者不能。

   **清理也必须是数出来的，不能是"跑一条 `rm -rf` 就算清过"。** 实测：

   ```
   $ rm -rf "${TMPDIR:-/tmp}"/pgp-gate.NOSUCH.* ; echo "rc=$?"
   rc=0                              ← 无匹配也返回 0
   $ mkdir -p /tmp/other_tmp
   $ TMPDIR=/tmp/other_tmp sh -c 'mktemp -d "$TMPDIR/pgp-gate.XXXXXX"'
   /tmp/other_tmp/pgp-gate.fFmgDu
   $ rm -rf "${TMPDIR:-/tmp}"/pgp-gate.* ; echo "rc=$?"
   rc=0                              ← 报成功
   $ ls -d /tmp/other_tmp/pgp-gate.*
   /tmp/other_tmp/pgp-gate.fFmgDu    ← 还在
   ```

   **"清理成功"和"什么也没删"在这条命令上输出完全一样**，而且 `-f` 让无匹配也返回 0。
   这和 `count()` 那条（`rc>1` 不能读成 0）是同一条病，只是这次的落点在清理动作上。

   **但"数出前后两个数"仍然不够——判据是"归零"，不是"变化"。** 下面这个输出：

   ```
   residue: 2 -> 1        ← 只删掉一个，这是失败了一半
   ```

   在"前后计数"的写法里读起来像"清理执行了，剩下一个"，也就是**像工作量**。
   这与本文档里"锚点归零"那条是同一条规则（"`> 0` 的计数不是判据"），我在这里
   第一次写的时候只走到了"变化"这一格。**`before` 是上下文，被比较的对象是 `after`。**

   ```
   $ before=$(ls -d "${TMPDIR:-/tmp}"/pgp-gate.* 2>/dev/null | wc -l | tr -d ' ')
   $ rm -rf "${TMPDIR:-/tmp}"/pgp-gate.*
   $ after=$(ls -d "${TMPDIR:-/tmp}"/pgp-gate.* 2>/dev/null | wc -l | tr -d ' ')
   $ echo "residue: $before -> $after"
   $ [ "$after" -eq 0 ] || echo "FAIL cleanup left $after behind (was $before)"
   ```

   **能红的是最后那一行；前两行只是给人看的。** 两侧都实测过：

   ```
   residue: 2 -> 1
   FAIL cleanup left 1 behind (was 2)     ← 部分删除，判据必须红
   residue: 2 -> 0                         ← 真清干净，判据通过
   ```

   **而且"它只覆盖当前 `TMPDIR`"这件事没有干净的补救办法，不要把它写成有。**
   在别的 `TMPDIR` 下产生的残留（另一个 shell、`sudo`、`launchd`、别的用户、容器）
   对这一整段**不可见**，既数不到也删不掉——上面那个例子就是这么漏的。

   直觉的补救是"把搜索面放宽"，比如 `ls -d /tmp/pgp-gate.* /var/folders/*/*/T/pgp-gate.*`。
   **那是枚举，枚举这条路不成立**：枚举出来的根之外的那些一个都不会出现，而这和
   "列全 awk 保留名"、"列全静默退化形态"是同一个陷阱——**它的失败方式是"想不全"，
   而想不全这件事不能靠再想一遍修好。**

   能取代枚举的只有两种，**两种都还没做**：

   | 办法 | 做法 | 现状 |
   |---|---|---|
   | 生产者留地址 | `mktemp` 之后把路径追加到一个固定位置的登记文件，清理时按登记删 | 未实施 |
   | 根固定死在脚本里 | 自建一个临时根并自己负责，不继承环境的 `TMPDIR` | 未实施 |

   所以现在的准确说法是：**"当前 `TMPDIR` 下的残留数"是一个有判据的数，
   "这台机器上还有没有残留"没有判据。** 后者要等上面两条之一落地。

   打一次 SIGKILL 只要一条（macOS 没有 `timeout(1)`，用后台 + `kill`）：

   ```
   $ sh -c 'sh scripts/pbft-devnet/gate.sh .tmp-gate-run & p=$!; sleep 2; kill -9 $p'
   $ ls -d "${TMPDIR:-/tmp}"/pgp-gate.*
   ```

   为什么这条值得实测而不是照着 `trap` 推：**推出来的版本说"在 `/tmp` 里"，而它在 `$TMPDIR` 里**。
   而验证错路径给出的答案是"没有残留"，和"这个 bug 不存在"长得一模一样。
6. **`exit 2` 的时候没有账目。** G0 读不到输入、或任何 `cannot` 分支触发时，
   脚本在吐判定之前就退出了，所以末行不会有 `gates accounted for`。
   这是有意的：`exit 2` 的含义是**没有产生判定**，不是"产生了零条判定"。

### 一次截断日志里同时出现的两条信号，守的不是同一件事

拿一份正常日志截断后重跑（`head -400` 于一份 1196 行的日志），会同时看到：

```
$ head -400 /tmp/ok/logs/geth.log > /tmp/cut/logs/geth.log
$ cp /tmp/ok/status-before-stop.txt /tmp/cut/
$ ./scripts/pbft-devnet/gate.sh /tmp/cut ; echo "rc=$?"
   FAIL  no 'Blockchain stopped': the log is a fragment, the node was killed, or the file was truncated
   FAIL  log stops at block 9 but the node recorded head 42: the log is truncated
rc=1
```

**不要把它总结成"两条比一条好"。** 它们各自单独都能被至少两种原因解释：

| 信号 | 单独看时的解释集 |
|---|---|
| G1 `no 'Blockchain stopped'` | ① 进程被杀 / 文件被截 ② 这台节点压根不打印这句（字面量错） |
| G2 `log stops at block 9 but head 42` | ① run 真在中途停了 ② `number=` 的解析漏了若干行 |

两条同时出现时，解释集才收窄：**"文件被截断"这一个原因同时解释了 G1 的末行缺失和 G2 的块数不足**，
而"字面量写错"解释不了 G2（G2 不依赖那个字面量），"解析漏行"解释不了 G1。所以这一组
信号的价值不在"数量多"，而在**它们取自两层不同的东西**（文件完整性 vs 数据一致性），
并且**对同一个原因给出的是两个独立的结果**。

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

> **这个哈希现在对不上了，是正常的 —— 请按下面的方式重新判断证据是否仍然有效，不要直接重算哈希然后怀疑数据。**
> 采证之后 `seal.go` 又被改过（`adcab441a`、`7f40c26e6` 等），但那几次**只动注释**：
> `git diff b5861d7b4 HEAD -- consensus/pbft/seal.go | grep -E '^[-+]' | grep -vE '^[-+][-+]' | grep -vE '^[-+]\s*(//|$)'`
> 的输出为空，也就是说可执行代码与采证时逐字节相同。**判断标准是"代码有没有变"，不是"文件哈希等不等"**。
> 另外这份证据采于 `f7c38120c` 之前那版 `dpos/dispatcher.go`；那次只给水位守卫加了锁、逻辑没变，
> 但要让证据严格绑在 HEAD 上，最省事的办法是重跑一次而不是靠推理对齐。

下表是**一个时间点的快照**（7278 行日志那次）。封块数与日志行数会随着你继续跑而增长，
所以这里**故意保留两个快照、不收敛成一个"正确数字"**：

- 主表用**最新快照**：187 块 / 187 行 WARN / 178 个段内 `+3.000s` 间隔 / 8 处重启断口；
- 保留更早那个 **6389 行快照**（164 : 164）作为**第二样本**。

**判据是比值与分布，不是绝对值。** 两个样本在**比值**上一致（都是严格 1:1），
这比任何一个单独的数字都强；谁跑得更久谁的数字就更大，争"哪个才对"没有意义。
（间隔分布那两行 `178` / `8` 是主表这次快照的数，第二样本对应的是它自己那一组，
不必对齐，看的是"绝大多数恰为 +3s、少数是重启断口"这个形状。）

| 判据 | 实测 |
|---|---|
| `Successfully seal and write new block` | **187** 次（跨 13 次启动） |
| 相邻块 header time 差恰为 **3.000s** | **178** 次 |
| 间隔"离群"值 | **8** 处，全部是重启断口：71 / 78 / 101 / 112 / 117 / 123 / 960 / 995 秒（末尾两个是跨会话） |
| `extra` 长度 | **0x113 = 275 字节** = `143 + 132×1`，全样本一致，`votes=1` |
| `seal timed out waiting for a confirm` | **0** |
| `height already has a confirm`（`ErrAlreadyConfirmedBlock`） | **0** |
| `confirm channel busy` | **0** |
| `fatal error` / `concurrent map` / `panic:` / `SIGSEGV` | **0 / 0 / 0 / 0** |
| 单进程最长连续出块 | **45 块** |
| `status` 分支 | 走到 "≥1 block" 并打印 `PASS: the chain is advancing` |
| 优雅关停 | 日志 `Blockchain stopped`，`stopped in 3~5s` |

**不要把这些数字读成"平均 6 秒一个块"。** 178 次相邻间隔全部**恰为** 3.000 秒；
剩下 8 处是进程被杀 → 重启的断口（见下一节），把它们算进均值会得出 6 秒级的假周期。

#### 一个值得记住的签名：封块数 : `FinishedProposal received fork` = 1 : 1

三个样本都是严格相等：6389 行那次是 **164 : 164**，7278 行这次是 **187 : 187**，
`ac87aeebc` 干净 worktree 实跑那次是 **17 : 17**（高度 1..17，序列逐元素相同，和都是 153）。
三种规模、同一个比值 —— 这就是"判据是比值不是绝对值"的来历。

**而且不只是条数相等**（这一条是 QA 补的，我在当前日志上复算过）：

- 封块的高度序列与 WARN 的高度序列**逐元素相同** —— 第 k 次封块的高度 == 第 k 行 WARN 的高度；
- 两个序列的**高度和都是 12845**。

所以这 187 行 WARN 是**按高度、按顺序一一配上的**，不是"计数碰巧相等"。
`WARN 数 > 封块数` 只会在重启重放/回退窗口出现（那时序列才会错开），
`WARN 数 < 封块数` 则说明有块封出来了却没走到水位守卫 —— 那才是要查的信号。

复算命令（下面粘的是当时那份日志的实测输出）：

```console
$ L=~/.pgp-devnet/logs/geth.log
$ A=$(grep -o 'Successfully seal and write new block.*number=[0-9]*' $L | grep -o '[0-9]*$')
$ B=$(grep -oE 'fork block ?height ?[0-9]+' $L | grep -o '[0-9]*$')
$ [ "$A" = "$B" ] && echo identical
identical
$ echo "$A" | paste -sd+ - | bc; echo "$B" | paste -sd+ - | bc
12845
12845
```

第二条 grep 用的是**容错模式** `fork block ?height ?[0-9]+`，不是笔误：这行日志的格式
在 `#20` 修前修后不一样（`fork blockheight1` → `fork block height 1`），
而 `~/.pgp-devnet/logs/geth.log` 是修前那份二进制写的。**同一个模式对两种格式都能匹配**，
所以你手上无论哪份日志都能直接跑。格式为何变、什么时候变的，见「已知缺口」里的日志格式回归。

这不是巧合。它对应 `Dispatcher.FinishedProposal` 里的水位守卫（`dpos/dispatcher.go`，
锚点用那句代码、不要用行号 —— 它在 `f7c38120c` 之后从 `:256` 漂到了 `:280`）：

```go
if height <= d.finishedHeight { return }
```

本节点每封一个块，链头就被自己的块推进一步，之后 `FinishedProposal` 再喂同一个高度
必然被这行短路，于是**恰好**打一行 WARN。

这个 1:1 有两层用处：

1. **判"链是不是健康"**：出错时这个比例会先崩（比如超时未封块会让封块数落后于 WARN）。
2. **它是 R19 在链头侧"构造上不可达"的现场签名**：`CleanProposals` 与 `processVote`
   无锁争用那批票数 map 的前提是"有第二个投票者"，而单节点 devnet 的每一票都来自
   自己那一票，且与 `FinishedProposal` 同协程。1:1 说明水位守卫逐块生效、
   没有第二条投票路径被走到 —— 这比"代码上推导不可达"硬。
   （争用本身已由 `f7c38120c` 上了锁；这个签名的用途不变：它是**当时**判"这条路径没被
   走到"的现场依据，重跑时仍然可以照这个比值来看链路健康。）

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

这行日志的**格式在 `9e6880e42` 前后不一样**，两版都记在这里，因为按时间排错时你手上的日志
可能是任意一版：

```
当前格式（9e6880e42 之后，= 上游原版），照抄真实日志：
  WARN [10-29|18:01:43.917] FinishedProposal received fork block height 1
  WARN [10-29|18:01:46.002] FinishedProposal received fork block height 2

修前格式（9e6880e42 之前），同样是照抄：
  WARN [10-29|13:52:03.379] FinishedProposal received fork blockheight1
  WARN [10-29|13:52:06.001] FinishedProposal received fork blockheight2
```

两组都是真实采样，不是按源码推的：修前两行来自旧二进制那份日志，当前格式两行来自
`ac87aeebc` 上**重新构建并实跑**的一次（干净 worktree，`git worktree add`，没碰共享工作树），
同一份日志里 `seal=14 / warn=14 / seal timed out=0`。

**所以 grep 要用容错模式**：`grep -E 'fork block ?height ?[0-9]+'`。
写成 `fork blockheight` 会漏掉修复后的日志，写成 `fork block height` 会漏掉修复前的。

机理在 `Dispatcher.FinishedProposal` 的水位守卫（`dpos/dispatcher.go`，找那句
`if height <= d.finishedHeight`，别按行号找 —— 它在 `f7c38120c` 之后已经漂过一次）。
链头追平之后，同一个高度再被喂进来就会被这里短路，并打这一行。

所以它的计数**不是随机的**：实测封块数与该 WARN 数**严格 1:1**
（6389 行快照 164 : 164，7278 行快照 187 : 187；两个高度序列还逐元素相同、高度和都是 12845，
见上面「一个值得记住的签名」）。本节点每封一个块就推进一步链头，
随后同高度的 `FinishedProposal` 必然被短路，正好一行。`WARN 数 > 高度数` 的部分来自
重启后的重放/回退窗口重复喂同一高度，但**每个成功封出的块恰好一行**这一点不变。

**这条同时是 M3 的判据，请一并记下来：**

- **M3 上线后这行 WARN 应当降到接近 0。** 那时会有别的生产者的块进来，水位守卫不再短路。
- 但"WARN 消失"**不等于**"一切变好"：它恰好是 R19 暴露面从 0 变成"每块一次"的信号
  （`CleanProposals` 重建票数 map 与 `processVote` 无锁写同一批 map 的竞态）。
  代码侧 `f7c38120c` 已经把重建整段放进 `d.mu`（`CleanProposals` 里 `d.mu.Lock()` 之后
  才 `make` 那四张 map），但**任务板上的 `#13` 仍未关闭** —— 以任务板为准，不要以
  commit 标题为准。
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
   `run.sh` 的 `env_password_supported()` 会先跑 `"$GETH_BIN" --help` 并**把输出整段接住**，
   再在 bash 里判断里面有没有 `GETH_PBFT_KEYSTORE_PASSWORD`：认就用环境变量（**密码既不进
   argv 也不落盘**），不认才退回命令行并打警告（否则节点会静默降级成只验证不出块）。
   `ARGV_PASSWORD=1` 可以强制用命令行。

   这里曾经写的是 `"$GETH_BIN" --help | grep -q GETH_PBFT_KEYSTORE_PASSWORD`，
   在 `set -o pipefail` 下 `grep -q` 一匹配就退出、写端 `--help` 收到 EPIPE，
   管道整体返回 **141**，于是探测**恒假**、"不认环境变量"分支被静默选中 —— 密码回到了 argv 里。
   **改法就是"不要在不该用管道的地方用管道"**：捕获输出再用 `case` 匹配。

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

### 想修的话，只有这三条路可走（**都尚未实施**）

（这条标题曾经写"两条"，但表里一直是 A / B / B' 三条 —— 留一句在这里，是因为"数量对不上"
本身就是文档不可信的信号，而后面每一条的取舍都要靠这张表。）

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
| B. 给 `metrics.Config` 加 `MarshalTOML` | 保留字段与 `--config` 读取，自己实现序列化并丢掉这两个键 | 一个方法，但**它接手整个 `[Metrics]` 段的编码**，把"丢两个键"放大成"重写一段的序列化"，风险与收益不成比例 | 可用，但不是首选位置 |
| **B'. 在 `dumpConfig` 的 marshal 前清零副本** | `cmd/geth/config.go:394 func dumpConfig()` 里，`tomlSettings.Marshal(&cfg)`（`:403`）**之前**把副本的 `InfluxDBPassword` / `InfluxDBToken` 清成空串 | 几行；`--config` 读取路径一个字节都不用改，升级零中断 | **推荐** |

**B' 之所以是正解**：泄漏发生在**输出侧**，而 `toml:"-"` 是**输入侧**的开关 ——
用输入侧的开关去堵输出侧，必然牺牲兼容性。B' 只在出口处脱敏，入口不动。
另外两个字段都带 `,omitempty`，清成空串后**键名和值一起消失**，输出里连
`InfluxDBPassword` 这个键都不会出现（已实测：两个 flag 传空串时，`dumpconfig` 里
这两个键的 grep 计数为 0）。凭据脱敏也因此和 `KeyStorePassword` 集中在同一处思路下。

**"改副本"不是 B' 引入的新语义，这个函数本来就是这么写的**：`:395` 的
`_, cfg := makeConfigNode(ctx)` 拿到的是值，紧接着 `:398-401` 就已经在
`cfg.Eth.Genesis = nil` 上改副本，并在输出里加了一行
`# Note: this config doesn't contain the genesis block.`。
所以 B' 与它同形、不改变调用方语义；而且**那行注释值得照抄一份**
（比如 `# Note: InfluxDB credentials are not included.`）——
不然把 `dumpconfig` 当"配置备份"用的运维会以为导出的文件是完整的。
清空但不说，和泄漏一样会让人做出错误判断。

#### 为什么不是 B：B 会**静默失效**，B' 不会

这是三条路径里唯一真正区分它们的依据 —— **不是"能不能做对"，而是"做错了会不会有人知道"**。

`naoina/toml` 的 `encode.go:305` `marshaler()` 用 `rv.Interface()` 取方法集，**不做 `Addr()`**；
而 `value()`（`encode.go:238`）在 `Kind` 分支之前就先调它。叠加两件事：

- `gethConfig.Metrics` 是**值字段**（`cmd/geth/config.go:125`：`Metrics  metrics.Config`）；
- 值字段的 `rv.Interface()` 拿到的是 `metrics.Config` **值**，方法集里不含 `*Config` 的方法。

于是 B 有**两个**静默失效点，我用最小程序在这棵树的 `naoina/toml` 版本上实测了三种写法：

```
Val = REWRITTEN = true          ← 值接收者 + MarshalTOML() ([]byte, error)（Marshaler）
                                  被调用了，但返回的裸片段落在"值的位置"，形状错

[Ptr]                            ← 指针接收者
User = "u"
Pass = "SECRET-PTR"              ← 压根没被调用，明文照旧泄漏，且无任何报错

[R]                              ← 值接收者 + MarshalTOML() (interface{}, error)（MarshalerRec）
User = "u"                       ← 唯一正确的写法：渲染成表，Pass 被丢掉
```

三行的泄漏检查：`SECRET-PTR: true`（泄漏）、`SECRET-REC: false`（安全）。
也就是说 B 要同时对**接收者形态**和**实现的接口**（`Marshaler` vs `MarshalerRec`）都对，
写错任一处都不报错、只是继续泄漏。

**所以选 B' 的理由是失败的可见性**：B' 只是赋值，写错了要么编译不过、要么配套测试立刻红；
B 需要正确实现一个接口，写错了什么都不说。这与本项目反复强调的"让失败响亮"是同一条原则。

> 一个易错点：`cmd/geth/config.go` 里并没有 `dumpConfigTOML` 这个生产变量 ——
> 带这个名字的是 `eth/ethconfig/secrets_test.go:56` 里的**测试本地镜像**
> （`toml.Config` 的值，不是函数），它存在的目的就是让测试用与生产相同的
> encoder 设置。要改生产行为，改的是 `dumpConfig()`（`:394`），不是测试里的那个镜像。

**A / B / B' 三条都还没有实施**，本轮按 team-lead 的决定留给独立迁移窗口。
如果将来做 B'，闸门要跟代码一起落地（否则等于没修）：
`eth/ethconfig/secrets_test.go` 里对 `InfluxDBPassword` / `InfluxDBToken` 设哨兵值，
断言 `dumpConfigTOML.Marshal` 的输出里既没有哨兵值、也没有键名 —— **删掉清零逻辑它就会红**。
这条不是可选项：B 与 B' **都**需要它，区别只在于 B 写错时它多半还是绿的（因为泄漏路径和
正确路径的输出可能相同），B' 写错时它一定红。

这两条是**待办（可验收的条件），不是计划（意愿）**：

1. **闸门与实现同一个提交。** 先合实现后补闸门，中间那个提交就是一个"看起来修好了"的状态。
2. **闸门必须被证明咬得住。** 临时删掉清零那两行 → 测试必须红；装回 → 必须绿。
   不接受"写完就是绿的，所以它有效"——这正是 `verify-secrets` 那个 `SKIP` 分支的教训
   （`t.Skip` 形态同理：在 `go test` 里它显示 `ok`，比没有测试更糟，因为它让人以为有）。

**本轮规避（用户侧可执行）**：

- **不要把 InfluxDB 凭据写进 `--config` 配置文件。** 用 flag 或环境变量传：
  `--metrics.influxdb.password` / `--metrics.influxdb.token`，
  或 `GETH_METRICS_INFLUXDB_PASSWORD` / `GETH_METRICS_INFLUXDB_TOKEN`。
  这不能阻止 `dumpconfig` 打印（它打印的是**合并后**的配置），但至少凭据不落在磁盘文件里。
- **`geth dumpconfig` 的输出本身就是要保护的文件。** 配了真实 InfluxDB 凭据的节点，
  它的 `dumpconfig` 输出等同于一份凭据副本，别贴进 issue、别上传到共享目录。
- 没开 `--metrics.influxdb*` 的节点泄漏的只是占位符 `"test"`，没有真实风险。

## 未验证 / 已知缺口

- **`verify-secrets` 仍然保留"静默跳过"分支 —— 这是结构性缺口，待修。**
  探测到"这个二进制不读环境变量"时，它会打印
  `SKIP GETH_PBFT_KEYSTORE_PASSWORD: this binary does not read it` 并**只跑 4 条断言**，
  然后照样 `PASS`。也就是说：**任何未来的探测缺陷都能重新打开同一个洞，而闸门自己不会红。**
  这不是假设 —— 已经发生过一次（`geth --help | grep -q` 在 `set -o pipefail` 下恒返回 141，
  探测恒假，后 4 条被跳过，总体仍打 PASS；见 `run.sh` 里 `env_password_supported()` 的注释）。

  修补方向（**未实施**）：

  1. 闸门结束前统计**实际执行**的断言条数，少于期望值就 `exit` 非零，
     并打印"跑了 N 条、预期 M 条"；
  2. 确实需要对不支持 env 通道的旧二进制放行时，走**显式开关**
     （如 `ALLOW_MISSING_ENV_CHANNEL=yes`），不许隐式。

  理由与 `CONFIRM_CLEAN=yes` 同源：把"我确实知情"变成必须写下来的东西。
  在它修好之前，看到 `SKIP` 不要当成设计好的宽容 —— 当成**闸门漏了一条**。
- `run.sh status` 的"≥1 个块"分支已经在真实链上跑通了（1..6、46→47、70→71 都采过），
  但**它是先在真实链上崩了三次才修通的**：`rpc()` 用 `"$*"` 拼参数导致
  `["0x1" false]` 不是合法 JSON、`json_get` 在 jq 下把缺失键打印成字面量 `null`
  再进算术展开、`cmd_status` 不校验字段就做算术。修法见脚本内注释。教训：
  "分支写好了"和"分支跑过"是两件事，只有出块之后才可能知道。
- 多节点（4 生产者 + 相互投票）：`pbft.Transport` 无实现，本树做不到。
  规划互通/多节点测试时**别用"等 ticker 排空就自愈"带过 R18 的第二档**：
  `(now_v, now_v+30]` 这一档不会永久丢块，但 `ChangeView` 用的 5s ticker 大于 3s 的
  `blockPeriod`，排空有可能**跨过一个出槽位**。落后对端重锚之后产出的竞争块，
  **不是"碰巧也算出自己"**：`ChangeView` 里是先 `ResetView(...)`、再用
  `GetNextOnDutyProducer(v.viewOffset)` 算 `isDposOnDuty`（`dpos/consensusview.go` 的
  `ChangeView`），所以**它的 duty 判定是拿重锚后的视图算出来的**，两边从重锚那一刻起就可能
  各自合法地判出"该我出块" —— 这是系统性不同步，不是小概率巧合。这个区别决定测试怎么写：
  前者可以靠长跑碰运气，后者必须**单独造场景**验。
- ~~**日志格式回归（移植引入，`#20`）**~~ —— **已修，`9e6880e42`。** 保留这条是因为它是
  "同一句话在两个地方各写一份、然后漂开"的最好样本：
  `dpos/log.go` 的四个 helper 曾是 `glog.Warn(fmt.Sprint(a...))`，而 `fmt.Sprint` 只在
  "两个操作数都不是字符串"时才插空格（`Sprint("viewOffset", 3)` → `viewOffset3`），
  上游原版走 `fmt.Sprintln`。**约 300 个调用点被静默改掉了格式**，而 `dpos/log.go` 的注释
  恰好写着"helper 名字保持不变、便于逐行对照原版 review"——名字保住了，语义没保住。
  修法是把拼参数抽成 `sprintArgs`（`strings.TrimSuffix(fmt.Sprintln(a...), "\n")`）并单独可测。
  **付过的代价**：修前修后的日志格式不同，任何按原文 grep 的东西都会一半失配，
  包括本文的证据行（我因此改了两版）。所以本文的 grep 一律用容错模式
  `fork block ?height ?[0-9]+`；你手上的旧日志多半还是连体格式，别以为是数据坏了。
- ELA SPV 服务会尝试连 ELA 主网（`startSpv` 对 legacy 链是强制的），devnet 上连不上
  任何 ELA peer，只会在日志里留下连接尝试，不影响空块出块。
