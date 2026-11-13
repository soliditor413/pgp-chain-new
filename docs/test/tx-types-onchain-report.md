# 五种交易类型上链验证报告（私链，PBFT→Parlia 分叉）

目标：验证 `LegacyTxType(0x00)`、`AccessListTxType(0x01)`、`DynamicFeeTxType(0x02)`、`BlobTxType(0x03)`、`SetCodeTxType(0x04)` 在分叉前后能否上链并成功执行，并给出前后行为差异。

结论一句话：**0/1/2 全程可用；3/4 的可用性由 Cancun/Prague 时间戳决定（不是 ForkHeight），分叉后未到时间同样被拒，到了时间全部成功。** 期间发现一处 legacy 段的费用口径不一致（不影响资金，影响回执读数）。

---

## 0. 结论摘要

| 类型 | legacy（1..99） | pre-cancun（100..168） | post-cancun（≥169） |
|---|---|---|---|
| 0x00 Legacy | ✅ 上链 block 20 | ✅ block 102 | ✅ block 509 |
| 0x01 AccessList | ✅ block 22 | ✅ block 103 | ✅ block 515 |
| 0x02 DynamicFee | ✅ block 24（见 §5.1 费用口径） | ✅ block 104 | ✅ block 520 |
| 0x03 Blob | ⛔ 池层拒绝 | ⛔ 池层拒绝 | ✅ block 535，`N%5==0`，`blobGasUsed=131072` |
| 0x04 SetCode | ⛔ 池层拒绝 | ⛔ 池层拒绝 | ✅ block 540，代码变 `0xef0100‖0x…1000`，nonce +2 |
| 0x05 未知类型 | ⛔ 解码拒绝 | ⛔ 解码拒绝 | ⛔ 解码拒绝 |

矩阵 18 格（3 窗口 × 6 类型）中，**17 格按预期通过**；剩余 1 格（legacy/type 2）交易本身成功，判定失败源于工具首版的费用基准写错，算术已闭合（§5.1），不是链的问题。

---

## 1. 测试环境

| 项 | 值 |
|---|---|
| 链 | 8 节点 / 4 个 PBFT 生产者（node0-3）+ 4 个 POSA 验证人（node4-7）/ cabinets 4 |
| `ForkHeight` | **100**（T0+300 附近） |
| `SystemContractDeployHeight` | 20 |
| Cancun..Fermi | 同一时间戳 **T0+600**（= 1794613200），经 `--bsc-time-forks` 整组启用 |
| breathe | 2592000s（窗口内不触发 `updateValidatorSetV2`） |
| chainId | 860621（系统合约里编死的 bscChainID） |
| 余额 | 每账户 10000 ETH |
| 费率分成 | `gasFeeRecipientRatio = 65`，sink = `0x…fee5` |
| 复现入口 | `PGP_STAKING_HOME=<dir> ./scripts/staking-devnet/run.sh init f` |

### 1.1 两次分叉的实测边界

| 块 | ts | extraLen | 出块人 | baseFee | blob/beacon/requests |
|---|---|---|---|---|---|
| 99（最后 legacy） | 1794612990 | **407** | node2（PBFT 生产者） | **无** | 无 |
| 100（首个 Parlia） | 1794612993 | **97** | node5（POSA） | **0** | 无 |
| 101 | 1794612996 | 269（追加 BLS 票） | node6 | 0 | 无 |
| 169（首个 Cancun/Prague） | **1794613200** = `cancunTime` | 269 | node6 | 0 | **全部出现** |

`ForkHeight=100` 与 `cancunTime=1794613200` 两个边界都精确落在配置值上：前者改共识与头形态（407→97、baseFee 从无到 0），后者改头字段集合（requestsHash 等出现）。

---

## 2. 门禁机制（为什么 3/4 在分叉后还会被拒）

四层叠加，逐层可观测：

| 层 | 判据 | 拒绝表现 |
|---|---|---|
| EIP-2718 解码 | 只认 0x01..0x04；0x00 走裸 RLP | `transaction type not supported`（不入池） |
| 子池位图 | 0/1/2/4 → legacy 池；3 → blob 池 | `received type N` |
| 池层 fork 门禁 | head 的 (number, time) 对 `IsBerlin/IsLondon/IsCancun/IsPrague` | `type 3 rejected, pool not yet in Cancun` |
| 区块执行 | `MakeSigner(number, time)` 的签名器能力位图 | 导入时 `transaction type not supported` |

`IsPGPLegacy` 不参与任何类型判断（只改费用归属、gas 退款、EIP-3607/3860/3529、bloom），因此 **0/1/2 在 legacy 段天然可用**，而 3/4 完全取决于 Cancun/Prague 的时间戳。

