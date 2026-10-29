# pgp-chain 切换 BSC(Parlia) 共识 + 系统合约迁移

| 项 | 值 |
|---|---|
| 状态 | 设计评审中（未开始编码） |
| 日期 | 2026-10-29 |
| 适用仓库 | `bsc`（链）、`bsc-genesis-contract`（系统合约） |
| 上游参照 | pgp-chain（geth 1.9.7 改造版，PBFT/DPoS 现状） |
| 关联文档 | `docs/design/pbft-port-design.md`（PBFT 出块阶段）——**本文档的"legacy 段只读回放"前提已作废**，见下文 §二 补注 |

## 一、目标与已定决策

把已经改造成"可同步 pgp 主网数据"的 `bsc` 节点，在某个高度起切换到 BSC 的 Parlia 共识，并在同一高度植入 `bsc-genesis-contract` 的系统合约。

| 议题 | 决策 |
|---|---|
| 分叉高度 | 先保持 `math.MaxUint64` 占位，devnet 全流程跑通后再填主网高度 |
| 验证人（阶段一） | 固定 12 个，名单由用户提供；不启用 Feynman/StakeHub |
| 验证人（阶段二） | 后续单独 fork 启用 StakeHub 质押选举 |
| Fast finality | 同一版本内启用（BLS vote attestation） |
| 系统合约字节码 | 改 `bsc-genesis-contract` 源码后自行编译，`go:embed` 打包 |
| 分叉后 ELA 网关 | **保留**：0x64/0x65 继续是 ELAMinter/ELAToken，SPV 预编译 0x3E8–0x3F3 全部保留；改为**把 BSC 的 tmHeaderValidate/iavlMerkleProofValidate 迁址**（0x71/0x72） |
| 网关交易语义 | **待定（D7）**：推荐方案 A —— 分叉后继续零成本（借 PassBalance + 全额退 gas + 重复充值检查）；若选方案 B 则网关交易需自备 gas |

## 二、现状（已核实的关键事实）

三个仓库不是三套独立代码，而是**同一条演进链**：

- `bsc/` 就是新的 pgp 节点。`params.PGPChainConfig` 已存在（chainId 860621），并已含 `Legacy *LegacyConfig`（`params/pgp_config.go:38`），其中 `ForkHeight` 是唯一的分叉开关，当前 `math.MaxUint64`。
- `consensus/router`（`consensus/router/router.go`）已实现"按高度路由"：`New(legacy, posa, forkHeight)`，`isForked(num) = num >= forkHeight`，覆盖 Author/VerifyHeader(s)/VerifyUncles/Prepare/Finalize/Seal/SealHash/CalcDifficulty/Delay 全部方法。**fork 前 `Seal` 直接报错**（设计如此）。
  - **补注（2026-10-29 修订）**：该前提已改变。新链在切 PoSA 之前需要靠 PBFT 继续出块，故 router 分叉前的 `Seal` 改为委派 PBFT 引擎，legacy 段不再是"只读回放"。详见 `docs/design/pbft-port-design.md`。对本文档的影响：T5-2 的网关语义、T3 的系统合约注入等**均不变**，但 T4 的 `eth.engine` 装配需要重做（见 T4 补注）。
- `consensus/pgp` 是只读回放引擎，只验不产块，`legacyBlockPeriod = 3`，与 pgp 链 3 秒出块一致。
- `consensus/parlia` 是完整 BSC Parlia（2559 行），但**对 Legacy/ForkHeight 零认知**（grep `Legacy|ForkHeight|IsPGPLegacy` 在 parlia 包内 0 命中）。
- `core/systemcontracts/` 的 17 个地址常量、`TryUpdateBuildInSystemContract` 的 `atBlockBegin` 语义、`upgrade.go` 的 `BeforeUpgrade/AfterUpgrade`（当前无人使用）都齐备。
- `PGPChainConfig` 目前 `Parlia: nil` → `IsInBSC() == false`，所以全链走 legacy 规则。

