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

# One-command PBFT devnet: Elastos arbiter keystores, a genesis whose
# legacy.producers is our own key set, and a node that can try to produce blocks.
#
#   ./run.sh init            # keys + genesis + geth init
#   ./run.sh start           # producing node (foreground, Ctrl-C stops it)
#   ./run.sh start-follower  # --pbft.enable=false, read-only follower
#   ./run.sh status          # eth_blockNumber + per-block extra length
#   ./run.sh stop            # SIGTERM the running node from another terminal
#   ./run.sh verify-keys     # re-open every keystore through dpos.GetDposAccount
#   ./run.sh clean           # delete keys, genesis, datadir and logs
#
# NOTHING in this script touches the repository tree. Keys, genesis, datadir,
# logs and the transient geth config all live under $PGP_DEVNET_HOME
# (default: $HOME/.pgp-devnet), so a stray `git add -A` cannot pick a private
# key up.
#
# See README.md in this directory for the runbook, the pass/fail criteria and the
# security notes.

set -euo pipefail

SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SELF_DIR/../.." && pwd)

# Repository layout. The binary goes to build/bin, never to the repository root:
# the root holds a historical `geth` binary that must not be overwritten.
GETH_BIN=${GETH_BIN:-$REPO_ROOT/build/bin/geth}

# Runtime layout, deliberately outside the repository.
HOME_DIR=${PGP_DEVNET_HOME:-$HOME/.pgp-devnet}
KEYS_DIR=$HOME_DIR/keys
DATA_DIR=$HOME_DIR/chain
LOG_DIR=$HOME_DIR/logs
GENESIS=$HOME_DIR/genesis.json
# Legacy artefact of an earlier revision of this script, which put the keystore
# password in a 0600 TOML file. pbft.Config.KeyStorePassword is now toml:"-", so
# that mechanism is gone; the path is kept only so stop/clean can delete a file a
# previous run may have left behind.
GETH_CONFIG=$HOME_DIR/geth-config.toml
PIDFILE=$HOME_DIR/geth.pid

# Producer set size. 1 is the only size a single node can ever finalise on its
# own (see README: quorum of a 1-key set is 0 signatures, of a 4-key set is 2
# votes, which one node cannot supply). Use 4 only for the future multi-node run.
PRODUCERS=${PRODUCERS:-1}

# Chain id. MUST differ from pgp mainnet's 860621: core/genesis.go's
# checkPGPDatabaseMatch refuses to start a chain with that id on a datadir whose
# genesis is not pgp mainnet's. It must also match genesis.json's config.chainId.
CHAIN_ID=${CHAIN_ID:-8606211}

HTTP_ADDR=${HTTP_ADDR:-127.0.0.1}
HTTP_PORT=${HTTP_PORT:-8555}
P2P_PORT=${P2P_PORT:-30399}
VERBOSITY=${VERBOSITY:-3}

# How long `stop` waits for SIGTERM before giving up. geth's miner closes before
# the consensus engine does, so a node that is mid-round can take up to one
# signTolerance (6s) longer than usual to exit. That is not a hang.
STOP_TIMEOUT=${STOP_TIMEOUT:-30}

# Set to 1 to force the password onto the command line even when the binary reads
# GETH_PBFT_KEYSTORE_PASSWORD. See the security notes in README.md.
ARGV_PASSWORD=${ARGV_PASSWORD:-0}

