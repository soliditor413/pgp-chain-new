#!/usr/bin/env python3
"""打印 Parlia devnet 的块结构摘要，用作 H 高度的验收判据。

用法：
    python3 inspect-blocks.py                    # 打全部块
    python3 inspect-blocks.py --last 12          # 只打最近 12 块
    python3 inspect-blocks.py --wait 200         # 等到高度 200 再打
    python3 inspect-blocks.py --port 18546       # 换端口

为什么不用 `geth console --exec`：console 会在同一个 datadir 上再起一个实例，
读到的 head 与正在运行的节点不同步（实测会只看到 block 0），而且 --exec 里写
多行 JS 的引号转义很容易出错。
"""
import argparse
import json
import sys
import time
import urllib.request

# 这个脚本只连本机节点，不能让环境里的 HTTP(S)_PROXY 接手：macOS 上常见的
# 代理变量会把 127.0.0.1 也送去上游，结果是 502 而不是连接被拒绝。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

URL = "http://127.0.0.1:18545"


def rpc(method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
    with _OPENER.open(req, timeout=10) as r:
        out = json.load(r)
    if "error" in out:
        raise RuntimeError(out["error"])
    return out["result"]


def head():
    return int(rpc("eth_blockNumber", []), 16)


def print_blocks(start, end):
    print(f"{'blk':>5} {'txs':>4} {'extra':>6} {'diff':>5}  coinbase")
    print("-" * 84)
    for i in range(start, end + 1):
        b = rpc("eth_getBlockByNumber", [hex(i), False])
        if not b:
            print(f"{i:>5}  <missing>")
            continue
        extra_len = (len(b["extraData"]) - 2) // 2
        print(f"{i:>5} {len(b['transactions']):>4} {extra_len:>6} "
              f"{int(b['difficulty'], 16):>5}  {b['miner']}")


def print_init_block():
    """block 1（或任意 ForkHeight）的系统交易形状，是 init 注入的直接证据。"""
    b = rpc("eth_getBlockByNumber", ["0x1", True])
    if not b:
        return
    print()
    print(f"block 1 的系统交易（共 {len(b['transactions'])} 笔）:")
    for t in b["transactions"]:
        inp = t["input"]
        print(f"  to={t['to']}  gas={int(t['gas'], 16)}  "
              f"inputLen={(len(inp) - 2) // 2}  selector={inp[:10]}")


def main():
    global URL
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18545)
    ap.add_argument("--last", type=int, default=0, help="只打最近 N 块；0 表示全部")
    ap.add_argument("--wait", type=int, default=0, help="等到这个高度再打印")
    args = ap.parse_args()
    URL = f"http://127.0.0.1:{args.port}"

    if args.wait:
        target = args.wait
        last_report = -1
        while True:
            h = head()
            if h >= target:
                print(f"已到达目标高度 {h} >= {target}")
                break
            # 每涨 20 块报一次，避免刷屏，也避免一条日志都没有让人怀疑卡死
            if h // 20 != last_report:
                print(f"  等待中: {h}/{target}")
                last_report = h // 20
            time.sleep(5)

    h = head()
    print(f"当前高度: {h}")
    print()
    start = 0
    if args.last and h >= args.last:
        start = h - args.last + 1
        print(f"（只显示最近 {args.last} 块，前面 {start} 块省略）")
        print()
    print_blocks(start, h)
    if h >= 1:
        print_init_block()
    return 0


if __name__ == "__main__":
    sys.exit(main())