**因此本项目的实际工作量不是"移植共识"，而是补齐 6 个精确的缺口**，下面每节对应一个。

## 三、目标形态

```
                      ┌──────────────────────────────────────────┐
height <  ForkHeight  │ router → consensus/pgp（只读，验 PBFT confirm）
                      │   rules: IsPGPLegacy（legacy 预编译集、65/35 分费、无 baseFee）
                      └──────────────────────────────────────────┘
height == ForkHeight  ── 分叉块（必须 % 200 == 0）──────────────
   块开始 (atBlockBegin)  SetCode × 17 个系统合约（不产生交易）
   Finalize 内顺序        init() 系统交易 → deposit → 分费
   区块头                 extra = 32 vanity + 1 count + 12×(20 addr + 48 BLS) + 65 seal
                      ┌──────────────────────────────────────────┐
height >  ForkHeight  │ router → consensus/parlia（出块 + 快终局）
                      │   rules: BSC 集 + ELA 预编译集（0x64/0x65 仍是合约，人/
                      │          BSC 轻客户端迁到 0x71/0x72）→ 网关继续可用
                      └──────────────────────────────────────────┘
```

## 四、任务分解

### T1 配置层：让 `PGPChainConfig` 成为"双时代"配置

文件：`params/pgp_config.go`（主）、`params/pgp_parlia.go`（新建）

1. `PGPChainConfig` 增加 `Parlia: &ParliaConfig{}`。这是 `IsInBSC()` 的唯一开关；一旦打开，`Rules()` 切到 BSC 语义（`Legacy` 分支仍优先，`params/config.go:2249`）。
2. **BSC 区块高度型 fork 全部对齐到 ForkHeight**：`Ramanujan/Niels/MirrorSync/Bruno/Euler/Gibbs/Moran/Planck/Luban/Plato/Hertz/Hertzfix = ForkHeight`。
   - 必要性：`CheckConfigForkOrder` 在 `IsInBSC()` 后立即生效（`params/config.go:1583`），不允许跳开前面的 fork 只开后面。
   - 语义：分叉那一步直接进入最新 BSC 规则集，不重演历史 fork 的行为差异。
   - `Luban` 必须开：它决定 extra 里带不带 48 字节 BLS 投票公钥，fast finality 依赖它。
3. 全部时间型 fork（Kepler…Pasteur/Jenner）保持 `nil`：不触发 Feynman（StakeHub 初始化）与后续所有合约升级，阶段一零负担。
4. 新建 `params/pgp_parlia.go`：
   - `ParliaBootstrapValidators []BootstrapValidator{Address, BLS}`：用户提供的 12 个地址 + BLS 公钥。
   - `func (c *ChainConfig) IsPGPParliaForkBlock(num *big.Int) bool`：`c.Legacy != nil && num == ForkHeight`。
   - 启动期校验：`ForkHeight != 0 && ForkHeight % epochLength == 0`（见 T2-3）。
   - ⚠️ 字段名**不得**以 `Block`/`Time` 结尾，否则 `core/forkid` 反射会改 EIP-2124 checksum（约束已写在 `params/pgp_config.go:33-37`）。

验收：新增 `params` 测试——`CheckConfigForkOrder()` 通过；`Rules(ForkHeight-1).IsPGPLegacy == true`、`Rules(ForkHeight).IsPGPLegacy == false`；边界两侧 `IsLuban/IsPlato` 取值符合预期。

### T2 parlia 分叉引导（本方案技术核心）

parlia 现在假定"从创世第 1 块就是自己的"，我们要它"从第 N 块接管"。五处必改。

**T2-1 引导快照** — `consensus/parlia/parlia.go:805 snapshot()`

`snapshot(chain, forkHeight-1, parentHash)` 会回溯到创世，用 BSC 布局解析 pgp 创世/PBFT 块的 `Extra`（PBFT 的 extra 是 RLP confirm 二进制），必然 `errInvalidExtra`。

