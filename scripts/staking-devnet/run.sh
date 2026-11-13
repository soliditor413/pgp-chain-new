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
#   ./run.sh build               # 预编译 txtypes 工具
#   ./run.sh phase [--wait W]    # 当前处于 legacy / pre-cancun / post-cancun
#   ./run.sh gate                # 起链后门槛：出块、8 节点一致、余额就绪
#   ./run.sh txtypes --type N    # 发一笔指定类型的交易并断言
#   ./run.sh matrix [--window W] # 5 类型 + 未知类型跑一遍
#   ./run.sh window             # 距下一个 breathe 边界还剩多少秒/块
#   ./run.sh check [--deep]     # 自检闸门：election 非空、mining==cabinets、集合一致
#                              # --deep 再加 8 节点 hash 一致性。非 0 退出 = 不该继续。

set -euo pipefail
SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib.sh
. "$SELF_DIR/lib.sh"

cmd_init() {
  local run=${1:?用法: run.sh init <a|b|c|d>}
  local seats cab breathe align legacy posa fork delay
  fork=40; delay=0
  case "$run" in
    a) seats=6; cab=6; breathe=300; align=1; legacy=0; posa=0 ;;
    b) seats=6; cab=5; breathe=300; align=1; legacy=0; posa=0 ;;
    c) seats=6; cab=6; breathe=300; align=1; legacy=0; posa=0 ;;
    d) seats=4; cab=4; breathe=2592000; align=0; legacy=0; posa=0 ;;
    # e: 4 个 legacy 生产者 + 4 个全新的 POSA 验证人（两批人完全不相交）
    e) seats=8; cab=4; breathe=300; align=1; legacy=4; posa=4 ;;
    # f: 交易类型矩阵。F=100 让 legacy 段有 ~300s 可以跑完 W0；
    #    Cancun..Fermi 排在 T0+600，于是分叉后还有 ~300s 的「未开 Cancun」窗口；
    #    breathe 拉到 30 天，避免 updateValidatorSetV2 混进测试窗口。
    f) seats=8; cab=4; breathe=2592000; align=1; legacy=4; posa=4; fork=100; delay=600 ;;
    *) die "未知 run: ${run}（可选 a/b/c/d/e/f）" ;;
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

  # legacy 段的费率分成：给一个非 0 比例，才能对比「分叉前按比例分给 fee sink」
  # 与「分叉后整笔进 SystemAddress」。收款地址用最后一个节点，它既不是发送方
  # 也不是接收方，余额增量好观测。
  local fee_rcpt="" fee_ratio=0
  if [ "$run" = f ]; then
    fee_ratio=65
    # 固定地址而不是节点地址：init 时 node 目录还没建出来（multi.sh 才建），
    # 而且这个地址不需要私钥，余额从 0 起算，增量最好观测。
    fee_rcpt=0x000000000000000000000000000000000000fee5
  fi

  local tfork=""
  [ "$delay" -gt 0 ] && tfork=$((ts + delay))

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
  BSC_TIME_FORKS=$tfork \
  GAS_FEE_RECIPIENT=${fee_rcpt:-0x0000000000000000000000000000000000000000} \
  GAS_FEE_RECIPIENT_RATIO=$fee_ratio \
    "$MULTI" init "$seats" "$fork"

  load_env
  echo
  echo "run=$run  seats=$SEATS_N  cabinets=$CABINETS  breathe=${BREATHE_INTERVAL:-default}  T0=$((ts))  fork=$fork"
  [ -n "$tfork" ] && echo "时间分叉（Cancun..Fermi）TS=$tfork  fee sink=$fee_rcpt ratio=$fee_ratio%"
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
    [ "$out" = "0x01" ] || die "节点 $i 的 BLS proof 未通过 0x66（返回 ${out}）；检查 voteAddress/chainId"
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

# tx 类型矩阵用的工具：预先编译，避免 W0 窗口被 go run 冷启动吃掉一半时间。
TXTOOLS=${TXTOOLS:-$REPO_ROOT/build/bin/txtypes}

cmd_build() {
  ( cd "$REPO_ROOT" && go build -o build/bin/txtypes ./scripts/staking-devnet/txtypes ) || die "编译 txtypes 失败"
  echo "已编译: $TXTOOLS"
}

# phase 需要读 genesis 的 chainConfig，显式传路径，不依赖环境里有没有 PGP_STAKING_HOME。
expect0_phase() {
  python3 "$SELF_DIR/expect.py" --port "$(node_http 0)" --genesis "$MULTI_HOME/genesis.json" phase
}

# run.sh phase [--wait legacy|pre-cancun|post-cancun]
cmd_phase() {
  load_env
  local want=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --wait) want=${2:?}; shift 2 ;;
      *) die "未知参数 $1" ;;
    esac
  done
  local i out cur
  for ((i = 0; i < 900; i++)); do
    out=$(expect0_phase 2>/dev/null || true)
    if [ -z "$out" ]; then cur="starting"; else cur=$(echo "$out" | jsonf 'd["phase"]'); fi
    if [ -z "$want" ] || [ "$cur" = "$want" ]; then
      echo "$out"
      return 0
    fi
    sleep 2
  done
  die "等待 phase=$want 超时（当前 ${cur}）"
}

