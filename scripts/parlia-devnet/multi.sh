#!/usr/bin/env bash
#
# Copyright 2026 The pgp-chain Authors
#

# Parlia multi-node devnet: N 个进程、N 席验证人、全互联。
#
#   ./multi.sh init [SEATS] [FORK_HEIGHT]   # N 个 datadir（各带 1 个 keystore）+ genesis + geth init
#   ./multi.sh start                        # 后台起 N 个节点，然后 admin.addPeer 组全网状
#   ./multi.sh status                       # 每个节点的高度 / 验证人集合 / 出块者分布
#   ./multi.sh wait N                       # 等到所有节点都到高度 N
#   ./multi.sh stop
#   ./multi.sh clean
#
# 为什么必须要多进程，不能像 run.sh 那样单进程跑 N 席
# ---------------------------------------------------
# Parlia 的 recents 防双签会拦下「同一个验证人连签」。窗口长度是
# len(Validators)/2+1，6 席即 4：签过一块之后，要再过 3 块（由别人签）才能再签。
# 所以 6 席里只上线 1 个进程时，block 1 之后必然卡在
#
#   Signed recently, must wait for others
#
# 一直等下去——不是配置错了，是数学上不可能。要跑过 200 块（epoch 块），
# 就必须让足够多的不同验证人轮流出块，也就是必须有多个进程、各自持钥。
#
# 端口分配
# --------
#   p2p  : 30300 + i
#   http : 18560 + i
# 全部绑 127.0.0.1。KEYS / 链数据 / 日志都在 $PGP_PARLIA_MULTI_HOME
# （默认 $HOME/.pgp-parlia-multi），不碰仓库。

set -euo pipefail

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SELF_DIR/../.." && pwd)
CONTRACT_ROOT=${CONTRACT_ROOT:-$REPO_ROOT/../bsc-genesis-contract}

GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}
HOME_DIR=${PGP_PARLIA_MULTI_HOME:-$HOME/.pgp-parlia-multi}
GENESIS=$HOME_DIR/genesis.json
LOG_DIR=$HOME_DIR/logs
# legacy 段（ForkHeight > 1）的 PBFT 生产者材料。只在 fork 模式用到。
ARBITER_DIR=$HOME_DIR/arbiter
ARBITER_MANIFEST=$ARBITER_DIR/producers.json
# keystore 密码明文写在命令行上（--pbft.keystore.password），因此这里必须是一个
# 一次性的 devnet 值，不能复用任何真实密码。密码本身不入库，也不写进 HOME_DIR
# 之外的地方；run.sh 走 GETH_PBFT_KEYSTORE_PASSWORD 环境变量是为了避免 ps 可见，
# 这里用 flag 是因为要在一个循环里给不同节点传不同参数，环境变量反而容易串。
ARBITER_PASSWORD=${ARBITER_PASSWORD:-forkdevnet}
ARBITER_PASSWORD_FILE=$ARBITER_DIR/password.txt
P2P_BASE=${P2P_BASE:-30300}
HTTP_BASE=${HTTP_BASE:-18560}
NETWORK_ID=${NETWORK_ID:-8606212}
# 日志级别。3 是日常；验完某个 DEBUG 埋点（例如 miner.makeEnv 的
# EstimateGasReservedForSystemTxs）时用 VERBOSITY=4，否则那一行不会进日志。
VERBOSITY=${VERBOSITY:-3}
# 块 gas 上限（genesis，hex）与 miner 目标值（十进制）。两者分开是因为它们回答
# 不同的问题：genesis 决定创世块的上限，miner.gaslimit 是节点收敛的目标，每块
# 最多只能改 parent.GasLimit/1024，所以差距大时要跑很多块才追平。
# 压测系统交易 gas 预留时把两者一起调小（例如 0x17D7840 / 25000000），否则
# 预留的 2000 万在 4000 万上限里看不出对用户交易的实际挤压。
GAS_LIMIT=${GAS_LIMIT:-0x2625a00}
MINER_GAS_LIMIT=${MINER_GAS_LIMIT:-30000000}
# 协议注入（Legacy.systemContractDeployHeight）。设成非 0 时，17 个系统合约
# 不再由 genesis alloc 带进来，而是在这个高度的块首由协议 SetCode + init。
#   DEPLOY_HEIGHT=38 OMIT_SYSTEM_ALLOC=1 ./multi.sh init 6 40
# 注意：D 必须 >= 2（EIP-150）且 <= ForkHeight-2。
DEPLOY_HEIGHT=${DEPLOY_HEIGHT:-0}
OMIT_SYSTEM_ALLOC=${OMIT_SYSTEM_ALLOC:-0}
# 1 = lubanBlock 钉哨兵（Luban 不启用，全程 pre-Luban 的 extraData 格式）。
LUBAN_SENTINEL=${LUBAN_SENTINEL:-0}

