# PGP 测试链验证报告：PBFT→POSA、质押选举、双签/掉线罚没

执行时间：2026-11-11（Asia/Shanghai）
被测二进制：`bsc/build/bin/geth`（本仓库 HEAD `7ca26d22e`）
链 ID：860621；系统合约镜像：`pgp-fork-v4`

---

## 0. 结论摘要

| 用例 | 结果 | 关键证据 |
|---|---|---|
| T1 PBFT→POSA 平滑交接 | **PASS（带时间相位偏差）** | 6 个 legacy 生产者全部参与；block 39→40 extra 671B→97B；6 节点 hash 一致；0x1000/0x2002 代码长度 28917/47087（Run A 时点；加 §8.5 补丁后 0x1000 为 29031） |
| T2 质押注册 | **PASS** | 6/6 `createValidator` 成功；票力与 BLS 票地址与 genesis 一致；0x66 预演返回 `0x01` |
| T3 选举与 top-N 切换 | **PASS** | epoch 200 携带 5 席（V5 出局）；203 后 V5 仅出 203 一块；加质押后 epoch 400 换成 V5，402 后 V4 停产 |
| T4 双签罚没 | **PASS（修复 `SystemReward.doInit` 后重跑）** | 0x68 返回 52B signer/height；block 71 一次成功：烧 200 ETH、jail 30 天、驱逐、奖励举报人 5 ETH；重复证据 → `AlreadySlashed` |
| T5 掉线升级到 felony | **PASS** | 50 分轻罪（block 253）；150 分重罪（block 653）：jail 2.0 天、烧 10 ETH、驱逐、链继续 3 席出块 |
| T6 换一批人做 POSA 验证人 | **PASS** | 1..39 只由 4 个 PBFT 生产者出块；40..115 只由 4 个全新 POSA 验证人出块，各 19 块，零交叉；8 节点 hash 全一致 |
| P0 StakeHub 空集合停链 | **已修复并回归（A+C）** | 修复前 block 81 组装失败、链停；修复后同一场景 breathe 块 **89** 正常出块（gas 24,528，集合不变），链继续到 218+；随后注册，breathe 块 **189** gas 1,344,532 正常换集合（§8.5） |

一句话：**分叉、质押、选举、集合切换、双签与掉线罚没全部按设计工作；过程中发现并修复了一个会导致永久停链的 P0（空 StakeHub 跨分叉 breathe 块），修复已通过对照回归。**

---

## 1. 环境与参数

| 项 | 值 |
|---|---|
| 平台 | macOS Intel / Go 1.25.0 / Foundry cast 1.x / Python 3.13 |
| 测试根目录 | `<workspace>/.devnet/<run>`（`$HOME` 下新建目录的 rename 被本机沙箱限制，见 §7.4） |
| 端口 | devp2p 30300+i，HTTP 18560+i，DPoS 20679+i |
| 共识参数 | epoch=200 块，块间隔 3s，turnLength=1，`minerHistoryCheckLen=(N/2+1)-1` |
| 罚没参数 | misdemeanor=50，felony=150，`clean()` 每次 -37 |

四套 run 的实际参数：

| Run | 节点/席 | cabinets | 角色划分 | F/D | Feynman | breathe | T0 | 首次 breathe |
|---|---|---|---|---|---|---|---|---|
| A | 6/6 | 6 | 同一批 6 人 | 40/20 | 0/0 | 300s | 1794367500 | **89**（非 100） |
| B | 6/6 | 5 | 同一批 6 人 | 40/20 | 0/0 | 300s | 1794368100 | 87 |
| C | 6/6 | 6 | 同一批 6 人 | 40/20 | 0/0 | 300s | 1794369600 | — |
| C2 | 6/6 | 6 | 同一批 6 人（重跑 T4） | 40/20 | 0/0 | 300s | — | — |
| D | 4/4 | 4 | 同一批 4 人 | 40/20 | 0/0 | 2592000s | 1794370009 | 无（窗口内） |
| **E** | **8/4+4** | **4** | **PBFT 生产者 node0-3 / POSA 验证人 node4-7，完全不相交** | 40/20 | 0/0 | 300s | 1794381000 | **88**（=T0+300） |

> 首次 breathe 落在 89/87 而不是 100，是因为 6 个 legacy 生产者启动时 DPoS 收敛花了约 30s，第 1 块的时间被 `now` 顶掉，相位整体前移。属于方案 §6 预期的偏差，判定按实测高度记录。Run E 把 DPoS 地址簿预置到 `<datadir>/geth/network/dpos/peers.json` 后收敛几乎是瞬时的，breathe 精确落在 T0+300 对应的块 88。

