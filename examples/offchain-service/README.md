# PGP 链外数据服务

服务商可以按固定价格出售一份 UTF-8 文本数据。买方取得报价，使用智能账户向报价指定地址付款，再凭交易哈希领取报价时的数据版本。商户只需读取可信节点，无需持有买方私钥。当前实现服务于一个预先配置的买方账户和会话地址，适合本地集成与演示。

## 启动

在仓库根目录运行。将账户、会话地址、收款地址和链 ID 换成实际部署值。节点需要提供历史交易回执和 finalized 区块查询。商户应独立配置可信的 PGP_RPC_URL。

```sh
export PGP_RPC_URL=http://127.0.0.1:18560
export PGP_ACCOUNT='填写智能账户地址'
export PGP_AGENT='填写会话地址'
export MERCHANT_ADDRESS='填写商户收款地址'
export PGP_SERVICE_TOKEN="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
python3 examples/offchain-service/service.py --database orders.sqlite \
  --dataset dataset.txt --price-wei 60 --recipient "$MERCHANT_ADDRESS" --chain-id 860621
```

启动前准备好 dataset.txt。以下买方命令在另一个终端执行，该终端需要通过可信渠道取得与服务端相同的 PGP_SERVICE_TOKEN。服务默认监听 127.0.0.1:8787。数据库保存报价时的文本、任务编号、价格和首次通过核验的交易哈希，重启时应使用原数据库。数据库文件包含明文交付内容。接入公网需要由部署方提供 TLS、请求限速、存储配额、备份和独立的客户身份隔离。

## 报价与交付接口

两个接口均接收 JSON 对象，使用 Authorization: Bearer 请求头，正文上限 4096 字节。凭证持有人可以代表配置的买方领取数据。

```sh
ORDER_DIR=$(mktemp -d)
curl -fsS -o "$ORDER_DIR/quote.json" http://127.0.0.1:8787/v1/orders \
  -H "Authorization: Bearer $PGP_SERVICE_TOKEN" -H 'Content-Type: application/json' \
  -d '{"requestId":"dataset-order-001"}'
TASK_ID=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["taskId"])' "$ORDER_DIR/quote.json")
cat "$ORDER_DIR/quote.json"
```

首次返回 201，重复使用同一 requestId 返回 200 和原报价。requestId 长度为 1 至 80 个字符，可用英文字母、数字、下划线和连字符。返回的 chainId、account、agent 标明付款上下文。将 taskId、recipient、amountWei、data 四个字段传给智能体付款工具。amountWei 是以 wei 为单位的十进制字符串，data 为 0x。sha256 是交付文本按 UTF-8 编码后的 SHA-256 十六进制摘要。

```sh
# 将 submit_purchase 成功返回的 txHash 填入 TX_HASH，继续使用报价所在的终端。
TX_HASH='填写实际交易哈希'
curl -sS -o "$ORDER_DIR/delivery.json" -w '%{http_code}\n' "http://127.0.0.1:8787/v1/orders/$TASK_ID/delivery" \
  -H "Authorization: Bearer $PGP_SERVICE_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"txHash\":\"$TX_HASH\"}"
```

服务核对链 ID、智能账户发出的 Paid 事件、会话地址、任务编号、收款人、金额和最终确认状态。200 delivered 返回 taskId、txHash、sha256、text。重复请求同一交易返回原文本。202 表示等待回执、链头变化或等待最终确认，用原任务和交易哈希重查即可。400 表示输入有误，401 表示凭证不匹配，404 表示任务或路径不存在，409 表示凭据与订单不匹配或订单上下文有变化，503 表示节点或存储暂时不可用。

付款事件证明资产转移，文本摘要用于核对交付版本。需要额外计算证明的应用可以使用仓库中的 PgpResultInbox。此数据服务执行的是文本交付，未自动生成零知识证明。

## 买方验收与故障定位

收到 HTTP 200 后，可在同一终端核对订单、交易和内容。摘要计算对象是 JSON 解码后的 text 字段按 UTF-8 编码所得的字节。保留原报价，避免误用其他订单的摘要。

```sh
python3 - "$ORDER_DIR" "$TX_HASH" <<'PYTHON'
import hashlib, json, sys
from pathlib import Path

order = Path(sys.argv[1])
quote = json.loads((order / "quote.json").read_text(encoding="utf-8"))
delivery = json.loads((order / "delivery.json").read_text(encoding="utf-8"))
if (delivery.get("status") != "delivered" or delivery.get("taskId") != quote["taskId"]
        or delivery.get("txHash") != sys.argv[2].lower()):
    raise SystemExit("交付状态、任务号或交易哈希不匹配")
digest = hashlib.sha256(delivery["text"].encode("utf-8")).hexdigest()
if digest != quote["sha256"] or digest != delivery["sha256"]:
    raise SystemExit("交付内容摘要与报价不一致")
print("交付已核对", quote["taskId"])
PYTHON
```

| 响应 | 发生条件 | 买方或商户应检查什么 |
| --- | --- | --- |
| 202 awaiting_receipt / chain_changed | 节点尚未返回回执，或回执区块已变化 | 稍后用原 taskId 和 txHash 再查 |
| 202 awaiting_finality | 付款已匹配，最终确认尚未达到或节点无法提供最终性 | 用 purchase_receipt 打开 includeFinality；持续为 unknown 时检查节点能力 |
| 409 payment_mismatch | 交易回滚、缺少匹配事件、收款人或金额不符 | 对照原报价和付款回执，查明原因后再决定是否需要付款 |
| 409 order_context_changed | 当前配置的账户、会话或链与已保存的报价不同 | 商户核对启动配置和数据库，恢复对应的订单环境 |
| 409 different_settlement | 此订单已用另一笔交易完成交付 | 用首次成功的交易哈希领取 |
| 503 wrong_node_chain / service_unavailable | 节点链 ID 与配置不同，或读取节点、数据、存储时失败 | 核对 RPC、数据文件和数据库可用性，再重试原请求 |

**重启与版本更新。** 商户更换价格、收款地址或 dataset.txt 后，已有 requestId 仍返回数据库中的原报价和内容。购买新版请使用新的 requestId。已成功交付的订单用相同交易哈希重取时，服务直接读取已保存的交付记录。备份与恢复需保留订单数据库，否则原任务号无法定位。
