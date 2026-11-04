# PGP 链系统合约部署方案（不依赖创世 alloc）

| 项 | 值 |
|---|---|
| 状态 | **v2 设计评审中**（v1 因 3 处事实错误被否，见修订记录） |
| 适用仓库 | `bsc`（BSC v1.8.1 fork，`Legacy.ForkHeight` 前 PBFT、之后 Parlia） |
| 合约源码仓库 | `bsc-genesis-contract`（本地同目录，pgp 分支） |
| 关联文档 | `docs/design/parlia-fork-design.md`（下称 PF 文档） |
| 一句话结论 | 代码与初始状态只能由协议在部署块 D 的块首直接写入；但**字节码必须从 pgp 分支源码重新编译**（仓库现有 embed 与源码脱节），且初始化应统一走协议内 EVM 调用，而不是手种存储 |

## 修订记录

| 版本 | 变更 |
|---|---|
| v1 | 初稿。选了 S3（零窗口）方向，但字节码来源、`alreadyInit`、ABI 路径三处判断错误 |
| **v2** | 按设计评审重写 §5.2 / §6 / §7；新增 §7.5（StakeHub 经济闭环）、R15–R17；修正 D/F 约束矛盾；补充边界情况 |

**v1 被证伪的三条（均已独立复核）**：

| 原断言 | 事实 |
|---|---|
| `jenner.MainnetValidatorContract` 存在 | ❌ `core/systemcontracts/jenner/mainnet/` 下只有 `PaymentLaneContract`；17 个合约在 `pascal/mainnet/` |
| 现有 embed 可响应 `init(bytes,uint256)` | ❌ 全仓 `grep 631663df00` 在 embed 中**零命中**，只有 `63e1c7392a`（`init()`）。源码 `BSCValidatorSet.sol:162` 有 `init(bytes,uint256)`，说明 **embed 资产与源码脱节** |
| B 类靠 `alreadyInit` 防重入 | ❌ `SystemV2.sol` 无该字段；`StakeHub is SystemV2, Initializable`，闸门是 OZ 4.9.6 的 `_initialized`（slot 0） |

---

## 1. 问题

### 1.1 约束

创世是 pgp-chain 的，已上线不可修改：

```go
// core/genesis.go:739-768
func DefaultPGPGenesisBlock() *Genesis {
    Alloc: GenesisAlloc{
        params.ELAMINTER: {Code: params.ELAMINTER_CODE, Balance: big.NewInt(0)},
        params.ELATOKEN:  {Code: params.ELATOKEN_CODE,  Balance: big.NewInt(0)},
        common.HexToAddress("0x5d48e17f…"): {Balance: initBalance},
    },
}
```

而 BSC 需要 18 个系统合约地址（`core/systemcontracts/const.go:3-23`）：`0x1000–0x1008`（9 个）、`0x2000–0x2007`（8 个）、`0x3000`（1 个）。这些地址在 pgp 链上是空账户。

### 1.2 现状会怎样失败

- **写路径静默空转**：EVM 对无代码地址的 `CALL` 直接成功返回空（`core/vm/evm.go:364-368`），`init()`/`deposit`/`slash` 都「成功但什么都没写」。
- **读路径硬失败**：`getCurrentValidators` 用 `eth_call` + ABI unpack（`consensus/parlia/parlia.go:2039-2083`），空返回必然 unpack 失败。
- **升级机制不兜底**：`upgradeBuildInSystemContract` 按 genesis hash 选网络，pgp 落 `defaultNet`（`core/systemcontracts/upgrade.go:1171-1187`），其升级表全 nil，`applySystemContractUpgrade` 只打一条日志就 return（`:1283-1288`）。即 PF 文档的 **R4**。

---

## 2. 为什么不能走常规 EVM 部署

`CREATE2: addr = keccak256(0xff‖sender‖salt‖keccak256(init_code))[12:]`。目标 `0x…1000` 要求 20 字节中前 152 位固定，单次命中概率 `2^-152`，期望试验 `5.7×10^45` 次。且地址依赖 `init_code`——为产出正确 runtime code 而改 init_code，地址就变；**「地址命中」与「字节码正确」是同一搜索问题的两个约束**。