- `Parlia` 结构体新增 `forkHeight uint64`、`bootstrap []params.BootstrapValidator`（`parlia.New` 从 chainConfig 读取）。
- `snapshot()` 循环开头：`if p.forkHeight != 0 && number == p.forkHeight-1 { return p.newBootstrapSnapshot(number, hash) }`。
- `number < p.forkHeight` 时禁止 `loadSnapshot`（防旧快照污染）。
- 引导快照：`Validators`=12 地址、`VoteAddrs`=BLS 公钥、`Recents` 空、`EpochLength=200`、`BlockInterval=3000`。

**T2-2 验证人来源分叉** — `parlia.go:1244 verifyValidators` / `prepareValidators` / `:1960 getCurrentValidators`

`getCurrentValidators` 会 `eth_call` 0x1000 的 `getMiningValidators`；分叉块的父块是 legacy 块，那里 0x1000 无代码，必然失败。

- `getCurrentValidators(hash, num)`：`if num < p.forkHeight { return bootstrap }`。
- `verifyValidators`/`prepareValidators` 在分叉块按 epoch 块处理（`ForkHeight % 200 == 0` 天然满足），但**不要**走 `IsOnLuban` 那个"把 BLS 公钥清零"的分支（`parlia.go:1272-1279`）——分叉块要写真实 BLS 公钥，否则 fast finality 要等下一个 epoch 才生效。

**T2-3 ForkHeight 必须是 epoch 整数倍**

否则分叉块 extra 没有验证人列表，引导快照与头部自洽性要靠额外分支兜。一行硬校验 + 一个单测，消掉整类边界问题。

**T2-4 系统合约初始化触发点** — `parlia.go:1465`（Finalize）、`:1573`（finalizeAndAssemble）

现在写死 `if header.Number.Cmp(common.Big1) == 0 { initContract(...) }`，改为"创世块或分叉块"：

```go
if header.Number.Cmp(common.Big1) == 0 || p.isBootstrapBlock(header) { initContract(...) }
```

两个关键点：
1. **两条路径必须完全对称**：`Finalize`（导入）与 `finalizeAndAssemble`（出块）里 init 的位置、顺序、`systemTxMode` 一致，否则出块节点与验证节点状态根分叉。
2. **init 要排在 `verifyValidators` 之前**：现顺序是 verifyValidators → TryUpdate → initContract。分叉块上让 init 先行，并注释说明"此顺序是共识的一部分"。

**T2-5 分叉块的 baseFee 断层** — `parlia.go:749`、`consensus/misc/eip1559/eip1559.go:39`

legacy 区块头 `BaseFee == nil`（pgp 从未启用 1559），Parlia 要求 baseFee 且校验父块。分叉块上 `VerifyEIP1559Header(parent=legacy)` 必报 `parent header is missing baseFee`。

- `verifyCascadingFields`：`if p.isBootstrapBlock(header) { 校验 header.BaseFee == params.InitialBaseFeeForBSC，并跳过父块 baseFee 检查 }`。
- `eip1559.CalcBaseFee`：把 `IsPGPLegacy` 分支**移到** `IsInBSC` 之前（`eip1559.go:63` 目前在前），否则 legacy 高度 baseFee 从 0 变 1 gwei。

验收：合成链单测"legacy 头（无 baseFee）→ 分叉块（InitialBaseFeeForBSC）"，且 `Finalize` 与 `finalizeAndAssemble` 产出相同状态根。

### T3 系统合约：编译、打包、注入

**T3-1 `bsc-genesis-contract` 源码改造（最小集）**

