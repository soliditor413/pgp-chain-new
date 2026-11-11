#!/usr/bin/env bash
# run.sh -- entry point for the staking/POSA devnet scenarios.
#
#   ./run.sh init <a|b|c|d|e>   # fresh rig under $PGP_STAKING_HOME/<run>/net
#   ./run.sh prepare            # BLS proofs + createValidator calldata (offline)
#   ./run.sh start              # foreground; run it as a background task (KEEPALIVE)
#   ./run.sh stop
#   ./run.sh start-node I / stop-node I
#   ./run.sh wait N
#   ./run.sh register           # createValidator for every seat (needs head >= fork)
#   ./run.sh delegate I ETH
#   ./run.sh evidence --height H --node I
#   ./run.sh status
#   ./run.sh window             # 距下一个 breathe 边界还剩多少秒/块
#   ./run.sh check [--deep]     # 自检闸门：election 非空、mining==cabinets、集合一致
#                              # --deep 再加 8 节点 hash 一致性。非 0 退出 = 不该继续。

set -euo pipefail
SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$SELF_DIR/lib.sh"

cmd_init() {
  local run=${1:?用法: run.sh init <a|b|c|d>}
  local seats cab breathe align legacy posa
  case "$run" in
    a) seats=6; cab=6; breathe=300; align=1; legacy=0; posa=0 ;;
    b) seats=6; cab=5; breathe=300; align=1; legacy=0; posa=0 ;;
    c) seats=6; cab=6; breathe=300; align=1; legacy=0; posa=0 ;;
    d) seats=4; cab=4; breathe=2592000; align=0; legacy=0; posa=0 ;;
    # e: 4 个 legacy 生产者 + 4 个全新的 POSA 验证人（两批人完全不相交）
    e) seats=8; cab=4; breathe=300; align=1; legacy=4; posa=4 ;;
    *) die "未知 run: $run（可选 a/b/c/d/e）" ;;
  esac

  local ts
  if [ "$align" = 1 ]; then
    # 未来 120s 起、300 的整数倍：块时间严格等于 T0+3n，breathe 落在 100/200/300。
    ts=$(printf '0x%x' "$(( ( $(date +%s) + 120 + 299 ) / 300 * 300 ))")
  else
    ts=$(printf '0x%x' "$(( $(date +%s) + 30 ))")
  fi

  mkdir -p "$STAKING_HOME"
  rm -rf "$STAKING_HOME/prep" "$STAKING_HOME/evidence.json"
  echo "$run" > "$STAKING_HOME/run"

  CABINETS=$cab \
  LEGACY_PRODUCERS=$legacy \
  POSA_VALIDATORS=$posa \
  FEYNMAN=1 \
  GENESIS_TIMESTAMP=$ts \
  BREATHE_INTERVAL=$breathe \
  DEPLOY_HEIGHT=20 \
  OMIT_SYSTEM_ALLOC=1 \
  BLS_VOTE=1 \
  BALANCE_WEI=0x21e19e0c9bab2400000 \
  HTTP_API='eth,net,web3,admin,debug' \
    "$MULTI" init "$seats" 40

  load_env
  echo
  echo "run=$run  seats=$SEATS_N  cabinets=$CABINETS  breathe=${BREATHE_INTERVAL:-default}  T0=$((ts))"
}

cmd_prepare() {
  load_env
  mkdir -p "$STAKING_HOME/prep"
  local k i dir addr vote proof msg input
  for ((k = 0; k < POSA_COUNT_N; k++)); do
    i=$((POSA_FIRST_N + k))
    dir=$(node_dir "$i")
    addr=$(val_addr "$i")
    vote=$(cat "$dir/bls/vote.addr")
    # 注意：urfave/cli 在遇到位置参数后就不再解析 flag，所以 --datadir 等必须
    # 放在 <operator> <pubkey> 之前，否则 DataDir 会退回 ~/.ethereum，报
    # "BLS wallet not exists"。
    proof=$("$GETH_BIN" bls account generate-proof \
      --datadir "$dir" --blspassword "$dir/bls/create-password.txt" \
      --chain-id "$CHAIN_ID" "$addr" "$vote" 2>/dev/null \
      | sed -n 's/^Proof: //p' | tr -d '\r')
    [ -n "$proof" ] || die "节点 $i 的 BLS proof 生成失败"
    # abi.encodePacked(operator, voteAddress, uint256 chainId)
    msg=$(cast keccak "0x${addr#0x}${vote#0x}$(printf '%064x' "$CHAIN_ID")")
    input="0x${msg#0x}${proof#0x}${vote#0x}"
    python3 - "$STAKING_HOME/prep/node$i.json" "$i" "$addr" "$vote" "$proof" "$input" <<'PY'
import json, sys
path, idx, addr, vote, proof, inp = sys.argv[1:7]
json.dump({"index": int(idx), "addr": addr, "vote": vote, "proof": proof,
           "precompileInput": inp}, open(path, "w"), indent=2)
PY
    echo "node$i proof generated (${#proof} hex chars)"
  done
}