---

## 2. T1 分叉平滑交接（Run A）

| 观测点 | 实测 | 判定 |
|---|---|---|
| legacy 1..39 出块者 | 6 个不同地址，9/11/8/4/3/4 块 | PASS |
| block 39 extra | 671 B（PBFT confirm 包） | PASS |
| block 40 extra | **97 B**（32 vanity + 65 seal） | PASS |
| Parlia 40..76 出块者 | 6 个不同地址，6/6/6/6/7/6 块 | PASS |
| 0x1000 / 0x2002 代码 | 28917 B / 47087 B（Run A 时点，与镜像 manifest 一致；§8.5 补丁后 0x1000 为 29031 B） | PASS |
| 6 节点 hash @40/89/105 | 完全一致 | PASS |
| router 日志 | `Created pgp-chain height router forkHeight=40 parlia=true producing=true` | PASS |
| 首个 breathe（89） | 含 1 笔 `to=0x1000 selector=0x1e4c1524` 的系统交易，gasUsed 2,007,996 | PASS |

---

## 3. T2 质押注册（Run B/C/D 公共前置）

以 Run B 为例：6 笔 `createValidator` 全部 `status=1`，每笔附带 48B BLS 票地址与 96B ownership proof，提交前 0x66 预演全部返回 `0x01`。

注册后的 `getValidatorElectionInfo(0,0)`：

| 验证人 | 票力（ETH） |
|---|---|
| V0 | 2600 |
| V1 | 2500 |
| V2 | 2400 |
| V3 | 2300 |
| V4 | 2200 |
| V5 | 2100 |

`totalLength=6`，与 genesis 的 6 个验证人一一对应；`voteAddrs` 与 `<datadir>/bls/vote.addr` 逐字节一致。

---

## 4. T3 选举与 top-N 切换（Run B）

| 高度 | 实测 | 判定 |
|---|---|---|
| 87（breathe） | 系统交易 `0x1e4c1524` 出现在块体 | PASS |
| 200（epoch） | extra 解出 **5 席**：V0..V4，V5 不在列 | PASS |
| 203（切换） | 203 之后 V5 只在 203 出了 1 块（该块仍用切换前快照），204 起 0 块 | PASS |
| 403..430 | 恰好 5 个出块者，V4 **0 块**，V5 **6 块** | PASS |
| 400（epoch） | extra 解出 **5 席**：V5,V0,V1,V2,V3（V4 出局） | PASS |
| 387（breathe） | 系统交易出现，`getMiningValidators()` 立即变为 [V5,V0,V1,V2,V3] | PASS |
| 6 节点 hash @430 | 完全一致 | PASS |

结论：**质押排序 → breathe 选举 → epoch 快照切换**这条链完整工作；从改质押到出块人变化约 200+checkLen 块（~10 分钟），属协议语义。

---

## 5. T4 双签罚没：先失败，修复 `SystemReward.doInit` 后通过

### 5.1 已证明正确的部分

- 取证工具构造同一高度（54）、同一 parentHash、不同 GasLimit 的两个 header，均用目标验证人（node0）的 keystore 重签；本地五项校验（number/parentHash/不同 seal/不同 SealHash/同一 signer）全部通过。
- `0x68` 预编译直接调用返回 **52 字节**：`signer=0x3794…488d`、`height=54`，与目标验证人一致。
- 说明：证据格式、`types.SealHash` 签名域、chainId=860621 都是对的。

### 5.2 第一轮失败（Run C）

```text
cast send --unlocked --from <另一个验证人> 0x1001 \
  "submitDoubleSignEvidence(bytes,bytes)" <h1> <h2>
→ tx 0xd175e5f690f258d50e57d7e0091b588050db15b4d98a0ae6bb0027b46bf88be3
→ status 0x0，block 72，gasUsed 1,022,475
→ revert: only operator is allowed to call the method
```

`cast call` 复现同样 revert。链上状态：

| 查询 | 结果 |
|---|---|
| `SystemReward(0x1002).isOperator(0x1001)` | `false` |
| `isOperator(0x1003)` / `(0x1005)` | `false` / `false` |
| `SystemReward.alreadyInit()` | `false` |
| `0x1002` 余额 | 528,759,312,500,000 wei（≈0.00053 ETH） |

### 5.3 根因

`SlashIndicator.submitDoubleSignEvidence` 末尾无条件执行：

```solidity
ISystemReward(SYSTEM_REWARD_ADDR).claimRewards(msg.sender, amount);
```

`SystemReward.claimRewards` 的修饰器是 `doInit onlyOperator`：