其他通道也不存在：`SSTORE` 只能写自身；本树无 EIP-7702 `setCode`；预编译表不含 `0x1000–0x3000`；`Genesis.Commit` 只接受 `Number == 0`（`core/genesis.go:658`）。

**结论：只能协议层 `SetCode` / `SetState` / 协议内 EVM 调用。**

---

## 3. 方案选择

| 方案 | 做法 | 问题 | 结论 |
|---|---|---|---|
| A | 普通交易 + CREATE2 | §2 已证不可行 | 排除 |
| B | 用系统交易执行部署 | legacy 段 `IsSystemTransaction` 不生效；交易无法把代码写到别人地址 | 排除 |
| C | 在 `TryUpdateBuildInSystemContract` 的 `atBlockBegin` 分支直接写状态 | 可行，一处改动覆盖全部调用点 | **采用** |
| D | 把部署伪装成 `pgpNet` 的一条 `Upgrade` | 语义错位、只 `SetCode`、每个满足门控的块重放 | 仅用于后续升级（§9） |
| E | 在 `core` 层新增独立步骤 | 调用点分散，漏一个即状态根分歧 | 备选 |

### 3.1 初始化必须与代码同块完成（S3）

1. **用户交易先于系统交易**。注意 v1 的论据有误：`core/state_processor.go:203-207` 那条「系统交易后不得再有普通交易」的检查被 `IsCancun` 门控，而 `PGPChainConfig` 未启 Cancun → 该检查在 pgp 上**不生效**。但结论不变：系统交易仍在 `Finalize` 阶段执行（`parlia.go:1544`），即在全部用户交易之后。
2. **`init()` 是外部可调、只受「未初始化」守卫**（`BSCValidatorSet.sol:162` 为 `external onlyNotInit`，`System.sol:41` 的 `require(!alreadyInit)` 无调用者限制）。
3. 因此若代码在 D 落盘、init 留到 F，**D 到 F 之间**（legacy 段）任何 EOA 都能先调 `init()`：恶意 payload ⇒ 验证人集合被写死；垃圾 payload ⇒ F 块 revert、全网停在 ForkHeight。

**定案 S3**：代码、初始化调用在 D 块块首、用户交易之前，同一协议步完成；F 块不再做合约动作。

| 方案 | D 块 | F 块 | 抢初始化窗口 |
|---|---|---|---|
| S1 | `SetCode` | 系统交易 `init` | D→F 全程 + F 块内 |
| S2 | `SetCode` + 毒化 `alreadyInit` | 块首清零再 init | 压回 F 块内，未消除；且类 B 无该字段，方案本身不成立 |
| **S3** | `SetCode` + 协议内初始化 | 无合约动作 | **零** |

---

## 4. 部署机制

### 4.1 注入点

`core/systemcontracts/upgrade.go:1147` 的 `TryUpdateBuildInSystemContract`，`if atBlockBegin {` 之后、`upgradeBuildInSystemContract(...)` 之前插入 `seedPGPSystemContracts(...)`。

理由：

1. **一处改动覆盖全部调用点**。全仓 `TryUpdateBuildInSystemContract` 共 **20 处**（含 1 处测试）：`core/state_processor.go:163`（导入）、`core/chain_makers.go:430`、`miner/worker.go`、`consensus/parlia/parlia.go` 两处（`atBlockBegin=false`）、`internal/ethapi/api.go` 两处、`eth/state_accessor.go` 两处、`eth/tracers/api.go` 十处。**这份清单就是实施的 DoD**，逐个核对，漏一处即状态根分歧。
2. 语义天然是「块首、交易之前」；`ProcessBeaconBlockRoot` / `ProcessParentBlockHash` 证明本树可以在块首直接调 EVM 而不产生交易。
3. 不污染 `defaultNet` 语义。

### 4.2 代价：签名要变

S3 需要在块首执行 EVM 调用，当前签名只有 `(config, blockNumber, lastBlockTime, blockTime, statedb, atBlockBegin)`，拿不到 header/chain。需扩为带 `header` 与 `chain`（或传入构造好的 `*vm.EVM`），20 处调用点同步调整。

### 4.3 D 块的执行顺序