| 文件 | 改动 | 原因 |
|---|---|---|
| `contracts/System.sol:11`、`contracts/SystemV2.sol` | `uint16 bscChainID = 0x0038` → `uint32 bscChainID = 860621` | 860621 = 0xD21CD，**溢出 uint16**，必须扩宽类型 |
| `contracts/SystemReward.sol` | 新增一次性 `initializeOperators(address[] calldata)`：`onlyCoinbase` + `onlyZeroGasPrice` + `!alreadyInit`，同时正确设置 `numOperator` 与 `alreadyInit` | 现有 `doInit` 只授权 0x1003/0x1005，而 `distributeFinalityReward` 要求 0x1000 是 operator，否则每 200 块必 revert |
| `scripts/validators.js` | 换成用户给的 12 个 `{consensusAddr, feeAddr, bscFeeAddr, votingPower}` + 12 个 BLS 公钥 | `BSCValidatorSet.INIT_VALIDATORSET_BYTES`（`BSCValidatorSet.sol:21`）是编译期常量，初始集合只能这样注入 |

不改的：其余合约保持原样（含 `deprecated/` 下 6 个 revert 合约——`initContract` 会调到它们，需要有 code）。

**T3-2 构建产物打包**

1. `python3 scripts/generate.py`（参数替换 + `forge build`）→ `node scripts/generate-genesis.js` 交叉验证。
2. 取 `out/<Contract>.sol/<Contract>.json` 的 `deployedBytecode.object`，写成**纯 hex 文本**（无 0x、无换行）。
3. 落到 `bsc/core/systemcontracts/pgp/`：`types.go` 用 `//go:embed mainnet/<Contract>`（照抄 `luban/types.go`），覆盖 `core/systemcontracts/const.go` 的 17 个地址：1000-1008、2000-2006、3000。

**T3-3 挂进 upgrade 机制** — `core/systemcontracts/upgrade.go`

1. 网络判定加一条：`case params.PGPGenesisHash: network = pgpNet`（`upgrade.go:1176`）。这是必须补的坑——现有 `defaultNet` 没有任何升级数据，`applySystemContractUpgrade(nil)` 只打印 "Empty upgrade config" 然后静默跳过。
2. 新增 `pgpUpgrade = map[string]*Upgrade{ pgpNet: {UpgradeName: "PGPParlia", Configs: [...17 条...]} }`（`ContractAddr` + `Code` + `CommitUrl`）。
3. `upgradeBuildInSystemContract` 末尾加：`if config.IsPGPParliaForkBlock(blockNumber) { applySystemContractUpgrade(pgpUpgrade[network], ...) }`。
4. `SystemReward.initializeOperators` 的调用：优先在 `parlia.isBootstrapBlock` 分支里多发一条系统交易（交易化初始化可被 trace/浏览器看见），而不是 `AfterUpgrade` 里直接改存储。

验收：`upgrade_test.go` 加用例——分叉块后 17 个地址 `GetCode` 非空；非分叉块不注入；`PGPGenesisHash` 正确映射 `pgpNet`。

### T4 引擎装配与 PoSA 透传（fast finality 前置）

现状：router **故意不实现** `consensus.PoSA`（`router.go:20-25` 注释解释了原因——`eth/backend.go:519` 会断言 `*parlia.Parlia` 并因 "Engine is not Parlia type" 启动失败），导致 vote pool 根本不创建、attestation 永远为 nil。

要开 fast finality：

1. **router 实现 height-gated PoSA 代理**：12 个方法（`consensus/consensus.go:139-153`）在 `num < forkHeight` 返回安全默认值（`IsSystemTransaction=false`、`EnoughDistance=true`、`IsLocalBlock=false`、`GetJustifiedNumberAndHash=(0,{},nil)`、`GetFinalizedHeader=nil`、`CheckFinalityAndNotify=noop`、`VerifyVote=nil`、`IsActiveValidatorAt=false`、`NextProposalBlock=(0,0,nil)`），`>= forkHeight` 透传 Parlia。每个方法都要注释"为什么这个默认值安全"，并加单测。
2. **backend 解包**：`eth/backend.go:509` 与 `:832`（`StartMining` 的 `Authorize`）改为先 `if r, ok := engine.(*router.Engine); ok { engine = r.PoSA() }` 再断言。
   - **更正（2026-10-29 核对）**：`:832` 是 `if parlia, ok := s.engine.(*parlia.Parlia); ok`，**带 ok 的断言、非致命**，不会导致 `--mine` 失败，只会静默跳过 `Authorize`。真正的硬断言只有 `:509-520` 的 votePool 段（`return nil, errors.New("Engine is not Parlia type")`）。两处都要改，但动机不同：`:509` 是启动失败，`:832` 是"能出块但 BLS 签名为空"的静默错误——后者更隐蔽，必须在 devnet 断言论 attestation 非 nil。