- 原 `doInit` 在 `alreadyInit==false` 时只授权 `LIGHT_CLIENT_ADDR(0x1003)` 与 `INCENTIVIZE_ADDR(0x1005)`；
- 紧接着 `onlyOperator` 检查的是 **`msg.sender`，即 SlashIndicator(0x1001)**，不在集合里 → revert；
- 由于整笔交易回滚，`doInit` 写入的 `alreadyInit=true` 也一并回滚。**下一次调用重复同样的过程，永远失败。**

也就是说，这不是"需要治理加一次 operator"就能绕过的状态，而是**每次调用都从零开始、必然失败**。D 块注入只 `SetCode`、没有为 `SystemReward` 写入初始 storage（manifest 中 `deployInit=false`），是缺陷的直接来源。

同源的潜在风险：`BSCValidatorSet.distributeFinalityReward` 也会以 0x1000 身份调 `claimRewards`，当时只因为 `0x1002` 余额不足 100 ETH 被跳过；一旦余额越过阈值，**每个 epoch 块都会 revert，直接停链**。

### 5.4 修复与重跑（Run C2）

修复方式（已包含在 `bsc-genesis-contract/contracts/SystemReward.sol`）：

```solidity
modifier doInit() {
    if (!alreadyInit) {
        operators[LIGHT_CLIENT_ADDR] = true;
        operators[INCENTIVIZE_ADDR] = true;
        operators[VALIDATOR_CONTRACT_ADDR] = true;   // 0x1000，供 finality reward
        operators[SLASH_CONTRACT_ADDR] = true;       // 0x1001，供举报奖励
        numOperator = 4;
        alreadyInit = true;
    }
    _;
}
```

随后按 `scripts/pgp-image-gen.py --artifacts ../bsc-genesis-contract/out` 重建镜像（SystemReward 3224→**3309** 字节），镜像哈希重钉为
`0xbb3d09faaba6d6264768d2bb9d52e6f5073b141026ccfad405599c3c38ca9573`（`core/systemcontracts/pgp/image_test.go`），重编 geth 后重跑。（该哈希是当时的中间版本；加入 §8.5 的长度保护后已变为 `0x7a98039c…`。）

重跑结果（Run C2，目标验证人 node0，证据高度 65）：

```text
precompile 0x68 -> 52B：signer=f703a9891f84db3faf66e9783c7fed03d6cac5d7  height=0x41(65)  ✅
cast send 0x1001 submitDoubleSignEvidence(h1,h2)
-> status 0x1  block 71  gasUsed 647,724
   log 0x1002 Transfer            burn 200 ETH
   log 0x1002 receiveDeposit
   log 0x2002 ValidatorJailed
   log 0x1000 validatorFelony
   log 0x2002 ValidatorSlashed    slashType=0 (DoubleSign)
   log 0x2005 GovToken sync
   log 0x1002 -> reporter         5 ETH (= 25 ETH 奖励池的 20%)
```

| 观测点 | 实测 | 判定 |
|---|---|---|
| 罚没类型 | `ValidatorSlashed(..., 0)` = DoubleSign | PASS |
| 监禁时长 | `jailUntil - block71.timestamp = 2,592,000 s = 30.0 天` | PASS |
| 烧币数量 | victim `StakeCredit.totalPooledBNB` 2100 → **1900 ETH**（−200） | PASS |
| 驱逐 | `isCurrentValidator=false`、`jailed=true`、`getMiningValidators()` 6→**5** | PASS |
| 举报奖励 | 举报人 +5 ETH（`rewardTo`），来自 SlashIndicator 的 25 ETH 池 | PASS |
| 重复提交同一证据 | revert `AlreadySlashed` | PASS |
| 解除监禁 | `unjail` → `0x2f64097e = SelfDelegationNotEnough()`（自质押低于门槛，符合预期） | PASS |

结论：**双签取证 → 0x68 校验 → felony → 烧 200 ETH → 监禁 30 天 → 驱逐 → 奖励举报人** 整条链路在链上完整工作。

---

## 6. T5 掉线升级到 felony（Run D）

**参数**：4 席 / cabinets 4 / Feynman 0/0 / breathe 2592000s（窗口内不触发 `clean()`）。