---

## 3. 矩阵明细（证据取自 `txtypes/results.jsonl`）

| 窗口 | 类型 | 结果 | 块 | 回执 type | gasUsed | 备注 |
|---|---|---|---|---|---|---|
| legacy | 0 | accepted | 20 | 0 | 21000 | 余额闭式吻合 |
| legacy | 1 | accepted | 22 | 1 | 25300 | access list 生效（+4300 gas） |
| legacy | 2 | accepted | 24 | 2 | 21000 | 手续费按 feeCap 扣（§5.1） |
| legacy | 3 | rejected | — | — | — | `… type 3 rejected, pool not yet in Cancun` |
| legacy | 4 | rejected | — | — | — | `… type 4 rejected, pool not yet in Prague` |
| legacy | 5 | rejected | — | — | — | `transaction type not supported` |
| pre-cancun | 0/1/2 | accepted | 102/103/104 | 0/1/2 | 21000/25300/21000 | 分叉后、Cancun 前 |
| pre-cancun | 3/4 | rejected | — | — | — | 与 legacy 相同的错误串 |
| post-cancun | 0/1/2 | accepted | 509/515/520 | 0/1/2 | 同上 | |
| post-cancun | 3 | accepted | **535**（535%5=0） | 3 | 21000 | `blobGasUsed=131072`、`blobGasPrice=1`、块 `excessBlobGas=0` |
| post-cancun | 4 | accepted | 540 | 4 | 36844 | 代码 `0xef0100…1000`、nonce +2、余额闭式吻合 |
| post-cancun | 5 | rejected | — | — | — | 解码层拒绝，pendingNonce 不变 |

每格都核对了五段：签名可还原（`Sender()` 自检）→ 发送返回 hash → 回执存在且 `blockNumber` 落在期望窗口 → `status=1` 且 `receipt.type` 等于期望 → 按类型的效果断言（余额闭式 / blob 字段 / 委托代码 + nonce）。

---

## 4. 分叉前后差异（实测对照）

| 差异 | legacy 段 | 分叉后 | 证据 |
|---|---|---|---|
| 3/4 可用性 | 池层拒 | Cancun/Prague 前仍拒；之后放行 | 同一 tx 三种结果，错误串逐字相同 |
| 头形态 | extra 407B、**无 baseFee** | extra 97/269B、**baseFee=0** | block 99 vs 100 |
| 费用归属 | `payLegacyFee`：65% 给 `0x…fee5`，其余给 coinbase | 整笔进 `consensus.SystemAddress` | sink 余额 114,790,000,000,000 与 §5.1 的算术完全吻合 |
| 1559 扣费基准 | **按 `gasFeeCap` 实扣**，回执 `effectiveGasPrice` 报 `gasTipCap` | 按 `min(feeCap, tip+baseFee)` = tip，回执与实扣一致 | 见 §5.1 |
| EIP-3607（有代码账户能否发交易） | 关闭 | 开启 | `state_transition.go:474` 的 `isLegacy` 分支 |
| gas 退款 | 全额退 + 退款系数固定 `gasUsed/2` | EIP-3529 口径 | 代码路径，本轮未构造差别化用例 |

---

## 5. 异常与说明

### 5.1 legacy 段的 1559 费用口径不一致（已定量确认，不影响资金安全）

现象：legacy/type-2 那笔交易，sender 实扣 `value + 21000×2gwei`，而回执里 `effectiveGasPrice = 1gwei`，按回执算出来的闭式少了 21,000,000,000,000 wei。

根因：legacy 段的 header **没有 `BaseFee` 字段**，`core/state_transition.go:291` 的 `TransactionToMessage` 走 `baseFee == nil` 分支，把 `msg.GasPrice` 设为 **`tx.GasFeeCap()`**；`buyGas` 按 `gasLimit × msg.GasPrice` 扣费，`effectiveTip`（`:753`）也直接等于该值，因此**扣费和分配都按 feeCap**，而回执的 `effectiveGasPrice` 是按 `gasTipCap` 报的。

定量验证：legacy 段（1..99）共 6 笔用户交易，按 feeCap 计的总手续费 176,600,000,000,000 wei，其 65% = **114,790,000,000,000**，与 fee sink 实际余额**逐 wei 相等**。所以：

- 没有资金丢失或凭空产生，费用按实扣值完整分配；
- 但**回执的 `effectiveGasPrice` 在 legacy 段不可作为扣费依据**——任何按该字段做账的工具（包括本报告工具的第一版）都会少算 `gasUsed × (feeCap − tip)`；
- 分叉后 header 有 `BaseFee=0`，effective 价格 = `min(feeCap, tip)`，两者一致，问题消失。