3. **vote 配置**：`config.Miner.VoteEnable = true`、`DisableVoteAttestation = false`、`--bls.wallet`/`--bls.password`/`--vote.journal` 齐备；BLS 账户用 `cmd/geth/blsaccountcmd.go`（`geth bls account new`）生成，与 T1 的 BLS 公钥一一对应。
4. **legacy 高度不参与投票**：fork 前 router 的 `Seal` 在 PBFT 阶段已委派 `consensus/pbft`（见 `pbft-port-design.md`），而**不是**返回错误。因此本条的前提要改写为：fork 前 `IsActiveValidatorAt` 返回 false、`Seal` 由 PBFT 处理，PoSA 的 vote pool 在 legacy 高度不该收到任何块，靠单测钉住。

验收：devnet 连跑 3 个 epoch，justified 高度递增；停一个验证人仍能凑够 2/3；`DisableVoteAttestation=true` 能降级。

### T5 边界规则、预编译与网关语义

`Parlia != nil` 会让 `IsInBSC()` 对**所有高度**为真。已逐个核对全仓库 30 处 `IsInBSC()` 调用，只有两处必须加 legacy 门控：

| 文件:行 | 现状 | 风险 | 处理 |
|---|---|---|---|
| `core/vm/interpreter.go:96` | `IsInBSC() && coinbase == contract.address` → `ErrCoinbaseAsContract` | 给 legacy 高度新增了不存在的规则，可能让历史块执行失败 | 加 `&& !IsPGPLegacy(blockNumber)` |
| `consensus/misc/eip1559/eip1559.go:63` | `CalcBaseFee` 先判 `IsInBSC` | legacy 高度 baseFee 从 0 变 1 gwei | 把 `IsPGPLegacy` 分支提前 |

已核实**无需改动**：`core/data_availability.go`（Cancun 未启用即早返回）、`core/blockchain.go:1753`（Cancun 门控）、`core/state_processor.go:396`（Cancun 门控）、`core/txpool/blobpool`（blob tx）、`genesis.go:599/616`（genesis 处 London(0)=false、Cancun=nil）、`state_transition.go:776`（已有 `rules.IsPGPLegacy` 前置）。

**T5-1 分叉后的预编译集：新增一张表，不改 BSC 的 14 套表**

BSC 的 `core/vm/contracts.go` 里有 14 套地址表（Byzantium→Osaka），每套都把 `tmHeaderValidate`/`iavlMerkleProofValidate` 放在 0x64/0x65。逐套改会产生 14 处 diff，而这些表只在 BSC 网络被选中。做法是仿照 legacy 集，**新增一套只给本链用的表**（`core/vm/contracts_pgp_parlia.go`）：

```
0x01..0x09   Ethereum 标准
0x66         blsSignatureVerify        （地址不能动：SlashIndicator.sol 以 assembly 硬编码 0x66/0x68）
0x68         verifyDoubleSignEvidence
0x71         tmHeaderValidate           ← 从 0x64 迁址
0x72         iavlMerkleProofValidate    ← 从 0x65 迁址
0x3E8..0x3F3 ELA / SPV 全量保留          （复用 contracts_pgp.go 的 12 个 spec）
```

