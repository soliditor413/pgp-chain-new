#!/usr/bin/env bash
#
# Copyright 2026 The pgp-chain Authors
# This file is part of the pgp-chain library.
#
# The pgp-chain library is free software: you can redistribute it and/or modify
# it under the terms of the GNU Lesser General Public License as published by
# the Free Software Foundation, either version 3 of the License, or
# (at your option) any later version.
#
# The pgp-chain library is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
# GNU Lesser General Public License for more details.
#
# You should have received a copy of the GNU Lesser General Public License
# along with the pgp-chain library. If not, see <http://www.gnu.org/licenses/>.

# N producers, N nodes, one host. This is the M4 T9 bench.
#
#   ./multinode.sh init          # keys + genesis + one datadir and nodekey per node
#   ./multinode.sh start         # start all nodes in the background
#   ./multinode.sh status        # per node: pid, height, devp2p peers, dpos port
#   ./multinode.sh spread        # how far apart the heights are (fork proxy)
#   ./multinode.sh kill 1        # SIGKILL node 1, for the recovery criterion
#   ./multinode.sh logs 1        # tail -f one node
#   ./multinode.sh stop          # SIGTERM all
#   ./multinode.sh verify-keys   # re-open every keystore through dpos.GetDposAccount
#   ./multinode.sh clean         # delete $PGP_MULTINODE_HOME (asks first)
#
# WHY THIS IS A SEPARATE SCRIPT FROM run.sh
#
# run.sh runs ONE node with --nodiscover --maxpeers 0. That is exactly right for
# what it tests (block production, the seal ordering, follower mode) and exactly
# wrong for what M4 tests. With no devp2p peers at all:
#
#   * no ELAMSG (0x08) is ever exchanged, so dpos.Routes never learns a single
#     peer address;
#   * with no learned address the DPoS TCP server has nobody to dial;
#   * with nobody to dial there is no consensus traffic to verify.
#
# A single node still produces blocks in that state, which is why the omission is
# easy to miss: the run looks healthy and proves nothing about the network layer.
# So this rig wires devp2p peers explicitly, gives every node its own DPoS port,
# and puts the whole producer set on one host.
#
# FOUR PORTS PER NODE, ALL DIFFERENT
#
#   devp2p   --port           30300+i   peer connections, carries ELAMSG
#   HTTP     --http.port       8600+i   JSON-RPC, used by `status`
#   DPoS     --pbft.net.port  20679+i   consensus messages
#   chain    --networkid      8606212   must not collide with run.sh's 8606211
#
# Nothing here touches the repository tree. Keys, genesis, datadirs, logs and
# pids all live under $PGP_MULTINODE_HOME (default ~/.pgp-devnet-multi).

set -euo pipefail

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SELF_DIR/../.." && pwd)

GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}
# bootnode is used for one thing only: turning a nodekey into the enode id that
# goes into --bootnodes. It is already in the tree (cmd/bootnode), which avoids
# hand-deriving a secp256k1 public key in bash.
BOOTNODE_BIN=${BOOTNODE_BIN:-$REPO_ROOT/build/bin/bootnode}

HOME_DIR=${PGP_MULTINODE_HOME:-$HOME/.pgp-devnet-multi}
KEYS_DIR=$HOME_DIR/keys
GENESIS=$HOME_DIR/genesis.json
LOG_DIR=$HOME_DIR/logs
PID_DIR=$HOME_DIR/pids
NODEKEY_DIR=$HOME_DIR/nodekeys

NODES=${NODES:-4}
CHAIN_ID=${CHAIN_ID:-8606212}
BASE_P2P_PORT=${BASE_P2P_PORT:-30300}
BASE_HTTP_PORT=${BASE_HTTP_PORT:-8600}
BASE_DPOS_PORT=${BASE_DPOS_PORT:-20679}

HTTP_ADDR=${HTTP_ADDR:-127.0.0.1}
# Where devp2p is reachable. Separate from DPOS_ADDRESS because they answer
# different questions: this one goes in the enode URL, the other one is what the
# producer advertises as its DPoS listener.
P2P_ADDRESS=${P2P_ADDRESS:-127.0.0.1}
# The address every node advertises for its DPoS listener. On this rig all nodes
# use 127.0.0.1 and the ports differ, which is legal: a DAddr record is IP:port,
# and dpos.Network keys its dial table by PID, not by IP.
DPOS_ADDRESS=${DPOS_ADDRESS:-127.0.0.1}
# Empty means "let the binary use its own default" (pbft.DefaultNetMagic,
# 202511). Set it only to run two independent rigs side by side.
DPOS_MAGIC=${DPOS_MAGIC:-}
# SEED_DPOS=1 pre-writes each node's DPoS address database, i.e. the peers.json
# path that bypasses the ELAMSG relay. See seed_dpos_peers.
SEED_DPOS=${SEED_DPOS:-1}

