# PGP 智能体支付与链外服务接入指南

**对外简介**　PGP 提供兼容 EVM 的智能账户和 AI 工具。用户指定收款方、允许的合约调用、原生币预算和期限，智能体在这些条件内采购服务。服务商可核对链上付款，交付报价对应的数据版本。以下能力已在本地开发环境实现；性能基准和开发网交易不能作为生产网络容量、可用性或审计结论。

## 设计覆盖及适用范围

1. **产品定位与项目目标（设计第 1、2 节）**　在既有 BSC 衍生节点之上部署应用合约。一次授权可支持多笔采购。当前预算以原生币 wei 计量。合约调用绑定准确的 calldata 哈希；通用 ERC-20 支出限额需要对应的代币策略。
2. **安全设计（第 3 节）**　模型在链外选择操作，独立签名器执行本地策略，账户合约再次检查目标、调用数据、预算、授权代次和期限。工具可按金额阈值标记历史付款供模型解释；持续监控、独立审计和运营告警由部署方安排。钱包提供操作记录与回执查询，通用全链浏览器不在此示例中。
3. **AI 接入（第 4 节）**　开发者通过 MCP 接入模型。交易费用可由代付账户承担，批量操作原子执行。此次补齐的服务商 HTTP 接口将报价、付款、最终确认和数据交付串联起来。初始服务使用单买方凭证和文本数据集。
4. **密码升级（第 5 节）**　账户可切换验证器；多签可恢复成员密钥，合约钱包成员可用 ERC-1271 投票。Parlia 最终性投票通过版本化入口验证当前 BLS 聚合签名，附 4、21、64 席基准。生产后量子算法、激活高度、密钥格式和迁移演练要在选型后确定。[密码与治理部署说明](proof-verification/README.md)
5. **隐私计算（第 6 节）**　社区可部署按版本注册的证明验证器，通过任务收件合约提交承诺与证明。链上按 EVM gas 计费，注册时还能限制验证器获得的 gas。证明具体能保证什么取决于验证器。示例采用签名证明；隐私池、私密支付、零知识电路及其审计属于应用方工作。

## 跑通一笔采购

在私有开发网部署并充值 PgpAgentAccount，由所有者为会话地址设置目标商户、空调用数据哈希、预算和到期时间。将同一个节点、会话地址、账户合约配置给代理工具和商户。单独启动 Clef 和会话签名服务，设置代付者及单笔 gas、费用上限。部署命令、配置样例和钱包启动方法见[账户接入说明](agent-account/README.md)。

1. 启动链外文本数据服务，调用 POST /v1/orders，用稳定的 requestId 获取报价。保留原始报价。
2. 比对报价中的 chainId、account、agent 与本地配置。把 taskId、recipient、amountWei 和 data 原样交给付款工具。
3. 依次调用 check_purchase、sign_purchase、simulate_purchase、submit_purchase。将签名加入预演和提交请求。遇到越权，先由用户决定是否修改授权。
4. 把返回的 txHash 和原 taskId 用于 purchase_receipt，设置 includeFinality 为 true。调用交付接口，等待 HTTP 200，再按 UTF-8 对 text 计算 SHA-256，与原始报价比对。HTTP 202 时重查原订单即可。
5. 用户在钱包控制台或 review_purchase_activity 中核对收款方、金额和任务号。应用如采用计算证明，还需查询 service_result。[商户接口、鉴权、启动与重试](offchain-service/README.md)

## MCP 接口