- **不含 0x64/0x65**：留给 `ELAMinter`/`ELAToken`（genesis 里已有 code）。
- **不含 0x67 `cometBFTLightBlockValidate`**：BC 轻客户端专用，本链不用。
- **不含 0x0A KZG**：Cancun 未启用。
- 选择方式：`Rules` 增加 `IsPGPParlia bool`（`c.Legacy != nil && !c.IsPGPLegacy(num)`），在 `activePrecompiledContracts` 与 `ActivePrecompiles` 里紧跟 `IsPGPLegacy` 分支插入。两张表必须**同时**改（dispatch map + EIP-2929 warm set），否则首访 gas 从 100 变 2600 —— 既有 `contracts_pgp_test.go` 已用"两表 key 必须等价"钉住这条规则。
- 地址兼容性（已核对）：BSC 占用 0x01–0x11、0x64–0x70、0x100；ELA 占用 0x3E8–0x3F3（`params/pgp_precompiles.go:45-56`，十进制 1000–1011）。0x71/0x72 未被任何一方占用；0x70 已被 BEP-706 的 `milliTimestamp`（Jenner）预定，所以不用 0x70。
- **迁址的合约侧连带**：`contracts/lib/0.6.x/MerkleProof.sol:71` 以 `staticcall(..., 0x65, ...)` 硬编码了旧地址。本仓库中该库已无调用方（`TokenRecoverPortal` 用的是 OpenZeppelin 版本），所以现在不影响编译产物；但**若将来恢复 BC 跨链**，必须同步改成 0x72。0x66/0x68 则相反：`SlashIndicator.sol:290/342`、`StakeHub.sol:1213` 都硬编码了，绝不能迁。

**T5-2 网关交易语义必须与共识切换同批改（否则含充值交易的块会被判非法）**

本次设计最容易漏掉的一条。ELA 网关的"零成本"不是合约给的，是 `core/state_transition.go` 给的，而且**四处判定全部按块高度门控**：

| 位置 | 现有条件 | 分叉后失效的后果 |
|---|---|---|
| `state_transition.go:594` `legacyGasRefund` | `rules.IsPGPLegacy && (Recharge 或 refundWithdraw)` | 全额退 gas 消失 |
| `state_transition.go:985` `borrowPassBalance` | 由 `legacyGasRefund` 触发 | 零余额账户无法发起充值 |
| `state_transition.go:923` `legacySettledRecharges` | `!IsPGPLegacy(blockNumber)` 直接返回 nil | **重复充值检查失效**（同一 ELA 交易可能被处理两次） |
| `state_transition.go:1039` `calcRefund` | `IsPGPLegacy` → 退款上限 1/2 | 上限变成 1/5 |

致命点在 `settlePassBalance`（`:1008-1030`）：交易结束后若发送者余额 **小于** 借入额，整笔交易回滚并返回 `ErrGasLimitReached` —— 这是**块级错误**，不是交易级失败。也就是说：

> 借了 1e18 又只退 tip（不退 baseFee 部分），余额必然 < 1e18 → 每一笔零余额充值都会让整块无效。

所以"保留地址但不保留零成本语义"行不通，必须二选一：

- **方案 A（推荐）继续零成本**：`legacyGasRefund` 的前置从"块高度"改为"交易性质 + 链配置了网关"（`params.IsPGPRechargeTx/IsPGPRefundWithdrawTx` 已有），退款范围从 tip 扩到 `gasUsed * gasPrice` 全额（含 baseFee），保证余额精确回到借入值；`legacySettledRecharges` 改为"块内存在网关交易"即扫描。
- **方案 B 取消借款**：`borrowPassBalance` 不再触发，网关交易自备 gas，零成本语义彻底放弃。业务上等于"充值地址必须先有余额"，`settlePassBalance` 的 case 2 不再可达。

**T5-3 legacy 门控补齐**：见上方表格的两处（`interpreter.go:96`、`eip1559.go:63`）。