VERBOSITY=${VERBOSITY:-3}
STOP_TIMEOUT=${STOP_TIMEOUT:-30}

log()  { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

need_tool() { command -v "$1" >/dev/null 2>&1; }

node_dir()       { printf '%s' "$HOME_DIR/node$1"; }
node_log()       { printf '%s' "$LOG_DIR/node$1.log"; }
node_pidfile()   { printf '%s' "$PID_DIR/node$1.pid"; }
node_keyfile()   { printf '%s' "$NODEKEY_DIR/node$1.key"; }
node_p2p_port()  { printf '%s' $(( BASE_P2P_PORT  + $1 )); }
node_http_port() { printf '%s' $(( BASE_HTTP_PORT + $1 )); }
node_dpos_port() { printf '%s' $(( BASE_DPOS_PORT + $1 )); }

# --- tooling ----------------------------------------------------------------

ensure_bins() {
    need_tool curl || die "curl is required (status/spread talk JSON-RPC through it)"
    need_tool go   || die "the go toolchain is required to build geth and bootnode"
    mkdir -p "$REPO_ROOT/build/bin"
    if [ ! -x "$GETH_BIN" ]; then
        log "building geth -> $GETH_BIN"
        ( cd "$REPO_ROOT" && go build -o "$GETH_BIN" ./cmd/geth )
    fi
    if [ ! -x "$BOOTNODE_BIN" ]; then
        log "building bootnode -> $BOOTNODE_BIN"
        ( cd "$REPO_ROOT" && go build -o "$BOOTNODE_BIN" ./cmd/bootnode )
    fi
}

# devnet_password mirrors run.sh: environment only, nothing on disk, nothing in
# the repository.
devnet_password() {
    if [ -n "${PGP_DEVNET_PASSWORD:-}" ]; then
        printf '%s' "$PGP_DEVNET_PASSWORD"
        return 0
    fi
    if [ -n "${PGP_DEVNET_PASSWORD_FILE:-}" ]; then
        [ -r "$PGP_DEVNET_PASSWORD_FILE" ] || die "cannot read PGP_DEVNET_PASSWORD_FILE=$PGP_DEVNET_PASSWORD_FILE"
        printf '%s' "$(tr -d '\r\n' < "$PGP_DEVNET_PASSWORD_FILE")"
        return 0
    fi
    die "no password: export PGP_DEVNET_PASSWORD (or PGP_DEVNET_PASSWORD_FILE)"
}

# env_password_supported: see the long note in run.sh. Capturing --help first
# rather than piping into `grep -q` avoids a SIGPIPE false negative under
# pipefail, and a false negative here puts the keystore password in argv.
env_password_supported() {
    local help
    help=$("$GETH_BIN" --help 2>&1 || true)
    case "$help" in
        *GETH_PBFT_KEYSTORE_PASSWORD*) return 0 ;;
        *) return 1 ;;
    esac
}

# --- JSON -------------------------------------------------------------------

# json_get reads stdin and prints the value at a jq path made of plain keys and
# [n] indices. Same contract as run.sh: a missing key prints the empty string,
# never the literal null.
json_get() {
    local path=$1 out=""
    if need_tool jq; then
        out=$(jq -r "$path") || return 1
        if [ "$out" = "null" ]; then out=""; fi
        printf '%s' "$out"
    elif need_tool python3; then
        python3 -c '
import json, re, sys
cur = json.load(sys.stdin)
for part in sys.argv[1].strip(".").split("."):
    if not part:
        continue
    m = re.fullmatch(r"([A-Za-z_][\w-]*)?((?:\[\d+\])+)", part)
    if not m:
        cur = cur[part]
        continue
    if m.group(1):
        cur = cur[m.group(1)]
    for idx in re.findall(r"\[(\d+)\]", m.group(2)):
        cur = cur[int(idx)]
print("" if cur is None else (json.dumps(cur) if isinstance(cur, (dict, list)) else cur))
' "$path"
    else
        die "need jq or python3 to read JSON"
    fi
}