1. 18 个 `SetCode`（tracing 标签 `CodeChangeSystemContractUpgrade`）+ `SetNonce(addr, 1)`（与 `HistoryStorageAddress` 的既有做法一致，见 `upgrade.go:1154-1155`；v1 写 `nonce: 0` 是错的）；
2. 协议内 EVM 调用完成初始化（**不再手种 `alreadyInit`**，见 §7）；
3. 自检：逐账户比对 `GetCodeHash` 与清单；
4. **幂等语义**：`codeHash` 已相等 ⇒ no-op；不相等 ⇒ `error`。**只有 `Process`（导入）路径把 error 升级为 panic**，`trace` / `eth_call` 回放路径返回错误即可——否则一次只读回放就能把进程打挂（评审 F-8）。

### 4.4 为什么不在 `Finalize`

legacy 块不走 `Finalize`；`Finalize` 在用户交易之后；部署若发生在交易之后，D 块内不同交易看到的 `EXTCODESIZE` 不同，直接状态根分歧。

---

## 5. 部署高度与配置

### 5.1 字段

```go
// params/pgp_config.go 的 LegacyConfig
SystemContractDeployHeight uint64 `json:"systemContractDeployHeight,omitempty"`
```

### 5.2 校验规则（启动期硬失败）

| 规则 | 理由 |
|---|---|
| `Legacy == nil` ⇒ 必须为 0 | 非 pgp 链不得有该值 |
| `ForkHeight == 0` 或 `== MaxUint64` ⇒ 必须为 0 | 没有 Parlia 时代就没有初始化时机 |
| `1 <= DeployHeight`，且 `DeployHeight <= ForkHeight - 2` | 统一口径：D 至少比 F 早两个块（v1 的 `D = F-1` 与自己的 `D+1 < F` 规则冲突，已修正） |
| **`IsLuban(ForkHeight) == true`** | 见 §5.3。**v1 写的「基线版本 >= luban」是错误表述**——ABI 路径由配置决定，不由字节码版本决定 |
| 运行时自检：`head >= DeployHeight` 且 `0x1000` 无代码 ⇒ 拒绝启动 | 唯一能抓住「旧二进制跑过了 D 块」的检查 |

### 5.3 ABI 路径由 `IsLuban` 决定，这是一处必须先修的配置缺口

```go
// consensus/parlia/parlia.go:424 / :453 / :1086 …
if !chainConfig.IsLuban(header.Number) { /* 走 pre-Luban ABI */ }
```

而 `params/pgp_config.go` 的 `PGPChainConfig` **没有 `LubanBlock`**（只有 `ShanghaiTime`），所以 `IsLuban` 恒为 false：

- 读路径用 `validatorSetABIBeforeLuban` 的 `getValidators()`（`lubanFork.go:16`），返回 `address[]`、无 BLS；
- 写路径 `initContract` 却用 `validatorSetABI` 的 `init(bytes,uint256)`（`parlia.go:2179`）。

**两条路径用了不同代的 ABI。** 必须先决策：要么给 `PGPChainConfig` 设 `LubanBlock`（使其与 `init(bytes,uint256)` 一致），要么改引擎统一用最终版 ABI。**这条不解决，部署方案无从谈起**——它不是本方案的产物，但会让本方案的行为无法预测。

### 5.4 多节点一致性

- 高度写进 `PGPChainConfig` 字面量（与 `DynamicArbiterHeight` 同法）。
- **forkid 不提供保护**：`gatherForks` 只反射顶层字段且只认 `Block`/`Time` 后缀，`Legacy` 被跳过（`core/forkid/forkid.go:269-297`）。
- 补偿：启动日志打印 `{chainId, forkHeight, deployHeight, imageHash}` + 只读 RPC + 上线检查单。

---

## 6. 字节码来源（v2 重写）

### 6.1 仓库现有 embed 不可用

三条独立证据：