启动 python3 examples/agent-account/agent_tools.py。传输为标准输入输出，每行一条 JSON-RPC 2.0 消息。initialize 返回协议版本 2024-11-05，tools/list 返回参数 JSON Schema 和只读标识。tools/call 的业务结果位于 result.content[0].text，按 JSON 解码。先检查 result.isError，再检查业务 status；业务拒绝也可能是一次正常的工具调用。

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"inspect_grant","arguments":{}}}
```

下文的“采购参数”指 recipient（20 字节地址）、taskId（32 字节任务号）、amountWei（十进制字符串）及可选 data（带 0x 前缀的偶数位十六进制，默认 0x）。“已签采购”在此基础上增加 signature（65 字节）。金额使用字符串以避免客户端整数精度丢失。

| 工具 | 输入 | 主要输出与使用条件 |
| --- | --- | --- |
| inspect_grant | 空对象 | status、recipient、callDataHash、remainingWei、expiresAt、generation、blockHash。用于采购前确认权限 |
| check_purchase | 采购参数 | within_grant、already_paid、needs_authorization 或 alternate_signature_required。拒绝时读取 reasons 或签名方案 |
| prepare_purchase | 采购参数 | 验证通过时返回 EIP-712 typedData；可交给实现相同策略的独立签名器 |
| sign_purchase | 采购参数 | 经 PGP_SIGNER_SOCKET 调用签名服务；成功返回 signed、signature。任务或会话须符合签名器本地策略 |
| simulate_purchase | 已签采购 | simulated、transaction、operations、feeEstimate、blockHash，用于预览调用及原生币影响 |
| submit_purchase | 已签采购 | 成功返回 submitted、txHash、taskId。代付超限返回 sponsor_budget_exceeded |
| prepare_purchase_batch | purchases 数组，1 至 32 项采购参数 | 同一规范区块检查总预算；返回各项 typedData，重复任务返回 duplicate_task |
| simulate_purchase_batch | purchases 数组，每项为已签采购 | 预演整笔批交易，须配置 PGP_BATCH |
| submit_purchase_batch | 同上 | 返回一个 txHash 和 taskIds；每项使用自己的 taskId 查询同一交易 |
| purchase_receipt | txHash、taskId、可选布尔 includeFinality | payment_included 返回匹配事件的收款方和金额。提供费用字段时，费用是整笔交易费用；开启最终性查询后检查 finality |
| review_purchase_activity | 可选 endBlock 整数、reviewAboveWei 十进制字符串 | 返回最多 500 个区块内的 payments、总额、previousEndBlock；分页使用返回的前一截止高度 |
| service_result | taskId | 从 PGP_RESULT_INBOX 读取 unrequested、awaiting_proof 或 result_verified，并在验证后返回 outputCommitment |

**重试规则**　submission_unconfirmed 表示节点可能已收到提交。保留原任务号，先核对链上支出和代付账户交易。awaiting_receipt 可轮询；reverted 表示交易执行失败；no_matching_payment 表示没有找到该账户、会话和任务的付款事件。chain_changed 需要重查规范链。finality 为 unknown 或 awaiting_finality 时延迟不可逆交付。链上付款成功与服务输出通过验证是两个独立条件。

## 合约与维护入口

| 组件 | 如何实现与使用 |
| --- | --- |
| PgpAgentAccount | 所有者调用 authorize(agent,target,dataHash,budget,expiry)，用 setPaused 或 revoke 管理会话。digest 绑定链、账户、会话、代次、任务和金额。pay 结算并发出 Paid，settled(taskId) 记录去重。setSignaturePolicy 切换后使用 payWithSignature |
| PgpPurchaseBatch | settle(account,agent,payments) 顺序调用账户，任一项失败会回滚整批，包括已扣预算 |
| PgpAccountCouncil | 成员对相同 target、data、salt 调用 vote。达到 quorum 后等待 delay，再 execute；rotate 通过理事会对自身的提案更换成员。relayVote 支持合约成员的签名投票 |
| PgpProofRegistry | 治理方 register(scheme,version,verifier,gasLimit) 注册不可复用的版本；disable 停用。应用调用 verify(scheme,version,statement,proof)，必须检查返回布尔值 |
| PgpResultInbox | 指定 requester 调用 request(taskId,inputCommitment)，服务方按 statement 生成证明并 submit。读取 results 或监听 Verified 获取结果承诺 |
| 会话签名器 | session_signer.py 从受限 Unix 套接字读取意图，复核本地策略和链上权限，再通过 Clef IPC 请求签名。主密钥、策略写权限和 Clef IPC 由可信运行环境保管 |
| 钱包 | console.html 使用浏览器 EVM 钱包连接指定链。支持直接授权、多签提案、恢复、历史支出和回执最终性查询 |

合约的精确参数类型、返回类型、事件索引可从编译 ABI 获取。在 examples 目录运行 forge build，再运行例如 forge inspect PgpAgentAccount abi。单元验证使用 python3 -B -m unittest discover -s examples/agent-account 和 python3 -B -m unittest discover -s examples/offchain-service（仓库根目录），合约验证在 examples 目录运行 forge test。
