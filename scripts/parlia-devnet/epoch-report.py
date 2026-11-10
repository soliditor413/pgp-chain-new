#!/usr/bin/env python3
"""解码 Parlia epoch 块的 extraData，判定它落在哪一种布局上。

两种布局，且 epoch 块可能同时带 attestation（Luban 启用 finality 之后是常态）：
    pre-Luban : vanity(32) + N*20            + [attestation] + seal(65)
    Luban+    : vanity(32) + N(1) + N*68     + [attestation] + seal(65)
Luban 版多出来的那个字节是**席位计数**（实测 4 席时为 0x04，不是固定 0x00），
所以 Luban 布局可以直接从它读出 N，再把到 seal 之前的剩余字节认定为 attestation。
pre-Luban 没有计数字节，只能靠「去掉 seal 后能否被 20 整除」来判断——那种布局下
finality 不存在（Luban 之前没有 attestation），所以不会遇到歧义。
每席 68 B = 20 B 验证人地址 + 48 B BLS 票地址；票地址全零表示该席没有配置 BLS 公钥。

为什么要单独一个脚本：multi.sh 的内联报告只回答「几席」，回答不了
「那 68 字节的后 48 字节是不是 BLS 票地址」。而 F-11a（0x264/0x265 迁移）
和 Luban 一起启用时，恰恰要靠这个偏移去证明 epoch 块带上了票地址。

用法：
    python3 epoch-report.py --port 18560 --height 200
    python3 epoch-report.py --port 18560            # 取当前 head
"""
import argparse
import json
import sys
import urllib.request

# 只连本机节点，不能让环境里的 HTTP(S)_PROXY 接手（macOS 上很常见），
# 否则 127.0.0.1 会被送去上游，得到 502 而不是连接结果。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def rpc(url, method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
    with _OPENER.open(req, timeout=10) as r:
        out = json.load(r)
    if "error" in out:
        raise RuntimeError(out["error"])
    return out["result"]


def classify(total, raw):
    """返回 (layout, 席位, 头部字节数, attestation 字节数)。

    头部 = vanity(+计数字节)。先试 Luban：它自带计数字节，可以直接算出
    「验证人段 + 头 + seal」应为多长，剩下的就是 attestation（可能为 0）。
    """
    if total < 32 + 65:
        return "too-short", 0, 0, 0

    # Luban 优先：第 33 个字节是席位数，据此可以把剩余字节解释成 attestation。
    count = raw[32]
    without_att = 32 + 1 + 68 * count + 65
    if 0 < count and without_att <= total:
        att = total - without_att
        # attestation 要么不存在，要么至少是 VoteAddressSet(≥1) + 96B 签名 +
        # VoteData(≥2 个哈希)，不可能是个零头。
        if att == 0 or att >= 100:
            return "Luban", count, 33, att

    # pre-Luban：无计数字节、也无 attestation，去掉 seal 后应被 20 整除。
    body = total - 32 - 65
    if body % 20 == 0:
        return "pre-Luban", body // 20, 32, 0

    return "unknown", 0, 0, 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18545)
    ap.add_argument("--height", type=int, default=0, help="0 表示取当前 head")
    args = ap.parse_args()
    url = f"http://127.0.0.1:{args.port}"

    h = args.height or int(rpc(url, "eth_blockNumber", []), 16)
    b = rpc(url, "eth_getBlockByNumber", [hex(h), False])
    if not b:
        print(f"块 {h} 不存在", file=sys.stderr)
        return 1

    raw = bytes.fromhex(b["extraData"][2:])
    total = len(raw)
    layout, seats, head_len, att_len = classify(total, raw)
    epoch = int(b.get("number"), 16)

    print(f"块 {epoch}  extra={total} B  txs={len(b['transactions'])}  "
          f"miner={b['miner']}")
    print(f"  布局判定 : {layout}   席位 : {seats}   "
          f"头部 : {head_len} B（32 vanity{' + 1 席位数' if head_len == 33 else ''}）"
          f"{f'   attestation : {att_len} B' if att_len else ''}")
    if layout == "Luban":
        expected = 33 + 68 * seats + 65 + att_len
        print(f"  长度校验 : 33 + 68*{seats} + 65 + {att_len} = {expected} "
              f"{'✓ 与实测一致' if expected == total else '✗ 与实测 %d 不符' % total}")
        if raw[32] != seats:
            print(f"  ✗ 席位数与计数字节矛盾：计数 0x{raw[32]:02x}，按长度算出 {seats} 席")
    elif layout == "pre-Luban":
        expected = 32 + 20 * seats + 65
        print(f"  长度校验 : 32 + 20*{seats} + 65 = {expected} "
              f"{'✓ 与实测一致' if expected == total else '✗ 与实测 %d 不符' % total}")

    if layout == "Luban":
        print(f"  席位数   : 0x{raw[32]:02x} = {raw[32]}")
        print("  逐席（20 B 地址 + 48 B 票地址）：")
        for i in range(seats):
            off = 33 + i * 68
            addr = "0x" + raw[off:off + 20].hex()
            vote = "0x" + raw[off + 20:off + 68].hex()
            zero = "  ← 全零，未设置票地址" if not any(raw[off + 20:off + 68]) else ""
            print(f"    [{i}] {addr}  vote={vote}{zero}")
    elif layout == "pre-Luban":
        print("  逐席（只有 20 B 地址，无票地址槽）：")
        for i in range(seats):
            off = 32 + i * 20
            print(f"    [{i}] 0x{raw[off:off + 20].hex()}")

    print(f"  seal     : 0x{raw[total - 65:].hex()[:32]}…（65 B，末位 {raw[-1]:02x}）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