| 观测点 | 实测 | 判定 |
|---|---|---|
| 停机 | node3 在 head≈50 停止（`stop-node`，SIGTERM + 等 LOCK 释放），其余 3 席继续出块 | PASS |
| 计数增长 | 停机后第 1 个越位块开始 +1，稳定 **1 次/4 块**（约 13s） | PASS（G-D1） |
| count=50 | **block 253** `validatorMisdemeanor(X, 1,032,000,000,000,000 wei ≈0.001032 ETH)`；计数**不清零**；未进入 maintenance（`maxNumOfMaintaining=0`） | PASS（G-D3） |
| count=100 | 按同一规则再次触发（未在停机后单独取证） | — |
| count=150 | **block 653**：`validatorFelony(X, 0)` + `ValidatorJailed(X)` + `ValidatorSlashed(X, 1794545220, 10e18, 1=DownTime)`；同一块内 `count` 归零 | PASS（G-D4） |
| 监禁时长 | `jailUntil - block653.timestamp = 172800 s = 2.0 天` | PASS |
| 驱逐 | `isCurrentValidator(X)=false`；`getMiningValidators()` 由 4 降为 **3** | PASS |
| 烧币 | victim 的 `StakeCredit.totalPooledBNB = 2090 ETH`，对照组 2100 ETH，差 **10 ETH** | PASS |
| 选举票力 | victim = **0**（jailed 强制为 0，覆盖了 2090 的池余额） | PASS |
| 链继续 | 660..697 恰好 3 个出块者（各 13 块），head 689→697 持续推进 | PASS（G-D5） |

结论：**掉线 → 50 轻罪 → 150 重罪 → 烧币 + 监禁 + 驱逐** 整条链路在链上完整工作，与合约参数（10 ETH、2 天）逐一吻合。该路径不经过 `SystemReward`，因此不受 §5 缺陷影响。

---

## 7. T6 PBFT 生产者与 POSA 验证人换成两批人（Run E）

**动机**：前几个 run 里 `legacy.producers` 和 `parliaGenesisValidators` 是同一批 6/4 人，这掩盖了一类问题——分叉时的交接可能只是"同一批人在两种引擎下继续出块"，而不是"控制权真的换了一手"。Run E 让两批人完全不相交。

**组网**：8 节点。node0-3 持 P-256 arbiter keystore，是 `legacy.producers`（PBFT 生产者，`--pbft.enable`）；node4-7 是 `parliaGenesisValidators`（`--pbft.enable=false`，纯跟随 + 分叉后出块）。cabinets=4。

| 项 | 值 |
|---|---|
| genesis `legacy.producers`（4 个 P-256 公钥） | `026893…3190`、`027be1…4d58`、`028661…99c9`、`035516…32d9` |
| genesis `legacy.parliaGenesisValidators` | `0x5d42…ab9e`、`0xd7de…9bf0`、`0xc749…7c0f`、`0xe8e2…5541`（= node4-7） |
| `parliaGenesisVotingPowers` / `Cabinets` | `[100,100,100,100]` / 4 |
| 两批人交集 | **空集** |

### 7.1 出块权交接

| 观测点 | 实测 | 判定 |
|---|---|---|
| legacy 段 block 1..39 出块者 | **恰好 4 个**：node3 12、node2 12、node1 10、node0 5；node4-7 **0 块** | PASS |
| 分叉点 | block **40**，miner `0x5d42…ab9e`（node4），该块首次出现 97B Parlia extra | PASS |
| POSA 段 block 40..115 出块者 | **恰好 4 个**：node4/5/6/7 **各 19 块**（turn 轮转严格均衡）；node0-3 **0 块** | PASS |
| 8 节点 hash @39 / @40 / @head(120) | 三次比对全部 `distinct=1` | PASS |
| `getMiningValidators()` | 就是 node4-7 四个地址，与 genesis 一致 | PASS |
| `getValidators()` / `numOfCabinets()` | 4 / 4 | PASS |
| router 日志（8 节点逐个核对） | node0-3：`forkHeight=40 parlia=true producing=true`；node4-7：同前但 `producing=false` | PASS |
| PBFT 退役日志 | node0-3：`PBFT: Parlia fork reached, PBFT machinery retired height=40 forkHeight=40 irreversible=39`；node4-7：同前但 `irreversible=0` | PASS |

### 7.2 两批人在合约层的身份也分离

| 节点 | 角色 | `isCurrentValidator` | StakeHub `createdTime` |
|---|---|---|---|
| node0-3 | legacy PBFT 生产者 | **false** | **0**（从未注册） |
| node4-7 | POSA 验证人 | **true** | 1794381174 / 177 / 180 / 183 |

分叉后才注册质押（`createValidator`，各 2100 ETH），注册后 `getValidatorElectionInfo` 返回 4 席、票力均为 2.1e21 量级。

### 7.3 首个 post-fork breathe 块

