# PGP 质押/POSA 迁移：评审结论页

评审范围：PBFT→POSA 分叉、验证人质押与选举、集合切换、双签与掉线罚没、分叉时换一批人
测试环境：自建 devnet（4–8 节点），`bsc/build/bin/geth`（HEAD `7ca26d22e`），chainId 860621
详细证据：`docs/test/staking-posa-test-report.md`（10 节，逐项实测数据）

---

## 1. 结论

**迁移主链路可用**：分叉交接、质押注册、按票力选举、epoch 快照切换、双签罚没、掉线升级到 felony，六条路径全部在链上跑通并有可复核证据。

**过程中发现 1 个会导致永久停链的 P0、2 个可观测性 P1、1 个测试基础设施缺口。** P0 是上游 BSC 继承来的潜在缺陷，被"分叉时新建 StakeHub"这种启用形态首次变成可达路径；**已按评审批准的 A+C 修复并完成对照回归（T7）**，当前状态为"漏做质押也不再停链，且会被自检和日志明确报出"。

---

## 2. 用例矩阵

| 用例 | 结论 | 一句话证据 |
|---|---|---|
| T1 PBFT→POSA 平滑交接 | PASS | block 39→40：extra 671B→97B；6 生产者全部参与；6 节点 hash 一致；0x1000/0x2002 代码长度与镜像 manifest 一致 |
| T2 质押注册 | PASS | 6/6 `createValidator` status=1；票力 2600..2100；`voteAddrs` 与 BLS 钱包逐字节一致；0x66 预演全 `0x01` |
| T3 选举与 top-N 切换 | PASS | epoch 200 携带 5 席且 V5 出局；加质押后 epoch 400 换入 V5、V4 停产，切换点 = epoch+checkLen |
| T4 双签罚没 | PASS（需 §3.2 的镜像补丁） | block 71 一次成功：烧 200 ETH、jail 30 天、驱逐、举报人 +5 ETH；重复证据 → `AlreadySlashed` |
| T5 掉线升级到 felony | PASS | 50 分→轻罪（block 253）；150 分→重罪（block 653）：jail 2.0 天、烧 10 ETH、驱逐；链以 3 席继续 |
| T6 换一批人做 POSA 验证人 | PASS | 1..39 只由 node0-3 出块；40..115 只由 node4-7 出块（各 19 块），零交叉；8 节点 hash 全一致；两批人在合约层身份互斥 |
| T7 空 StakeHub 跨 breathe（P0 回归） | **PASS（修复后）** | 修复前链停在 81；修复后同场景 breathe 块 **89** 正常出块（gas 24,528、集合不变），链续到 218+；补做注册后 breathe 块 **189** gas 1,344,532 正常换集合 |

---

## 3. 缺陷清单

### 3.1 P0：StakeHub 为空时首个 post-fork breathe 块永久停链

**现象**：F=40、breathe=300s、未注册质押 → chain 停在 block 80，此后每 10s 重试一次，永不成块。

```text
ERROR apply message failed                  msg= err="invalid opcode: INVALID"
ERROR commitWork failed to assemble a block number=81 err="invalid opcode: INVALID"
```

**根因链**（三段，全部实测确认）：

1. `feynmanfork.go` 在 breathe 块用 `getValidatorElectionInfo` + `maxElectedValidators` 算出让集，StakeHub 为空 → 出让集为空 → 打包 `updateValidatorSetV2([],[],[])`。
2. `BSCValidatorSet._forceMaintainingValidatorsExit` 里 `numOfFelony=0 >= _validatorSet.length=0` 成立，进入为"全部被 jail"准备的兜底分支，执行 `unjailedValidatorSet[0] = _validatorSet[0]` —— 对零长数组取下标。
3. `BSCValidatorSet.sol` 是 `pragma solidity 0.6.4`，0.6.x 把数组越界编译成 **`INVALID`(0xfe)**（而非 0.8 的 `Panic(0x32)` revert），于是"无返回数据 + 烧光 gas"，`Finalize` 返回错误，块永远组装不出来。

