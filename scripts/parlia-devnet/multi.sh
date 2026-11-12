#!/usr/bin/env bash
#
# Copyright 2026 The pgp-chain Authors
#

# Parlia multi-node devnet: N 个进程、N 席验证人、全互联。
#
#   ./multi.sh init [SEATS] [FORK_HEIGHT]   # datadir + arbiter 密钥 + genesis + geth init
#   ./multi.sh start                        # 后台起 N 个节点（--bootnodes 全互联 + DPoS 预置）
#   ./multi.sh start-node I / stop-node I   # 单节点启停（掉线测试用）
#   ./multi.sh status                       # 高度 / 验证人集合 / 出块者分布
#   ./multi.sh wait N                       # 等到所有节点都到高度 N
#   ./multi.sh stop
#   ./multi.sh clean
#
# 运行期间不要编辑这个文件：`start` 会一直阻塞在 `wait`，而这个 bash 进程是
# 边读边执行的，文件被改写后它会按字节偏移读到半条语句并以 exit 2 退出，
# 子节点一起被回收。staking-devnet 的 run.sh start 已经把脚本快照到
# $STAKING_HOME/.multi.snapshot.sh 再执行，所以那条路径不受影响；直接调用
# multi.sh start 时才需要注意。
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
# 1 = 给每个节点建一个 BLS 钱包与 BLS 账户（fast finality 用），公钥写进 genesis 的
# legacy.parliaGenesisVoteAddrs，节点启动时用 --vote --blspassword --blswallet 解锁。
#
# 为什么默认 0：开启后每个验证人的 RLP 元组从 4 项变 5 项，D 块写入的 state root
# 随之改变（这是预期的，不是 bug）。默认关闭保证「不关心 finality」的跑法与之前
# 逐字节一致，也让主网形态（无票地址）成为默认路径而不是特例。
BLS_VOTE=${BLS_VOTE:-0}

# ---- 质押/选举测试用的开关（默认值保持既有 devnet 行为） -------------------
# 出块席位。0 = 取 SEATS；>0 时必须 <= SEATS（多出的候选人只参与选举）。
CABINETS=${CABINETS:-0}
# 每个验证人的 genesis 余额（hex wei）。默认 100 ETH；质押测试至少要 2100 ETH。
BALANCE_WEI=${BALANCE_WEI:-0x56bc75e2d63100000}
# genesis 时间戳（hex 字符串，或十进制数）。留空则用 gen-genesis 的默认值。
GENESIS_TIMESTAMP=${GENESIS_TIMESTAMP:-}
# 1 = genesis 里写 keplerTime=0 + feynmanTime=0（选举路径从创世就活）。
# 代价：分叉后第一个 breathe 块就会按 StakeHub 重算集合。若那时还没人注册，
# BSCValidatorSet 会保留原集合继续出块（长度保护，见 contracts/BSCValidatorSet.sol），
# 但质押与选举等于没上线——用 staking-devnet 的 `run.sh check` 断言 election.total > 0。
FEYNMAN=${FEYNMAN:-1}
# 1 = 把 9 个历史 BSC 块分叉也钉在 FORK_HEIGHT（复刻主网钉高后的形态）；
# 0 = 压在 0（合约版本取最新，历史上一直这么跑）。二者在 legacy 段只有
# IsNano 黑名单检查这条不受 IsPGPLegacy 兜底，详见 gen-genesis.py 的注释。
ALL_FORKS_AT_FORK_HEIGHT=${ALL_FORKS_AT_FORK_HEIGHT:-0}
# 非空 = 把 Cancun/Haber/HaberFix/Bohr/Pascal/Prague/Lorentz/Maxwell/Fermi 全组钉在这个
# 时间戳（Unix 秒）。这一组不是可选的，开 Fermi 就得全给。留空 = 都不启用。
BSC_TIME_FORKS=${BSC_TIME_FORKS:-}
# 覆盖 breathe block 间隔（秒）。留空 = 不传 flag（内核默认 86400）。
BREATHE_INTERVAL=${BREATHE_INTERVAL:-}
# legacy 生产者数量。0 = 取 SEATS；1 = 回退单仲裁人（组网不收敛时用）。
LEGACY_PRODUCERS=${LEGACY_PRODUCERS:-0}
# DPoS 直连端口基址（--pbft.net.port）。
DPOS_BASE=${DPOS_BASE:-20679}
# JSON-RPC 暴露的 API 列表。
HTTP_API=${HTTP_API:-eth,net,web3,admin}
# --bootnodes 需要的 enode id 由 bootnode 从 nodekey 推出，不手搓 secp256k1。
BOOTNODE_BIN=${BOOTNODE_BIN:-$REPO_ROOT/build/bin/bootnode}
# DPoS 地址簿预置脚本（DPoS 传输层没有 bootnode 参数，只能 gossip 或预置）。
SEED_PEERS_PY=${SEED_PEERS_PY:-$REPO_ROOT/scripts/pbft-devnet/seed-peers.py}