# list_len <file> <dotted-path> prints the length of the list at that path.
#
# Separate from json_get because "| length" is jq syntax with no equivalent in the
# python fallback: that fallback walks plain keys and indices, so a filter would
# be looked up as a literal key named "producers | length" and fail with a
# KeyError that says nothing about the real problem.
list_len() {
    local file=$1 path=$2
    if need_tool jq; then
        if [ -z "$path" ]; then jq -r 'length' "$file"; else jq -r ".$path | length" "$file"; fi
    elif need_tool python3; then
        python3 -c '
import json, sys
doc = json.load(open(sys.argv[1]))
for part in sys.argv[2].strip(".").split("."):
    if part:
        doc = doc[part]
print(len(doc))
' "$file" "$path"
    else
        die "need jq or python3 to read JSON"
    fi
}

rpc_at() {
    local port=$1 method=$2
    shift 2
    local params="" sep="" arg
    for arg in "$@"; do
        params+="$sep$arg"
        sep=","
    done
    curl -s -m 5 -X POST -H 'Content-Type: application/json' \
        --data "{\"jsonrpc\":\"2.0\",\"method\":\"$method\",\"params\":[$params],\"id\":1}" \
        "http://$HTTP_ADDR:$port"
}

# --- init -------------------------------------------------------------------