| 观测点 | 实测 | 判定 |
|---|---|---|
| breathe 高度 | **88**（timestamp = T0+300 = 1794381300，`breathe` 子命令按 `floor(t/300)` 变更判定） | PASS |
| 块体 | 1 笔系统交易 `to=0x1000 selector=0x1e4c1524`（= `updateValidatorSetV2`），from = 出块 coinbase | PASS |
| gasUsed | 1,307,839（同一函数在 6 席时是 2,007,996，随席位数线性缩放） | PASS |
| 第二个 breathe | **188**（T0+600），gasUsed 310,598——集合未变化，`doUpdateState` 被跳过，只走 `clean()` 与记账 | PASS |
| breathe 相位 | 88 → 188 → …，每 100 块（300s）一次，与 `floor(t/300)` 判定完全吻合 | PASS |
| extra | 39 → 407B（PBFT 格式）；40 → 97B（32 vanity + 65 seal）；86 起 269B（追加 172B BLS 票聚合） | PASS |
| 之后 | 89/90… 继续出块，出块者仍是那 4 人 | PASS |

结论：**控制权确实从一批人交到了另一批人手上**，legacy 段与 POSA 段各自只认自己的人，跨段越权出块为 0；路由、日志、合约状态三方一致。

### 7.4 这次交接暴露的 P0（见 §8）

Run E 第一次启动时把注册放在 breathe 块之后，结果 block 81 组装失败、全链停在 80。根因不是"换批人"，而是 §8 那条与换批人无关的缺陷——只要分叉后第一个 breathe 块时 StakeHub 为空就会触发。上面的判定是在"分叉后立刻注册"的正确操作序列下取得的。

---

## 8. 新发现 P0（已修复）：StakeHub 为空时首个 post-fork breathe 块永久停链

### 8.1 现象

Run E 第一轮：F=40，breathe=300s，T0 对齐到 300 整数倍，未注册任何质押。chain 在 block 80 停止前进。

```text
node4.log
INFO  Commit new sealing work    number=80 ...
ERROR apply message failed                     msg= err="invalid opcode: INVALID"
ERROR commitWork failed to assemble a block    number=81 err="invalid opcode: INVALID"
（此后每 10s 重试一次，永不成块）
```

**先把一件事说清楚：这次调用发生在分叉之后，不是分叉之前。** 分叉点 F=40，失败的是 block 81。三个阶段的边界互不相同，容易混：

| 阶段 | 高度 | 谁在执行 `Finalize` | 会不会调 `updateValidatorSetV2` |
|---|---|---|---|
| legacy 段 | 1..39 | PBFT（`router` 按高度分派，Parlia 不参与出块与收尾） | **不会**——Parlia 的 `Finalize` 根本没被调用 |
| Parlia 已接管、但还没跨过 breathe 边界 | 40..80 | Parlia | **不会**——`isBreatheBlock(parent.Time, header.Time)` 为 false |
| 分叉后第一个 breathe 块 | **81**（`t=1794379500`，恰好是 T0+300） | Parlia | **会**，第一次需要从 StakeHub 重算集合 → 空集合 → 停链 |

所以缺陷不在"分叉前后"这条轴上，而在**两条独立时间轴的交叉点**：
- 分叉（F=40）决定"谁来出块"；
- breathe 边界（每 300s 一个桶）决定"什么时候从 StakeHub 重算集合"。

两者之前没有任何耦合，第一个 post-fork breathe 块是它们第一次相遇。**推论**：如果分叉高度正好落在一个 breathe 边界上，那么分叉那个块自己就会触发（`IsOnFeynman` 只跳过 *Feynman* 激活块，没有为 legacy→Parlia 这次分叉做同样的跳过），停链会提前到 F 块。

顺带澄清一个容易误读的日志：legacy 段每个块都会打 `Unexpected error when getting snapshot: invalid validators bytes`（§9.1），说明 **Parlia 的快照读取路径在分叉前确实会被调到**。但那是只读路径，不产生系统交易，与本节的问题无关。

### 8.2 定位

`Consensus/parlia/parlia.go` 的 `Finalize` 在 breathe 块上依次执行 `distributeIncoming` → `distributeFinalityReward` → `updateValidatorSetV2`；前三者在无交易、Kepler 已激活时是空操作，**唯一真正上链的就是 `updateValidatorSetV2`**。

用 `debug_traceCall` 复现（给 devnet 打开 `debug` API 后）：

| 调用 | 结果 |
|---|---|
| `updateValidatorSetV2([], [], [])` | `invalid opcode: INVALID`，**吃掉全部 gas**，无返回数据 |
| `updateValidatorSetV2([0x5d42…], [100], [48B vote])` | 成功，gas 571,625，内部回调 `0x1001.clean()` |

也就是说：**空集合必然失败，非空集合正常**。

### 8.3 根因（合约侧）

`BSCValidatorSet.sol` 的 `_forceMaintainingValidatorsExit`：