这是"分叉前后行为差异"里最容易被忽略的一条，建议在主网上线前确认是修（让 legacy 头也带 BaseFee）还是记录为已知偏差。

### 5.2 工具首版的两处断言错误（已修，不影响结论）

| 现象 | 根因 | 修正 |
|---|---|---|
| blob 交易 sender 差额少 131072 | 闭式漏了 blob 费 `blobGasUsed × blobGasPrice` | 显式计入 |
| setcode 代码断言失败但 nonce +2 | 未传 `--delegate`，默认零地址 = **清除委托**，代码为空是正确行为 | 传 `0x…1000`；`--delegate 0x0` 单独作为"清除"用例 |

两处都是工具口径问题，链上执行自始至终正确（`status=1`、`receipt.type` 正确）。

### 5.3 已知限制

| 项 | 说明 |
|---|---|
| blob 只在 `N % 5 == 0` 的块打包 | `BlobEligibleBlockInterval=5`，devnet 上等 ≤5 块 |
| blob sidecar 只支持 v0 | BSC 禁用 PeerDAS(v1)，`validation.go:167-181` 强制 |
| devnet 非 archive | 历史 `eth_call` 约 128 块后失效；本报告所有断言都基于回执/余额，不受影响 |
| 区块层拒绝 | 无法用 RPC 触发（池已拒），由 Go 单测覆盖（§6） |

---

## 6. 负向用例（实链）

| 用例 | 结果 | 关键证据 |
|---|---|---|
| setcode 授权 nonce 错（`--auth-nonce-off 0`） | ✅ 符合预期 | `status=1` 但代码保持 `0x`、nonce 只 +1 → 证明授权失败被**静默跳过** |
| setcode 空授权列表 | ✅ 符合预期 | 池层拒：`set code tx must have at least one authorization tuple` |
| 授权到零地址（清除委托） | ✅ 符合预期 | 代码由 `0xef0100…1000` 变回 `0x`，nonce +2 |
| tip 低于门槛（1 wei） | ✅ 符合预期 | `transaction gas price below minimum: gas tip cap 1, minimum needed 50000000` |

最后一条实证了参数事实：**开矿后池的入池门槛就是 50,000,000 wei**（`StartMining` 会把 miner 的 `GWei/20` 灌进池的 `MinTip`），不是"入池但不打包"。

Go 单测（区块层 + 池层，确定性强于实链窗口）：

```bash
go test ./core/ -run TestTxType        # MakeSigner/TransactionToMessage 在 fork 前后的取舍
go test ./core/txpool/ -run TestTxTypeForkGateAtSubmission
```

---

## 7. 复现

```bash
cd bsc
export PGP_STAKING_HOME=<workspace>/.devnet/f
./scripts/staking-devnet/run.sh build          # 预编译 txtypes（W0 只有 ~300s）
./scripts/staking-devnet/run.sh init f
./scripts/staking-devnet/run.sh prepare
./scripts/staking-devnet/run.sh start          # 另开终端
./scripts/staking-devnet/run.sh gate
./scripts/staking-devnet/run.sh matrix --window legacy
./scripts/staking-devnet/run.sh phase --wait pre-cancun
./scripts/staking-devnet/run.sh matrix --window pre-cancun
./scripts/staking-devnet/run.sh phase --wait post-cancun
./scripts/staking-devnet/run.sh matrix --window post-cancun
```

单笔：`./scripts/staking-devnet/run.sh txtypes --type 3`（`--node` 可换账户，`--expect reject` / `--expect-error` / `--expect-auth-skipped` 用于负向）。

---

## 8. 结论

1. **五种交易类型在分叉后（且 Cancun/Prague 生效后）全部能正常上链执行**，回执 `status=1`、`type` 与交易一致、执行效果可量化。
2. **分叉本身不改变类型可用性**——`ForkHeight` 只改共识与头形态；3/4 由时间型分叉决定，分叉与 Cancun 之间的窗口里它们仍被拒，错误串与 legacy 段逐字相同。
3. **0/1/2 在 legacy 段即可用**，但 type 2 的扣费按 `gasFeeCap` 而非 `gasTipCap`，回执字段与实扣不一致（§5.1），这是本轮最值得跟进的一条偏差。
4. blob 与 setcode 的特有语义（`N%5` 打包、sidecar v0、授权静默跳过、自授权 nonce +2）均已按预期工作，负向用例全部符合预期。