# verify_proofs 只能分叉之后跑：0x66 属于 Feynman 预编译集，legacy 段没有它。
verify_proofs() {
  local k i out
  for ((k = 0; k < POSA_COUNT_N; k++)); do
    i=$((POSA_FIRST_N + k))
    out=$(cast call "$BLS_PRECOMPILE" \
      "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["precompileInput"])' "$STAKING_HOME/prep/node$i.json")" \
      --rpc-url "$(rpc_url "$i")")
    [ "$out" = "0x01" ] || die "节点 $i 的 BLS proof 未通过 0x66（返回 $out）；检查 voteAddress/chainId"
    echo "node$i 0x66 proof OK"
  done
}

cmd_start() {
  load_env
  # start 会一直阻塞在 `wait`（KEEPALIVE=1），也就是整条链的生命周期都压在这一次
  # bash 进程里。而这个进程是**边读脚本边执行**的：运行期间只要有人编辑 multi.sh，
  # bash 就会按字节偏移继续读被改写的文件，读到半条语句就
  #   multi.sh: line N: syntax error near unexpected token `;;'
  # 然后退出（exit 2），子 geth 随父 shell 一起被回收。
  # 实测代价：一条跑了 44 分钟、已经到 block 771 的链，因为我在注释里加了两行而全掉，
  # 报错还长得像脚本本身有语法错误（bash -n 是过的）。
  # 所以让它执行一份快照：运行期间改动 multi.sh 不再影响这条已经起来的链。
  local snap="$STAKING_HOME/.multi.snapshot.sh"
  cp "$MULTI" "$snap"
  # 快照换了位置，multi.sh 内部由 SELF_DIR 推出的 REPO_ROOT 就不再指向仓库，
  # 那两个由它推导的二进制路径必须显式传进去（PGP_PARLIA_MULTI_HOME 已在 lib.sh 导出）。
  GETH_BIN="$GETH_BIN" BOOTNODE_BIN="$BOOTNODE_BIN" SEED_PEERS_PY="$SEED_PEERS_PY" \
    KEEPALIVE=1 bash "$snap" start
}

cmd_stop() { "$MULTI" stop; }
cmd_start_node() { load_env; "$MULTI" start-node "${1:?用法: run.sh start-node I}"; }
cmd_stop_node() { load_env; "$MULTI" stop-node "${1:?用法: run.sh stop-node I}"; }
cmd_wait() { wait_head "${1:?用法: run.sh wait N}"; }

cmd_register() {
  load_env
  [ -f "$MULTI_HOME/devnet.env" ] || die "先 init"
  local h
  h=$(head 0)
  [ "$h" -ge "${FORK:-0}" ] || die "当前 head=$h < fork=${FORK:-0}：createValidator 的 0x66 预编译只在分叉后可用，等分叉再注册"
  verify_proofs

  local run k i addr value vote proof
  run=$(cat "$STAKING_HOME/run" 2>/dev/null || echo b)
  local -a vals=(2600 2500 2400 2300 2200 2100)
  for ((k = 0; k < POSA_COUNT_N; k++)); do
    i=$((POSA_FIRST_N + k))
    addr=$(val_addr "$i")
    if [ "$run" = b ]; then
      value="${vals[$k]}ether"
    else
      value="2100ether"
    fi
    vote=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["vote"])' "$STAKING_HOME/prep/node$i.json")
    proof=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["proof"])' "$STAKING_HOME/prep/node$i.json")
    cast send --unlocked --from "$addr" --value "$value" --gas-limit 5000000 \
      --legacy --gas-price 1gwei \
      "$STAKE_HUB" \
      "createValidator(address,bytes,bytes,(uint64,uint64,uint64),(string,string,string,string))" \
      "$addr" "$vote" "$proof" "(0,5000,5000)" "(\"Node$i\",\"\",\"\",\"\")" \
      --rpc-url "$(rpc_url "$i")" > "$STAKING_HOME/register-$i.log" 2>&1 \
      || { cat "$STAKING_HOME/register-$i.log"; die "节点 $i 注册失败"; }
    echo "registered node$i (value=$value)"
  done

  local total
  total=$(expect0 election | jsonf 'd["total"]')
  [ "$total" -gt 0 ] || die "注册流程跑完但 election.total 仍为 0：不能离开这个状态"
  echo
  echo "election.total=$total"
  # 注册完把剩余窗口打出来。这不是装饰：分叉后第一个 breathe 块之前没注册完，
  # 选举就一直是空的（合约保护住了不停链，但质押体系等于没生效）。
  cmd_window
}

