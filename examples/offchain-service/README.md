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

先准备好 dataset.txt 和 MERCHANT_ADDRESS。服务默认监听 127.0.0.1:8787。数据库保存报价时的文本、任务编号、价格和首次通过核验的交易哈希，重启时应使用原数据库。数据库文件包含明文交付内容。接入公网需要由部署方提供 TLS、请求限速、存储配额、备份和独立的客户身份隔离。

## 报价与交付接口

两个接口均接收 JSON 对象，使用 Authorization: Bearer 请求头，正文上限 4096 字节。凭证持有人可以代表配置的买方领取数据。

```sh
curl -sS http://127.0.0.1:8787/v1/orders \
  -H "Authorization: Bearer $PGP_SERVICE_TOKEN" -H 'Content-Type: application/json' \
  -d '{"requestId":"dataset-order-001"}'
```

首次返回 201，重复使用同一 requestId 返回 200 和原报价。requestId 长度为 1 至 80 个字符，可用英文字母、数字、下划线和连字符。返回的 chainId、account、agent 标明付款上下文。将 taskId、recipient、amountWei、data 四个字段传给智能体付款工具。amountWei 是以 wei 为单位的十进制字符串，data 为 0x。sha256 是交付文本按 UTF-8 编码后的 SHA-256 十六进制摘要。

```sh
curl -sS "http://127.0.0.1:8787/v1/orders/$TASK_ID/delivery" \
  -H "Authorization: Bearer $PGP_SERVICE_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"txHash\":\"$TX_HASH\"}"
```

服务核对链 ID、智能账户发出的 Paid 事件、会话地址、任务编号、收款人、金额和最终确认状态。200 delivered 返回 taskId、txHash、sha256、text。重复请求同一交易返回原文本。202 表示等待回执、链头变化或等待最终确认，用原任务和交易哈希重查即可。400 表示输入有误，401 表示凭证不匹配，404 表示任务或路径不存在，409 表示凭据与订单不匹配或订单上下文有变化，503 表示节点或存储暂时不可用。

付款事件证明资产转移，文本摘要用于核对交付版本。需要额外计算证明的应用可以使用仓库中的 PgpResultInbox。此数据服务执行的是文本交付，未自动生成零知识证明。