`debug_traceCall` 对照实验：

| 调用 | 结果 |
|---|---|
| `updateValidatorSetV2([], [], [])` | `invalid opcode: INVALID`，吃掉全部 gas |
| `updateValidatorSetV2([1 个验证人], [100], [48B vote])` | 成功，gas 571,625，回调 `0x1001.clean()` |

**触发面比"无人质押"更宽**：`getValidatorElectionInfo` 对 jailed 验证人返回 0 票力，`getTopValidatorsByVotingPower` 会过滤掉 0 票力条目。所以**任何选不出非零票力验证人的 breathe 块**都会停链——无人质押、全部 jail、或 `maxElectedValidators` 被治理改成 0（后者被合约拒绝，见下）。

#### BSC 主网为什么没炸（这是本节最重要的事实）

逐条核对上游代码后的结论：**BSC 有完全相同的缺陷，Go 层和合约层都没有空集合保护**（已比对 `bnb-chain/bsc` master 的 `consensus/parlia/feynmanfork.go`，`updateValidatorSetV2` 与 `getTopValidatorsByVotingPower` 逻辑与上游逐字一致，两边都没有空集合分支）。它没炸，靠的是"这种状态在 BSC 的生命周期里从未出现"：

| 保护 | 机制 | 对我们是否成立 |
|---|---|---|
| Parlia 从创世就在跑 | block 0 的 `init(bytes,uint256)` 就把 `currentValidatorSet` 填满，不存在空集合启动态 | ❌ **不成立**：我们的 Parlia 是分叉才接管，StakeHub 在 D 块才部署、分叉时是空的 |
| Feynman 激活块跳过更新 | `if (!IsOnFeynman(...))` 才做 `updateValidatorSetV2`，激活块本身不更新 | ⚠️ 部分成立：只跳过"激活那一个块"，下一个 breathe 块（BSC 是 24h 后）照样会跑 |
| 迁移中间态兜底 | `_tmpMigratedValidatorSet` / `removeTmpMigratedValidator`（现已 deprecated）用于过渡期集合不空 | ❌ **不成立**：我们的 StakeHub 是全新部署，没有可迁移的旧集合 |
| `maxElectedValidators` 不可为 0 | 初始 45；治理 setter `if (newMax == 0 \|\| newMax > 500) revert` | ✅ 成立 |
| 不可能全员 jail | jailed 的票力为 0；要全员掉线需每人连续 2 天（`downtimeJailTime = 2 days`） | ✅ 成立（实际操作不可达） |

**换句话说**：这条路径在 BSC 上是"理论可达、实际不可达"，上游默认它不会发生，于是没写保护；我们是**第一个把 StakeHub 从空开始建、又要跨过分叉的启用形态**，正好踩进这个洞。

这也说明修合约**不会**带来任何"与 BSC 行为不一致"的风险——那个分支 BSC 从来没走过。

#### 修复方案与实施结果（评审批准 A+C，已完成）

| 方案 | 改动 | 效果 | 风险 | 状态 |
|---|---|---|---|---|
| **A 合约加长度保护** | `_forceMaintainingValidatorsExit` 开头加 `if (_validatorSet.length == 0) return (new Validator[](0), new bytes[](0));`，与 `SlashIndicator.clean()` 已有的写法一致 | 本轮不换集合、保留当前验证人，链继续出块；后续有人质押后自然接管 | 需重建镜像 + 重钉 `imageHash`；改变系统合约字节（只在"本来会停链"的场景下状态根不同） | ✅ **已实施**，deployedBytecode 28917→29031，`imageHash = 0x7a98039c…` |
| **B 共识层跳过空集合** | `feynmanfork.go` 在出让集为空时跳过本次更新 | 不改合约、不改镜像 | 改变状态根（省略了一笔本该存在的系统交易），主网上线后要改需按高度 gate | ⛔ 不采用（与 A 效果重叠，且动状态根语义） |
| **C 运维闸门** | `run.sh check [--deep]` 自检 + `run.sh window` 剩余窗口 + `register` 结尾断言 `total>0` | 让"没质押就上链"这种配置错误立刻暴露，而不是等到第一个 breathe 块 | 零代码风险 | ✅ **已实施** |