```solidity
// 2. get unjailed validators from validatorSet
if (numOfFelony >= _validatorSet.length) {      // 空集合时 0 >= 0 恒成立
    unjailedValidatorSet = new Validator[](1);
    unjailedVoteAddrs = new bytes[](1);
    unjailedValidatorSet[0] = _validatorSet[0];  // 对零长数组取下标
    unjailedVoteAddrs[0] = _voteAddrs[0];
    ...
}
```

`BSCValidatorSet.sol` 的 pragma 是 **`solidity 0.6.4`**。0.6.x 把数组越界编译成 **`INVALID`(0xfe) 操作码**（不是 0.8 的 `Panic(0x32)` revert），于是表现为"无返回数据 + 烧光 gas"，在 geth 里就是 `invalid opcode: INVALID`，`Finalize` 直接返回错误，块永远组装不出来。

触发条件比"StakeHub 为空"更宽：`getTopValidatorsByVotingPower` 会过滤掉 `votingPower == 0` 的条目，所以**任何 breathe 块只要选不出非零票力的验证人**（无人质押、或全部被 jail 到票力归零、或全部票力为 0）都会停链。

### 8.4 影响

1. **永久停机，且没有链上恢复手段**（修复前的行为）：停的那个块永远组装不出来，不会自愈；只能靠协调升级/回滚状态。
2. 生产迁移的硬前置条件：分叉高度必须保证"分叉后第一个 breathe 块之前 StakeHub 里已有非零票力的验证人"。这个前置条件目前没有任何机制保证，也没有任何检查会在启动时报错。
3. 测试脚手架也踩到了：Run A/B/C 之所以没暴露，是因为注册恰好发生在首个 breathe 块之前（先 `register` 再观察 breathe），属于运气而非设计。

### 8.5 修复（已实施 A+C）

评审结论是 **A + C**（B 不单独做：效果一样但会动状态根语义，A 更彻底）。

#### A. 合约加长度保护

`bsc-genesis-contract/contracts/BSCValidatorSet.sol`，`_forceMaintainingValidatorsExit` 开头：

```solidity
if (_validatorSet.length == 0) {
    return (new Validator[](0), new bytes[](0));
}
```

`updateValidatorSetV2` 下游紧接着就是 `if (validatorSetTemp.length != 0) { doUpdateState(...) }`，所以返回空集合的效果正好是"本轮不更换集合、保留 `currentValidatorSet`"，与 `SlashIndicator.clean()` 里 `if (validators.length == 0) return;` 的写法一致。

配套的可观测性（属于 C 的一部分，不改状态）——`consensus/parlia/feynmanfork.go` 在出让集为空时打一条 Warn，否则"质押没生效"和"选举跑了但没人变化"在日志里无法区分：

```go
if len(eValidators) == 0 {
    log.Warn("validator election returned an empty set, keeping the current validator set",
        "number", header.Number, "stakeHubValidators", len(validatorItems),
        "maxElectedValidators", maxElectedValidators)
}
```

重建后：`BSCValidatorSet` deployedBytecode **28917 → 29031** B（+114），镜像
`imageHash = 0x7a98039cbc213abbee5cdd270dae999a6e5d3f7c5225e6bdb236ca0f4e53ec12`（已重钉）。链上 0x1000 代码长度实测 29031，与产物一致。

#### C. 运维闸门

`scripts/staking-devnet/run.sh` 新增两个子命令，都已固化进脚手架：

| 子命令 | 作用 |
|---|---|
| `window` | 距下一个 breathe 边界还剩多少秒/块（按最近 20 块实测块间隔推算，不假设固定 3s）。这是"还能补救的时间" |
| `check [--deep]` | 自检闸门：`StakeHub 非空` / `mining 席位数 == cabinets` / `mining 集合 == election top-N`；`--deep` 再加全部节点 hash 一致。**非 0 退出** |

另外 `register` 结尾不再只打印 election，而是**断言 `total > 0` 后自动打印剩余窗口**——注册完就跑 `window`，把"还差多少时间"摆在眼前。

#### T7 回归：故意让空 StakeHub 跨过 breathe 块

这才是这次修复的正面验证。刻意**不注册**任何质押，让第一个 post-fork breathe 块在空 StakeHub 上运行：