mkdir -p "$LOG_DIR"

die() { echo "错误: $*" >&2; exit 1; }
need_bin() { [ -x "$GETH_BIN" ] || die "找不到 ${GETH_BIN}；先跑 go build -o build/bin/geth ./cmd/geth"; }
need_bootnode() { [ -x "$BOOTNODE_BIN" ] || die "找不到 ${BOOTNODE_BIN}；先跑 go build -o build/bin/bootnode ./cmd/bootnode"; }
node_dir() { echo "$HOME_DIR/node$1"; }
node_http() { echo $((HTTP_BASE + $1)); }
node_p2p() { echo $((P2P_BASE + $1)); }
node_dpos() { echo $((DPOS_BASE + $1)); }

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

  # 角色划分（默认沿用旧行为：所有节点既是 legacy 生产者又是 POSA 验证人）：
  #   node 0 .. producers_n-1           legacy PBFT 生产者（P-256 arbiter）
  #   node posa_first .. posa_first+M-1 POSA 验证人（EVM 地址进 genesis 验证人集合）
  # 设了 POSA_VALIDATORS 时两批人完全不相交，用来验证「分叉时换一批出块人」。
  local posa_n=${POSA_VALIDATORS:-0}
  local producers_n=${LEGACY_PRODUCERS:-0}
  local posa_first=0
  if [ "$posa_n" -gt 0 ]; then
    [ "$producers_n" -gt 0 ] || die "设了 POSA_VALIDATORS 就必须同时给 LEGACY_PRODUCERS"
    [ $((producers_n + posa_n)) -eq "$seats" ] || \
      die "LEGACY_PRODUCERS($producers_n) + POSA_VALIDATORS($posa_n) 必须等于 SEATS($seats)"
    posa_first=$producers_n
  else
    posa_n=$seats
    [ "$producers_n" = 0 ] && producers_n=$seats
  fi
  [ "$producers_n" -ge 1 ] || die "legacy 生产者至少 1 席"
  [ "$producers_n" -le "$seats" ] || die "LEGACY_PRODUCERS=$producers_n 不能大于节点数 $seats"
  [ "$posa_n" -ge 4 ] || die "POSA 验证人至少 4 席（recents 窗口 N/2+1），收到 $posa_n"

  # init 会 rm -rf 掉 node*，所以在节点还活着时跑 init 等于把 datadir 从运行中的
  # 进程脚下抽走：进程继续持有已删除的 inode 出块，端口也还占着，随后 start 又因为
  # 端口被占用而只起来一半节点，读到的却是旧链（实测踩过一次：init 之后查 head
  # 拿到 80，而新创世的 T0 还在两分钟后 —— 一条根本不存在的链）。
  # 这里用和 start 一样的锁探测 + pid 探测挡住它。
  local running=""
  running=$(lock_holders)
  if [ -z "$running" ]; then
    local pf
    for pf in "$HOME_DIR"/node*/geth.pid; do
      [ -f "$pf" ] || continue
      if kill -0 "$(cat "$pf")" 2>/dev/null; then running="$running ${pf%/geth.pid}"; fi
    done
  fi
  if [ -n "$running" ]; then
    die "还有节点在跑（${running}），init 会删掉它们的 datadir。先 ./multi.sh stop 并等它输出「所有节点已退出」"
  fi

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
    if [ "$i" -ge "$posa_first" ] && [ "$i" -lt $((posa_first + posa_n)) ]; then
      echo "$addr,$addr,$addr,100,-" >> "$HOME_DIR/validators.conf"
    fi

    if [ "$BLS_VOTE" = "1" ] && [ "$i" -ge "$posa_first" ] && [ "$i" -lt $((posa_first + posa_n)) ]; then
      # 这里要写**两个**密码文件，因为同一个 --blspassword 在两个场景下的格式不同：
      #
      #   CLI（geth bls wallet create / account new）：utils.GetPassPhraseWithList
      #   把文件按行切成列表逐个消费，行数不够就退化成交互提示——在非 TTY 里
      #   不是报错而是挂住。所以 create-password.txt 要写多行相同口令。
      #
      #   运行时（core/vote/vote_signer.go:43-52）：os.ReadFile 后直接
      #   string(content) 当密码，**换行会算进密码**。所以 password.txt 必须
      #   是单行且没有结尾换行，用 printf '%s' 而不是 echo。
      #
      # 用同一个文件同时满足两者是不可能的：多行文件在运行时报
      # "wrong password for wallet entered: invalid checksum"，
      # 单行文件在 CLI 阶段会挂住。踩过一次，所以写成两条注释。
      #
      # 口令必须 >= 10 字符且是 7-bit 可打印 ASCII（core.ValidatePasswordFormat）。
      mkdir -p "$dir/bls"
      printf 'bls-devnet-pw\nbls-devnet-pw\nbls-devnet-pw\nbls-devnet-pw\n' > "$dir/bls/create-password.txt"
      printf '%s' 'bls-devnet-pw' > "$dir/bls/password.txt"
      chmod 600 "$dir/bls/create-password.txt" "$dir/bls/password.txt"
      {
        "$GETH_BIN" bls wallet create --datadir "$dir" --blspassword "$dir/bls/create-password.txt"
        "$GETH_BIN" bls account new   --datadir "$dir" --blspassword "$dir/bls/create-password.txt"
      } > "$dir/bls/create.log" 2>&1 \
        || { tail -20 "$dir/bls/create.log"; die "节点 $i 的 BLS 钱包/账户创建失败"; }
      # 直接从 EIP-2335 keystore 里读 pubkey，而不是解析 `bls account list` 的输出：
      # 后者是给人看的，格式一变脚本就坏；前者是钱包自己写下的、必须存在的字段。
      python3 - "$dir/bls/keystore" > "$dir/bls/vote.addr" <<'PY' \
        || die "节点 $i 的 BLS 公钥读不出来"
