#!/usr/bin/env bash
# lib.sh -- shared helpers for the staking devnet harness.
#
# Layout:
#   $PGP_STAKING_HOME/<run>/net        the multi.sh rig (datadirs, genesis, logs)
#   $PGP_STAKING_HOME/<run>/prep       BLS proofs + createValidator calldata
#   $PGP_STAKING_HOME/<run>/evidence   double-sign evidence
#
# One scenario per directory so the four runs never share a datadir or a chain.

set -euo pipefail

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SELF_DIR/../.." && pwd)

GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}
MULTI=${MULTI:-$REPO_ROOT/scripts/parlia-devnet/multi.sh}
BOOTNODE_BIN=${BOOTNODE_BIN:-$REPO_ROOT/build/bin/bootnode}
SEED_PEERS_PY=${SEED_PEERS_PY:-$REPO_ROOT/scripts/pbft-devnet/seed-peers.py}
STAKING_HOME=${PGP_STAKING_HOME:-$HOME/.pgp-staking/default}
MULTI_HOME=$STAKING_HOME/net
export PGP_PARLIA_MULTI_HOME=$MULTI_HOME

HTTP_BASE=${HTTP_BASE:-18560}
P2P_BASE=${P2P_BASE:-30300}
DPOS_BASE=${DPOS_BASE:-20679}
CHAIN_ID=${CHAIN_ID:-860621}

VALIDATOR=0x0000000000000000000000000000000000001000
SLASH=0x0000000000000000000000000000000000001001
STAKE_HUB=0x0000000000000000000000000000000000002002
BLS_PRECOMPILE=0x0000000000000000000000000000000000000066
EVIDENCE_PRECOMPILE=0x0000000000000000000000000000000000000068

die() { echo "错误: $*" >&2; exit 1; }

node_dir() { echo "$MULTI_HOME/node$1"; }
node_http() { echo $((HTTP_BASE + $1)); }
node_p2p() { echo $((P2P_BASE + $1)); }
node_dpos() { echo $((DPOS_BASE + $1)); }
rpc_url() { echo "http://127.0.0.1:$(node_http "$1")"; }
val_addr() { cat "$(node_dir "$1")/validator.addr"; }

load_env() {
  [ -f "$MULTI_HOME/devnet.env" ] || die "缺少 $MULTI_HOME/devnet.env；先跑 run.sh init <a|b|c|d>"
  # shellcheck disable=SC1090
  . "$MULTI_HOME/devnet.env"
  SEATS_N=${SEATS:-0}
  PRODUCERS_N=${LEGACY_PRODUCERS:-$SEATS_N}
  POSA_FIRST_N=${POSA_FIRST:-0}
  POSA_COUNT_N=${POSA_COUNT:-$SEATS_N}
  [ "${SEATS_N:-0}" -ge 1 ] || die "devnet.env 里的 SEATS 不合法"
}

# head [node] -> current height
head() {
  local node=${1:-0}
  python3 "$SELF_DIR/expect.py" --port "$(node_http "$node")" head \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"])'
}

wait_head() {
  local target=$1 node=${2:-0} t0
  t0=$(date +%s)
  while :; do
    local h
    h=$(head "$node" 2>/dev/null || echo -1)
    if [ "$h" -ge "$target" ]; then
      echo "head=$h (>= $target), $(( $(date +%s) - t0 ))s"
      return 0
    fi
    if [ $(( $(date +%s) - t0 )) -gt 1800 ]; then
      die "等待 head>=$target 超时（当前 $h），看 $MULTI_HOME/logs/node$node.log"
    fi
    sleep 5
  done
}

# expect <args...> against node 0
expect0() { python3 "$SELF_DIR/expect.py" --port "$(node_http 0)" "$@"; }

# 从 stdin 的 JSON 取一个表达式。给 check/register 这类要拿值的调用点用，
# 免得每处都写一遍 python3 -c 的单行解析（也免得被 set -e 吞掉错误）。
jsonf() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
