#!/usr/bin/env python3
"""生成 Parlia devnet 的完整 genesis.json。

为什么需要它
------------
`core/systemcontracts/upgrade.go` 的升级表只注册了 mainnet / chapel / rialto 三个
网络。任何其他 genesis hash 都落到 defaultNet，而 defaultNet 的每张升级表都是
nil，`applySystemContractUpgrade` 见到 nil 直接 return（upgrade.go:1283-1287）。
对这条链来说这是个好消息 —— BSC 内嵌的旧字节码不会覆盖我们的合约；代价是
**17 个系统合约必须由 genesis alloc 自己带进来**，否则 0x1000 上是空账户，
init() 调用会「成功」但什么也没写，然后在第一个 epoch 块炸掉。

地址映射与 bsc-genesis-contract/genesis-template.json 逐条一致，不是猜的。

与 bsc-genesis-contract/scripts/generate-genesis.js 的分工
----------------------------------------------------------
那个脚本服务于 BSC 自己的 genesis（chainId 56/97、BSC 的历史分叉高度、
INIT_VALIDATORSET_BYTES 编译期常量）。这里要的是另一件事：
  * chainId 是 pgp devnet 的；
  * legacy 段存在，共识在某个高度才切到 Parlia；
  * 验证人名单不进字节码，而是走 legacy.parliaGenesis*。
所以是独立的生成器，而不是给那个脚本加参数。两者的共同前提只有一条：
合约字节码来自同一份 forge 产物。
"""
import argparse
import json
import os
import sys

# 地址 -> forge 产物。地址取自 bsc-genesis-contract/genesis-template.json，
# 顺序按地址排，便于对比。
CONTRACTS = [
    ("0x0000000000000000000000000000000000001000", "BSCValidatorSet", "BSCValidatorSet.sol"),
    ("0x0000000000000000000000000000000000001001", "SlashIndicator", "SlashIndicator.sol"),
    ("0x0000000000000000000000000000000000001002", "SystemReward", "SystemReward.sol"),
    ("0x0000000000000000000000000000000000001003", "TendermintLightClient", "TendermintLightClient.sol"),
    ("0x0000000000000000000000000000000000001004", "TokenHub", "TokenHub.sol"),
    ("0x0000000000000000000000000000000000001005", "RelayerIncentivize", "RelayerIncentivize.sol"),
    ("0x0000000000000000000000000000000000001006", "RelayerHub", "RelayerHub.sol"),
    ("0x0000000000000000000000000000000000001007", "GovHub", "GovHub.sol"),
    ("0x0000000000000000000000000000000000001008", "TokenManager", "TokenManager.sol"),
    ("0x0000000000000000000000000000000000002000", "CrossChain", "CrossChain.sol"),
    ("0x0000000000000000000000000000000000002001", "Staking", "Staking.sol"),
    ("0x0000000000000000000000000000000000002002", "StakeHub", "StakeHub.sol"),
    ("0x0000000000000000000000000000000000002003", "StakeCredit", "StakeCredit.sol"),
    ("0x0000000000000000000000000000000000002004", "BSCGovernor", "BSCGovernor.sol"),
    ("0x0000000000000000000000000000000000002005", "GovToken", "GovToken.sol"),
    ("0x0000000000000000000000000000000000002006", "BSCTimelock", "BSCTimelock.sol"),
    ("0x0000000000000000000000000000000000003000", "TokenRecoverPortal", "TokenRecoverPortal.sol"),
]

# 出块方地址。BSC 用它作为 coinbase（extra 之外的那个 coinbase 字段），
# 也是所有系统交易的 from。模板里的值是 0xffff…fFFE，保持一致。
SYSTEM_COINBASE = "0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE"

# lubanBlock 的哨兵值：与 Legacy.ForkHeight 的 math.MaxUint64 是同一套约定
# ——「排在这个高度」在执行上等价于「永不启用」，因为区块高度到不了这里。
# params.CheckConfigForkOrder 会把 >= math.MaxInt64 的块型 fork 当作未排期，
# 所以它不会要求 planck 等前驱分叉先被启用，也不会约束 plato 等后继分叉。
LUBAN_SENTINEL = 2**63 - 1