mkdir -p "$LOG_DIR"

die() { echo "错误: $*" >&2; exit 1; }
need_bin() { [ -x "$GETH_BIN" ] || die "找不到 ${GETH_BIN}；先跑 go build -o build/bin/geth ./cmd/geth"; }
node_dir() { echo "$HOME_DIR/node$1"; }
node_http() { echo $((HTTP_BASE + $1)); }
node_p2p() { echo $((P2P_BASE + $1)); }

# JSON-RPC 一行调用。macOS 的系统代理会把 127.0.0.1 也送到上游，所以显式绕过。
rpc() {
  python3 - "$1" "$2" "$3" <<'PY'
import json, sys, urllib.request
port, method, params = sys.argv[1], sys.argv[2], sys.argv[3]
op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
req = urllib.request.Request(f"http://127.0.0.1:{port}",
    json.dumps({"jsonrpc": "2.0", "method": method, "params": json.loads(params), "id": 1}).encode(),
    {"Content-Type": "application/json"})
try:
    print(json.dumps(json.loads(op.open(req, timeout=10).read())))
except Exception as e:
    print(json.dumps({"error": str(e)}))
PY
}

cmd_init() {
  local seats=${1:-6}
  local fork_height=${2:-1}
  need_bin
  [ -d "$CONTRACT_ROOT/out" ] || die "找不到 $CONTRACT_ROOT/out；先在 bsc-genesis-contract 里跑 forge build"
  case "$seats" in ''|*[!0-9]*) die "SEATS 必须是正整数，收到 '$seats'" ;; esac
  [ "$seats" -ge 4 ] || die "SEATS 必须 >= 4；recents 窗口是 N/2+1，少于 4 个签名者无法越过 block 1"

  rm -rf "$HOME_DIR"/node*
  mkdir -p "$HOME_DIR" "$LOG_DIR"
  : > "$HOME_DIR/validators.conf"

  local -a addrs=()
  local i addr dir
  for ((i = 0; i < seats; i++)); do
    dir=$(node_dir "$i"); mkdir -p "$dir"
    printf 'devnet\n' > "$dir/password.txt"
    addr=$("$GETH_BIN" account new --datadir "$dir" --password "$dir/password.txt" \
            | grep -o '0x[0-9a-fA-F]\{40\}' | head -1)
    [ -n "$addr" ] || die "节点 $i 的 keystore 创建失败"
    addr=$(echo "$addr" | tr 'A-Z' 'a-z')
    addrs+=("$addr")
    echo "$addr" > "$dir/validator.addr"
    echo "$addr,$addr,$addr,100,-" >> "$HOME_DIR/validators.conf"
  done

  local balances=()
  for addr in "${addrs[@]}"; do balances+=(--balance "$addr=0x56bc75e2d63100000"); done

  # legacy 段需要自己的生产者：block 1..ForkHeight-1 由 PBFT 出，而 PBFT 的生产者
  # 是 Elastos P-256 公钥，不是 EVM 的 secp256k1。所以 fork 模式下额外生成一个
  # arbiter keystore，公钥写进 genesis 的 legacy.producers，私钥交给 node 0。
  #
  # 只要 1 席：legacyMajorityCount(1) = int(2*1/3) = 0，即单生产者集合下一个零投票的
  # confirm 就是合法的（consensus/pgp/confirm.go 的注释写明了这是上游行为，不是 bug）。
  # 多席则需要多把私钥同时在线投票，单机做不到。
  #
  # 这一步也把「自派生密钥」这件事说清楚了：这些 key 与 pgp 主网那 12 个生产者无关，
  # 只用于本地验证分叉逻辑。
  local -a producer_args=()
  if [ "$fork_height" -gt 1 ]; then
    rm -rf "$ARBITER_DIR"
    # go run 的包路径是相对仓库根解析的，所以必须切到 REPO_ROOT 再跑；
    # 脚本本身可以在任意目录被调用。
    ( cd "$REPO_ROOT" && PGP_DEVNET_PASSWORD="$ARBITER_PASSWORD" \
        go run ./scripts/pbft-devnet/keygen -out "$ARBITER_DIR" -n 1 ) \
      > "$LOG_DIR/keygen.log" 2>&1 \
      || { tail -20 "$LOG_DIR/keygen.log"; die "生成 legacy arbiter 密钥失败"; }
    # 0600：这个文件里就是 keystore 的解密口令。
    printf '%s' "$ARBITER_PASSWORD" > "$ARBITER_PASSWORD_FILE"
    chmod 600 "$ARBITER_PASSWORD_FILE"
    local prod
    prod=$(python3 -c '
import json,sys
m=json.load(open(sys.argv[1]))
print(m["producers"][0])
' "$ARBITER_MANIFEST") || die "读不出 $ARBITER_MANIFEST 里的公钥"
    producer_args+=(--producers "$prod")
    echo "legacy 生产者（自派生 P-256，与主网无关）: $prod"
  fi

  local -a deploy_args=()
  [ "$DEPLOY_HEIGHT" != "0" ] && deploy_args+=(--deploy-height "$DEPLOY_HEIGHT")
  [ "$OMIT_SYSTEM_ALLOC" != "0" ] && deploy_args+=(--omit-system-contract-alloc)
  [ "$LUBAN_SENTINEL" != "0" ] && deploy_args+=(--luban-sentinel)

  python3 "$SELF_DIR/gen-genesis.py" \
    --contracts-out "$CONTRACT_ROOT/out" \
    --validators "$HOME_DIR/validators.conf" \
    --fork-height "$fork_height" \
    --cabinets "$seats" \
    --gas-limit "$GAS_LIMIT" \
    "${deploy_args[@]}" \
    "${producer_args[@]}" \
    "${balances[@]}" \
    --out "$GENESIS"

  for ((i = 0; i < seats; i++)); do
    dir=$(node_dir "$i")
    "$GETH_BIN" --datadir "$dir" init "$GENESIS" > "$LOG_DIR/init$i.log" 2>&1 \
      || { tail -10 "$LOG_DIR/init$i.log"; die "节点 $i geth init 失败"; }
  done

  echo
  echo "节点 / 验证人（共 $seats 席，各自持钥）："
  for ((i = 0; i < seats; i++)); do
    printf '  [%d] %s  p2p=%s  http=%s\n' "$i" "${addrs[i]}" "$(node_p2p "$i")" "$(node_http "$i")"
  done
  echo "ForkHeight : $fork_height"
  echo "下一步     : ./multi.sh start"
}

