#!/usr/bin/env bash
#
# Copyright 2026 The pgp-chain Authors
#

# Parlia devnet: 一条从 genesis 直接进入 Parlia 共识的 devnet。
#
#   ./run.sh init H [SEATS]  # 生成 SEATS 个 keystore + genesis（ForkHeight=H）+ geth init
#   ./run.sh start           # 出块（前台，Ctrl-C 停）
#   ./run.sh status          # 高度 + 每块 extra 长度 + H 块的系统交易 selector
#   ./run.sh wait N          # 等到高度 N（每 10 s 报一次进度）
#   ./run.sh stop            # 停
#   ./run.sh clean           # 删除 $PGP_PARLIA_HOME
#
# SEATS（默认 1）
# ---------------
# genesis 的 parliaGenesisValidators 里放 SEATS 个地址（全部由本脚本现生成），
# 节点只解锁第 0 个并签名，其余 5 个不在线。这不是「6 节点网络」，而是**单节点
# 6 席配置**：验证人集合是 6，出块的是其中 1 个。
#
# 这么做的理由：要验证的东西里，只有「多节点能否互联同步」需要 6 个进程，而
# 那部分是 BSC 原有逻辑、不是本次改造引入的风险。剩下四条 —— epoch 块 extra 长度
# = 32+1+6×68+65 = 506 B、验证人切换发生在 (N/2+1)*TurnLength-1 = 203 块、
# init 收 6 席 payload、getMiningValidators 返回 6 地址+6 空 bytes —— 全部与
# 进程数无关，单进程就能测出来。代价是出块变慢（非 in-turn 要等 wiggle），
# 平均 3–5 s/块而不是 3 s/块。
#
# 用真实 6 个验证人地址（各自持钥）跑时，把各节点的 keystore 分别放进自己的
# datadir，并让 --unlock 指向自己那个地址即可；genesis 侧的 6 席列表不变。
#
# 为什么默认 ForkHeight=1
# ------------------------
# H>1 意味着 block 1..H-1 必须由 legacy PBFT 出块。那需要 P-256 生产者私钥
# （legacy.producers 里的压缩公钥），而 Parlia 要的是 secp256k1 私钥——两套完全
# 不同的密钥体系，一个 keystore 装不下。H=1 让 block 0 之后直奔 Parlia，把
# 「Parlia 侧通不通」和「legacy 密钥怎么配」解耦开。
#
# H=1 仍然满足 D9 约束：1 % 200 = 1，不是 epoch 块（epoch 长度 200 是因为
# 未激活 Lorentz；见 consensus/parlia/parlia.go:58-63）。
#
# 想恢复到「从 legacy 切过来」的形态，把 init 的 H 设成 20 之类的值，并额外提供
# legacy.producers 与 P-256 keystore。脚本不会代劳这件事，因为静默编造一套
# legacy 密钥比报错更危险。
#
# NOTHING in this script touches the repository tree. Keys, genesis, datadir and
# logs live under $PGP_PARLIA_HOME (default: $HOME/.pgp-parlia-devnet).

set -euo pipefail

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SELF_DIR/../.." && pwd)
CONTRACT_ROOT=${CONTRACT_ROOT:-$REPO_ROOT/../bsc-genesis-contract}

GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}
HOME_DIR=${PGP_PARLIA_HOME:-$HOME/.pgp-parlia-devnet}
CHAIN_DIR=$HOME_DIR/chain
LOG_DIR=$HOME_DIR/logs
GENESIS=$HOME_DIR/genesis.json
PASSWORD=$HOME_DIR/password.txt
PIDFILE=$HOME_DIR/geth.pid
HTTP_PORT=${HTTP_PORT:-18545}
NETWORK_ID=${NETWORK_ID:-8606211}

mkdir -p "$CHAIN_DIR" "$LOG_DIR"

die() { echo "错误: $*" >&2; exit 1; }

need_bin() {
  [ -x "$GETH_BIN" ] || die "找不到 $GETH_BIN；先跑 go build -o build/bin/geth ./cmd/geth"
}