def load_bytecode(out_dir: str, sol_file: str, name: str) -> str:
    path = os.path.join(out_dir, sol_file, name + ".json")
    if not os.path.exists(path):
        sys.exit(f"找不到合约产物 {path}；先在 bsc-genesis-contract 里跑一次 forge build")
    with open(path) as f:
        d = json.load(f)
    code = d.get("deployedBytecode", {}).get("object", "")
    if not code or code == "0x":
        sys.exit(f"{path} 的 deployedBytecode 为空")
    if not code.startswith("0x"):
        code = "0x" + code
    return code


def parse_validators(path: str):
    """读 validators conf：每行 consensusAddr,feeAddr,bscFeeAddr,votingPower,bLSPublicKey。

    只关心 consensusAddr —— RLP payload 由节点从 legacy.parliaGenesis* 现编，
    genesis 里不需要它。这里读 conf 唯一的目的，是让 genesis 的验证人列表和
    节点配置里的列表出自同一份文件，避免两边各写一份、改一边忘另一边。
    """
    addrs = []
    with open(path) as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line:
                continue
            cols = line.split(",")
            if len(cols) != 5:
                sys.exit(f"{path}:{lineno} 需要 5 个字段，实际 {len(cols)}")
            addr = cols[0].strip()
            if not addr.startswith("0x"):
                addr = "0x" + addr
            addrs.append(addr.lower())
    if not addrs:
        sys.exit(f"{path} 里没有验证人")
    return addrs


def build_config(args, validators):
    """组装 config 段。

    分叉布局是「现代档 + 单点切换」：Parlia / Luban / Plato / Hertz 全部落在
    ForkHeight。这样 H 块既是共识切换点，也是 Luban 的 epoch 长度起点，不存
    在一段「Parlia 已启用但 extra 还是 pre-Luban 格式」的过渡区。

    时间类分叉一律不写。devnet 是全新链，H 块的墙钟时间要跑到才知道，genesis
    里没法预先钉死；写死了只会得到一个 ActivateAtTime 永不满足的分叉，然后
    epoch 长度卡在 defaultEpochLength。等链跑起来拿到 H 的 timestamp 再补。
    """
    cfg = {
        "chainId": args.chain_id,
        "homesteadBlock": 0,
        "eip150Block": 0,
        "eip155Block": 0,
        "eip158Block": 0,
        "byzantiumBlock": 0,
        "constantinopleBlock": 0,
        "petersburgBlock": 0,
        "istanbulBlock": 0,
        "muirGlacierBlock": 0,
        # BSC 的历史分叉全部压在 0，因为它们描述的是「合约版本」，
        # 而我们要用的正是最新那版字节码。
        "ramanujanBlock": 0,
        "nielsBlock": 0,
        "mirrorSyncBlock": 0,
        "brunoBlock": 0,
        "eulerBlock": 0,
        "nanoBlock": 0,
        "moranBlock": 0,
        "gibbsBlock": 0,
        "planckBlock": 0,
        "berlinBlock": 0,
        "londonBlock": 0,
        # 现代档，与共识切换同高度。lubanBlock 单独处理：哨兵值表示不启用，
        # 见 --luban-sentinel。
        "lubanBlock": LUBAN_SENTINEL if args.luban_sentinel else args.fork_height,
        "platoBlock": args.fork_height,
        "hertzBlock": args.fork_height,
        "hertzfixBlock": args.fork_height,
        # ParliaConfig 是空结构体：有这个 key 就等于「Parlia 已启用」
        "parlia": {},
        "legacy": {
            "forkHeight": args.fork_height,
            # 0 表示不启用协议注入（旧行为：代码由下面的 alloc 带进来）。
            "systemContractDeployHeight": args.deploy_height,
            "gasFeeRecipient": args.gas_fee_recipient,
            "gasFeeRecipientRatio": args.gas_fee_recipient_ratio,
            "passBalance": 0,
            # verifySeal gates PBFT confirm verification. It defaults to ON in
            # params.PGPChainConfig and must stay ON whenever producers are
            # configured: a chain that lists producers it never checks signatures
            # against is a chain where any header is acceptable, i.e. the field
            # would be decoration.
            "verifySeal": args.verify_seal,
            "producers": args.producers,
            "parliaGenesisValidators": validators,
            "parliaGenesisVotingPowers": [args.voting_power] * len(validators),
            "parliaGenesisCabinets": args.cabinets or len(validators),
        },
    }
    return cfg