cmd_delegate() {
  load_env
  local i=${1:?用法: run.sh delegate I ETH}
  local eth=${2:?用法: run.sh delegate I ETH}
  local addr
  addr=$(val_addr "$i")
  cast send --unlocked --from "$addr" --value "${eth}ether" --legacy --gas-price 1gwei \
    "$STAKE_HUB" "delegate(address,bool)" "$addr" true \
    --rpc-url "$(rpc_url "$i")" > "$STAKING_HOME/delegate-$i.log" 2>&1 \
    || { cat "$STAKING_HOME/delegate-$i.log"; die "节点 $i delegate 失败"; }
  echo "delegated ${eth} ETH to node$i ($addr)"
}

cmd_evidence() {
  local height="" node=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --height) height=${2:?}; shift 2 ;;
      --node) node=${2:?}; shift 2 ;;
      *) die "未知参数 $1" ;;
    esac
  done
  [ -n "$height" ] && [ -n "$node" ] || die "用法: run.sh evidence --height H --node I"
  load_env
  cast block "$height" --rpc-url "$(rpc_url 0)" --json >/dev/null 2>&1 || true
  ( cd "$REPO_ROOT" && go run ./scripts/staking-devnet/doublesign emit \
      --rpc "$(rpc_url 0)" \
      --height "$height" \
      --keystore "$(node_dir "$node")/keystore" \
      --password-file "$(node_dir "$node")/password.txt" \
      --chain-id "$CHAIN_ID" \
      --out "$STAKING_HOME/evidence.json" )
  python3 - "$STAKING_HOME/evidence.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(f"signer={d['signer']} height={d['height']} "
      f"header1={len(d['header1'])//2-1}B header2={len(d['header2'])//2-1}B")
PY
}

cmd_status() {
  load_env
  local h
  h=$(head 0)
  echo "head=$h  fork=${FORK:-?}  breathe=${BREATHE_INTERVAL:-default}"
  echo "--- mining set"
  expect0 mining
  echo "--- election"
  expect0 election
  echo "--- producers [max(1,h-19), h]"
  expect0 producers "$(( h > 20 ? h - 19 : 1 ))" "$h"
}

# 距下一个 breathe 边界的剩余窗口。分叉后、第一个 breathe 块之前，这就是
# "还能补救的时间"——超过它而 StakeHub 仍为空，链就停在那个块上（报告 §8）。
cmd_window() {
  load_env
  expect0 window --interval "${BREATHE_INTERVAL:-86400}" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("head=%s  下一个 breathe 边界 ts=%s  还剩 %ss（约 %s 块，块间隔 %ss）"
      % (d["head"], d["nextBreatheTs"], d["secondsUntil"], d["blocksUntil"], d["blockSeconds"]))
'
}