log()  { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# --- small helpers ----------------------------------------------------------

need_tool() { command -v "$1" >/dev/null 2>&1; }

# json_get reads the JSON on stdin and prints the value at a jq-style path.
# jq is preferred; python3 is the fallback so the script works on a machine
# without jq.
json_get() {
    local path=$1 out=""
    if need_tool jq; then
        # jq -r prints the literal `null` for a missing key, and a bare `null`
        # reaching an arithmetic expansion makes bash look for a variable called
        # `null` (and abort under set -u, with a message that names neither the
        # path nor the response). Normalise it to the empty string so both
        # backends share one contract: "missing means empty".
        out=$(jq -r "$path") || return 1
        if [ "$out" = "null" ]; then
            out=""
        fi
        printf '%s' "$out"
    elif need_tool python3; then
        python3 -c '
import json, re, sys
path = sys.argv[1]
doc = json.load(sys.stdin)
cur = doc
for part in path.strip(".").split("."):
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
if isinstance(cur, (dict, list)):
    print(json.dumps(cur))
elif cur is None:
    print("")
else:
    print(cur)
' "$path"
    else
        die "need jq or python3 to read JSON"
    fi
}

# rpc sends one JSON-RPC call. Parameters are joined with commas: "$*" would
# glue them together without separators, so the two-argument call
# `eth_getBlockByNumber "0x1" false` would serialise as `["0x1" false]` -- not
# JSON, and geth answers with a parse error instead of a block. That failure is
# silent unless the caller checks for it, which is exactly how the per-block part
# of `status` managed to look implemented while never having run.
rpc() {
    local method=$1
    shift
    local params="" sep="" arg
    for arg in "$@"; do
        params+="$sep$arg"
        sep=","
    done
    curl -s -m 5 -X POST -H 'Content-Type: application/json' \
        --data "{\"jsonrpc\":\"2.0\",\"method\":\"$method\",\"params\":[$params],\"id\":1}" \
        "http://$HTTP_ADDR:$HTTP_PORT"
}

# devnet_password resolves the keystore password from the environment only. No
# password is ever written into the repository or into a keygen-created file.
devnet_password() {
    if [ -n "${PGP_DEVNET_PASSWORD:-}" ]; then
        printf '%s' "$PGP_DEVNET_PASSWORD"
        return 0
    fi
    if [ -n "${PGP_DEVNET_PASSWORD_FILE:-}" ]; then
        [ -r "$PGP_DEVNET_PASSWORD_FILE" ] || die "cannot read PGP_DEVNET_PASSWORD_FILE=$PGP_DEVNET_PASSWORD_FILE"
        # Strip the trailing newline an editor adds.
        printf '%s' "$(tr -d '\r\n' < "$PGP_DEVNET_PASSWORD_FILE")"
        return 0
    fi
    die "no password: export PGP_DEVNET_PASSWORD (or PGP_DEVNET_PASSWORD_FILE)"
}

ensure_geth() {
    if [ -x "$GETH_BIN" ]; then
        return 0
    fi
    need_tool go || die "geth binary missing at $GETH_BIN and the go toolchain is not on PATH"
    log "building geth -> $GETH_BIN"
    ( cd "$REPO_ROOT" && mkdir -p build/bin && go build -o "$GETH_BIN" ./cmd/geth )
}

# env_password_supported reports whether this binary reads the keystore password
# from GETH_PBFT_KEYSTORE_PASSWORD.
#
# urfave/cli prints a flag's environment variables in --help, which is the only
# cheap way to tell a binary that predates the environment wiring apart from one
# that supports it. Guessing would be worse than asking: a wrong guess starts the
# node as a silent verifier.
#
# Do NOT write this as `"$GETH_BIN" --help | grep -q ...`. Under `set -o pipefail`
# that reads a SIGPIPE as a failure: grep exits at the match, geth is still
# writing, the writer gets EPIPE, the pipeline returns 141 and the answer is
# "no". Measured on this tree: `geth --help | grep -q GETH_PBFT_KEYSTORE_PASSWORD`
# returns 141 five times out of five with pipefail and 0 without -- and the
# consequence of the false negative is not cosmetic: cmd_start then falls back to
# `--pbft.keystore.password`, putting the password in argv where `ps` can see it.
# Capturing the output first has no such race.
env_password_supported() {
    local help
    help=$("$GETH_BIN" --help 2>&1 || true)
    case "$help" in
        *GETH_PBFT_KEYSTORE_PASSWORD*) return 0 ;;
        *) return 1 ;;
    esac
}