配套的可观测性（属于 C，不改状态根）：`feynmanfork.go` 出让集为空时打 `log.Warn`，否则"质押没生效"与"选举跑了但没人变化"在日志里无法区分：

```text
WARN validator election returned an empty set, keeping the current validator set
     number=89 stakeHubValidators=0 maxElectedValidators=45
```

**T7 回归（故意不注册，让空 StakeHub 跨过 breathe 块）**：

| 观测点 | 修复前 | 修复后 |
|---|---|---|
| 第一个 post-fork breathe 块 | block 81 组装失败，链停 | block **89** 正常出块 |
| 该块 gasUsed | — | **24,528**（空集合 → 跳过 `doUpdateState`） |
| 后续 | 停 | 90/91… 继续，head 到 218+；mining 仍 4 席未被清空 |
| `check` | 不存在 | **FAIL、退出码 1**，打印"还剩 195s（约 65 块）" |
| 补注册后 | — | `check` 转 PASS；breathe 块 **189** gas **1,344,532** 正常换集合 |

两个 breathe 块的 gas 差（24.5k vs 1.34M）就是"跳过 / 执行 `doUpdateState`"的量化证据。

### 3.2 P0 关联：`SystemReward.doInit` 缺 operator（已修，需确保进镜像）

第一轮 T4 失败的根因：`claimRewards` 的 `doInit onlyOperator` 只授权 0x1003/0x1005，而 `msg.sender` 是 SlashIndicator(0x1001)；整笔回滚又把 `alreadyInit=true` 一起撤销，**每次调用都从零开始、必然失败**。同源风险：`BSCValidatorSet.distributeFinalityReward` 也以 0x1000 身份调 `claimRewards`，一旦 0x1002 余额越过 100 ETH，每个 epoch 块都会 revert → 停链。

已在 `SystemReward.sol` 补上 0x1000/0x1001 授权（`numOperator=4`），SystemReward 3224→3309B。当前镜像 `imageHash`
`0x7a98039cbc213abbee5cdd270dae999a6e5d3f7c5225e6bdb236ca0f4e53ec12` 已包含这处修复与 §3.1 的长度保护。

**上线前置**：这个补丁必须进最终镜像，否则双签罚没不可用、且 finality 奖励一旦达标就停链。

### 3.3 P1：可观测性

| 编号 | 问题 | 影响 | 建议 |
|---|---|---|---|
| P1-1 | legacy 段每个块都打一条 `Unexpected error when getting snapshot: invalid validators bytes`（实测 39–40 条/节点） | 真问题会被噪声淹没；也让人误以为 legacy 段状态有问题 | 在 snapshot 读路径加 `IsPGPLegacy` 短路 |
| P1-2 | 非生产者节点退役时 `irreversible=0`（只有生产者有值 = 39） | 只看跟随节点日志的运维无法判断 legacy finality 在哪结束 | 日志里区分"未参与"与"水位为 0"，或标注 producer=false |
| P1-3 | `OnVoteAccepted: ProcessVote failed err=alreadyExistVote` 周期性刷屏（仅生产者） | 噪声，但频率高（实测约 60 条/节点，约 5 分钟运行） | 降级为 Debug 或做去重限流 |

### 3.4 P2：测试基础设施缺口