import glob, json, os, sys
files = sorted(glob.glob(os.path.join(sys.argv[1], "*.json")))
if len(files) != 1:
    sys.exit("expected exactly one BLS keystore in %s, got %d" % (sys.argv[1], len(files)))
pub = json.load(open(files[0]))["pubkey"]
print(pub if pub.startswith("0x") else "0x" + pub)
PY
    fi
  done

  local balances=()
  for addr in "${addrs[@]}"; do balances+=(--balance "$addr=$BALANCE_WEI"); done

  # legacy 段需要自己的生产者：block 1..ForkHeight-1 由 PBFT 出，而 PBFT 的生产者
  # 是 Elastos P-256 公钥，不是 EVM 的 secp256k1。所以 fork 模式下额外生成一个
  # arbiter keystore，公钥写进 genesis 的 legacy.producers，私钥交给 node 0。
  #
  # 只要 1 席：F-14 之后 legacyMajorityCount(1) = 1，封块人自己的那一票就够；在那之前
  # 它是 0，单生产者集合下一个零投票的 confirm 也算合法，等于根本不验票（见
  # consensus/pgp/confirm.go 的注释）。多席需要多把私钥同时在线投票，单机做不到。
  #
  # 这一步也把「自派生密钥」这件事说清楚了：这些 key 与 pgp 主网那 12 个生产者无关，
  # 只用于本地验证分叉逻辑。
  # 变量已在函数开头算好（producers_n / posa_n / posa_first）。
  local -a producer_args=()
  if [ "$fork_height" -gt 1 ]; then
    rm -rf "$ARBITER_DIR"
    # go run 的包路径是相对仓库根解析的，所以必须切到 REPO_ROOT 再跑；
    # 脚本本身可以在任意目录被调用。
    ( cd "$REPO_ROOT" && PGP_DEVNET_PASSWORD="$ARBITER_PASSWORD" \
        go run ./scripts/pbft-devnet/keygen -out "$ARBITER_DIR" -n "$producers_n" ) \
      > "$LOG_DIR/keygen.log" 2>&1 \
      || { tail -20 "$LOG_DIR/keygen.log"; die "生成 legacy arbiter 密钥失败"; }
    # 0600：这个文件里就是 keystore 的解密口令。
    printf '%s' "$ARBITER_PASSWORD" > "$ARBITER_PASSWORD_FILE"
    chmod 600 "$ARBITER_PASSWORD_FILE"
    local prod
    while IFS= read -r prod; do
      [ -n "$prod" ] || continue
      producer_args+=(--producers "$prod")
    done < <(python3 -c '
import json,sys
m=json.load(open(sys.argv[1]))
print("\n".join(m["producers"]))
' "$ARBITER_MANIFEST") || die "读不出 $ARBITER_MANIFEST 里的公钥"
    echo "legacy 生产者（自派生 P-256，与主网无关）: $producers_n 席"
  fi

  local -a deploy_args=()
  [ "$DEPLOY_HEIGHT" != "0" ] && deploy_args+=(--deploy-height "$DEPLOY_HEIGHT")
  [ "$OMIT_SYSTEM_ALLOC" != "0" ] && deploy_args+=(--omit-system-contract-alloc)
  [ "$LUBAN_SENTINEL" != "0" ] && deploy_args+=(--luban-sentinel)

  # BLS 票地址：fast finality 的聚合签名要用它验签，而验证人集合里的票地址在真实
  # 链上来自 StakeHub 注册。devnet 没有选举，所以这里从 genesis 的 5-tuple 注入。
  # 空列表 = 不启用 finality（epoch 块的 48 字节票地址全零，见审计 04 §1.2）。
  local -a vote_args=()
  if [ "$BLS_VOTE" = "1" ]; then
    local vote k
    for ((k = 0; k < posa_n; k++)); do
      i=$((posa_first + k))
      dir=$(node_dir "$i")
      vote=$(cat "$dir/bls/vote.addr" 2>/dev/null) || die "节点 $i 没有 BLS 票地址；BLS_VOTE=1 时 init 应当已经建好"
      [ -n "$vote" ] || die "节点 $i 的 BLS 票地址是空的"
      vote_args+=(--vote-addrs "$vote")
      echo "BLS 票地址 [node$i] $vote"
    done
  fi

  local cabinets=${CABINETS:-0}
  [ "$cabinets" = 0 ] && cabinets=$seats
  local -a time_args=()
  [ -n "$GENESIS_TIMESTAMP" ] && time_args+=(--timestamp "$GENESIS_TIMESTAMP")
  if [ "$FEYNMAN" = "1" ]; then
    # 只允许 0：非 0 会在激活块再次 initialize() 已经初始化过的 5 个合约。
    time_args+=(--feynman-time 0 --kepler-time 0)
  fi
  # Cancun..Fermi 整组（含 Lorentz/Maxwell/Fermi 的块间隔切换）。必须晚于最后一个
  # legacy 块的时间，否则 Cancun 会往历史 header 里写 blob 字段。
  [ -n "${BSC_TIME_FORKS:-}" ] && time_args+=(--bsc-time-forks "$BSC_TIME_FORKS")

  local -a allfork_args=()
  [ "$ALL_FORKS_AT_FORK_HEIGHT" = "1" ] && allfork_args=(--all-forks-at-height)

  python3 "$SELF_DIR/gen-genesis.py" \
    --contracts-out "$CONTRACT_ROOT/out" \
    --validators "$HOME_DIR/validators.conf" \
    --fork-height "$fork_height" \
    --cabinets "$cabinets" \
    --gas-limit "$GAS_LIMIT" \
    "${deploy_args[@]+"${deploy_args[@]}"}" \
    "${time_args[@]+"${time_args[@]}"}" \
    "${producer_args[@]+"${producer_args[@]}"}" \
    "${vote_args[@]+"${vote_args[@]}"}" \
    "${balances[@]+"${balances[@]}"}" \
    "${allfork_args[@]+"${allfork_args[@]}"}" \
    --out "$GENESIS"

  for ((i = 0; i < seats; i++)); do
    dir=$(node_dir "$i")
    "$GETH_BIN" --datadir "$dir" init "$GENESIS" > "$LOG_DIR/init$i.log" 2>&1 \
      || { tail -10 "$LOG_DIR/init$i.log"; die "节点 $i geth init 失败"; }
  done

  # devp2p nodekey 预生成：--bootnodes 需要每个节点的 enode id，而 id 只能从
  # nodekey 推出来。放在 start 之前，peering 从进程启动就开始，不依赖 admin API
  # 的 addPeer（那条路仍保留为启动后的幂等兜底）。
  need_bootnode
  local -a enodes=()
  for ((i = 0; i < seats; i++)); do
    dir=$(node_dir "$i")
    [ -f "$dir/geth/nodekey" ] || "$BOOTNODE_BIN" -genkey "$dir/geth/nodekey" >/dev/null 2>&1 \
      || die "节点 $i 的 devp2p nodekey 生成失败"
    local id
    id=$("$BOOTNODE_BIN" -nodekey "$dir/geth/nodekey" -writeaddress) \
      || die "节点 $i 的 enode id 推导失败"
    enodes+=("enode://$id@127.0.0.1:$((P2P_BASE + i))")
  done
  local BOOTNODES
  BOOTNODES=$(IFS=,; echo "${enodes[*]}")

  # DPoS 地址簿预置：DPoS 传输层没有 bootnode 参数，地址只有两条来路——devp2p 上的
  # ELAMSG gossip，或这个文件。预置之后 producer 在启动瞬间就连上，块时间才能严格
  # 等于 T0+3n（否则 gossip 收敛的 ~40s 会把相位顶前）。
  if [ "$producers_n" -ge 2 ] && [ -f "$ARBITER_MANIFEST" ] && [ -f "$SEED_PEERS_PY" ]; then
    local -a seed_args=()
    for ((i = 0; i < producers_n; i++)); do
      seed_args+=(--node "127.0.0.1:$((DPOS_BASE + i)):$(node_dir "$i")")
    done
    python3 "$SEED_PEERS_PY" --manifest "$ARBITER_MANIFEST" --force \
      "${seed_args[@]}" > "$LOG_DIR/seed-peers.log" 2>&1 \
      || { tail -20 "$LOG_DIR/seed-peers.log"; die "DPoS 地址簿预置失败"; }
  fi

  # start / start-node 只读这份文件：breathe 间隔、端口基址、bootnodes 一旦在
  # 不同节点上不一致，就会直接变成状态根分叉。
  cat > "$HOME_DIR/devnet.env" <<EOF
# Generated by multi.sh init. start/start-node 只读它。
SEATS=$seats
FORK=$fork_height
CABINETS=$cabinets
GENESIS_TIMESTAMP='${GENESIS_TIMESTAMP:-}'
BREATHE_INTERVAL='${BREATHE_INTERVAL:-}'
DPOS_BASE=$DPOS_BASE
LEGACY_PRODUCERS=$producers_n
POSA_FIRST=$posa_first
POSA_COUNT=$posa_n
NETWORK_ID=$NETWORK_ID
HTTP_API='$HTTP_API'
BOOTNODES='$BOOTNODES'
EOF

  echo
  echo "节点 / 验证人（共 $seats 席，各自持钥）："
  for ((i = 0; i < seats; i++)); do
    printf '  [%d] %s  p2p=%s  http=%s\n' "$i" "${addrs[i]}" "$(node_p2p "$i")" "$(node_http "$i")"
  done
  echo "ForkHeight : $fork_height"
  echo "下一步     : ./multi.sh start"
}