# 上线前的自检闸门（评审 C）。退出码非 0 就代表"现在这个状态不该继续"。
#
# 最重要的一条是 election.total > 0：分叉后如果 StakeHub 还是空的，第一个
# breathe 块就会把出让集算成空集。合约侧的长度保护（评审 A）已经保证那种情况
# 下不停链、保留现集合，但"保留现集合"意味着质押体系其实没生效——链在跑，
# 选举是死的。这条断言就是把这个静默失败变成显式失败。
cmd_check() {
  load_env
  local deep=0
  [ "${1:-}" = "--deep" ] && deep=1
  local h fork fail=0
  h=$(head 0)
  fork=${FORK:-0}
  echo "== 自检 =="
  echo "head=$h  fork=$fork  breathe=${BREATHE_INTERVAL:-86400}s  seats=$SEATS_N  生产者=$PRODUCERS_N  POSA=$POSA_COUNT_N"

  local cabinets total mining_n
  cabinets=$(expect0 cabinets | jsonf 'd["cabinets"]')
  mining_n=$(expect0 mining | jsonf 'len(d["validators"])')
  total=$(expect0 election | jsonf 'd["total"]')

  printf '  cabinets=%s   mining=%s   stakeHubElection=%s\n' "$cabinets" "$mining_n" "$total"

  if [ "$h" -lt "$fork" ]; then
    echo "  [--] head < fork：还没分叉，质押检查从 fork 之后开始"
    [ "$mining_n" -eq "$POSA_COUNT_N" ] || { echo "  [FAIL] 分叉前 mining 应为 $POSA_COUNT_N 席，实际 $mining_n"; fail=1; }
  else
    if [ "$total" -gt 0 ]; then
      echo "  [ OK ] StakeHub 非空（$total 席），breathe 选举有数据可用"
    else
      echo "  [FAIL] StakeHub 为空：第一个 post-fork breathe 块将选不出验证人。"
      echo "         合约长度保护会保留现集合（链不停），但质押/选举等于没上线。"
      echo "         立刻跑 ./run.sh register；剩余窗口："
      expect0 window --interval "${BREATHE_INTERVAL:-86400}" \
        | python3 -c 'import json,sys;d=json.load(sys.stdin);print("           还剩 %ss（约 %s 块）" % (d["secondsUntil"], d["blocksUntil"]))'
      fail=1
    fi
    [ "$mining_n" -eq "$cabinets" ] \
      && echo "  [ OK ] mining 席位数 = cabinets = $cabinets" \
      || { echo "  [FAIL] mining=$mining_n 与 cabinets=$cabinets 不一致"; fail=1; }
    # mining 集合必须等于 election 的 top-N（cabinets 是上限）。
    # 只在 election 非空时比：空集合时这一条必然失败，但根因是上面那条，
    # 重复报会把真正的结论淹掉。
    if [ "$total" -eq 0 ]; then
      echo "  [--] 跳过 mining/election 集合比对（election 为空，根因见上）"
    else
      local same
      same=$(python3 - "$(expect0 mining)" "$(expect0 election)" "$cabinets" <<'PY'
import json, sys
mining = {a.lower() for a in json.loads(sys.argv[1])["validators"]}
el = json.loads(sys.argv[2])
pairs = sorted(zip((p for p in el["powers"]), (a.lower() for a in el["validators"])),
               key=lambda kv: (-int(kv[0]), kv[1]))
top = {a for _, a in pairs[: int(sys.argv[3])]}
print("yes" if mining == top else f"no: mining-only={sorted(mining-top)} election-only={sorted(top-mining)}")
PY
)
      [ "$same" = "yes" ] \
        && echo "  [ OK ] mining 集合 = election top-$cabinets" \
        || { echo "  [FAIL] mining 集合与 election top-$cabinets 不一致：$same"; fail=1; }
    fi
  fi

  if [ "$deep" = 1 ]; then
    local ports="" i hres agree
    for ((i = 0; i < SEATS_N; i++)); do ports="${ports:+$ports,}$(node_http "$i")"; done
    hres=$(python3 "$SELF_DIR/expect.py" --port "$(node_http 0)" hashes "$ports" head)
    agree=$(echo "$hres" | jsonf 'd["agree"]')
    if [ "$agree" = "True" ]; then
      echo "$hres" | jsonf '"  [ OK ] 全部 %d 个节点 hash 一致 @%d" % (len(d["rows"]), d["rows"][0]["number"])'
    else
      echo "$hres" | jsonf '"  [FAIL] 节点之间 hash 不一致：%s" % [(r["port"], (r["hash"] or "none")[:18]) for r in d["rows"]]'
      fail=1
    fi
  fi

  echo
  if [ "$fail" -ne 0 ]; then
    echo "自检未通过" >&2
    return 1
  fi
  echo "自检通过"
}

case "${1:-}" in
  init)       shift; cmd_init "$@" ;;
  prepare)    shift; cmd_prepare "$@" ;;
  verify)     shift; load_env; verify_proofs "$@" ;;
  start)      shift; cmd_start "$@" ;;
  stop)       shift; cmd_stop "$@" ;;
  start-node) shift; cmd_start_node "$@" ;;
  stop-node)  shift; cmd_stop_node "$@" ;;
  wait)       shift; cmd_wait "$@" ;;
  register)   shift; cmd_register "$@" ;;
  delegate)   shift; cmd_delegate "$@" ;;
  evidence)   shift; cmd_evidence "$@" ;;
  status)     shift; cmd_status "$@" ;;
  window)     shift; cmd_window "$@" ;;
  check)      shift; cmd_check "$@" ;;
  *)
    sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