| 编号 | 问题 | 影响 |
|---|---|---|
| P2-1 | `bsc-genesis-contract/test/Deployer.sol` 不调用 `init()`，`setUp()` 在数组下标处 panic（`panic: array out-of-bounds access (0x32)`） | 合约级 Foundry 用例（`--match-contract SlashIndicator` 等）跑不起来，只能靠链上集成测试覆盖 |
| P2-2 | `TestImageProvenanceIsCommitted` 失败（`sourceDirty is true`） | `bsc-genesis-contract` 的 `M contracts/SystemReward.sol` 提交后自动转绿；`TestImageHashIsPinned`、`TestCodeHashesMatchImageFiles` 已通过 |
| P2-3 | 本轮修掉 3 个"看起来正常、其实错"的脚手架缺陷 | 详见详细报告 §9.8：bash 3.2 空数组导致 geth 静默不起、`init` 不拦运行中节点、`expect.py` 用 `bool()` 解析 cast 输出导致三个断言永远成立 |

> P2-3 里的第三条值得单独强调：`bool(["false"])` 是 `True`。如果没发现，`isCurrentValidator=false`（验证人已被驱逐）这类关键断言会永远"通过"。所有布尔断言已改为按值解析。

---

## 4. 上线前置条件（checklist）

| # | 条件 | 当前状态 | 负责侧 |
|---|---|---|---|
| 1 | `SystemReward.doInit` 补丁进入最终镜像，`imageHash` 已重钉且 `TestImageHashIsPinned` 绿 | ✅ 已完成（当前镜像 `0x7a98039c…` 已含该补丁） | 合约 + 镜像 |
| 2 | `bsc-genesis-contract` 的改动提交，`TestImageProvenanceIsCommitted` 转绿 | ❌ 待提交 | 仓库 |
| 3 | P0 修复方案定版（A+C），重建镜像并重钉 hash | ✅ 已完成：长度保护 + `imageHash = 0x7a98039c…`，T7 回归通过 | 架构 |
| 4 | 分叉高度 H 与部署高度 D 满足约束：`2 <= D <= F-2`；且 **F 之后第一个 breathe 块之前完成验证人质押**（主网 breathe=24h 时窗口最坏只有 1 个块，建议要求 F 距下一个 breathe 边界 ≥2h，详见附录 A.5） | ⚠️ 设计文档里 H 仍是 `MaxUint64` 占位 | 架构 + 运维 |
| 5 | 起链自检：断言 `election.total > 0`、`mining 数量 == cabinets`、`mining 集合 == election top-N` | ✅ 已实现 `run.sh check [--deep]`，非 0 退出；`run.sh window` 给出剩余窗口 | 脚本 |
| 6 | 罚没参数与设计文档一致：misdemeanor=50、felony=150、`clean()` −37、doubleSign 200 ETH / 30 天、downtime 10 ETH / 2 天 | ✅ 实测已逐一核对 | 合约 |
| 7 | 分叉高度、`keplerTime`/`feynmanTime` 必须为 0（非 0 会在激活块重复 `initialize()` 已初始化合约 → revert → 停链） | ✅ devnet 已验证；主网配置待定 | 架构 |

---

## 5. 剩余风险与未覆盖场景

**未覆盖（按风险排序）**：

1. **非验证人越权**：伪造 seal 的块、以及"非当前集合成员提交 header"在 `VerifyHeader` 层的行为——本轮只从"出块人集合"侧面验证，没有构造敌意 header。
2. **主动退出**：验证人 `unbond` / `undelegate` 后集合如何收缩（T3 只验证了"票力变化"这条等价路径）。
3. **多验证人同时故障**：T5 只停 1 席（4 席中停 1 仍可出块）。停到 quorum 以下、以及 `maxFelonyBetweenBreatheBlock=2` 这层限制在跨 breathe 窗口时的行为没测。
4. **finality 投票失败路径**：BLS 聚合签名校验失败、票地址不匹配、attestation 缺失时的降级行为。
5. **跨链桥 / TokenHub**：本轮完全没覆盖。
6. **规模**：只在 4–8 节点、秒级 breathe 下验证过；72 席 + 24h breathe 的组合没跑过。
7. **合约级单测**：受 P2-1 阻塞，目前所有合约行为只能靠链上集成测试反推。