| 观测点 | 修复前 | 修复后（T7） |
|---|---|---|
| 第一个 post-fork breathe 块 | block 81，**组装失败** | block **89**，正常出块 |
| 该块 gasUsed | —（永不出块） | **24,528**（空集合 → 跳过 `doUpdateState`，只走 `clean()` 与记账） |
| 该块系统交易 | — | `0x1e4c1524` 成功上链，1 笔 |
| 后续出块 | 停 | 90/91… 继续，head 到 218+ |
| mining 集合 | — | 仍为 4 席（**未被清空**） |
| 日志 | `invalid opcode: INVALID` 每 10s 刷一条 | `validator election returned an empty set, keeping the current validator set number=89 stakeHubValidators=0 maxElectedValidators=45`（4 个节点各一条） |
| `check` | 不存在 | **FAIL 且退出码 1**，并打印"还剩 195s（约 65 块）" |

随后注册 → `check` 转 PASS（`stakeHubElection=4`，`mining 集合 = election top-4`，8 节点 hash 一致 @212）→ 下一个 breathe 块 **189** 正常换集合：

| breathe 块 | StakeHub | gasUsed | 系统交易 | 效果 |
|---|---|---|---|---|
| 89 | 空 | **24,528** | `0x1e4c1524` | 保留原集合 |
| 189 | 4 席 | **1,344,532** | `0x1e4c1524` | 正常 `doUpdateState` |

两个块的 gas 差（24.5k vs 1.34M）就是"跳过 / 执行 `doUpdateState`"的直接量化。

同一条链上复核 T6 的交接结论仍然成立：legacy 1..39 只由 node0-3 出块（11/11/11/6），POSA 40..218 只由 node4-7 出块（45/45/45/44），非授权 0、越界 0。

**结论：P0 已消除。** 现在即使漏做注册，链也会继续出块并保留原集合，同时日志与 `check` 都会明确报出"质押没生效"；补上注册后，下一个 breathe 块自动接管。

---

## 9. 其他观察

1. **pre-fork 快照日志噪声**：legacy 段每个块都出现一条 `Unexpected error when getting snapshot: invalid validators bytes`（Run E 实测 39~40 条/节点，覆盖 block 0..38），链未受影响。原因是某个读路径对 legacy extra 套用了 Parlia 解析；建议后续在该路径加 `IsPGPLegacy` 短路。
2. **重复投票日志**：`OnVoteAccepted: ProcessVote failed err=alreadyExistVote` 周期性出现，只在 PBFT 生产者（node0-3）上出现，属去重路径的正常噪声。
3. **`irreversible` 水位只在生产者上有值**：非生产者节点退役时打 `irreversible=0`（它们从不收集 PBFT confirm）。日志注释已说明这只是"legacy finality 结束位置"的展示，不参与决策，但**只看跟随节点日志的运维人员会误以为 legacy 段没有 finality**。
4. **gas 地板**：节点的 legacy pool 最低 tip 是 50,000,000 wei；`cast send` 默认 1 wei 会被拒，必须 `--legacy --gas-price 1gwei`。已固化进脚手架。
5. **沙箱路径**：`$HOME` 下新建目录的 `rename` 被本机沙箱拒绝（`geth account new` 报 `operation not permitted`），改用工作区内 `.devnet/` 后正常。这是执行环境限制，不是代码问题。
6. **BLS proof 命令的 flag 顺序**：`geth bls account generate-proof --datadir ... <operator> <pubkey>`，flag 必须在位置参数之前，否则 `DataDir` 退回 `~/.ethereum`，报 `BLS wallet not exists`。
7. **镜像哈希已变更两次，当前钉的是 `0x7a98039c…`**：`SystemReward.doInit` 补丁后是
   `0xbb3d09faaba6d6264768d2bb9d52e6f5073b141026ccfad405599c3c38ca9573`；
   §8.5 的 `BSCValidatorSet` 长度保护后又变成
   `0x7a98039cbc213abbee5cdd270dae999a6e5d3f7c5225e6bdb236ca0f4e53ec12`。
   `go test ./core/systemcontracts/pgp/ -run 'TestImageHashIsPinned|TestCodeHashesMatchImageFiles'` 通过；
   `TestImageProvenanceIsCommitted` 仍会失败（`sourceDirty is true`），因为 `bsc-genesis-contract` 里的
   `M contracts/SystemReward.sol` 与 `M contracts/BSCValidatorSet.sol` 还没提交——**提交之后该用例才会转绿**，不是代码问题。