# 起链后、进矩阵前的最低门槛：出块已开始、8 节点一致、发送账户都有余额。
# 它挡掉的是"链还没起来就发 tx"这类假失败，不是功能性断言。
cmd_gate() {
  load_env
  local h i ports="" low=0
  # 创世前 RPC 已经在，但 head 还没有块；用 || echo 0 兜住"连接被拒"的阶段。
  h=$(head 0 2>/dev/null || echo 0)
  for ((i = 0; h < 2 && i < 180; i++)); do sleep 2; h=$(head 0 2>/dev/null || echo 0); done
  [ "$h" -ge 2 ] || die "链没有出块（head=${h}）"

  for ((i = 0; i < SEATS_N; i++)); do ports="${ports:+$ports,}$(node_http "$i")"; done
  local agree
  agree=$(python3 "$SELF_DIR/expect.py" --port "$(node_http 0)" hashes "$ports" head | jsonf 'd["agree"]')
  [ "$agree" = "True" ] || die "节点之间 head hash 不一致"

  for i in 0 1 2 3 4 5; do
    local addr bal
    addr=$(val_addr "$i")
    bal=$(python3 "$SELF_DIR/expect.py" --port "$(node_http 0)" balance "$addr" | jsonf 'd["eth"]')
    printf '  node%d %s 余额 %s ETH\n' "$i" "${addr:0:10}.." "$bal"
    awk -v b="$bal" 'BEGIN{exit !(b < 1)}' && low=1
  done
  [ "$low" = 0 ] || die "有账户余额低于 1 ETH"
  echo "gate 通过: head=${h}（8 节点一致，账户余额就绪）"
}

# 一个交易类型固定用一个账户：delegated 账户有在途限制，混用会互相踩。
tx_node() {
  case "$1" in
    0) echo 0 ;;
    1) echo 1 ;;
    2) echo 2 ;;
    3) echo 4 ;;
    4) echo 3 ;;
    *) echo 0 ;;
  esac
}

# run.sh txtypes --type N [工具自己的参数...]
cmd_txtypes() {
  load_env
  [ -x "$TXTOOLS" ] || die "缺少 ${TXTOOLS}；先跑 ./run.sh build"
  local ty="" nodenv=""
  local -a args=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --type) ty=${2:?}; shift 2 ;;
      --node) nodenv=${2:?}; shift 2 ;;
      *) args+=("$1"); shift ;;
    esac
  done
  [ -n "$ty" ] || die "用法: run.sh txtypes --type 0..5 [--node I] [--tip ... --expect ...]"

  local node from to
  node=${nodenv:-$(tx_node "$ty")}
  from=$(val_addr "$node")
  to=$(val_addr 5)

  "$TXTOOLS" \
    --genesis "$MULTI_HOME/genesis.json" \
    --rpc "$(rpc_url "$node")" \
    --keystore "$(node_dir "$node")/keystore" \
    --password-file "$(node_dir "$node")/password.txt" \
    --from "$from" --to "$to" --type "$ty" \
    "${args[@]+"${args[@]}"}"
}

# run.sh matrix [--window legacy|pre-cancun|post-cancun]
# 期望值由工具按 genesis chainConfig 推导，这里只负责挑类型、串联、归档。
cmd_matrix() {
  load_env
  local window=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --window) window=${2:?}; shift 2 ;;
      *) die "未知参数 $1" ;;
    esac
  done

  local dir="$STAKING_HOME/txtypes"
  mkdir -p "$dir"
  local file="$dir/results.jsonl"
  local fail=0 pass=0 skipped=0 ty out rc

  echo "== 交易类型矩阵 window=${window:-auto} =="
  for ty in 0 1 2 3 4 5; do
    local extra=()
    [ "$ty" = "3" ] && extra+=(--timeout 300s)
    # setcode 的委托目标：BSCValidatorSet（D 块起有代码），委托后 getCode 会是
    # 0xef0100‖0x…1000，能直接比对。
    [ "$ty" = "4" ] && extra+=(--delegate 0x0000000000000000000000000000000000001000)
    rc=0
    out=$("$TXTOOLS" \
      --genesis "$MULTI_HOME/genesis.json" \
      --rpc "$(rpc_url "$(tx_node "$ty")")" \
      --keystore "$(node_dir "$(tx_node "$ty")")/keystore" \
      --password-file "$(node_dir "$(tx_node "$ty")")/password.txt" \
      --from "$(val_addr "$(tx_node "$ty")")" \
      --to "$(val_addr 5)" --type "$ty" \
      "${extra[@]+"${extra[@]}"}" 2>&1) || rc=$?
    echo "$out" >> "$file"
    printf '  type %s: %s\n' "$ty" "$(echo "$out" | jsonf 'd.get("outcome","?") + " expect=" + d.get("expect","?") + " phase=" + d.get("phase","?")' 2>/dev/null || echo '解析失败')"
    if [ $rc -eq 0 ]; then pass=$((pass + 1)); else fail=$((fail + 1)); fi
  done
  echo "结果: pass=$pass fail=$fail  （明细 ${file}）"
  [ $fail -eq 0 ] || return 1
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
  build)      shift; cmd_build "$@" ;;
  phase)      shift; cmd_phase "$@" ;;
  gate)       shift; cmd_gate "$@" ;;
  txtypes)    shift; cmd_txtypes "$@" ;;
  matrix)     shift; cmd_matrix "$@" ;;
  check)      shift; cmd_check "$@" ;;
  *)
    sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