cmd_init() {
  local fork_height=${1:-1}
  local seats=${2:-1}
  need_bin
  [ -d "$CONTRACT_ROOT/out" ] || die "找不到 $CONTRACT_ROOT/out；先在 bsc-genesis-contract 里跑 forge build"

  case "$seats" in
    ''|*[!0-9]*) die "SEATS 必须是正整数，收到 '$seats'" ;;
  esac
  [ "$seats" -ge 1 ] || die "SEATS 必须 >= 1"

  # 幂等：重新 init 会重建 datadir，旧链数据必须一起清掉，否则 genesis 与已有
  # 块对不上，报的是「数据库里的 genesis 与文件不匹配」这种不好读的错。
  rm -rf "$CHAIN_DIR"
  mkdir -p "$CHAIN_DIR"

  printf 'devnet\n' > "$PASSWORD"

  # 生成 SEATS 个账户，全部放在同一个 keystore 目录里。节点只解锁第 0 个，
  # 其余的存在但不上线 —— 见文件头对 SEATS 的说明。
  local -a addrs=()
  local i addr
  for ((i = 0; i < seats; i++)); do
    addr=$("$GETH_BIN" account new --datadir "$CHAIN_DIR" --password "$PASSWORD" \
            | grep -o '0x[0-9a-fA-F]\{40\}' | head -1)
    [ -n "$addr" ] || die "第 $i 个 keystore 创建失败"
    addrs+=("$(echo "$addr" | tr 'A-Z' 'a-z')")
  done
  echo "${addrs[0]}" > "$HOME_DIR/validator.addr"

  # 5 列 conf：consensusAddr,feeAddr,bscFeeAddr,votingPower,bLSPublicKey
  # 每席三者同址。BLS 列留 `-`：4 元组 payload 不带 BLS 段，合约侧的
  # decodeValidator 在第 4 个字段就置 success。
  : > "$HOME_DIR/validators.conf"
  local balances=()
  for addr in "${addrs[@]}"; do
    echo "$addr,$addr,$addr,100,-" >> "$HOME_DIR/validators.conf"
    balances+=(--balance "$addr=0x56bc75e2d63100000")
  done
  cp "$HOME_DIR/validators.conf" "$HOME_DIR/validators.conf.bak"

  python3 "$SELF_DIR/gen-genesis.py" \
    --contracts-out "$CONTRACT_ROOT/out" \
    --validators "$HOME_DIR/validators.conf" \
    --fork-height "$fork_height" \
    --cabinets "$seats" \
    "${balances[@]}" \
    --out "$GENESIS"

  "$GETH_BIN" --datadir "$CHAIN_DIR" init "$GENESIS" > "$LOG_DIR/init.log" 2>&1 \
    || { tail -20 "$LOG_DIR/init.log"; die "geth init 失败"; }

  echo
  echo "验证人 : ${#addrs[@]} 席（本机签名者 = 第 0 席）"
  for ((i = 0; i < ${#addrs[@]}; i++)); do
    printf '  [%d] %s\n' "$i" "${addrs[i]}"
  done
  echo "ForkHeight: $fork_height"
  echo "下一步 : ./run.sh start"
}

cmd_start() {
  need_bin
  [ -f "$GENESIS" ] || die "还没有 genesis；先跑 ./run.sh init <H>"
  local addr
  addr=$(cat "$HOME_DIR/validator.addr")

  # --allow-insecure-unlock 是必须的：--unlock 会把私钥放进节点内存，而 geth
  # 默认拒绝在 HTTP RPC 暴露时这么做。devnet 上可以接受，主网绝不可以。
  #
  # exec 而不是后台 + nohup：进程要留在前台，Ctrl-C 才能干净地停。
  exec "$GETH_BIN" \
    --datadir "$CHAIN_DIR" --networkid "$NETWORK_ID" \
    --nodiscover --maxpeers 0 \
    --mine --miner.etherbase "$addr" \
    --unlock "$addr" --password "$PASSWORD" --allow-insecure-unlock \
    --miner.gaslimit 30000000 \
    --http --http.addr 127.0.0.1 --http.port "$HTTP_PORT" \
    --http.api eth,net,web3,admin \
    --syncmode full --verbosity 3
}

cmd_stop() {
  if [ -f "$PIDFILE" ]; then
    kill "$(cat "$PIDFILE")" 2>/dev/null || true
    rm -f "$PIDFILE"
    echo "已发送 SIGTERM"
  else
    pkill -f "pgp-parlia-devnet/chain" 2>/dev/null && echo "已按 datadir 路径停掉" || echo "没有在跑的节点"
  fi
}

cmd_status() {
  python3 "$SELF_DIR/inspect-blocks.py" --port "$HTTP_PORT"
}

cmd_wait() {
  local target=${1:?用法: ./run.sh wait <height>}
  python3 "$SELF_DIR/inspect-blocks.py" --port "$HTTP_PORT" --wait "$target"
}

cmd_clean() {
  [ -d "$HOME_DIR" ] || { echo "没有 $HOME_DIR"; return; }
  echo "将删除 $HOME_DIR（含 keystore 与链数据）"
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
    sed -n '6,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