1. **版本错位**：`jenner/mainnet/` 只有 `PaymentLaneContract`；17 个合约在 `pascal/mainnet/`。v1 引用的 `jenner.MainnetValidatorContract` 是**不存在的变量**。
2. **ABI 错位**：全仓 embed 字节码中 `631663df00`（`init(bytes,uint256)`）**零命中**，只有 `63e1c7392a`（`init()`）。而源码 `BSCValidatorSet.sol:162` 已经是 `init(bytes calldata, uint256)`。说明 **embed 资产是上游主网旧产物，从未按 pgp 分支重编**。
3. **常量错位**：`System.sol:11` 的 `bscChainID = 860621`（主网是 56），`SlashIndicator.sol:281` 用它编跨链包；`StakeHub.sol:281` 的 `__Protectable_init_unchained(0x0262aB0E…D913)` 是硬编码账户。**沿用主网字节码会把链 ID 写成 56、把保护地址写成别人的账户。**

后果：若按 v1 部署 pascal 的 `ValidatorContract` 再调 `init(bytes,uint256)`，0x1000 无 fallback → **D 块必然 revert**。

### 6.2 必须做的事

1. **从 `bsc-genesis-contract` 的 pgp 分支编译 18 个合约**，产出新的 `deployedBytecode`；
2. 新建 embed 包（如 `core/systemcontracts/pgpimage/`），**不复用** `pascal/` 或 `jenner/` 目录；
3. 生成脚本入仓（编译器版本、optimizer、commit 全部记录），产出 `CodeHash` 清单；
4. 测试断言 `keccak(hexDecode(embedVar)) == CodeHash`，且 **`0x1000` 的字节码必须包含 `631663df00`**（这条断言直接回归 F-1）；
5. 删除 v1 §6.1「与主网逐字节相同」的全部表述——它现在是错的，且会误导审计。

### 6.3 地址一致性还要证明什么

地址一致性不再是问题（直接写到常量地址），要证明的是**语义一致性**：

- 选择器集合与 `consensus/parlia/abi.go` 的四个 ABI 一致（`init`/`getMiningValidators`/`getValidators`/`deposit`/`slash`/`distributeFinalityReward`/`updateValidatorSetV2` 等）；
- `bscChainID`、`SYSTEM_REWARD_ADDR` 等常量与本链一致（860621 / 0x1002）；
- 与 `bsc-genesis-contract` 的 tag 一一对应，脚本可复现。

---

## 7. 初始化（v2 重写：不再手种存储）

### 7.1 按「有没有一次性初始化入口」分类

对 `bsc-genesis-contract/contracts` 逐个核对的结果：

| 类 | 合约 | 初始化入口 | 做法 |
|---|---|---|---|
| A1 | `0x1000` Validator | `init(bytes,uint256)`，`external onlyNotInit`（`BSCValidatorSet.sol:162`） | 协议内 EVM 调用，payload 由 `Legacy.ParliaGenesis*` 编码（selector `0x1663df00`） |
| A2 | `0x1001` Slash、`0x1003` LightClient、`0x1004` TokenHub、`0x1005` RelayerIncentivize、`0x1006` RelayerHub、`0x2000` CrossChain | `init()`，`onlyNotInit` | 协议内 EVM 调用，selector `0xe1c7392a` |
| B1 | `0x2002` StakeHub、`0x2004` Governor、`0x2005` GovToken、`0x2006` Timelock、`0x3000` TokenRecoverPortal | `initialize()`，`initializer onlyCoinbase onlyZeroGasPrice` | **协议内 EVM 调用**（`from = coinbase`、`gasPrice = 0` 天然满足修饰器） |
| B2 | `0x1002` SystemReward、`0x1007` GovHub、`0x1008` TokenManager、`0x2001` Staking、`0x2007` PaymentLane | 无一次性初始化入口 | 只 `SetCode` |
| C | `0x2003` StakeCredit | `initialize(address,string)`，`onlyStakeHub` | **不在此列**——由 StakeHub 按验证人逐个创建/初始化 |

**结论：v1 §7 的「类 B 手种存储 + 写 `alreadyInit=1`」整段删除。** 理由（评审 F-2/F-3，已复核）：

- `StakeHub is SystemV2, Initializable`，`SystemV2.sol` 没有 `alreadyInit` 字段，闸门是 OZ 4.9.6 的 `_initialized`（slot 0）；
- `initialize()` 自带 `initializer` 修饰器，调用一次即置位，**不需要、也不应该手写 slot**；
- B1 的 5 个合约在 `consensus/parlia/feynmanfork.go:40-46` 本来就有「同一份 calldata 调 5 个地址」的先例，可直接复用该形态。