cmd_start() {
  need_bin
  [ -f "$GENESIS" ] || die "还没有 genesis；先跑 ./multi.sh init [SEATS] [FORK_HEIGHT]"
  local seats
  seats=$(ls -d "$HOME_DIR"/node* 2>/dev/null | wc -l | tr -d ' ')
  [ "$seats" -ge 4 ] || die "只找到 $seats 个 datadir"

  local i dir addr
  # fork 模式（genesis 里 legacy.producers 非空）下，block 1..ForkHeight-1 要由
  # PBFT 出块。只有 node 0 拿到 arbiter 私钥，所以只有它加 --pbft.enable；其余节点
  # 显式 --pbft.enable=false，在 legacy 段是纯 follower，到 ForkHeight 之后才靠
  # Parlia 出块。
  #
  # 为什么要显式给 false 而不是不给：不给（默认）也是 false，但显式写出来能让日志和
  # 进程命令行直接回答「这个节点在 legacy 段该不该出块」。
  local fork_producers
  fork_producers=$(python3 -c '
import json,sys
g=json.load(open(sys.argv[1]))
print(len(g.get("config",{}).get("legacy",{}).get("producers") or []))
' "$GENESIS") || die "读不出 $GENESIS 的 legacy.producers"
  local fork_mode=0
  [ "$fork_producers" -gt 0 ] && fork_mode=1

  for ((i = 0; i < seats; i++)); do
    dir=$(node_dir "$i"); addr=$(cat "$dir/validator.addr")
    local -a pbft_args=(--pbft.enable=false)
    if [ "$fork_mode" = 1 ] && [ "$i" = 0 ]; then
      # --pbft.miner.address 必须与 --miner.etherbase 同址，否则 cmd/utils/flags.go
      # 的 setEtherbase 直接 Fatalf（两者写同一个 header.Coinbase，而它进 PBFT seal hash）。
      # 这里统一用 node 0 自己的验证人地址：Parlia 段要求 coinbase == 出块验证人，
      # legacy 段不在乎 coinbase（空块、零手续费），一个值同时满足两段。
      # --pbft.keystore.password 收的是**文件路径**，不是密码本身
      # （cmd/utils 的 MakePbftPassword 会去读那个文件；直接传密码会 Fatal，
      # 而且错误信息刻意不回声参数内容，免得把密码写进日志）。
      pbft_args=(--pbft.enable
                 --pbft.keystore "$ARBITER_DIR/arbiter-0.dat"
                 --pbft.keystore.password "$ARBITER_PASSWORD_FILE"
                 --pbft.miner.address "$addr")
    fi
    # --nodiscover + --maxpeers：不靠 discovery，靠下面的 admin.addPeer 显式组网，
    # 避免同机多实例通过 UDP 探测互相发现出非确定拓扑。
    "$GETH_BIN" \
      --datadir "$dir" --networkid "$NETWORK_ID" \
      --port "$(node_p2p "$i")" \
      --nodiscover --maxpeers "$seats" \
      --mine --miner.etherbase "$addr" \
      --unlock "$addr" --password "$dir/password.txt" --allow-insecure-unlock \
      --miner.gaslimit "$MINER_GAS_LIMIT" \
      --http --http.addr 127.0.0.1 --http.port "$(node_http "$i")" \
      --http.api eth,net,web3,admin \
      --syncmode full --verbosity "$VERBOSITY" \
      "${pbft_args[@]}" \
      > "$LOG_DIR/node$i.log" 2>&1 &
    echo $! > "$dir/geth.pid"
    if [ "$fork_mode" = 1 ] && [ "$i" = 0 ]; then
      echo "节点 0 已启动 pid=$! （legacy 段 PBFT 生产者）p2p=$(node_p2p "$i") http=$(node_http "$i")"
    else
      echo "节点 $i 已启动 pid=$! p2p=$(node_p2p "$i") http=$(node_http "$i")"
    fi
  done

  # 等 HTTP 就绪
  local tries=0
  for ((i = 0; i < seats; i++)); do
    until rpc "$(node_http "$i")" web3_clientVersion '[]' 2>/dev/null | grep -q '"result"'; do
      tries=$((tries + 1))
      [ "$tries" -gt 200 ] && die "节点 $i 的 HTTP 在 40 s 内没有就绪，看 $LOG_DIR/node$i.log"
      sleep 0.2
    done
  done
  echo "全部 $seats 个节点 HTTP 就绪"

  # 全网状 addPeer。enode 从 admin_nodeInfo 取，不需要事先推导 nodekey。
  # 把自己也加一遍是无害的：geth 会直接忽略/报错，返回值不检查。
  local j enode_j
  for ((i = 0; i < seats; i++)); do
    for ((j = 0; j < seats; j++)); do
      [ "$i" = "$j" ] && continue
      enode_j=$(rpc "$(node_http "$j")" admin_nodeInfo '[]' \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["result"]["enode"] if "result" in d else "")')
      [ -n "$enode_j" ] || die "拿不到节点 $j 的 enode"
      rpc "$(node_http "$i")" admin_addPeer "[\"$enode_j\"]" > /dev/null
    done
  done
  echo "已建立全网状 peer 连接"

  sleep 3
  for ((i = 0; i < seats; i++)); do
    printf '  节点 %d peers = %s\n' "$i" \
      "$(rpc "$(node_http "$i")" net_peerCount '[]' | python3 -c 'import json,sys; d=json.load(sys.stdin); print(int(d.get("result","0x0"),16) if "result" in d else "?")')"
  done
  echo
  echo "看高度 : ./multi.sh status"
  echo "等出块 : ./multi.sh wait 205"

  # KEEPALIVE=1 时留在前台 wait，直到所有节点退出。
  # 这不是可选项：如果 start 把子进程留给调用者所在的 shell，那个 shell 一退出，
  # 子进程会被一并回收（实测：节点启动后 40 s 全部消失）。要长期跑就得让承载它的
  # 进程一直活着。
  if [ "${KEEPALIVE:-0}" = "1" ]; then
    echo
    echo "KEEPALIVE=1：前台等待所有节点退出（Ctrl-C 停）"
    trap 'cmd_stop; exit 0' INT TERM
    local pids=()
    for ((i = 0; i < seats; i++)); do pids+=("$(cat "$(node_dir "$i")/geth.pid")"); done
    wait "${pids[@]}" 2>/dev/null || true
  fi
}

cmd_stop() {
  local stopped=0 dir
  for dir in "$HOME_DIR"/node*; do
    [ -f "$dir/geth.pid" ] || continue
    if kill "$(cat "$dir/geth.pid")" 2>/dev/null; then stopped=$((stopped + 1)); fi
    rm -f "$dir/geth.pid"
  done
  echo "已向 $stopped 个节点发送 SIGTERM"
  sleep 2
  pkill -f "pgp-parlia-multi/node" 2>/dev/null && echo "已清理残留进程" || true
}

cmd_status() {
  python3 - "$HOME_DIR" "$HTTP_BASE" "$P2P_BASE" <<'PY'
import json, os, sys, urllib.request
home, http_base = sys.argv[1], int(sys.argv[2])
op = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def rpc(port, method, params=None):
    req = urllib.request.Request(f"http://127.0.0.1:{port}",
        json.dumps({"jsonrpc": "2.0", "method": method, "params": params or [], "id": 1}).encode(),
        {"Content-Type": "application/json"})
    try:
        return json.loads(op.open(req, timeout=5).read())
    except Exception as e:
        return {"error": str(e)}

def block(port, num):
    r = rpc(port, "eth_getBlockByNumber", [num, False])
    return r.get("result") or r

nodes = sorted(int(d[4:]) for d in os.listdir(home) if d.startswith("node") and d[4:].isdigit())
ok = [n for n in nodes if "result" in rpc(http_base + n, "web3_clientVersion")]
if not ok:
    sys.exit("没有可达的节点")

print(f"可达节点: {ok}")
heights = {}
for n in ok:
    r = rpc(http_base + n, "eth_blockNumber")
    heights[n] = int(r["result"], 16)
print("高度:", ", ".join(f"N{n}={heights[n]}" for n in ok))
head = max(heights.values())

# 每块 extra 长度 + 出块者。样本按 ForkHeight 切成两段：legacy 段的 extra 是
# PBFT confirm 包，Parlia 段才是 32+1+N*68+65。混在一起统计会得出两个错误结论——
# 一是把 275 B 的 PBFT 包当成"解出 2 席"的 epoch 块，二是把单生产者 legacy 段的
# 出块集中误当成 Parlia 轮值退化。
fork = 0
try:
    with open(os.path.join(home, "genesis.json")) as f:
        fork = int(json.load(f)["config"].get("legacy", {}).get("forkHeight", 0))
except Exception:
    pass

extras, miners = {}, {}
for num in range(max(1, head - 59), head + 1):
    b = block(http_base + ok[0], hex(num))
    if not b or "extraData" not in b:
        continue
    extras[num] = (len(b["extraData"]) - 2) // 2
    miners[num] = b["miner"].lower()

legacy_extras = {n: L for n, L in extras.items() if fork and n < fork}
posa_extras = {n: L for n, L in extras.items() if not (fork and n < fork)}

print(f"块数: {head}    ForkHeight: {fork if fork else '未配置（全链 legacy）'}"
      f"    样本: 最近 {min(60, head)} 块")
for label, sel in (("legacy 段", legacy_extras), ("Parlia 段", posa_extras)):
    if not sel:
        continue
    counts = {}
    for n in sel:
        counts[miners[n]] = counts.get(miners[n], 0) + 1
    print(f"{label}（{len(sel)} 块样本）出块者分布: {len(counts)} 个不同地址")
    for a, c in sorted(counts.items(), key=lambda kv: -kv[1]):
        print(f"  {a}  {c} 块")

EC, SS = 32, 65          # Parlia extra = vanity 32 + 验证人列表 + seal 65
if posa_extras:
    common_len = max(set(posa_extras.values()), key=list(posa_extras.values()).count)
    print(f"Parlia extra 长度众数: {common_len} B  (非 epoch = {EC + SS} = {EC + SS})")
    epoch_blocks = sorted(n for n, L in posa_extras.items() if L != EC + SS)
    for n in epoch_blocks:
        L = posa_extras[n]
        seats = (L - EC - SS - 1) // 68
        print(f"  epoch/变长块 #{n}: extra={L} B  => 解出 {seats} 席"
              f"（期望 {EC}+1+{seats}x68+{SS} = {EC + 1 + seats * 68 + SS}）")
    if not epoch_blocks:
        print("  （区间内没有 epoch 块；epoch=200，等跑过 200 再看）")
if legacy_extras:
    lens = sorted(set(legacy_extras.values()))
    print(f"legacy extra 长度: {lens} B（PBFT confirm 包 + 生产者公钥，"
          f"不是 Parlia 布局，不套用 {EC}+1+Nx68+{SS}）")
PY
}

cmd_wait() {
  local target=${1:?用法: ./multi.sh wait <height>}
  python3 - "$HOME_DIR" "$HTTP_BASE" "$target" <<'PY'
import json, os, sys, time, urllib.request
home, http_base, target = sys.argv[1], int(sys.argv[2]), int(sys.argv[3], 0)
op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def head(port):
    req = urllib.request.Request(f"http://127.0.0.1:{port}",
        json.dumps({"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}).encode(),
        {"Content-Type":"application/json"})
    try:
        return int(json.loads(op.open(req, timeout=5).read())["result"], 16)
    except Exception:
        return -1
ports = [http_base + int(d[4:]) for d in sorted(os.listdir(home))
         if d.startswith("node") and d[4:].isdigit()]
t0 = time.time()
while True:
    hs = [head(p) for p in ports]
    cur = min(hs)
    if cur >= target:
        print(f"\n所有节点已达高度 {target}（最低 {cur}），用时 {time.time()-t0:.0f}s")
        break
    print(f"  {time.strftime('%H:%M:%S')}  最低高度 {cur} / {target}  各节点={hs}", end="\r")
    if time.time() - t0 > 3600:
        print(f"\n超时：1 小时只到 {cur}，看 {home}/logs/node*.log")
        sys.exit(1)
    time.sleep(10)
PY
}

cmd_clean() {
  [ -d "$HOME_DIR" ] || { echo "没有 $HOME_DIR"; return; }
  echo "将删除 ${HOME_DIR}（含 $HOME_DIR 下所有 keystore 与链数据）"
  read -r -p "确认? [y/N] " ans
  [ "$ans" = "y" ] || { echo "取消"; return; }
  rm -rf "$HOME_DIR"
  echo "已删除"
}

case "${1:-}" in
  init)   shift; cmd_init "$@" ;;
  start)  shift; cmd_start "$@" ;;
  stop)   shift; cmd_stop "$@" ;;
  status) shift; cmd_status "$@" ;;
  wait)   shift; cmd_wait "$@" ;;
  clean)  shift; cmd_clean "$@" ;;
  *)
    sed -n '6,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