8. **本轮修掉的三个脚手架缺陷**（都是"看起来正常、实际错误"的类型，值得单独立项）：
   - `multi.sh launch_node` 用 `"${arr[@]}"` 展开可能为空的参数数组。macOS 自带 bash 是 **3.2**，在 `set -u` 下空数组展开会报 `unbound variable`，导致整条 `geth` 命令在 fork 前就失败；而脚本紧接着已经写了 `geth.pid` 并打印"已启动"。Run E 第一轮 8 个节点里有 4 个 PBFT 生产者就是这样静默死掉的。统一改成 `${arr[@]+"${arr[@]}"}`，并在 `cmd_start` 里加了启动后 `kill -0` 存活校验。
   - `multi.sh init` 会 `rm -rf node*`，但没拦"节点还在跑"。运行中跑 init 会把 datadir 从活着的进程脚下抽走，进程继续在已删除的 inode 上出块并占着端口，随后 `start` 只起来一半节点、读到的是旧链。已加锁探测 + pid 探测。
   - `expect.py` 用 `bool(v)` 解析 cast 的 `--json` 输出。cast 对单个 bool 有时给 `true`、有时给 `["true"]`，而 `bool(["false"])` 也是 `True`——**`isCurrentValidator`/`jailed`/`exist` 这三个断言曾经永远成立**。已改为按值解析 `to_bool()`。同源的还有 `cabinets`（`["4"]`）和 `getValidators()`（`[["0x.."]]`）的多层包装。

---

## 10. 复现

```bash
cd bsc
go build -o build/bin/geth ./cmd/geth
go build -o build/bin/bootnode ./cmd/bootnode

# --- Run B：选举与 top-N 切换 ---
export PGP_STAKING_HOME=<workspace>/.devnet/b
./scripts/staking-devnet/run.sh init b      # 6 节点 / 5 席 / F=40 / D=20 / Feynman=0 / breathe=300
./scripts/staking-devnet/run.sh prepare     # 6 份 BLS proof + 0x66 预演材料
./scripts/staking-devnet/run.sh start       # 前台，另开终端跑下面的命令
./scripts/staking-devnet/run.sh wait 40
./scripts/staking-devnet/run.sh register    # 2600..2100
./scripts/staking-devnet/run.sh wait 205    # epoch 200 已过，203 切换
./scripts/staking-devnet/run.sh delegate 5 1000
./scripts/staking-devnet/run.sh wait 405    # epoch 400 已过，402 切换
./scripts/staking-devnet/run.sh status
./scripts/staking-devnet/run.sh stop

# --- Run E：两批人交接（4 个 PBFT 生产者 + 4 个全新 POSA 验证人） ---
export PGP_STAKING_HOME=<workspace>/.devnet/e
./scripts/staking-devnet/run.sh init e      # 8 节点 / 4 席 / F=40 / D=20 / breathe=300
./scripts/staking-devnet/run.sh prepare     # 只有 node4-7 有 BLS 票
./scripts/staking-devnet/run.sh start
./scripts/staking-devnet/run.sh wait 41
./scripts/staking-devnet/run.sh window      # 距下一个 breathe 边界还剩多少秒/块
./scripts/staking-devnet/run.sh check       # 注册前跑：应当 FAIL（election 为空）
./scripts/staking-devnet/run.sh register    # 建议在首个 post-fork breathe 块之前完成
./scripts/staking-devnet/run.sh check --deep  # 注册后跑：应 PASS，含 8 节点 hash 一致性
./scripts/staking-devnet/run.sh status
python3 scripts/staking-devnet/expect.py --port 18560 breathe 40 --interval 300
python3 scripts/staking-devnet/expect.py hashes 18560,18561,18562,18563,18564,18565,18566,18567 40
./scripts/staking-devnet/run.sh stop

# --- T7：故意不注册，验证空 StakeHub 跨过 breathe 块不再停链（§8.5） ---
export PGP_STAKING_HOME=<workspace>/.devnet/t7
./scripts/staking-devnet/run.sh init e
./scripts/staking-devnet/run.sh prepare
./scripts/staking-devnet/run.sh start        # 全程不跑 register
# 等到首个 post-fork breathe 块（head 越过它）：
#   预期：链继续出块；该块 gasUsed ≈ 24.5k；日志出现
#   "validator election returned an empty set, keeping the current validator set"
./scripts/staking-devnet/run.sh check        # 预期仍 FAIL（质押没生效），退出码 1
./scripts/staking-devnet/run.sh register     # 补做注册
./scripts/staking-devnet/run.sh check --deep # 预期 PASS
# 下一个 breathe 块：gasUsed ≈ 1.34M，正常换集合
```

> `register` 的时机在修复前是**硬约束**：分叉在 40，首个 post-fork breathe 在 T0+300 对应的块（Run E 是 88、T7 是 89），窗口内没注册完就停链。修复后（§8.5）不再是硬约束——链会保留原集合并继续出块，但仍应尽早注册，并用 `check` 确认 `election.total > 0`（否则质押与选举等于没上线）。`run.sh register` 会先做 `verify_proofs`（0x66 只在分叉后可用），所以顺序只能是 `wait 41` → `register`。