### 7.2 仍需手种的情况

只有当某个合约的**构造函数之外**存在必须预置的 wiring 常量、且没有 `initialize` 入口时才需要手种。当前核对结果是：**没有**。若实施时发现，必须满足 §7.3 的四道闸，并在清单里标注 `kind: wiring|constant`，禁止 `live`。

### 7.3 四道闸（保留，且升级为强制规范）

1. 槽位由编译产物 `storageLayout` 解析生成，**禁止手打十六进制槽位**；
2. `imageHash` 钉子（编码方式见 §7.4）；
3. 语义探针测试：应用后调只读 getter 断言业务合理（不能是「读回来等于写进去」的同义反复）；
4. 离线 `pgpcheck`：指定高度 dump 18 个账户 `(codeHash, slotCount, keccak(sorted dump))` 与清单 diff。

### 7.4 `imageHash` 的规范序列化（v1 未定义）

v1 只写「规范序列化」，跨语言会飘。定死：

- 键按字典序排序、UTF-8、无空格；
- 数字一律十进制字符串（禁用浮点，`codeLen`/`nonce` 也转字符串）；
- 数组保持声明顺序（顺序本身是语义）；
- `imageHash = keccak256(UTF8(canonicalJSON))`；
- 测试提供 **canonical 编码器**，并用两个不同语言的实现交叉验证一次。

### 7.5 StakeHub 经济闭环（评审 F-4，Go/No-Go 项）

jenner/pascal 版 `updateValidatorSetV2` 在 epoch 结束会对每个 incoming 调 `IStakeHub.distributeReward`。StakeHub 若没有 `consensusToOperator` 映射，会走 `creditContract == 0` 分支，把奖励 `call` 给 `SYSTEM_REWARD_ADDR`（0x1002）。后果：**每个 epoch 的验证人奖励全部沉进 0x1002，验证人零收益。**

这与「禁止种 live 数据」直接冲突。三个选项，必须选一个并写进文档：

| 选项 | 做法 | 代价 |
|---|---|---|
| ① 注册映射 | 部署后由治理/协议调用 `StakeHub` 注册 consensus→operator 映射，并创建 StakeCredit | 需要一次受控的写操作，且要处理「注册前产生的奖励」 |
| ② 改奖励路径 | 修改合约，让无映射时回落到 `deposit` 到 0x1000 | 需要改合约源码并重编（与 §6.2 同批） |
| ③ 首版不部署 `0x2002` | 明确不启用 StakeHub 质押，`updateValidatorSetV2` 不调用 | 需要确认引擎在该版本不会调 `distributeReward`（取决于 ABI 路径，见 §5.3） |

**未定之前不应批准编码。**

---

## 8. 与 ForkHeight 的衔接

### 8.1 D 块

- 取值：`D <= ForkHeight - 2`（与 §5.2 一致）。
- 完成 §4.3 的三步，**不产生交易、没有 receipt**。
- **日志注意**：块首 EVM 调用产生的 `LOG` 会进 `statedb` 日志缓冲，但没有 `SetTxContext`、不进 receipt、不进 bloom。需确认 `eth_getLogs` 的 pending 路径与 legacy bloom 公式不会把它们挂到任何交易的 receipt 上。

### 8.2 F 块

- `initContract` 触发条件收敛为 `number == 1 || isPGPInitBlock(number)`（`parlia.go:1544`、`:1652`）。采纳 S3 后 `isPGPInitBlock` 恒为 false。
- **处置**：删除 `isPGPInitBlock` 及其调用点；同时删除 `parlia.go:2179` 的 `validatorSetData` 分支，否则留下指向死路径的代码。

### 8.3 D→F 之间

- `eth_getCode` / `eth_getStorageAt` / `eth_call` 都能看到新状态。
- **运维注意**：`getMiningValidators()` 此时已可调用，但**不参与共识**（legacy 段是 PBFT）。监控若当成「当前验证人集合」会误判。

### 8.4 与 PF 文档的冲突

PF 文档 §三 的图（`:58-68`）与 T2-4（`:96-105`）写的是「ForkHeight 块首 `SetCode × 17` + `Finalize` init」，与本方案冲突。**这两处的修订是合并前置，不是「实施时顺手改」。**