cmd_init() {
    ensure_bins
    local password
    password=$(devnet_password)

    local i
    for ((i = 0; i < NODES; i++)); do
        if [ -e "$(node_dir "$i")/geth" ]; then
            die "datadir $(node_dir "$i") already holds a chain. './multinode.sh clean' first:
  the producer set determines the genesis hash, and geth refuses to init a
  datadir whose stored genesis differs (GenesisMismatchError)."
        fi
    done

    mkdir -p "$HOME_DIR" "$LOG_DIR" "$PID_DIR" "$NODEKEY_DIR" "$KEYS_DIR"
    chmod 700 "$HOME_DIR" "$KEYS_DIR" "$NODEKEY_DIR"

    if [ -f "$KEYS_DIR/producers.json" ] && [ "${FORCE_KEYS:-0}" != "1" ]; then
        log "reusing existing arbiter keystores in $KEYS_DIR (FORCE_KEYS=1 to regenerate)"
    else
        log "generating $NODES Elastos arbiter keystore(s) in $KEYS_DIR"
        local -a keygen_args=( -out "$KEYS_DIR" -n "$NODES" )
        if [ "${FORCE_KEYS:-0}" = "1" ]; then
            keygen_args+=( -force )
        fi
        ( cd "$REPO_ROOT" && PGP_DEVNET_PASSWORD="$password" \
            go run ./scripts/pbft-devnet/keygen "${keygen_args[@]}" >/dev/null )
    fi
    chmod 700 "$KEYS_DIR"
    chmod 600 "$KEYS_DIR"/*.dat

    local manifest=$KEYS_DIR/producers.json
    local have
    have=$(list_len "$manifest" "producers")
    # The producer set and the node count have to match: a producer with no node
    # never votes, so the set can never reach its threshold, and the rig would sit
    # at height 0 with every node looking healthy.
    if [ "$have" != "$NODES" ]; then
        die "manifest lists $have producers but NODES=$NODES.
  Regenerate: FORCE_KEYS=1 NODES=$NODES ./multinode.sh init   (after 'clean')"
    fi

    log "rendering genesis -> $GENESIS"
    render_genesis "$manifest"

    # Nodekeys first: --bootnodes needs every enode id, and the ids come from
    # these keys. Generating them here, instead of letting geth create
    # datadir/geth/nodekey on first start, is what makes the topology knowable
    # before any node runs.
    log "generating $NODES devp2p nodekey(s) -> $NODEKEY_DIR"
    for ((i = 0; i < NODES; i++)); do
        local kf
        kf=$(node_keyfile "$i")
        if [ ! -s "$kf" ]; then
            "$BOOTNODE_BIN" -genkey "$kf" >/dev/null 2>&1 || die "bootnode -genkey failed for node $i"
        fi
        chmod 600 "$kf"
    done

    for ((i = 0; i < NODES; i++)); do
        local dd
        dd=$(node_dir "$i")
        mkdir -p "$dd"
        log "geth init node $i -> $dd"
        "$GETH_BIN" init --datadir "$dd" "$GENESIS" 2>&1 | tail -2
        write_node_env "$i" "$manifest"
    done

    log ""
    log "== multinode bench ready =="
    log "  home      : $HOME_DIR"
    log "  nodes     : $NODES   producers: $have   confirm threshold: $(( NODES * 2 / 3 ))"
    log "  chain id  : $CHAIN_ID"
    log "  devp2p    : $(node_p2p_port 0) .. $(node_p2p_port $(( NODES - 1 )))"
    log "  http      : $(node_http_port 0) .. $(node_http_port $(( NODES - 1 )))"
    log "  dpos      : $(node_dpos_port 0) .. $(node_dpos_port $(( NODES - 1 )))"
    log ""
    log "next: ./multinode.sh start && sleep 10 && ./multinode.sh status"
}

# render_genesis substitutes the producer list and the chain id into the shared
# template, by JSON parsing rather than text replacement.
render_genesis() {
    local manifest=$1 tmp=$GENESIS.tmp
    if need_tool jq; then
        jq --argjson prods "$(jq '.producers' "$manifest")" --argjson cid "$CHAIN_ID" \
            '.config.legacy.producers = $prods | .config.chainId = $cid' \
            "$SELF_DIR/genesis.json" > "$tmp"
    elif need_tool python3; then
        python3 - "$SELF_DIR/genesis.json" "$manifest" "$tmp" "$CHAIN_ID" <<'PY'
import json, sys
template, manifest, out, chain_id = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
doc = json.load(open(template))
producers = json.load(open(manifest))["producers"]
if not producers:
    sys.exit("manifest lists no producers")
doc["config"]["legacy"]["producers"] = producers
doc["config"]["chainId"] = chain_id
json.dump(doc, open(out, "w"), indent=2)
PY
    else
        die "need jq or python3 to render the genesis"
    fi
    local count
    [ -s "$tmp" ] || die "genesis render produced an empty file"
    count=$(list_len "$tmp" "config.legacy.producers")
    [ "$count" -ge 1 ] 2>/dev/null || die "rendered genesis has no producers"
    mv "$tmp" "$GENESIS"
    log "  producers in genesis: $count"
}

# enode_of prints enode://<128 hex>@<p2p host>:<p2p port> for one nodekey.
enode_of() {
    local id
    id=$("$BOOTNODE_BIN" -nodekey "$(node_keyfile "$1")" -writeaddress) || return 1
    printf 'enode://%s@%s:%s' "$id" "$P2P_ADDRESS" "$(node_p2p_port "$1")"
}

# bootnodes_of prints the comma separated --bootnodes value for node i: every
# other node in the rig.
#
# This replaced a static-nodes.json in each datadir, which this tree does not
# read: geth logs "The static-nodes.json file is deprecated and ignored. Use
# P2P.StaticNodes in config.toml instead." at ERROR level and then runs with zero
# peers. The rig still looked alive -- block height stayed at 0 but so did every
# other node, so `spread` reported agreement -- which is exactly the failure mode
# a bench must not have. --bootnodes needs discovery on, so start_one no longer
# passes --nodiscover.
#
# Full mesh rather than a star around node 0: ELAMSG address records travel over
# devp2p, and a star would make the DPoS dial table depend on node 0 staying up
# in the one scenario (kill -9) this bench exists to test.
bootnodes_of() {
    local i=$1 list="" sep="" j
    for ((j = 0; j < NODES; j++)); do
        if [ "$j" = "$i" ]; then
            continue
        fi
        list+="$sep$(enode_of "$j")"
        sep=","
    done
    printf '%s' "$list"
}

# account_field <manifest> <index> <field> reads one field of the arbiter with
# that keygen index.
#
# Not json_get ".accounts[$i].$field": keygen sorts the accounts array by public
# key so that producers[0] is the block-1 duty slot, and that sort is independent
# of each entry's own "index" field. Indexing the array positionally therefore
# hands node 0 whichever key sorts first, which is why an earlier run had node 1
# loading arbiter-0.dat. It still worked (each node's keystore and miner address
# came from the same entry, and all four keys are in the set), but "which node
# holds which key" becomes unreadable, and this rig exists to be read.
account_field() {
    local manifest=$1 idx=$2 field=$3 out=""
    if need_tool jq; then
        out=$(jq -r --argjson i "$idx" --arg f "$field" \
            '.accounts[] | select(.index == $i) | .[$f]' "$manifest")
        if [ "$out" = "null" ]; then out=""; fi
        printf '%s' "$out"
    elif need_tool python3; then
        python3 -c '
import json, sys
doc = json.load(open(sys.argv[1]))
idx, field = int(sys.argv[2]), sys.argv[3]
for a in doc["accounts"]:
    if a.get("index") == idx:
        print(a.get(field, ""))
        break
' "$manifest" "$idx" "$field"
    else
        die "need jq or python3 to read JSON"
    fi
}

# write_node_env records everything `start` needs for one node, so the running
# path never parses the manifest. Values are single-quoted: these are paths and
# hex strings, but a datadir with a space would otherwise split the source.
write_node_env() {
    local i=$1 manifest=$2 keystore miner
    keystore=$(account_field "$manifest" "$i" keystore)
    miner=$(account_field "$manifest" "$i" minerAddress)
    [ -n "$keystore" ] || die "manifest has no arbiter with index $i"
    [ -n "$miner" ] || die "manifest has no minerAddress for arbiter $i"
    [ -f "$keystore" ] || die "manifest names a keystore that does not exist: $keystore"
    cat > "$(node_dir "$i")/node.env" <<EOF
# Generated by multinode.sh init. Node $i of $NODES.
KEYSTORE='$keystore'
MINER='$miner'
P2P_PORT='$(node_p2p_port "$i")'
HTTP_PORT='$(node_http_port "$i")'
DPOS_PORT='$(node_dpos_port "$i")'
BOOTNODES='$(bootnodes_of "$i")'
EOF
}

# seed_dpos_peers writes node i's DPoS address database directly, so the TCP
# server knows where the other producers listen without learning it over ELAMSG.
#
# This is the B half of the T9 isolation plan: it takes the devp2p relay out of
# the picture so a bench failure can only mean the consensus layer is broken.
# With SEED_DPOS=0 the same datadir runs the real path (A), and the difference
# between the two runs is entirely "how did the address get here".
#
# It is a file write rather than a debug flag on purpose. dpos/p2p already loads
# PID -> address from this file at startup (addrmgr.loadPeers, "Loaded N addresses
# from file" in the log), and Network.ConnectPeers only asks the address manager
# for a dial target -- it does not care whether the entry came from ELAMSG or from
# disk. So the whole of B is this function; nothing in geth changes.
#
# Format, from addrmgr.go:33-41 (no json tags on those structs, so the field names
# are the Go ones, and PID is a [33]byte so it encodes as an array of numbers).
#
# The address for producer P is DPOS_ADDRESS:BASE_DPOS_PORT+<node index>, and the
# node index is the keygen "index" field -- NOT the position in producers[], which
# is sorted by public key and therefore a different order.
seed_dpos_peers() {
    local i=$1 dd
    dd=$(node_dir "$i")
    mkdir -p "$dd/geth/network/dpos"
    python3 - "$KEYS_DIR/producers.json" "$i" "$DPOS_ADDRESS" "$BASE_DPOS_PORT" \
        "$dd/geth/network/dpos/peers.json" <<'PY'
import json, sys
manifest, self_index = sys.argv[1], int(sys.argv[2])
host, base_port, out = sys.argv[3], int(sys.argv[4]), sys.argv[5]
accounts = json.load(open(manifest))["accounts"]
addrs = []
for a in accounts:
    if a["index"] == self_index:
        continue
    key = bytes.fromhex(a["publicKey"])
    if len(key) != 33:
        sys.exit("public key %s is %d bytes, want 33" % (a["publicKey"], len(key)))
    addrs.append({"PID": list(key), "Addr": "%s:%d" % (host, base_port + a["index"])})
json.dump({"Addresses": addrs}, open(out, "w"), indent=2)
PY
    chmod 600 "$dd/geth/network/dpos/peers.json"
}

# --- start / stop -----------------------------------------------------------

node_pid() {
    local f
    f=$(node_pidfile "$1")
    [ -f "$f" ] || return 1
    cat "$f" 2>/dev/null
}

node_alive() {
    local pid
    pid=$(node_pid "$1") || return 1
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

# ENV_PW_OK is computed once per run: --help costs a process start, and asking
# four times for the same binary is four chances to believe different answers.
ENV_PW_OK=0

start_one() {
    local i=$1 dd logf pidf
    dd=$(node_dir "$i")
    logf=$(node_log "$i")
    pidf=$(node_pidfile "$i")

    [ -f "$dd/node.env" ] || die "node $i has no node.env: './multinode.sh init' first"
    if node_alive "$i"; then
        die "node $i is already running (pid $(node_pid "$i"))"
    fi

    local KEYSTORE MINER P2P_PORT HTTP_PORT DPOS_PORT BOOTNODES
    # shellcheck disable=SC1090
    source "$dd/node.env"

    local -a pbft_args=(
        --pbft.enable
        --pbft.keystore "$KEYSTORE"
        --pbft.miner.address "$MINER"
        --pbft.net.address "$DPOS_ADDRESS"
        --pbft.net.port "$DPOS_PORT"
        # The cap has to cover the whole set, because every producer is on this
        # one host: the DPoS server refuses the (n+1)-th inbound connection from
        # an IP and logs it at Debug only, so a cap below N turns a 4-node rig
        # into a 3-node one with no visible error.
        --pbft.net.maxnodeperhost "$NODES"
    )
    if [ -n "$DPOS_MAGIC" ]; then
        pbft_args+=( --pbft.net.magic "$DPOS_MAGIC" )
    fi

    local password
    password=$(devnet_password)
    if [ "$ENV_PW_OK" = "1" ] && [ "${ARGV_PASSWORD:-0}" != "1" ]; then
        export GETH_PBFT_KEYSTORE_PASSWORD="$password"
    else
        pbft_args+=( --pbft.keystore.password "$password" )
        warn "node $i: binary does not read GETH_PBFT_KEYSTORE_PASSWORD; password is in argv"
    fi

    if [ "$SEED_DPOS" = "1" ]; then
        seed_dpos_peers "$i"
    fi

    : > "$logf"
    # `env -u` instead of `unset`: this runs inside a loop over the nodes, so
    # unsetting GETH_BIN here would take it away from every later node (and trip
    # `set -u`). geth applies every GETH_*-prefixed variable as a config
    # override, so our own variables have to be scrubbed from the child only --
    # GETH_PBFT_KEYSTORE_PASSWORD stays, because geth genuinely reads that one.
    #
    # --maxpeers must cover N-1 bootnode peers; run.sh's --maxpeers 0 would
    # disconnect every one of them, and devp2p is what carries the DPoS address
    # relay. Discovery stays on because --bootnodes only seeds discovery.
    env -u GETH_BIN -u BOOTNODE_BIN \
        "$GETH_BIN" \
        --datadir "$dd" \
        --networkid "$CHAIN_ID" \
        --nodekey "$(node_keyfile "$i")" \
        --bootnodes "$BOOTNODES" --maxpeers "$NODES" --port "$P2P_PORT" \
        --http --http.addr "$HTTP_ADDR" --http.port "$HTTP_PORT" \
        --http.api eth,net,web3 \
        --mine --verbosity "$VERBOSITY" \
        "${pbft_args[@]}" \
        </dev/null >>"$logf" 2>&1 &
    printf '%s\n' "$!" > "$pidf"
    log "  node $i: pid $!  rpc $HTTP_ADDR:$HTTP_PORT  dpos :$DPOS_PORT  log $logf"
}

cmd_start() {
    [ -f "$GENESIS" ] || die "no genesis at $GENESIS: './multinode.sh init' first"
    ensure_bins
    if env_password_supported; then ENV_PW_OK=1; fi
    local i=${1:-}
    if [ -n "$i" ]; then
        # Starting a single node is what the recovery criterion needs: after
        # 'kill 3', restarting the whole set would refuse on the three that are
        # still running.
        case "$i" in
            *[!0-9]*) die "node index must be a number, got '$i'" ;;
        esac
        [ "$i" -lt "$NODES" ] || die "node index $i out of range 0..$(( NODES - 1 ))"
        start_one "$i"
        return 0
    fi
    for ((i = 0; i < NODES; i++)); do
        start_one "$i"
    done
    log ""
    log "all $NODES nodes launched. They need a few seconds to handshake:"
    log "  ./multinode.sh status"
}

cmd_stop() {
    local i any=0
    for ((i = 0; i < NODES; i++)); do
        if node_alive "$i"; then
            any=1
            kill -TERM "$(node_pid "$i")" 2>/dev/null || true
        fi
    done
    if [ "$any" = "0" ]; then
        log "no nodes are running"
        return 0
    fi
    # One deadline for the set rather than one per node: a node mid-round can take
    # ~6s (signTolerance) to unwind, and N sequential deadlines would take N times
    # as long to report a genuine hang.
    local start elapsed alive
    start=$(date +%s)
    while :; do
        alive=0
        for ((i = 0; i < NODES; i++)); do
            if node_alive "$i"; then
                alive=$(( alive + 1 ))
            fi
        done
        if [ "$alive" -eq 0 ]; then
            break
        fi
        elapsed=$(( $(date +%s) - start ))
        if [ "$elapsed" -ge "$STOP_TIMEOUT" ]; then
            warn "$alive node(s) still alive after ${STOP_TIMEOUT}s; try './multinode.sh kill <i>'"
            return 1
        fi
        sleep 0.2
    done
    for ((i = 0; i < NODES; i++)); do
        rm -f "$(node_pidfile "$i")"
    done
    log "stopped in $(( $(date +%s) - start ))s"
}

# cmd_kill is SIGKILL on purpose: the T9 recovery criterion is about a producer
# that died without unwinding, not one that left the set politely.
cmd_kill() {
    local i=${1:-}
    [ -n "$i" ] || die "usage: ./multinode.sh kill <node-index>"
    case "$i" in
        ''|*[!0-9]*) die "node index must be a number, got '$i'" ;;
    esac
    [ "$i" -lt "$NODES" ] || die "node index $i out of range 0..$(( NODES - 1 ))"
    node_alive "$i" || die "node $i is not running"
    local pid
    pid=$(node_pid "$i")
    kill -9 "$pid"
    rm -f "$(node_pidfile "$i")"
    log "SIGKILL -> node $i (pid $pid)"
    log "expect: the remaining $(( NODES - 1 )) producers keep finalising. With"
    log "        threshold $(( NODES * 2 / 3 )) of $NODES, losing one still leaves a quorum."
}

# --- status -----------------------------------------------------------------

# parse_quantity reads an RPC response on stdin and prints the result as a
# decimal number, or "-" when the call did not produce one.
#
# The shape is validated before any arithmetic: `$(( 0x... ))` on a non-number
# aborts the script under set -e, and a dead node's empty response would then look
# like a crash of the script rather than a dead node.
parse_quantity() {
    local raw hex
    raw=$(cat)
    hex=$(printf '%s' "$raw" | json_get '.result' 2>/dev/null || true)
    case "$hex" in
        0x*|0X*) ;;
        *) printf '-'; return 0 ;;
    esac
    case "${hex#0[xX]}" in
        ''|*[!0-9a-fA-F]*) printf '-'; return 0 ;;
    esac
    printf '%d' "$(( hex ))"
}

height_of() { rpc_at "$1" eth_blockNumber | parse_quantity; }
peers_of()  { rpc_at "$1" net_peerCount   | parse_quantity; }

cmd_status() {
    printf '%-6s %-8s %-9s %-8s %-10s\n' node pid height devp2p dpos-port
    local i pid h p
    for ((i = 0; i < NODES; i++)); do
        if node_alive "$i"; then pid=$(node_pid "$i"); else pid='-'; fi
        h=$(height_of "$(node_http_port "$i")")
        p=$(peers_of "$(node_http_port "$i")")
        printf '%-6s %-8s %-9s %-8s %-10s\n' "$i" "$pid" "$h" "$p" "$(node_dpos_port "$i")"
    done
    log ""
    log "'-' means the RPC did not answer. A node can be alive and still not answer"
    log "during its first seconds, so re-run before reading '-' as a failure."
    log "A node whose devp2p count is 0 cannot receive a DAddr record at all."
}

# cmd_spread is the fork check that does not need a fork detector: N nodes that
# agree on a height within a block or two are finalising the same chain, while a
# node that lost the set runs ahead without limit.
cmd_spread() {
    local i h heights="" min="" max="" fmt=""
    for ((i = 0; i < NODES; i++)); do
        h=$(height_of "$(node_http_port "$i")")
        heights+="$fmt$h"
        fmt=" "
        if [ "$h" = "-" ]; then
            continue
        fi
        if [ -z "$min" ] || [ "$h" -lt "$min" ]; then min=$h; fi
        if [ -z "$max" ] || [ "$h" -gt "$max" ]; then max=$h; fi
    done
    if [ -z "$min" ]; then
        die "no node answered the RPC; nothing to compare"
    fi
    printf 'heights: %s\n' "$heights"
    printf 'min %s  max %s  spread %s\n' "$min" "$max" "$(( max - min ))"
    if [ $(( max - min )) -le 2 ]; then
        log "OK: within 2 blocks -- one chain (a lone producer runs away without limit)"
    else
        warn "spread > 2: the set is not finalising the same chain"
    fi
}

cmd_logs() {
    local i=${1:-0}
    [ -f "$(node_log "$i")" ] || die "no log for node $i"
    tail -n 80 -f "$(node_log "$i")"
}

cmd_verify_keys() {
    ensure_bins
    local password
    password=$(devnet_password)
    ( cd "$REPO_ROOT" && PGP_DEVNET_PASSWORD="$password" \
        go run ./scripts/pbft-devnet/keygen -out "$KEYS_DIR" -n "$NODES" -verify )
}

cmd_clean() {
    if [ ! -d "$HOME_DIR" ]; then
        log "nothing to delete: $HOME_DIR does not exist"
        return 0
    fi
    local i
    for ((i = 0; i < NODES; i++)); do
        if node_alive "$i"; then
            die "node $i is still running; './multinode.sh stop' first"
        fi
    done
    warn "clean deletes $HOME_DIR entirely: the arbiter keystores, the nodekeys,"
    warn "every datadir and every log. The producer set cannot be recovered from"
    warn "anywhere else -- a new init generates a new set and a new genesis."
    if [ "${CONFIRM_CLEAN:-}" != "yes" ]; then
        if [ ! -t 0 ]; then
            die "refusing to delete without confirmation: set CONFIRM_CLEAN=yes"
        fi
        printf 'type "delete" to confirm: '
        local answer=""
        read -r answer
        [ "$answer" = "delete" ] || die "aborted; nothing was deleted"
    fi
    rm -rf "$HOME_DIR"
    log "deleted $HOME_DIR"
}

usage() {
    cat <<'USAGE'
usage: ./multinode.sh <command>

  init            generate N arbiter keystores, one genesis, one datadir and
                  nodekey per node, and the static peer list
  start [i]       start all N nodes in the background, or only node i
  status          per node: pid, height, devp2p peer count, dpos port
  spread          height spread across nodes; > 2 means they disagree
  kill <i>        SIGKILL node i (recovery criterion)
  logs <i>        tail -f node i (default 0)
  stop            SIGTERM all nodes and wait up to STOP_TIMEOUT
  verify-keys     re-open every keystore through dpos.GetDposAccount
  clean           delete $PGP_MULTINODE_HOME (asks for confirmation)

environment:
  PGP_DEVNET_PASSWORD       required by init/start/verify-keys
  PGP_MULTINODE_HOME        runtime root (default $HOME/.pgp-devnet-multi)
  NODES                     producer and node count (default 4)
  CHAIN_ID                  default 8606212
  BASE_P2P_PORT             default 30300 (node i uses base+i)
  BASE_HTTP_PORT            default 8600
  BASE_DPOS_PORT            default 20679
  DPOS_MAGIC                unset -> binary default (pbft.DefaultNetMagic)
  SEED_DPOS                 1 (default) pre-writes peers.json so the DPoS TCP
                            layer dials without the ELAMSG relay; 0 exercises
                            the real discovery path
  FORCE_KEYS=1              regenerate the keystores
  CONFIRM_CLEAN=yes         skip clean's confirmation prompt
  ARGV_PASSWORD=1           force the keystore password onto the command line
  VERBOSITY                 geth verbosity (default 3)
USAGE
}

cmd=${1:-}
case "$cmd" in
    init)        cmd_init ;;
    start)       shift; cmd_start "$@" ;;
    stop)        cmd_stop ;;
    kill)        shift; cmd_kill "$@" ;;
    status)      cmd_status ;;
    spread)      cmd_spread ;;
    logs)        shift; cmd_logs "$@" ;;
    verify-keys) cmd_verify_keys ;;
    clean)       cmd_clean ;;
    ''|help|-h|--help)
        usage
        log ""
        log "NODES=$NODES  PGP_MULTINODE_HOME=$HOME_DIR  CHAIN_ID=$CHAIN_ID" ;;
    *)           die "unknown command: $cmd (try: init start stop kill status spread logs clean)" ;;
esac