# lock_holders 打印仍占用 datadir 锁的节点名（node0 node1 …），空表示都没占用。
#
# 为什么要它：`stop` 结束后 pid 文件已被删，如果某次 stop 失败、或进程是被外部
# 杀掉的，光看 pid 文件会得出「0 个节点」的假结论，而实际上 geth 还在 flush。
# 这时 start 会撞上 geth 的 datadir LOCK，报
#   Fatal: Failed to create the protocol stack: datadir already used by another process
# 然后被脚本翻译成「HTTP 40 s 内没有就绪」，看不出真正原因。
# 不用 ps/pgrep：本机沙箱里 ps 会被直接拒绝。用 flock 试锁是唯一稳的判据。
lock_holders() {
  python3 - "$HOME_DIR" <<'PY'
import fcntl, glob, os, sys
held = []
for p in sorted(glob.glob(os.path.join(sys.argv[1], "node*", "geth", "LOCK"))):
    try:
        f = open(p, "a")
    except OSError:
        continue
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(f, fcntl.LOCK_UN)
    except OSError:
        held.append(os.path.basename(os.path.dirname(os.path.dirname(p))))
    f.close()
print(" ".join(held))
PY
}

# launch_node 启动一个节点。所有可变参数都来自 devnet.env（init 落盘），这样
# start / start-node 不会出现「某个节点漏传 breathe 间隔」这种分叉级差异。
# 这一整段（以及 init 里给 gen-genesis.py 拼参数的地方）都在 set -u 下展开可选
# 参数数组，因此统一用 "${arr[@]+"${arr[@]}"}" 而不是 "${arr[@]}"。
#
# 原因是 macOS 自带的 bash 是 3.2，而 3.2 在 set -u 下把**空数组**的 "${a[@]}"
# 当成未定义变量："bls_args[@]: unbound variable"。Bash 4.4 才修掉这个行为。
# 后果极其隐蔽：整条 geth 命令在 fork 之前就失败，节点根本没起来，可脚本紧接着
# 已经执行了 `echo $! > geth.pid` 并打印「节点 N 已启动」—— pid 文件里是上一条
# 后台任务的 pid，进程表里根本没有这个节点。实测跑 Run E 时 8 个节点里 4 个
# PBFT 生产者就是这么静默死掉的（它们没有 BLS 钱包，bls_args 为空），链路一段
# 都不出块。cmd_start 里的存活校验就是为此加的。
launch_node() {
  local i=$1 dir addr
  dir=$(node_dir "$i"); addr=$(cat "$dir/validator.addr")
  local -a pbft_args=(--pbft.enable=false)
  if [ "$i" -lt "${PRODUCERS_N:-0}" ]; then
    # 每个生产者都有自己的 arbiter keystore（arbiter-$i.dat）。--pbft.miner.address
    # 必须与 --miner.etherbase 同址，否则 cmd/utils/flags.go 的 setEtherbase 直接
    # Fatalf（两者写同一个 header.Coinbase，而它进 PBFT seal hash）。这里统一用
    # 节点自己的 EVM 地址：Parlia 段要求 coinbase == 出块验证人，legacy 段只把它
    # 当作 seal hash 的一部分，一个值同时满足两段。
    # --pbft.keystore.password 收的是**文件路径**，不是密码本身。
    pbft_args=(--pbft.enable
               --pbft.keystore "$ARBITER_DIR/arbiter-$i.dat"
               --pbft.keystore.password "$ARBITER_PASSWORD_FILE"
               --pbft.miner.address "$addr"
               --pbft.net.address 127.0.0.1
               --pbft.net.port "$(node_dpos "$i")"
               --pbft.net.maxnodeperhost "${SEATS_N:-$i}")
  fi
  # finality 投票三件套：--vote 打开 Miner.VoteEnable，--blspassword/--blswallet
  # 告诉它去哪拿 BLS 私钥。缺 --vote 时节点照常出块但从不投票。
  local -a bls_args=()
  if [ -f "$dir/bls/vote.addr" ]; then
    bls_args=(--vote
              --blspassword "$dir/bls/password.txt"
              --blswallet "$dir/bls/wallet")
  fi
  # bootnodes 只负责播种 discovery，所以给了它就不能再给 --nodiscover。
  local -a net_args=()
  if [ -n "${BOOTNODES:-}" ]; then
    net_args=(--bootnodes "$BOOTNODES")
  else
    net_args=(--nodiscover)
  fi
  local -a breathe_args=()
  [ -n "${BREATHE_INTERVAL:-}" ] && breathe_args=(--override.breatheblockinterval "$BREATHE_INTERVAL")
  "$GETH_BIN" \
    --datadir "$dir" --networkid "$NETWORK_ID" \
    --port "$(node_p2p "$i")" \
    --nat extip:127.0.0.1 \
    "${net_args[@]}" --maxpeers "${SEATS_N:-$i}" \
    --mine --miner.etherbase "$addr" \
    --unlock "$addr" --password "$dir/password.txt" --allow-insecure-unlock \
    --miner.gaslimit "$MINER_GAS_LIMIT" \
    --http --http.addr 127.0.0.1 --http.port "$(node_http "$i")" \
    --http.api "${HTTP_API:-eth,net,web3,admin}" \
    --syncmode full --verbosity "$VERBOSITY" \
    "${breathe_args[@]+"${breathe_args[@]}"}" \
    "${pbft_args[@]}" \
    "${bls_args[@]+"${bls_args[@]}"}" \
    > "$LOG_DIR/node$i.log" 2>&1 &
  echo $! > "$dir/geth.pid"
  if [ "$i" -lt "${PRODUCERS_N:-0}" ]; then
    echo "节点 $i 已启动 pid=$! （legacy 段 PBFT 生产者）p2p=$(node_p2p "$i") dpos=$(node_dpos "$i") http=$(node_http "$i")"
  else
    echo "节点 $i 已启动 pid=$! p2p=$(node_p2p "$i") http=$(node_http "$i")"
  fi
}