---

## 9. 后续升级

### 9.1 按 genesis hash 接入 `pgpNet`

```go
// core/systemcontracts/upgrade.go:1174-1187
case params.PGPGenesisHash:
    network = pgpNet
```

**已知缺陷**：`GenesisHash` 是包级全局变量（`core/blockchain.go:505`），多链共存进程会互相污染。实施时改为显式参数或从 config 推导。

### 9.2 不复用 BSC 的 fork 强序链

BSC 的时间型门控要求非 optional fork 连续启用，想只开 `jenner` 就得把前面十几个全开。改用 pgp 自己的高度表 `Legacy.SystemContractUpgrades []{Name, Height}`：严格递增、全部 `> ForkHeight`、每项都能在 `pgpNet` 找到同名 `Upgrade`。

### 9.3 与基线的关系

1. 基线是**静态镜像**，不是「在 D 块重放 BSC 升级表」；
2. `pgpNet` 表里任何 `Height <= ForkHeight` 的条目都是配置错误，启动期拒绝；
3. `UpgradeConfig` 增加可选 `FromCodeHash`，`SetCode` 前断言，不等则 panic；
4. 存储迁移写在 `BeforeUpgrade` 钩子里（该钩子目前产线无人实现，`upgrade.go:35-42`）。

---

## 10. 风险清单

| 编号 | 风险 | 缓解 |
|---|---|---|
| R1 | 初始化被抢跑 | 采用 S3（零窗口）；`initializer`/`onlyNotInit` 由调用自身置位，不手写 slot |
| R2 | 状态根分歧 | 高度编译进 `PGPChainConfig`；`imageHash` 进日志/RPC/检查单 |
| R3 | 部署高度不一致但 forkid 相同 | 已知 forkid 不覆盖 `Legacy`；靠 RPC 对账 + 检查单 |
| R4 | 字节码与 fork 版本错配 | §5.3 的 `IsLuban` 断言；`CodeHash` 钉子 |
| R5 | 槽位写错但链照跑 | §7.3 四道闸 |
| R6 | EVM 观察时点不一致 | 只改一处；20 处调用点逐个核对 |
| R7 | snap sync 不重放 D 块 | 状态里已有代码；但 `debug_trace` 看不到部署步。**FastNode（NoTries）是另一条路径，root 传播需单独验证** |
| R8 | 幂等语义 | `codeHash` 相等 ⇒ no-op；不等 ⇒ error；仅 `Process` 路径 panic |
| R9 | 回滚不可能 | D 之后只能 reorg 到 D 之前 |
| R10 | 「隐形部署」无法解释 | 文档化 `imageHash` + `pgpcheck` 复现命令 |
| R11 | TokenHub 余额 | **已定：不镜像** 176,405.56 BNB；将来用可审计转账注入 |
| R12 | Feynman 与 init 顺序倒置 | 校验 `FeynmanTime > ForkHeight` |
| R13 | BLS 票权为空 | 与 PF 文档 D6 一并处理 |
| R14 | 全局 `GenesisHash` 污染 | 改为显式参数 |
| **R15** | **embed 资产与源码脱节**（本次评审发现） | §6.2：从 pgp 分支重编 + `631663df00` 断言 |
| **R16** | **StakeHub 无映射 → 奖励全进 0x1002** | §7.5 三选一，未定不得编码 |
| **R17** | **主网字节码的 `bscChainID=56` / 硬编码 protect 地址** | 只能用重编产物；清单钉 `bscChainID == 860621` |
| **R18** | 块首 EVM 调用的 LOG 可能污染 receipt/bloom | §8.1 的验证项 |

---

## 11. 前置条件（Go / No-Go）

1. **字节码来源定案**：pgp 分支重编 + 新 embed 包 + 生成脚本入仓（未完成不得进实施）；
2. **`IsLuban(ForkHeight)` 决策**：设 `LubanBlock` 或统一 ABI（§5.3）；
3. **StakeHub 经济闭环三选一**（§7.5）；
4. `bsc-genesis-contract` 的 `init(bytes,uint256)` 与 `0x1663df00` 断言成立；
5. BLS 公钥来源与 PF 文档 D6 一致；
6. `D`、`F` 绝对值已定，且 `D <= F - 2`；
7. 20 处调用点清单签字确认（DoD）；
8. 全网节点在 D 之前完成升级（forkid 发现不了，只能靠检查单）。