**已知但可接受**：

- 首次 breathe 高度会比"理论值"前移（DPoS 收敛把第 1 块的时间顶掉），Run A 落在 89 而非 100；预置 DPoS 地址簿后可精确对齐（Run E/T7 分别落在 88/89，都等于 T0+300 对应的块）。
- 从改质押到出块人变化需要 `epoch + checkLen` 块（约 10 分钟），是协议语义不是缺陷。

---

## 6. 待决策（请批准）

| # | 决策 | 选项 | 建议 |
|---|---|---|---|
| D1 | P0 修复路径 | A / B / C | ✅ **已批准 A+C 并实施完成**（见 §3.1 与 T7） |
| D2 | 是否现在补齐 P1-1/P1-2/P1-3 三条可观测性修复 | 现在做 / 上线前做 / 只记 TODO | 上线前做（都只动日志，风险低，但 P1-1 影响排障效率） |
| D3 | 下一轮测试优先级 | 见 §5 的 1–3 项 | 「非验证人越权」+「主动退出」优先，这两条直接关系经济安全 |

---

## 7. 产物

| 文件 | 内容 |
|---|---|
| `docs/test/staking-posa-test-report.md` | 完整测试报告（10 节，逐项实测数据与命令） |
| `docs/test/staking-posa-review-brief.md` | 本文件（评审结论页） |
| `docs/test/staking-posa-test-plan.md` | 经确认的测试方案 |
| `scripts/staking-devnet/` | `run.sh`（init/prepare/start/register/delegate/evidence/status/**window**/**check**）、`expect.py`（只读断言，输出 JSON，含 `hashes`/`window`）、`doublesign/`（双签取证工具）、`lib.sh` |
| `scripts/parlia-devnet/multi.sh` | 组网脚本：角色切分（`LEGACY_PRODUCERS`/`POSA_VALIDATORS`）、bootnode 播种、DPoS 地址簿预置、启动后存活校验 |
| `core/systemcontracts/pgp/image/*` + `manifest.json` | 重建后的系统合约镜像（`imageHash = 0x7a98039c…`，含 SystemReward 与 BSCValidatorSet 两处补丁） |

---

## 附录 A：BSC 的 StakeHub 数据从哪来，我们能照搬吗

评审时第一个会问的问题："BSC 跑得好好的，它那份数据是怎么来的？抄过来不就行了？"

### A.1 结论先说

**BSC 的 StakeHub 不是"初始化"出来的，是 40 个验证人自己 `createValidator` 进去的。** 它所依赖的窗口和我们完全一样（Feynman 激活块 → 下一个 breathe 块），区别只在它的 breathe 是 24 小时、而且它只需要经历这一次。

**机制可以照搬，安全性不能照搬。**

### A.2 三条证据（说明"没有自动灌数据这回事"）

| # | 事实 | 位置 |
|---|---|---|
| 1 | `initialize()` 是**无参函数**，只写 10 个参数（`maxElectedValidators=45`、`unbondPeriod=7d`、`downtimeSlashAmount=10e18`…），没有任何验证人列表入参 | `StakeHub.sol:268-282` |
| 2 | `_validatorSet` 全仓**只有一个写入点**，且只有 `.add`、没有 `.remove`——被 jail 的验证人留在集合里、只是票力置 0（这正是"全员 jail ⇒ 出让集为空"的成因） | `StakeHub.sol:370`（在 `createValidator` 内） |
| 3 | `BSCValidatorSet` 的迁移遗留 `_tmpMigratedValidatorSet` / `_tmpMigratedVoteAddrs` 标着 `@dev deprecated`，`removeTmpMigratedValidator` 函数体是 `revert("deprecated")`——现在**没有**任何迁移灌数据的通路 | `BSCValidatorSet.sol:65-66, 615-617` |

### A.3 四种"初始化"必须分开看

