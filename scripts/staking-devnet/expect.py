#!/usr/bin/env python3
"""expect.py -- read-only assertions against a running pgp staking devnet.

Every subcommand prints JSON (one object per line where it makes sense) so the
shell wrappers never have to parse cast's human-readable output.

The chain is the source of truth: no state is cached here, and every call is
made against `latest` (a devnet is not an archive node, so historical
`eth_call` silently fails after ~128 blocks).
"""

import argparse
import json
import subprocess
import sys
import urllib.request

PROXY = urllib.request.build_opener(urllib.request.ProxyHandler({}))

VALIDATOR = "0x0000000000000000000000000000000000001000"
SLASH = "0x0000000000000000000000000000000000001001"
STAKE_HUB = "0x0000000000000000000000000000000000002002"


def rpc(port, method, params=None):
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}",
        json.dumps({"jsonrpc": "2.0", "method": method, "params": params or [], "id": 1}).encode(),
        {"Content-Type": "application/json"},
    )
    out = json.loads(PROXY.open(req, timeout=20).read())
    if "error" in out:
        raise SystemExit(f"rpc {method} failed: {out['error']}")
    return out["result"]


def cast_call(port, to, sig, *args):
    cmd = ["cast", "call", to, sig, *args,
           "--rpc-url", f"http://127.0.0.1:{port}", "--json"]
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode != 0:
        raise SystemExit(f"cast call failed: {' '.join(cmd)}\n{p.stderr.strip()}")
    try:
        return json.loads(p.stdout)
    except json.JSONDecodeError:
        raise SystemExit(f"cast returned non-JSON: {p.stdout!r}")


def to_int(v):
    if isinstance(v, bool):
        return int(v)
    if isinstance(v, int):
        return v
    s = str(v).strip()
    return int(s, 0)


def to_bool(v):
    """cast --json 对单个 bool 有时给 true，有时给 ["true"]。

    直接 bool(v) 是个陷阱：bool(["false"]) 为 True，会让「验证人已被踢出」这类
    断言永远成立。所以必须按值解析，而不是按真值转换。
    """
    while isinstance(v, list):
        if len(v) != 1:
            raise SystemExit(f"不可解析的 bool: {v!r}")
        v = v[0]
    if isinstance(v, bool):
        return v
    s = str(v).strip().lower()
    if s in ("true", "0x1", "1"):
        return True
    if s in ("false", "0x0", "0"):
        return False
    raise SystemExit(f"不可解析的 bool: {v!r}")


def block(port, num):
    tag = num if isinstance(num, str) else hex(num)
    return rpc(port, "eth_getBlockByNumber", [tag, False])


def head(port):
    return int(rpc(port, "eth_blockNumber"), 16)


def cmd_head(a):
    print(json.dumps({"head": head(a.port)}))


def cmd_block(a):
    b = block(a.port, a.number)
    if b is None:
        raise SystemExit(f"block {a.number} not found")
    print(json.dumps({
        "number": int(b["number"], 16),
        "hash": b["hash"].lower(),
        "miner": b["miner"].lower(),
        "timestamp": int(b["timestamp"], 16),
        "extraLen": (len(b["extraData"]) - 2) // 2,
        "gasUsed": int(b["gasUsed"], 16),
        "txs": len(b.get("transactions") or []),
    }))


def cmd_hashes(a):
    """Cross-node consistency: block hash at a fixed height on every listed port.

    `number` may be "head" so the caller compares liveness without pinning a
    height it cannot know in advance.
    """
    rows = []
    for port in [int(x) for x in a.ports.split(",") if x.strip()]:
        n = head(port) if a.number == "head" else int(a.number)
        b = block(port, n)
        rows.append({"port": port, "number": n,
                     "hash": b["hash"].lower() if b else None,
                     "miner": b["miner"].lower() if b else None})
    hashes = {r["hash"] for r in rows}
    print(json.dumps({"rows": rows, "distinctHashes": len(hashes),
                      "agree": len(hashes) == 1 and None not in hashes}))


def cmd_election(a):
    raw = cast_call(a.port, STAKE_HUB,
                    "getValidatorElectionInfo(uint256,uint256)(address[],uint256[],bytes[],uint256)",
                    str(a.offset), str(a.limit))
    addrs, powers, votes, total = raw
    print(json.dumps({
        "validators": [x.lower() for x in addrs],
        "powers": [str(to_int(x)) for x in powers],
        "voteAddrs": list(votes),
        "total": to_int(total),
    }))