---

## 12. 测试策略

| 编号 | 测试 | 断言 |
|---|---|---|
| T1 | `TestImageCodeHashes` | 18 个 `CodeHash` == `keccak(hexDecode(embedVar))` |
| **T1b** | **`TestValidatorCodeHasInitBytesSelector`** | `0x1000` 的 embed 字节码包含 `631663df00`（直接回归 F-1） |
| T2 | `TestImageHash` | `imageHash` == canonical 编码的 keccak |
| T3 | `TestImageSlotNames` | 若存在手种槽位，名字能被 `storageLayout` 解析 |
| T4 | `TestDeployBlockCode`（D-1 / D / D+1） | D-1 空、D 起非空且不变；`nonce == 1` |
| T5 | `TestDeployInitialized` | A1/A2 的 `alreadyInit == true`；B1 的 OZ `_initialized` 已置位；语义探针 |
| T6 | `TestDeployIdempotent` | 同 statedb 二次调用 ⇒ no-op；code hash 不等时 error |
| **T7** | **`TestMintImportTraceStateRootEqual`** | 出块 / 导入 / `eth_call` 回放**三路**在 D/F/F+1 状态根一致；再加 NoTries 变体 |
| T8 | `TestForkBlockInitSucceeded` | F 块后 `getMiningValidators()`（或 pre-Luban `getValidators()`，按 §5.3 的决策）与配置一致 |
| T9 | `TestNoInitWindow` | D/F 块里调用 `init(恶意 payload)` 的交易必须 revert，区块仍合法 |
| T10 | `TestDeployGoldenStateRoot` | 钉死 D、F 块 stateRoot |
| T11 | `TestPGPNetUpgradeRouting` | `GenesisHash = PGPGenesisHash` 走 `pgpNet` |
| T12 | `TestUpgradeFromCodeHashMismatch` | `FromCodeHash` 不匹配 ⇒ panic |
| T13 | `TestConfigValidation` | §5.2 每条规则正反例 |
| T14 | `TestRefuseStartAfterMissedDeploy` | `head >= D` 且无代码 ⇒ 启动报错 |
| **T15** | **`TestDeployLogsDoNotEnterReceipts`** | 块首 EVM 调用的 LOG 不出现在任何交易 receipt / bloom 里 |

---

## 13. 待定项

| 项 | 说明 |
|---|---|
| 字节码编译基线 | 用哪个 commit / 编译器版本；必须与 `consensus/parlia/abi.go` 的 ABI 对齐 |
| `IsLuban(ForkHeight)` | 设 `LubanBlock` 还是统一 ABI（§5.3） |
| StakeHub 经济闭环 | §7.5 三选一 |
| `D`、`F` 绝对值 | 建议 `F = 642`、`D = F - 2` |
| BLS 公钥来源 | 与 PF 文档 D6 一并定 |
| `0x2007` PaymentLane | `IsJenner` 未启用时是死代码（`ResolveLaneState` 返回空），清单应标注「无调用者」 |

---

## 14. 实施顺序

1. 定案字节码来源，重编 18 个合约，产出新 embed 包 + `CodeHash` 清单 + 生成脚本（**未完成不得进实施**）；
2. 解决 `IsLuban` / ABI 路径不一致（§5.3）；
3. 定 StakeHub 经济方案（§7.5）；
4. 扩 `TryUpdateBuildInSystemContract` 签名 + `seedPGPSystemContracts`（SetCode + 协议内初始化），20 处调用点逐个核对，跑 T7/T15；
5. 删除 `isPGPInitBlock` 与 `validatorSetData` 死分支；修订 PF 文档 §三 / T2-4（合并前置）；
6. 配置校验 + 启动自检（T13、T14）；
7. `pgpNet` + 高度表 + `FromCodeHash`（T11、T12）；
8. devnet 全流程：两节点逐块比状态根；D 后 `eth_getCode` 非空；F 块验证人集合正确；T9 抢 init 交易失败；
9. `pgpcheck` 工具与上线检查单。