cmd_start() {
  need_bin
  [ -f "$GENESIS" ] || die "还没有 genesis；先跑 ./multi.sh init [SEATS] [FORK_HEIGHT]"
  [ -f "$HOME_DIR/devnet.env" ] || die "缺少 $HOME_DIR/devnet.env；先跑 ./multi.sh init"
  local held
  held=$(lock_holders)
  [ -z "$held" ] || die "这些节点的 datadir 还被占用：${held}。先 ./multi.sh stop 并等它输出「所有节点已退出」；不要用 kill -9，强杀会丢 state 导致重启后重封（审计 F-20）"

  # devnet.env 是 init 落盘的唯一真相：breathe 间隔、bootnodes、DPoS 端口基址
  # 在节点之间不一致会直接变成状态根分叉。
  # shellcheck disable=SC1090
  . "$HOME_DIR/devnet.env"
  local seats=${SEATS:-0}
  [ "$seats" -ge 4 ] || die "devnet.env 里的 SEATS=$seats 不合法"
  SEATS_N=$seats
  PRODUCERS_N=${LEGACY_PRODUCERS:-$seats}

  local i
  for ((i = 0; i < seats; i++)); do
    launch_node "$i"
  done

  # 立刻校验每个 pid 真的对应一个活着的 geth，而不是相信「已启动」这句话。
  # 空数组展开失败会让 geth 在 fork 前就退出，pid 文件里写着上一条后台任务的
  # pid；不查的话要等 40 s 才在 HTTP 就绪循环里报一句「看 nodeN.log」，把
  # 「拼参数写错」误报成「启动慢」。
  sleep 1
  local dead=()
  for ((i = 0; i < seats; i++)); do
    if ! kill -0 "$(cat "$(node_dir "$i")/geth.pid" 2>/dev/null)" 2>/dev/null; then
      dead+=("$i")
    fi
  done
  if [ "${#dead[@]}" -gt 0 ]; then
    for i in "${dead[@]}"; do
      echo "---- node$i 启动即退出，日志尾部 ----" >&2
      tail -n 20 "$LOG_DIR/node$i.log" >&2 || true
    done
    die "节点 ${dead[*]} 启动失败（见上面的日志）"
  fi
  echo "全部 $seats 个节点进程存活"

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
  local stopped=0 dir pid alive waited pids=()
  for dir in "$HOME_DIR"/node*; do
    [ -f "$dir/geth.pid" ] || continue
    pid=$(cat "$dir/geth.pid")
    pids+=("$pid")
    if kill "$pid" 2>/dev/null; then stopped=$((stopped + 1)); fi
  done
  rm -f "$HOME_DIR"/node*/geth.pid
  echo "已向 $stopped 个节点发送 SIGTERM"

  # 等进程真正退出，而不是睡固定 2 秒就返回。
  # 原因很具体：geth 收到 SIGTERM 后要先 flush state 再释放 datadir 的 LOCK。
  # 实测 stop 后 3 秒就 start，4 个节点里有 2 个直接
  #   Fatal: Failed to create the protocol stack: datadir already used by another process
  # 起不来，而脚本只会报「HTTP 40 s 内没有就绪」，看不出真正原因。
  # 这里用 kill -0 轮询（不依赖 ps/pgrep：本机沙箱里 ps 会被拒）。
  # 超时后**不**补 SIGKILL：强杀会丢未 flush 的 state，重启后节点回滚并重封，
  # 被 peer 判为双签（见审计 F-20），比多等几十秒严重得多。
  waited=0
  while :; do
    alive=0
    for pid in "${pids[@]:-}"; do
      if kill -0 "$pid" 2>/dev/null; then alive=$((alive + 1)); fi
    done
    # 进程退出和锁释放不是同一时刻：/proc 里 pid 没了，flock 可能还挂在
    # 正在关闭的 fd 上。两个条件都满足才算停干净。
    if [ "$alive" -eq 0 ] && [ -z "$(lock_holders)" ]; then
      echo "所有节点已退出，datadir 锁已释放（等待 ${waited}s）"
      return
    fi
    if [ "$waited" -ge 30 ]; then
      echo "警告：${waited}s 后仍未停干净（存活进程 $alive 个，锁占用: $(lock_holders)），未强杀"
      return
    fi
    sleep 1
    waited=$((waited + 1))
  done
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
        body = L - EC - SS          # 去掉 vanity 与 seal
        # 两种布局：Luban 之后是 1 字节计数 + 每席 68 B（20 地址 + 48 BLS 公钥）；
        # Luban 未启用时没有计数前缀，每席只有 20 B 地址。先按 20 B 整除判断，
        # 否则再试 Luban 布局——用同一个公式套两种布局会报出"解出 1 席"这种假异常。
        if body % 20 == 0:
            seats = body // 20
            print(f"  epoch/变长块 #{n}: extra={L} B  => {seats} 席"
                  f"（pre-Luban 布局：{EC}+{seats}x20+{SS} = {L}）")
        else:
            seats = (body - 1) // 68
            print(f"  epoch/变长块 #{n}: extra={L} B  => {seats} 席"
                  f"（Luban 布局：{EC}+1+{seats}x68+{SS} = {EC + 1 + seats * 68 + SS}）")
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

cmd_start_node() {
  local i=${1:?用法: ./multi.sh start-node <index>}
  need_bin
  [ -f "$HOME_DIR/devnet.env" ] || die "缺少 $HOME_DIR/devnet.env；先跑 ./multi.sh init"
  local dir
  dir=$(node_dir "$i")
  [ -f "$dir/validator.addr" ] || die "节点 $i 不存在（$dir）"
  case " $(lock_holders) " in *" node$i "*) die "节点 $i 的 datadir 还被占用" ;; esac
  # shellcheck disable=SC1090
  . "$HOME_DIR/devnet.env"
  SEATS_N=${SEATS:-0}
  PRODUCERS_N=${LEGACY_PRODUCERS:-$SEATS_N}

  launch_node "$i"

  local tries=0
  until rpc "$(node_http "$i")" web3_clientVersion '[]' 2>/dev/null | grep -q '"result"'; do
    tries=$((tries + 1))
    [ "$tries" -gt 300 ] && die "节点 $i 的 HTTP 60 s 内没有就绪，看 $LOG_DIR/node$i.log"
    sleep 0.2
  done
  echo "节点 $i HTTP 就绪"

  # 重新接回全网：bootnodes 只在启动时播种 discovery，重启后要等 discovery 重新
  # 收敛；这里用 admin.addPeer 直接补齐，避免停机测试后网络要几十秒才恢复。
  local j enode_i enode_j
  enode_i=$(rpc "$(node_http "$i")" admin_nodeInfo '[]' \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["result"]["enode"] if "result" in d else "")')
  for ((j = 0; j < SEATS_N; j++)); do
    [ "$j" = "$i" ] && continue
    enode_j=$(rpc "$(node_http "$j")" admin_nodeInfo '[]' \
      | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["result"]["enode"] if "result" in d else "")')
    [ -n "$enode_j" ] || continue
    rpc "$(node_http "$i")" admin_addPeer "[\"$enode_j\"]" > /dev/null
    [ -n "$enode_i" ] && rpc "$(node_http "$j")" admin_addPeer "[\"$enode_i\"]" > /dev/null
  done
  echo "节点 $i 已重新接入全网"
}