def cmd_mining(a):
    addrs, votes = cast_call(a.port, VALIDATOR, "getMiningValidators()(address[],bytes[])")
    print(json.dumps({"validators": [x.lower() for x in addrs], "voteAddrs": list(votes)}))


def cmd_current(a):
    # 单返回值的动态数组，cast --json 会多包一层：[["0x..","0x.."]]
    addrs = cast_call(a.port, VALIDATOR, "getValidators()(address[])")
    while isinstance(addrs, list) and len(addrs) == 1 and isinstance(addrs[0], list):
        addrs = addrs[0]
    print(json.dumps({"validators": [x.lower() for x in addrs]}))


def cmd_cabinets(a):
    # cast call --json 对单返回值有时给 "4"，有时给 ["4"]，两种都要吃下。
    v = cast_call(a.port, VALIDATOR, "numOfCabinets()(uint256)")
    if isinstance(v, list):
        v = v[0]
    print(json.dumps({"cabinets": to_int(v)}))


def cmd_indicator(a):
    exist, count, height = cast_call(a.port, SLASH, "indicators(address)(bool,uint256,uint256)", a.addr)
    print(json.dumps({"exist": to_bool(exist), "count": to_int(count), "height": to_int(height)}))


def cmd_vbasic(a):
    created, jailed, jail_until = cast_call(
        a.port, STAKE_HUB, "getValidatorBasicInfo(address)(uint256,bool,uint256)", a.addr)
    print(json.dumps({"createdTime": to_int(created), "jailed": to_bool(jailed),
                      "jailUntil": to_int(jail_until)}))


def cmd_iscurrent(a):
    print(json.dumps({"isCurrent": to_bool(cast_call(
        a.port, VALIDATOR, "isCurrentValidator(address)(bool)", a.addr))}))