**forkid 不兼容**：新增 `*Block` 字段会改变 EIP-2124 checksum（`core/forkid` 反射收集），未升级的 pgp 1.9.7 节点握手会 `ErrForkIDRejected`。这是分叉的必然结果，需在公告中写明"ForkHeight 前必须完成升级"。

验收：
1. 已有 pgp 主网 datadir 完整重放（0 → 当前高度），状态根一致；
2. 断言分叉前后预编译集差异只有 `{−0x64, −0x65, −0x67, +0x71, +0x72}`，ELA 的 12 个地址两端都在；
3. 分叉后用零余额账户发一笔 `Recharge`：交易成功、`Receipt.GasUsed == 0`、发送者余额回到 0；
4. 重复提交同一 `Recharge` 被 `errLegacyTxAlreadyCompleted` 拒绝。

### T6 devnet 与测试

1. **devnet 参数**：`ForkHeight = 400`（2 个 epoch），genesis 时间戳设近期；`ParliaBootstrapValidators` 先用临时 12 个地址（主网名单到位后只改 `params/pgp_parlia.go` + 重编译合约）。
2. **起链脚本**：2 节点（1 validator + 1 follower）+ 1 个只回放 legacy 的节点；`--mine` 只给 validator。
3. **断言清单**：
   - 0–399：legacy 出块/同步正常，无 Parlia 日志。
   - 400 起：`seal` 正常、coinbase 在 12 人集合内、difficulty ∈ {1,2}。
   - 400 块状态里 17 个系统合约 code 非空、`alreadyInit == true`。
   - `deposit` 累积到 0x1000；600/800 块 `distributeFinalityReward` 不 revert。
   - 两节点逐块状态根一致（重点 400、600、800）。
   - 400 之后 `eth_getCode(0x64)` 仍是 ELAMinter 合约（不是预编译）；`0x71` 表现为预编译；ELA 12 个预编译地址行为与分叉前一致。
4. **回归**：`go run ./build/ci.go test -short` 与 `go run ./build/ci.go lint`（AGENTS.md 要求）。

### T7 上线与运维

1. **分叉高度**：主网 head + ≥ 2 周缓冲，且必须是 200 的整数倍；一旦定死不可改。
2. **升级包**：新二进制 + BLS 钱包/密码文件 + 启动参数（`--mine`、`--vote`、`--bls.*`）；私钥离线分发。
3. **业务公告**：分叉后 ELA 充提关闭、跨链桥/小额跨链下线；ELAMinter/ELAToken 余额仍可读（state 不变，只是不可调用）。
4. **监控**：seal 失败率、`assembleVoteAttestationSkipCounter`、justified/finalized 滞后、系统合约调用 gas 异常。
5. **回滚边界**：ForkHeight 前可回退旧二进制；之后不可回退（两条链永久分裂）。

## 五、风险清单（按严重度）