cmd_stop_node() {
  local i=${1:?用法: ./multi.sh stop-node <index>}
  local dir pid waited=0
  dir=$(node_dir "$i")
  [ -f "$dir/geth.pid" ] || die "节点 $i 没有 pid 文件（可能已经停了）"
  pid=$(cat "$dir/geth.pid")
  kill "$pid" 2>/dev/null || true
  rm -f "$dir/geth.pid"
  while kill -0 "$pid" 2>/dev/null; do
    [ "$waited" -ge 60 ] && { echo "警告：节点 $i 60 s 内未退出，未强杀"; return; }
    sleep 1; waited=$((waited + 1))
  done
  # 进程退出和锁释放不是同一时刻，两个条件都满足才算停干净。
  while :; do
    case " $(lock_holders) " in
      *" node$i "*) ;;
      *) break ;;
    esac
    [ "$waited" -ge 90 ] && { echo "警告：node$i 的 datadir 锁 90 s 未释放，未强杀"; return; }
    sleep 1; waited=$((waited + 1))
  done
  echo "节点 $i 已停止（等待 ${waited}s）"
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
  start-node) shift; cmd_start_node "$@" ;;
  stop-node)  shift; cmd_stop_node "$@" ;;
  status) shift; cmd_status "$@" ;;
  wait)   shift; cmd_wait "$@" ;;
  clean)  shift; cmd_clean "$@" ;;
  *)
    sed -n '6,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 1
    ;;
esac