def cmd_code(a):
    code = rpc(a.port, "eth_getCode", [a.addr, "latest"])
    print(json.dumps({"bytes": (len(code) - 2) // 2}))


def cmd_balance(a):
    wei = int(rpc(a.port, "eth_getBalance", [a.addr, "latest"]), 16)
    print(json.dumps({"wei": str(wei), "eth": wei / 10**18}))


def cmd_producers(a):
    rows = []
    for n in range(a.start, a.end + 1):
        b = block(a.port, n)
        if not b:
            continue
        rows.append({"number": n, "miner": b["miner"].lower()})
    counts = {}
    for r in rows:
        counts[r["miner"]] = counts.get(r["miner"], 0) + 1
    print(json.dumps({"rows": rows, "counts": counts, "distinct": len(counts)}))


def cmd_breathe(a):
    """Heights in [start, head] where floor(t/interval) changes.

    This is the exact predicate the engine uses (feynmanfork.go:24-30), so it
    needs no extra flag from the node.
    """
    h = head(a.port)
    out = []
    prev = None
    for n in range(a.start, h + 1):
        b = block(a.port, n)
        if not b:
            continue
        t = int(b["timestamp"], 16)
        cur = t // a.interval
        if prev is not None and cur != prev:
            out.append({"number": n, "timestamp": t})
        prev = cur
    print(json.dumps({"head": h, "breathes": out}))


def cmd_epochset(a):
    """Decode the validator list out of an epoch block's extraData (post-Luban)."""
    b = block(a.port, a.number)
    if not b:
        raise SystemExit(f"block {a.number} not found")
    raw = bytes.fromhex(b["extraData"][2:])
    if len(raw) < 33 + 65:
        raise SystemExit(f"extra too short: {len(raw)}")
    count = raw[32]
    # post-Luban: vanity(32) + count(1) + count*68 + turnLength(1) + seal(65)
    # + optional vote attestation. The attestation is a suffix, so the validator
    # list is always the first count*68 bytes after the count byte.
    start = 33
    end = start + count * 68
    if end > len(raw):
        raise SystemExit(f"cannot decode extra: len={len(raw)} count={count}")
    layout = "luban"
    addrs = ["0x" + raw[start + i * 68: start + i * 68 + 20].hex() for i in range(count)]
    print(json.dumps({"number": a.number, "layout": layout, "validators": addrs,
                      "suffixBytes": len(raw) - end}))


def cmd_window(a):
    """How far the chain is from the next breathe boundary.

    This is the number that matters operationally: `updateValidatorSetV2` only
    runs on a breathe block, so the first post-fork breathe block is the last
    moment a chain with an empty StakeHub can be rescued (see the report §8).
    Predictive on purpose -- it must work before the boundary arrives.
    """
    h = head(a.port)
    b = block(a.port, h)
    if b is None:
        raise SystemExit(f"block {h} not found")
    t = int(b["timestamp"], 16)
    next_ts = (t // a.interval + 1) * a.interval
    # observed block spacing, so the ETA does not assume 3s forever
    n = min(a.sample, h - 1)
    step = 3.0
    if n > 0:
        prev = block(a.port, h - n)
        if prev:
            step = (t - int(prev["timestamp"], 16)) / n
    print(json.dumps({
        "head": h,
        "headTimestamp": t,
        "interval": a.interval,
        "nextBreatheTs": next_ts,
        "secondsUntil": next_ts - t,
        "blockSeconds": round(step, 3),
        "blocksUntil": int((next_ts - t) / step) if step > 0 else None,
    }))


def cmd_txscan(a):
    """Find txs in a block paying `to` with a given 4-byte selector."""
    b = rpc(a.port, "eth_getBlockByNumber", [hex(a.number), True])
    if not b:
        raise SystemExit(f"block {a.number} not found")
    hits = []
    for tx in b.get("transactions") or []:
        if (tx.get("to") or "").lower() != a.to.lower():
            continue
        data = tx.get("input") or tx.get("data") or ""
        if data.lower().startswith(a.selector.lower()):
            hits.append({"hash": tx["hash"], "selector": data[:10], "from": tx["from"]})
    print(json.dumps({"number": a.number, "hits": hits}))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18560)
    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("head").set_defaults(fn=cmd_head)

    p = sub.add_parser("block"); p.add_argument("number", type=int); p.set_defaults(fn=cmd_block)

    p = sub.add_parser("hashes")
    p.add_argument("ports", help="逗号分隔的 HTTP 端口列表")
    p.add_argument("number", help="块号，或 head")
    p.set_defaults(fn=cmd_hashes)

    p = sub.add_parser("election")
    p.add_argument("--offset", type=int, default=0)
    p.add_argument("--limit", type=int, default=0)
    p.set_defaults(fn=cmd_election)

    sub.add_parser("mining").set_defaults(fn=cmd_mining)
    sub.add_parser("current").set_defaults(fn=cmd_current)
    sub.add_parser("cabinets").set_defaults(fn=cmd_cabinets)

    p = sub.add_parser("indicator"); p.add_argument("addr"); p.set_defaults(fn=cmd_indicator)
    p = sub.add_parser("vbasic"); p.add_argument("addr"); p.set_defaults(fn=cmd_vbasic)
    p = sub.add_parser("iscurrent"); p.add_argument("addr"); p.set_defaults(fn=cmd_iscurrent)
    p = sub.add_parser("code"); p.add_argument("addr"); p.set_defaults(fn=cmd_code)
    p = sub.add_parser("balance"); p.add_argument("addr"); p.set_defaults(fn=cmd_balance)

    p = sub.add_parser("producers"); p.add_argument("start", type=int); p.add_argument("end", type=int)
    p.set_defaults(fn=cmd_producers)

    p = sub.add_parser("breathe")
    p.add_argument("start", type=int)
    p.add_argument("--interval", type=int, required=True)
    p.set_defaults(fn=cmd_breathe)

    p = sub.add_parser("epochset"); p.add_argument("number", type=int); p.set_defaults(fn=cmd_epochset)

    p = sub.add_parser("window")
    p.add_argument("--interval", type=int, required=True, help="breathe 间隔（秒）")
    p.add_argument("--sample", type=int, default=20, help="用最近多少个块估算块间隔")
    p.set_defaults(fn=cmd_window)

    p = sub.add_parser("txscan")
    p.add_argument("number", type=int)
    p.add_argument("to")
    p.add_argument("selector")
    p.set_defaults(fn=cmd_txscan)

    a = ap.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