| 编号 | 风险 | 影响 | 缓解 |
|---|---|---|---|
| R1 | `Finalize` 与 `finalizeAndAssemble` 的 init/verifyValidators 顺序不一致 | 出块节点与验证节点状态根分叉，链停摆 | 两条路径共用同一个 `bootstrap()` 函数；devnet 逐块比对状态根 |
| R2 | 分叉块 baseFee 从 nil 跳到 1 gwei | 分叉块被全网拒绝 | T2-5 特判 + 专项单测 |
| R3 | `ForkHeight % 200 != 0` | extra 无验证人列表，引导快照不自洽 | T2-3 启动期硬校验 + 单测 |
| R4 | `defaultNet` 无升级数据 | 系统合约不注入，Parlia 调空地址，治理/质押/slash 全失效但链照跑（静默失败） | T3-3 加 `pgpNet` + code 非空断言 |
| R5 | router 不实现 PoSA | `Authorize` 不执行，分叉后 `--mine` 静默失败 | T4-2 backend 解包 + devnet 出块验证 |
| R6 | 验证人/BLS 私钥分发错乱 | 出块人不在集合内 → 全网拒块 | 名单与 BLS 公钥同源生成（`validators.js` 与 `params/pgp_parlia.go` 用同一份 JSON）+ 一致性测试 |
| R7 | 网关语义没跟着改（`legacyGasRefund`/`borrowPassBalance` 仍按块高度门控） | 零余额充值交易的余额回不到借入值 → `settlePassBalance` 返回 `ErrGasLimitReached` → **含该交易的整块被判非法** | T5-2 方案 A；devnet 专项用例"零余额账户充值 + 重复充值" |
| R8 | forkid 变更 | 未升级节点握手失败 | 公告要求全节点在 ForkHeight 前升级 |
| R9 | 重复充值检查（`legacySettledRecharges`）在分叉后失效 | 同一笔 ELA 交易可被处理两次 → 超额铸币 | T5-2 把前置条件改为"块内存在网关交易"，并保留 `errLegacyTxAlreadyCompleted` 单测 |
| R10 | 预编译集是共识的一部分，地址集合差一个就分叉 | 分叉块之后全网行为不一致 | T5-1 的集合等价性断言（差异只允许 `{−0x64,−0x65,−0x67,+0x71,+0x72}`） |

## 六、待评审的设计选项

| 编号 | 选项 | 备选 | 建议 |
|---|---|---|---|
| D1 | 分叉后 gasLimit 保持 8.7e9 | 渐进降到 40M（每块最多降 1/256，约 1376 块 ≈ 3.4 天） | 保持 8.7e9，降 gasLimit 单独做治理动作 |
| D2 | ForkHeight 必须 % 200 == 0 | 允许任意高度，给"分叉块不带验证人列表"写专门分支 | 接受硬校验 |
| D3 | 分叉块把 initContract 提到 verifyValidators 之前 | 分叉块直接跳过 verifyValidators | 前者（保留自校验） |
| D4 | SystemReward 加 `initializeOperators` 合约函数 | 用 `BeforeUpgrade` 钩子直接写存储槽 | 前者（可 trace） |
| D5 | ~~SPV 预编译随网关停用~~ **已定：全部保留**；BSC 的 `tmHeaderValidate`/`iavlMerkleProofValidate` 迁到 0x71/0x72 | 迁到 0x70/0x71（0x70 已被 Jenner 的 milliTimestamp 预留，不推荐） | 新增一张 `PrecompiledContractsPGPParlia` 表，不改 BSC 的 14 套表 |
| D6 | fast finality 与共识切换同版本上线 | 首版只出块，下一版开 finality | 同版本（按已定决策），但 devnet 分两步验证 |
| D7 | 网关交易分叉后走**方案 A**：继续零成本（借 PassBalance + 退到 `gasUsed*gasPrice` 全额） | 方案 B：取消借款，网关交易自备 gas | 方案 A（保持业务连续性；方案 B 会让所有零余额充值脚本失败） |

## 七、需要你提供的输入

1. **12 个验证人**：共识地址 + 收费地址 + voting power + BLS 公钥（或只给地址，用 `geth bls account new` 生成私钥后回填公钥，私钥离线交付）。
2. **主网分叉高度**（devnet 通过后再定）。
3. 确认网关在分叉后的**定价**：方案 A 下充值/提现继续保持零成本（gas 由系统承担，每笔约 `gasUsed × 1 gwei`），是否接受这笔持续支出。

## 八、建议实施顺序

T1 → T2 → T3 →（devnet 先跑通"只出块、无 finality"）→ T5（预编译迁址 + 网关语义）→ T4（开 finality）→ T6 全量回归 → T7 上线。

理由：
- T5 的网关语义（R7/R9）会改变**交易级**共识结果，必须与 T1/T2 同批上线，不能留到"下一个版本"；
- T4 依赖 T1/T2 的出块正确性，先让链跑起来再叠加投票，任何问题都能二分定位。