def main() -> int:
    ap = argparse.ArgumentParser(description="生成 Parlia devnet genesis.json")
    ap.add_argument("--contracts-out", required=True, help="bsc-genesis-contract/out 目录")
    ap.add_argument("--validators", required=True, help="validators conf（5 列）")
    ap.add_argument("--fork-height", type=int, default=20)
    ap.add_argument("--chain-id", type=int, default=8606211)
    ap.add_argument("--voting-power", type=int, default=100)
    ap.add_argument("--cabinets", type=int, default=0, help="0 = 取验证人数")
    ap.add_argument("--gas-fee-recipient", default="0x0000000000000000000000000000000000000000")
    ap.add_argument("--gas-fee-recipient-ratio", type=int, default=0)
    ap.add_argument("--gas-limit", default="0x2625a00")
    ap.add_argument("--timestamp", default="0x5e9da7ce")
    ap.add_argument("--balance", action="append", default=[], metavar="ADDR=HEXWEI",
                    help="给地址预置余额，可重复")
    ap.add_argument("--producers", action="append", default=[], metavar="HEX33",
                    help="legacy 段 PBFT 生产者压缩公钥（33 字节 hex，无 0x），可重复。"
                         "留空则 legacy 段没有生产者集合：verifySeal 会因为 "
                         "'no configured producer set' 拒绝每一个 legacy 块，"
                         "且没有任何节点能出块")
    ap.add_argument("--verify-seal", choices=["true", "false"], default=None,
                    help="是否校验 PBFT confirm。默认在有 producers 时为 true，否则 false")
    ap.add_argument("--out", required=True)
    ap.add_argument("--deploy-height", type=int, default=0,
                    help="Legacy.systemContractDeployHeight：协议在这个高度安装并初始化 17 个"
                         "系统合约。0 表示不启用（沿用 genesis alloc 带代码的旧行为）。"
                         "启用时必须满足 2 <= D <= ForkHeight-2。")
    ap.add_argument("--luban-sentinel", action="store_true",
                    help="把 lubanBlock 钉在哨兵高度，表示 Luban 不启用：引擎全程走 "
                         "pre-Luban 的 extraData 格式（无 validator count 前缀、无 BLS 公钥、"
                         "无 vote attestation）。不写这个开关时 lubanBlock 与 ForkHeight 同高。")
    ap.add_argument("--omit-system-contract-alloc", action="store_true",
                    help="不把 17 个系统合约写进 genesis alloc，让 0x1000..0x3000 在 D 块之前"
                         "保持空账户，由协议注入安装。配合 --deploy-height 使用。")
    args = ap.parse_args()

    validators = parse_validators(args.validators)
    if args.fork_height <= 0:
        sys.exit("--fork-height 必须 > 0；这里是「从哪个高度起切 Parlia」，不是「Parlia 从 0 开始」")
    if args.deploy_height:
        if args.deploy_height < 2:
            sys.exit("--deploy-height 必须 >= 2：12 个初始化调用会把剩余 gas 全部转发给"
                     "子调用，EIP-150 的 63/64 上限没生效时会在扣费阶段直接 out of gas")
        if args.deploy_height > args.fork_height - 2:
            sys.exit(f"--deploy-height={args.deploy_height} 必须 <= fork-height-2="
                     f"{args.fork_height - 2}：ForkHeight 块要读 0x1000 的验证人集合，"
                     "并留一个块给注入失败时补救")
    elif args.omit_system_contract_alloc:
        sys.exit("--omit-system-contract-alloc 必须配 --deploy-height，否则 0x1000 永远没有代码")
    if args.cabinets and args.cabinets != len(validators):
        sys.exit(f"--cabinets={args.cabinets} 与验证人数 {len(validators)} 不一致")

    # 规范成 params.DefaultLegacyProducers 的写法：无 0x、小写。
    producers = []
    for key in args.producers:
        k = key.strip()
        if k.startswith(("0x", "0X")):
            k = k[2:]
        if len(k) != 66:
            sys.exit(f"--producers 需要 33 字节压缩公钥（66 个 hex 字符），收到 {len(k)} 个字符")
        try:
            bytes.fromhex(k)
        except ValueError:
            sys.exit(f"--producers 不是合法 hex: {key!r}")
        producers.append(k.lower())
    if len(set(producers)) != len(producers):
        sys.exit("--producers 有重复项；重复的公钥会在 on-duty 轮值里占两个席位")

    # verifySeal 默认跟随 producers：有生产者就开真验签。允许显式覆盖，因为
    # 「列出生产者但不校验」是调查用的，不是配置选项。
    if args.verify_seal is None:
        verify_seal = bool(producers)
    else:
        verify_seal = args.verify_seal == "true"
    if verify_seal and not producers:
        sys.exit("--verify-seal=true 但没有 --producers：每个 legacy 块都会被拒")
    if args.fork_height > 1 and not producers:
        sys.exit(
            f"--fork-height={args.fork_height} 需要 legacy 段自己出块到 {args.fork_height - 1}，"
            "因此必须给 --producers，并且至少一个节点用 --pbft.enable 持有对应私钥"
        )
    args.verify_seal = verify_seal

    alloc = {}
    if args.omit_system_contract_alloc:
        # 0x1000..0x3000 保持空账户：这正是线上 pgp-chain 的形态（创世已上线、
        # alloc 不可改），代码与初始状态改由 D 块的协议注入安装。冒烟必须走这条
        # 路，否则「D 之前没有代码」这个前提根本没被验证过。
        pass
    else:
        for addr, name, sol in CONTRACTS:
            alloc[addr] = {"balance": "0x0", "code": load_bytecode(args.contracts_out, sol, name)}
    alloc[SYSTEM_COINBASE.lower()] = {"balance": "0x0"}

    for item in args.balance:
        if "=" not in item:
            sys.exit(f"--balance 需要 ADDR=HEXWEI 形式，收到 {item!r}")
        addr, wei = item.split("=", 1)
        alloc.setdefault(addr.lower(), {"balance": "0x0"})["balance"] = wei

    genesis = {
        "config": build_config(args, validators),
        # block 0 是 legacy 块（0 < ForkHeight），extra 不需要 Parlia 格式。
        # snapshot 也不会去解析它：Parlia 的快照锚点设在 ForkHeight-1，
        # 见 consensus/parlia/pgp_validator_init.go。
        "extraData": "0x",
        "gasLimit": args.gas_limit,
        "difficulty": "0x1",
        "mixHash": "0x" + "00" * 32,
        "coinbase": SYSTEM_COINBASE,
        "nonce": "0x0",
        "timestamp": args.timestamp,
        "alloc": alloc,
        "number": "0x0",
        "gasUsed": "0x0",
        "parentHash": "0x" + "00" * 32,
    }

    with open(args.out, "w") as f:
        json.dump(genesis, f, indent=2)
        f.write("\n")

    # 只统计带 code 的条目：balance-only 条目没有 code，len("")//2-1 会变成 -1。
    total = sum(len(v["code"]) // 2 - 1 for v in alloc.values() if v.get("code"))
    print(f"chainId        : {args.chain_id}")
    print(f"ForkHeight     : {args.fork_height}")
    print(f"legacy 生产者  : {len(producers)} 席，verifySeal={args.verify_seal}")
    for i, k in enumerate(producers):
        print(f"  [{i}] {k}")
    if not producers:
        print("  （空：legacy 段没有生产者，只在 ForkHeight=1 时可用）")
    print(f"验证人         : {len(validators)} 席")
    for i, a in enumerate(validators):
        print(f"  [{i}] {a}")
    print(f"系统合约       : {len(CONTRACTS)} 个，共 {total} 字节 deployedBytecode")
    print(f"alloc 条目     : {len(alloc)}")
    print(f"已写入         : {args.out}")
    epoch = 200  # 未开 Lorentz，defaultEpochLength
    print()
    print(f"注意：epoch 长度 = {epoch}（未激活 Lorentz），块间隔 3000ms。")
    print(f"      ForkHeight={args.fork_height}，H%{epoch}={args.fork_height % epoch}"
          f"（{'是' if args.fork_height % epoch == 0 else '不是'} epoch 块）。")
    if args.fork_height % epoch == 0:
        print("      ⚠️ H 落在 epoch 块上：H 块会要求带验证人列表，而那时合约刚写入、"
              "getCurrentValidators 读的是 parent 状态，会读到空集合。换一个高度。")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