| 状态 | 写入点 | BSC 主网 | 本链 |
|---|---|---|---|
| `BSCValidatorSet` 初始验证人集合 | `init(bytes,uint256)` | 编译期常量（创世合约自带） | D 块的协议注入（`Legacy.SystemContractDeployHeight`） |
| Parlia 快照初始集合 | `config.Legacy.ParliaGenesisValidators` | 创世 header 的 extra | `ForkHeight-1` 的快照锚点（`pgp_validator_init.go`） |
| StakeHub 参数 | `initialize()` | Feynman 激活块的系统交易 | D 块注入的 12 个 `init` 之一（实测 `maxElectedValidators()=45`） |
| **StakeHub 验证人集合** | **只有 `createValidator`** | 验证人自己报名 | 同左，且**只能分叉后** |

前两行才是"链一启动就能出块"的原因；第四行才是 breathe 块读的那份数据。两者从来没有自动同步过——这就是 §3.1 的 P0 的来源。

### A.4 为什么"分叉前预注册"这条路走不通

`createValidator` 要验 `blsProof`，要 0x66；而 0x66 只在 `PrecompiledContractsFeynman` 里，且 `Rules()` 的**第一个 case** 就是：

```go
case rules.IsPGPLegacy:                  // num < Legacy.ForkHeight
    return pgpLegacyPrecompiledContracts()   // 12 个 ELA 预编译 + 标准集，不含 0x66/0x68
```

所以 legacy 段调用 `createValidator` 必然失败。`scripts/staking-devnet/run.sh` 里 `register` 那条 `head >= fork` 的硬闸就是这个原因，不是保守写法。

**BSC 也一样**：Feynman 激活前 `IsFeynman(num,time)=false` → 没有 0x66 → 它也注册不了。它的窗口同样是"激活块之后 → 下一个 breathe 块之前"。

### A.5 窗口宽度对比

| | breathe 间隔 | 注册窗口 | 谁保证窗口够用 |
|---|---|---|---|
| BSC 主网（Feynman，2023） | 24 h（`params.BreatheBlockInterval = 24*3600`） | 激活块 → 下一个 UTC 0 点，最长 24 h | 人工挑激活时刻 + 运营纪律，**只此一次** |
| 本链 devnet（本轮） | 300 s | F → 下一个 300s 边界，实测 40..87（48 块 ≈ 144 s） | 脚本里 `wait 41` 后立刻 `register` |
| 本链主网（若 breathe=24h） | 24 h | F → 下一个 UTC 0 点，**最坏只有 1 个块**（F 落在 23:59:57） | 目前没有任何机制保证 |

### A.6 可选路径评估

| 路径 | 可行性 | 评价 |
|---|---|---|
| 分叉前预注册 | ❌ | 0x66 在 legacy 段不存在，物理上不可能 |
| 分叉后立刻注册 | ✅ | Run E 已实测；主网窗口最长 24 h，但最坏 1 个块，属于人工保证 |
| F 选在离 breathe 边界足够远处（建议 ≥2 h） | ✅ | 零代码成本，必须写进上线检查单（§4 条件 4） |
| 合约加长度保护（§3.1 方案 A） | ✅ | **唯一真正的机制保证**，建议采纳 |
| 在 D 块把创世验证人直接写进 StakeHub | ⚠️ | 技术上可行，但要同时构造每个验证人的 `StakeCredit` 合约与份额账本（`getValidatorElectionInfo` 读 `IStakeCredit(creditContract).totalPooledBNB()`），等于把"验证人自质押"改成"创世预质押"，经济含义变了，不建议 |

### A.7 一句话给评审

BSC 的"没问题"是**运营纪律 + 一次性窗口**的结果，不是代码不变量。我们这次要跨过的形态（分叉时新建 StakeHub）BSC 从没经历过，所以照抄流程不会照抄到安全性——要么把 F 选好并写进检查单，要么把合约补上。