is_running() {
    [ -f "$PIDFILE" ] || return 1
    local pid
    pid=$(cat "$PIDFILE" 2>/dev/null || true)
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

pid_of() { cat "$PIDFILE" 2>/dev/null || true; }

# --- commands ---------------------------------------------------------------

cmd_init() {
    ensure_geth
    need_tool go || die "the go toolchain is required: keygen runs through 'go run'"
    local password
    password=$(devnet_password)

    if [ -e "$DATA_DIR/geth" ]; then
        die "datadir $DATA_DIR already holds a chain. Use './run.sh clean' first:
  a new producer set changes the genesis hash, and geth refuses to init a
  datadir whose stored genesis differs (GenesisMismatchError)."
    fi

    mkdir -p "$HOME_DIR" "$LOG_DIR"
    chmod 700 "$HOME_DIR"
    mkdir -p "$KEYS_DIR"
    chmod 700 "$KEYS_DIR"

    if [ -f "$KEYS_DIR/producers.json" ] && [ "${FORCE_KEYS:-0}" != "1" ]; then
        log "reusing existing arbiter keystores in $KEYS_DIR (FORCE_KEYS=1 to regenerate)"
    else
        log "generating $PRODUCERS Elastos arbiter keystore(s) in $KEYS_DIR"
        local -a keygen_args=( -out "$KEYS_DIR" -n "$PRODUCERS" )
        [ "${FORCE_KEYS:-0}" = "1" ] && keygen_args+=( -force )
        ( cd "$REPO_ROOT" && PGP_DEVNET_PASSWORD="$password" \
            go run ./scripts/pbft-devnet/keygen "${keygen_args[@]}" >/dev/null )
    fi
    chmod 700 "$KEYS_DIR"
    chmod 600 "$KEYS_DIR"/*.dat

    log "rendering genesis -> $GENESIS"
    render_genesis

    log "geth init"
    "$GETH_BIN" init --datadir "$DATA_DIR" "$GENESIS" 2>&1 | tail -3

    local miner
    miner=$(json_get '.accounts[0].minerAddress' < "$KEYS_DIR/producers.json")
    log ""
    log "== devnet ready =="
    log "  home          : $HOME_DIR"
    log "  keys          : $KEYS_DIR ($PRODUCERS arbiter(s))"
    log "  genesis       : $GENESIS"
    log "  datadir       : $DATA_DIR"
    log "  chain id      : $CHAIN_ID"
    log "  block-1 miner : $miner   (producer #0 of the sorted set)"
    log ""
    log "next: ./run.sh start      (producing, foreground)"
    log "      ./run.sh status     (another terminal)"
}

# render_genesis substitutes the producer list into the checked-in template.
#
# The template is valid JSON with a single placeholder string in
# config.legacy.producers, and the substitution is done by a JSON parser rather
# than by text replacement, so a malformed render cannot be written by accident.
render_genesis() {
    local tmp=$GENESIS.tmp
    if need_tool jq; then
        jq --argjson prods "$(jq '.producers' "$KEYS_DIR/producers.json")" \
            '.config.legacy.producers = $prods' "$SELF_DIR/genesis.json" > "$tmp"
    elif need_tool python3; then
        python3 - "$SELF_DIR/genesis.json" "$KEYS_DIR/producers.json" "$tmp" <<'PY'
import json, sys
template, manifest, out = sys.argv[1], sys.argv[2], sys.argv[3]
doc = json.load(open(template))
producers = json.load(open(manifest))["producers"]
if not producers:
    sys.exit("manifest lists no producers")
doc["config"]["legacy"]["producers"] = producers
json.dump(doc, open(out, "w"), indent=2)
PY
    else
        die "need jq or python3 to render the genesis"
    fi
    local count
    count=$(json_get '.config.legacy.producers | length' < "$tmp")
    [ "$count" -ge 1 ] 2>/dev/null || die "rendered genesis has no producers"
    mv "$tmp" "$GENESIS"
    log "  producers in genesis: $count"
}

# cmd_start wires the producer settings and starts geth.
#
# How the keystore password gets in: pbft.Config.KeyStorePassword is marked
# toml:"-" in consensus/pbft/config.go, i.e. it is excluded from --config and
# from `geth dumpconfig`. That is deliberate -- a password that can be read back
# out of a file on disk is not a secret -- so the only ways in are the
# --pbft.keystore.password flag and the GETH_PBFT_KEYSTORE_PASSWORD environment
# variable.
#
# The environment variable is preferred, because command line arguments are
# world readable through `ps` for the lifetime of the process. Whether the binary
# reads it is checked from --help rather than assumed: a binary predating the
# environment wiring would otherwise start as a silent verifier.
cmd_start() {
    [ -f "$GENESIS" ] || die "no genesis at $GENESIS: run './run.sh init' first"
    [ -d "$DATA_DIR/geth" ] || die "datadir $DATA_DIR is empty: run './run.sh init' first"
    is_running && die "a devnet node is already running (pid $(pid_of)); './run.sh stop' first"
    ensure_geth
    mkdir -p "$LOG_DIR"

    local keystore miner password
    keystore=$(json_get '.accounts[0].keystore' < "$KEYS_DIR/producers.json")
    miner=$(json_get '.accounts[0].minerAddress' < "$KEYS_DIR/producers.json")
    [ -f "$keystore" ] || die "keystore $keystore is missing: run './run.sh init'"
    password=$(devnet_password)

    local -a pbft_args=( --pbft.enable --pbft.keystore "$keystore"
                         --pbft.miner.address "$miner" )
    if env_password_supported && [ "${ARGV_PASSWORD:-0}" != "1" ]; then
        export GETH_PBFT_KEYSTORE_PASSWORD="$password"
    else
        pbft_args+=( --pbft.keystore.password "$password" )
        warn "this geth does not read GETH_PBFT_KEYSTORE_PASSWORD: the keystore password"
        warn "goes on the command line and is visible to 'ps' while the node runs"
    fi

    local logfile=$LOG_DIR/geth.log
    log "starting producing node (chain $CHAIN_ID, miner $miner)"
    log "  log: $logfile"
    log "  rpc: http://$HTTP_ADDR:$HTTP_PORT   Ctrl-C stops the node"
    log ""

    # The pid must be recorded before exec so that './run.sh stop' from another
    # terminal addresses geth itself: exec keeps the pid.
    printf '%s\n' "producer" > "$HOME_DIR/mode"
    printf '%s\n' "$$" > "$PIDFILE"
    # geth applies every GETH_*-prefixed variable in the environment as a config
    # override, so it would otherwise warn "Unknown config environment variable
    # envvar=GETH_BIN" about our own variable -- and, worse, silently honour any
    # other GETH_* the operator happens to have exported. Copy the path into a
    # local name, then clear ours; GETH_PBFT_KEYSTORE_PASSWORD stays, because
    # that one geth genuinely reads.
    local geth_exe=$GETH_BIN
    unset GETH_BIN
    exec > >(tee -a "$logfile") 2>&1
    exec "$geth_exe" \
        --datadir "$DATA_DIR" \
        --networkid "$CHAIN_ID" \
        --nodiscover --maxpeers 0 --port "$P2P_PORT" \
        --http --http.addr "$HTTP_ADDR" --http.port "$HTTP_PORT" \
        --http.api eth,net,web3 \
        --mine --verbosity "$VERBOSITY" \
        ${pbft_args[@]+"${pbft_args[@]}"}
}

cmd_start_follower() {
    [ -d "$DATA_DIR/geth" ] || die "datadir $DATA_DIR is empty: run './run.sh init' first"
    is_running && die "a devnet node is already running (pid $(pid_of)); './run.sh stop' first"
    ensure_geth
    mkdir -p "$LOG_DIR"

    local logfile=$LOG_DIR/geth-follower.log
    log "starting follower node (--pbft.enable=false, no keystore, no mining)"
    log "  log: $logfile"
    log "  rpc: http://$HTTP_ADDR:$HTTP_PORT   Ctrl-C stops the node"
    log ""

    printf '%s\n' "follower" > "$HOME_DIR/mode"
    printf '%s\n' "$$" > "$PIDFILE"
    exec > >(tee -a "$logfile") 2>&1
    exec "$GETH_BIN" \
        --datadir "$DATA_DIR" \
        --networkid "$CHAIN_ID" \
        --nodiscover --maxpeers 0 --port "$P2P_PORT" \
        --http --http.addr "$HTTP_ADDR" --http.port "$HTTP_PORT" \
        --http.api eth,net,web3 \
        --pbft.enable=false --verbosity "$VERBOSITY"
}

cmd_stop() {
    if ! is_running; then
        log "no devnet node is running"
        rm -f "$PIDFILE" "$GETH_CONFIG"
        return 0
    fi
    local pid start elapsed
    pid=$(pid_of)
    local mode=""
    [ -f "$HOME_DIR/mode" ] && mode=$(cat "$HOME_DIR/mode")
    if [ "$mode" = "producer" ]; then
        log "SIGTERM -> $pid (a mid-round seal can take up to ~6s to unwind; that is by design)"
    else
        log "SIGTERM -> $pid"
    fi
    kill -TERM "$pid" 2>/dev/null || true
    start=$(date +%s)
    while kill -0 "$pid" 2>/dev/null; do
        elapsed=$(( $(date +%s) - start ))
        if [ "$elapsed" -ge "$STOP_TIMEOUT" ]; then
            warn "still alive after ${STOP_TIMEOUT}s; the shell that exec'd it may have died. Try: kill -9 $pid"
            return 1
        fi
        sleep 0.2
    done
    elapsed=$(( $(date +%s) - start ))
    log "stopped in ${elapsed}s"
    rm -f "$PIDFILE" "$GETH_CONFIG"
}

cmd_status() {
    local raw number
    raw=$(rpc eth_blockNumber) || die "cannot reach the RPC endpoint on http://$HTTP_ADDR:$HTTP_PORT"
    number=$(printf '%s' "$raw" | json_get '.result' 2>/dev/null || true)
    [ -n "$number" ] || die "unexpected eth_blockNumber response: $raw"

    local dec=$(( number ))
    log "eth_blockNumber = $number ($dec)"
    log ""

    if [ "$dec" -eq 0 ]; then
        log "no block has been produced."
        diagnose_stall
        return 1
    fi

    local tip=$dec first=1
    [ "$tip" -gt 1 ] && first=$(( tip - 1 ))
    local prev_ts=0 prev_num=-1
    local n hdr num_hex ts extra diff miner extra_len
    for n in $(seq "$first" "$tip"); do
        hdr=$(rpc eth_getBlockByNumber "\"$(printf '0x%x' "$n")\"" false)
        num_hex=$(printf '%s' "$hdr" | json_get '.result.number' 2>/dev/null || true)
        ts=$(printf '%s' "$hdr" | json_get '.result.timestamp' 2>/dev/null || true)
        extra=$(printf '%s' "$hdr" | json_get '.result.extraData' 2>/dev/null || true)
        diff=$(printf '%s' "$hdr" | json_get '.result.difficulty' 2>/dev/null || true)
        miner=$(printf '%s' "$hdr" | json_get '.result.miner' 2>/dev/null || true)
        # Check every field before doing arithmetic on it. Without this the script
        # dies with `null: unbound variable` -- naming neither the height nor the
        # RPC response, which is the opposite of a useful error.
        if [ -z "$num_hex" ] || [ -z "$ts" ] || [ -z "$extra" ] || [ -z "$diff" ]; then
            die "eth_getBlockByNumber 0x$(printf '%x' "$n") returned no usable block: $hdr"
        fi
        case "$extra" in
            0x*) ;;
            *) die "block $n has a malformed extraData: $extra" ;;
        esac
        extra_len=$(( (${#extra} - 2) / 2 ))
        local votes=$(( (extra_len - 143) / 132 ))
        log "block $(( num_hex ))  ts=$(( ts ))  difficulty=$(( diff ))  extra=${extra_len}B  votes=$votes"
        log "          coinbase=$miner"
        if [ "$prev_num" -ge 0 ]; then
            log "          interval=$(( ts - prev_ts ))s (expected 3)"
        fi
        prev_ts=$ts prev_num=$n
    done
    log ""
    log "PASS: the chain is advancing; extra length must be 143 + 132*votes"
}

# cmd_verify_secrets is the script-layer guard for the keystore password.
#
# The Go-layer guard (eth/ethconfig/secrets_test.go) pins the toml:"-" tag on the
# type. This one checks the artifact an operator actually runs: a real binary,
# a real `dumpconfig`, a real sentinel value. The two are complementary -- the Go
# test can be satisfied by a mechanism the binary does not use, and the binary
# can be built from a tree whose test never ran.
#
# Three assertions, all of which have to hold:
#   1. the sentinel password does not appear in the output;
#   2. no key by that name appears, in either spelling (Go's KeyStorePassword or
#      the snake_case key_store_password a tool might normalise to);
#   3. the [Eth.Pbft] section IS there. Without this one, a build that dropped the
#      whole section would pass checks 1 and 2 while guarding nothing.
#
# Both input channels are exercised: the command line flag and
# GETH_PBFT_KEYSTORE_PASSWORD (skipped on a binary that predates it).
cmd_verify_secrets() {
    ensure_geth

    local sentinel="SENTINEL-verify-secrets-$$-do-not-emit"
    local out failures=0

    log "checking $GETH_BIN"
    log "  sentinel: $sentinel"
    log ""

    # Channel 1: the flag. This is the value that used to reach the output.
    if out=$("$GETH_BIN" --pbft.enable --pbft.keystore.password "$sentinel" dumpconfig 2>&1); then
        check_dumpconfig_output "$out" "$sentinel" "--pbft.keystore.password" || failures=$(( failures + 1 ))
    else
        warn "dumpconfig failed with the password flag:"
        printf '%s\n' "$out" | tail -5 >&2
        failures=$(( failures + 1 ))
    fi

    # Channel 2: the environment variable, when the binary supports it.
    if env_password_supported; then
        if out=$(GETH_PBFT_KEYSTORE_PASSWORD="$sentinel" "$GETH_BIN" dumpconfig 2>&1); then
            check_dumpconfig_output "$out" "$sentinel" "GETH_PBFT_KEYSTORE_PASSWORD" || failures=$(( failures + 1 ))
        else
            warn "dumpconfig failed with GETH_PBFT_KEYSTORE_PASSWORD:"
            printf '%s\n' "$out" | tail -5 >&2
            failures=$(( failures + 1 ))
        fi
    else
        log "  SKIP  GETH_PBFT_KEYSTORE_PASSWORD: this binary does not read it"
    fi

    log ""
    if [ "$failures" -ne 0 ]; then
        die "$failures dumpconfig check(s) failed: the keystore password can reach disk"
    fi
    log "PASS: neither channel puts the keystore password into geth's serialized config"
    # Scope note, printed on success too: a green run here does NOT mean dumpconfig
    # is secret-free. The [Metrics] InfluxDB password/token are still emitted in
    # clear (upstream defect, deliberately not fixed in this round because
    # toml:"-" would break existing --config files on upgrade). See README,
    # "已知同类隐患：InfluxDB 密码/令牌".
    log "NOTE: this gate covers the PBFT keystore password only; the [Metrics]"
    log "      InfluxDB password/token are still emitted in clear (known, unfixed)."
}

# check_dumpconfig_output applies the three assertions to one dumpconfig run.
check_dumpconfig_output() {
    local out=$1 sentinel=$2 channel=$3 failed=0

    if printf '%s' "$out" | grep -qF -- "$sentinel"; then
        warn "  FAIL  [$channel] the sentinel password is in the output"
        printf '%s\n' "$out" | grep -nF -- "$sentinel" | head -3 >&2
        failed=1
    else
        log "  ok    [$channel] no sentinel value"
    fi
    local key
    for key in keystorepassword key_store_password; do
        if printf '%s' "$out" | grep -qiF -- "$key"; then
            warn "  FAIL  [$channel] the key name \"$key\" is in the output"
            failed=1
        else
            log "  ok    [$channel] no \"$key\" key"
        fi
    done
    if printf '%s' "$out" | grep -qF -- '[Eth.Pbft]'; then
        log "  ok    [$channel] [Eth.Pbft] section present (the checks above are not vacuous)"
    else
        warn "  FAIL  [$channel] no [Eth.Pbft] section: fix that first, it makes the other checks meaningless"
        failed=1
    fi
    return $failed
}

# diagnose_stall explains the two known reasons a devnet produces nothing, so an
# operator does not have to read the source to interpret blockNumber 0.
diagnose_stall() {
    local mode=""
    [ -f "$HOME_DIR/mode" ] && mode=$(cat "$HOME_DIR/mode")
    if [ "$mode" = "follower" ]; then
        log "  this node runs with --pbft.enable=false: it never produces blocks, it"
        log "  only follows someone else's. blockNumber 0 is expected with no producer."
        return 0
    fi
    local logfile=$LOG_DIR/geth.log
    log "diagnosis:"
    if [ ! -f "$logfile" ]; then
        log "  (no log at $logfile - was the node started by this script?)"
        return 0
    fi
    if grep -q 'Collect majority signs' "$logfile"; then
        log "  the node DID build a confirm (see 'Collect majority signs' in $logfile)"
        if grep -q 'seal timed out waiting for a confirm' "$logfile"; then
            log "  but Seal then timed out: the confirm was created before Seal started"
            log "  waiting for it, and consensus/pbft/seal.go drains the round channels"
            log "  in between, so it is discarded. See README.md, 'known blocker'."
        fi
        if grep -q 'height already has a confirm, refusing to propose' "$logfile"; then
            log "  the height is now blocked by the confirm already recorded in the"
            log "  block pool, so every retry answers ErrAlreadyConfirmedBlock."
        fi
    elif grep -q 'Not preparing a header for sealing' "$logfile"; then
        log "  the node refuses to prepare work; see the 'not preparing' lines in $logfile"
    elif grep -q 'cannot open arbiter keystore' "$logfile"; then
        log "  the keystore could not be opened: check the password and the file mode."
    elif grep -q 'SignerNotOnduty\|not on duty\|is not onDuty' "$logfile"; then
        log "  the node is not the on-duty producer for this view."
    else
        log "  nothing conclusive in $logfile; run with VERBOSITY=4 and look for 'PBFT' lines."
    fi
}

cmd_verify_keys() {
    [ -f "$KEYS_DIR/producers.json" ] || die "no keystores at $KEYS_DIR"
    ( cd "$REPO_ROOT" && PGP_DEVNET_PASSWORD="$(devnet_password)" \
        go run ./scripts/pbft-devnet/keygen -out "$KEYS_DIR" -verify )
}

cmd_clean() {
    if [ -d "$HOME_DIR" ]; then
        local answer
        answer=${CONFIRM_CLEAN:-}
        if [ -z "$answer" ]; then
            printf 'delete %s (keys, genesis, chain data, logs)? [y/N] ' "$HOME_DIR"
            read -r answer || answer=""
        fi
        case "$answer" in
            y|Y|yes|YES) ;;
            *) die "aborted; nothing was deleted" ;;
        esac
        rm -rf "$HOME_DIR"
        log "removed $HOME_DIR"
    else
        log "nothing to clean ($HOME_DIR does not exist)"
    fi
}

usage() {
    cat <<EOF
usage: ./run.sh <command>

  init             create arbiter keystores, render genesis, run 'geth init'
  start            start the producing node in the foreground (Ctrl-C stops it)
  start-follower   start with --pbft.enable=false (read-only, no key, no mining)
  status           print eth_blockNumber and the last blocks' extra length
  stop             SIGTERM a node started by 'start' (works from any terminal)
  verify-keys      re-open every keystore through dpos.GetDposAccount
  verify-secrets   assert geth's dumpconfig never emits the keystore password
  clean            delete \$PGP_DEVNET_HOME after confirmation
  build            compile build/bin/geth only

environment:
  PGP_DEVNET_PASSWORD        keystore password (required for init/start)
  PGP_DEVNET_PASSWORD_FILE   ... or a file holding it (0600 recommended)
  PGP_DEVNET_HOME            runtime root (default \$HOME/.pgp-devnet)
  PRODUCERS                  arbiter count for 'init' (default 1)
  CHAIN_ID                   chain id, must match genesis.json (default $CHAIN_ID)
  HTTP_ADDR / HTTP_PORT      RPC bind (default $HTTP_ADDR:$HTTP_PORT)
  P2P_PORT                   p2p port (default $P2P_PORT)
  VERBOSITY                  geth verbosity (default $VERBOSITY)
  FORCE_KEYS=1               regenerate the keystores in 'init'
  ARGV_PASSWORD=1            force the password onto the command line even though
                             GETH_PBFT_KEYSTORE_PASSWORD is supported (debugging only)
  STOP_TIMEOUT               seconds 'stop' waits before giving up (default $STOP_TIMEOUT)

see README.md in this directory for the runbook and the known blocker.
EOF
}

case "${1:-}" in
    init)           cmd_init ;;
    start)          cmd_start ;;
    start-follower) cmd_start_follower ;;
    stop)           cmd_stop ;;
    status)         cmd_status ;;
    verify-keys)    cmd_verify_keys ;;
    verify-secrets) cmd_verify_secrets ;;
    clean)          cmd_clean ;;
    build)          ensure_geth && log "geth: $GETH_BIN" ;;
    ""|-h|--help|help) usage ;;
    *)              usage; exit 1 ;;
esac
