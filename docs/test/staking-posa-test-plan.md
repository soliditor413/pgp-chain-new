# PGP 测试链验证方案：PBFT→POSA、质押选举、双签/掉线罚没

> 目标：在单机多节点测试链上验证 (1) PBFT 出块并平滑切到 POSA；(2) 切换后的质押、选举与验证人集合切换；(3) 双签、不出块等作恶的处罚机制。
> 结论：**可行**，但必须先接受 6 条与原始设想不同的实现事实（§0），并做 3 处**仅 devnet** 的小改造（§2.4）。生产默认配置与已钉死的系统合约镜像（`pgp-fork-v4`）不改。

---

## 0. 决定方案形态的 6 条事实（先读这一节）

| # | 事实 | 代码证据 | 对测试的影响 |
| --- | --- | --- | --- |
| F1 | **仓库里没有 `contracts/election/`**。POSA 的"选举"是 BSC 原生 `StakeHub(0x2002)` + `BSCValidatorSet(0x1000)`，由 Go 侧 `consensus/parlia/feynmanfork.go` 在 **breathe block** 驱动 | 全仓 `election` 目录 0 命中；`feynmanfork.go:99-128` | 目标 2 测的是真实运行路径，不测自定义合约。原 `staking-election-fork-design.md` 在本仓库不存在 |
| F2 | **选举路径默认休眠**：`PGPChainConfig` 与 devnet genesis 都没有 `feynmanTime`，`IsFeynman` 恒 false | `params/pgp_config.go`；`gen-genesis.py:113-171` | 验证目标 2 必须给 **devnet genesis** 打开 Feynman（不改生产默认） |
| F3 | **`feynmanTime` 只能是 0**（创世即激活）。设未来值会在激活块再次调用 `initialize()`，而 5 个合约已在 D 块初始化过，`initializer` 修饰器 revert → Finalize 失败 → 停链 | `parlia.go:1548-1554`；`StakeHub.sol:268`；`gen-genesis.py:106-111` 的注释已说明时间型分叉不能预钉 | `--feynman-time 0 --kepler-time 0`，且生成器直接拒绝非 0 值 |
| F4 | **首个 post-fork breathe 块若 StakeHub 为空，链会永久停**：`updateValidatorSetV2([])` → `_forceMaintainingValidatorsExit` 里 `numOfFelony(0) >= length(0)` 成立 → 读 `_validatorSet[0]` 越界 revert | `BSCValidatorSet.sol` 该函数 | **必须在第一个 breathe 块之前完成全部 validator 注册**，这是本方案的第一硬门 |
| F5 | **选举结果写链上即时生效，但共识层只在 epoch 块读取**：`number % epoch == minerHistoryCheckLen` 才切集合，`checkLen=(N/2+1)*TurnLength-1` | `snapshot.go:243,374` | 6 席时首次切换在 **203**（读 header 200），5 席时在 **402**（读 header 400）。观察窗口必须按此设计，不能"改完质押立刻看出块人变了" |
| F6 | **legacy 生产者可以是 N 个**：`keygen -n N` 每个仲裁人一把 P-256 keystore，每节点独立 DPoS 端口；但 legacy 段从此依赖 DPoS 组网，6 席时每块需要 4 票 | `keygen/main.go`；`seed-peers.py` 头部记录了六生产者台架实测；`confirm.go:279` | 默认按 SEATS 生成（A/B/C 6 个、D 4 个）。**devp2p 用 `--bootnodes` 全互联；DPoS 没有 bootnode 参数**，地址只能靠 devp2p 上的 ELAMSG gossip 或预置 `peers.json` |


附带两条会直接影响判定的参数事实：

- **罚没阈值**：`misdemeanor=50`、`felony=150`；`maxNumOfMaintaining` 从未初始化（=0），所以 `canEnterMaintenance` 恒 false → **轻罪只清空待分配收益，不进维护态，计数可一路累到 150**。
- **`clean()` 每个 breathe block 把所有 `count>37` 的记录减 37** → 选举场景的短 breathe 间隔会吃掉掉线计数，**掉线场景必须用大间隔**（与选举场景隔离运行）。

---

## 1. 被测系统形态（一张表）

| 组件 | 位置 | 行为 |
| --- | --- | --- |
| 分叉路由 | `consensus/router/router.go` | `block < legacy.forkHeight` 走 PBFT，`>=` 走 Parlia；高度只由 genesis 的 `config.legacy.forkHeight` 决定，**没有 CLI flag** |
| 系统合约注入 | `core/pgp_systemcontract_deploy.go` | 仅在 `config.legacy.systemContractDeployHeight`（D）块首：17 个地址 SetCode + 12 个 `init`，`0x1000` 走 `init(bytes,uint256)` |
| 验证人来源 | `0x1000 BSCValidatorSet` | `getMiningValidators()`：验证人数 ≤ `numOfCabinets` 时全返回；否则返回 `currentValidatorSet` 前 N 个（`maxNumOfWorkingCandidates=0`，洗牌分支不生效） |
| 选举 | `0x2002 StakeHub` | `getValidatorElectionInfo` 按 `totalPooledBNB` 降序（ jailed 记 0），取 top `maxElectedValidators`(45) → 系统交易写 `0x1000.updateValidatorSetV2` |
| 触发点 | `feynmanfork.go:28` | `isBreatheBlock(parent.Time, header.Time)`：`t/interval` 跨桶即触发，interval 默认 86400s |
| 出块轮转 | `snapshot.go` | `TurnLength=1`，epoch=200 块，块间隔 3000ms；地址升序轮转 |
| 掉线罚没 | `0x1001 SlashIndicator` | 每个越位块给"本该出块的人"记 1；50→misdemeanor，150→felony+`downtimeSlash`（烧 10 代币、jail 2 天） |
| 双签罚没 | `0x1001` + `0x68` 预编译 | 同高度、同 parentHash、不同签名且同一 signer；烧 200 代币、jail 30 天、`felony` 驱逐；证据 28800 块内有效；**要求 signer 已在 StakeHub 注册** |


---

## 2. 测试环境

### 2.1 主机与工具（已具备）

| 项 | 值 |
| --- | --- |
| 平台 | macOS Intel，Go 1.25.0，Python 3.13，Node 22 |
| Foundry | `/usr/local/bin/{forge,cast,anvil}` 已装 |
| 构建 | `cd bsc && go build -o build/bin/geth ./cmd/geth`；`cd bsc-genesis-contract && forge build`（`out/` 必须存在） |
| 链 ID | **860621**（合约内 `bscChainID` 是编译期常量，不能改） |


### 2.2 四个隔离运行（互不污染 datadir）

| Run | 目的 | SEATS | CABINETS | F / D | Feynman | Breathe (s) | 预算 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| **A** | PBFT→POSA 平滑交接 | 6 | 6 | 40 / 20 | 开 | 300 | ~6–8 min |
| **B** | 质押 + 选举 + top-N 换人 | 6 | **5** | 40 / 20 | 开 | **300** | ~22 min |
| **C** | 双签 → felony/jail/驱逐 | 6 | 6 | 40 / 20 | 开 | 300 | ~12 min |
| **D** | 掉线 → 50/150 升级 | **4** | 4 | 40 / 20 | 开 | **2592000** | ~33 min |


每个 run 用独立的 `PGP_STAKING_HOME=$HOME/.pgp-staking/<run>`，`MULTI_HOME=$PGP_STAKING_HOME/net`，端口基址 `P2P 30300 / HTTP 18560`（同一时刻只跑一个 run，端口不冲突）。

**为什么 B 用 5 席**：6 个候选人、5 个席位，才能验证"第 6 名落选 → 加质押后进入前 5 → 原第 5 名退出"的 top-N 截断；6/6 只能验证"票力归零→退出"。
**为什么 D 用 4 席**：掉线记分是"每个越位块 1 分"，4 席时 150 分 ≈ 600 块（30 min），6 席要 900 块（45 min）。
**为什么 D 的 breathe 调到 30 天**：让 `clean()` 在测试窗口内一次都不跑，保证计数单调；用默认 86400 有 ~2% 概率跨桶被扣 37 分。
**legacy 生产者**：每个 run 都按 SEATS 生成（A/B/C 6 个、D 4 个），每节点持自己的 arbiter keystore，PBFT 段按 pubkey 排序轮转出块；6 席的 confirm 阈值是 4 票，比单仲裁人更接近真实网络（pgp 测试网是 12 个）。

### 2.3 genesis 参数矩阵（每个值都有原因）

| 参数 | A | B | C | D | 原因 |
| --- | --- | --- | --- | --- | --- |
| `legacy.forkHeight` | 40 | 40 | 40 | 40 | PBFT 段 40×3s=120s，足够观察且不拖时间；必须 `>=4` 且 `%200 != 0` |
| `legacy.systemContractDeployHeight` | 20 | 20 | 20 | 20 | 留出 21..39 观察注入结果；约束 `2<=D<=F-2` |
| `legacy.producers` | **6 个自派生 P-256** | **6 个** | **6 个** | **4 个** | 每节点一把 arbiter keystore，轮转出块；比单仲裁人更接近真实 |
| `legacy.parliaGenesisValidators` | 6 | 6 | 6 | 4 | 分叉时锚点快照的集合 |
| `legacy.parliaGenesisCabinets` | 6 | **5** | 6 | 4 | 决定实际出块席位 |
| `legacy.parliaGenesisVoteAddrs` | 6×48B | 同 | 同 | 4×48B | 不开 BLS 则 finality 永远为空 |
| `parlia` | `{}` | `{}` | `{}` | `{}` | 有 key 即启用 Parlia |
| `keplerTime` | **0** | **0** | **0** | **0** | 顺序校验要求 feynman 之前必须有 kepler；0 = 创世即激活 |
| `feynmanTime` | **0** | **0** | **0** | **0** | 0 = 创世即激活；非 0 会二次 initialize |
| `lubanBlock` | 40 | 40 | 40 | 40 | 与分叉同高，epoch extra 用 post-Luban 布局 |
| genesis `timestamp` | **`ceil((now+120)/300)*300`** | 同 A | 同 A | `now+60` | A/B/C 让块时间严格等于 `T0+3n`，breathe 落在 100/200/300；D 不需要相位 |
| 每验证人余额 | 10000 ETH | 同 | 同 | 同 | `minSelfDelegationBNB=2000` + 1 lock + gas + delegate 余量 |


> **时间戳为什么必须对齐且在未来**：PBFT 的 `header.Time = parent.Time+3`，落后墙钟就取 `now`；Parlia 同样。若 genesis 时间戳在过去，相位会被 `now` 顶掉，breathe 高度不可预测。把 T0 设为"未来 120s 起、300 的整数倍"，块时间就是 `T0+3n`，breathe 精确落在 100/200/300。预留 120s 是因为 6 个 legacy 生产者要先完成 DPoS 组网，否则第 1 块会被 `now` 顶掉、相位整体前移。
> >
> **为什么四个 run 都开 kepler/feynman**：测试链要验证的是"最终形态"，不是"Feynman 之前"的形态。全部开 0 的代价是每个 run 都必须先注册再跨首个 breathe 块（F4）；收益是选举路径在每个 run 都是活的，A 还能顺带验证"选举结果与现状一致时是幂等的"。
> >
> **如果确实要测 Feynman 激活块本身**（而不是"创世即已激活"）：唯一干净的做法是让 D 块不初始化那 5 个合约（改 `core/systemcontracts/pgp/manifest.json` 的 `deployInit`，或加一个 devnet 开关），让激活块里的 `initializeFeynmanContract` 完成初始化。代价是动了钉死的镜像元数据、且激活块失败会直接停链，风险高于收益；本轮不做，只在报告里注明"激活块路径未覆盖"。

### 2.4 三处前置改造（只动 devnet，不动生产默认）

**（1）`bsc/scripts/parlia-devnet/gen-genesis.py`**

| 改动 | 内容 |
| --- | --- |
| 新增参数 | `--feynman-time`、`--kepler-time`（int，默认 None）→ 写入 `cfg["feynmanTime"]` / `cfg["keplerTime"]`（JSON 整数） |
| 新增校验 | `feynman_time` 非 None 而 `kepler_time` 为 None → 退出；`feynman_time not in (None,0)` → 退出并说明二次 initialize |
| 放宽 cabinets | 第 239 行 `args.cabinets != len(validators)` 改为 `not (0 < args.cabinets <= len(validators))`；节点侧 `pgpinit` 本来就允许 `Cabinets <= n` |


**（2）`bsc/scripts/parlia-devnet/multi.sh`**（保持所有现有默认行为）

| 改动 | 内容 |
| --- | --- |
| 新增环境变量 | `CABINETS`（默认=SEATS）、`BALANCE_WEI`（默认仍 100 ETH）、`GENESIS_TIMESTAMP`（hex 字符串）、`FEYNMAN`（默认 1）、`BREATHE_INTERVAL`、`HTTP_API`、`LEGACY_PRODUCERS`（默认=SEATS，设 1 回退单仲裁人）、`DPOS_BASE`（默认 20679） |
| legacy 密钥 | `keygen -n $LEGACY_PRODUCERS`（不再 `-n 1`）；`--producers` 按 pubkey 逐个传 **N 次**；每节点 `--pbft.enable --pbft.keystore $ARBITER_DIR/arbiter-$i.dat --pbft.keystore.password $ARBITER_PASSWORD_FILE --pbft.miner.address <自己的 EVM 地址> --pbft.net.port $((DPOS_BASE+i)) --pbft.net.address 127.0.0.1 --pbft.net.maxnodeperhost $SEATS` |
| devp2p 组网 | 预生成 devp2p nodekey（`bootnode -genkey`），用 `bootnode -nodekey ... -writeaddress` 推出 enode id，得到全局同一份 `BOOTNODES=<全部 N 个 enode>`（含自己也无害，geth 会忽略自拨），每节点 `--bootnodes "$BOOTNODES" --maxpeers $SEATS`；**必须去掉 `--nodiscover`**（bootnodes 只播种 discovery）。现有 `admin_addPeer` 全互联保留为启动后的幂等兜底 |
| DPoS 组网 | **在 `geth init` 之后、`start` 之前**调 `seed-peers.py --manifest $ARBITER_MANIFEST --force --node 127.0.0.1:$((DPOS_BASE+i)):$HOME_DIR/node$i ...`（N 个 `--node`，按 keygen index 顺序），预写每节点的 `geth/network/dpos/peers.json`；运行中写会被 addrmgr 覆盖 |
| `cmd_init` | 余额改用 `$BALANCE_WEI`；gen-genesis 追加 `--cabinets`、`--timestamp`、`--feynman-time 0 --kepler-time 0`；结束写 `$HOME_DIR/devnet.env`（SEATS/F/CABINETS/TS/BREATHE/DPOS_BASE/LEGACY_PRODUCERS/NETWORK_ID） |
| `cmd_start` | 先 `source devnet.env`；把单节点启动抽成 `launch_node i`；有 `BREATHE_INTERVAL` 时给**每个**节点加 `--override.breatheblockinterval` |
| 新增子命令 | `start-node I` / `stop-node I`：`stop-node` 复用现有"`kill -0` + `lock_holders` 双判据"，**绝不 SIGKILL**（F-20：强杀会丢 state、重启重封被判双签） |


**（3）新增 `bsc/scripts/staking-devnet/`**

```
run.sh          # 唯一入口：init/start/stop/start-node/stop-node/wait/wait-breathe/wait-epoch/register/delegate/evidence/scenario/report
lib.sh          # HOME 布局、rpc()（绕过 macOS 代理）、cast 封装、die_gate
prepare.sh      # 启动前离线生成 BLS proof + createValidator calldata + 0x66 预演
stake.sh        # 链上写：register / delegate / undelegate / evidence-submit
expect.py       # 链上读 + 断言：election-info / mining-set / current-set / indicator / validator / producers / breathe
report.py       # 生成 report/<run>.md + raw JSON
doublesign/main.go  # 双签取证：emit（构造并重签 header2）/ verify（独立 ecrecover）
README.md       # 四个 run 的参数、门禁、已知坑
```

> `prepare.sh` 里生成 BLS proof 的命令：
> `geth bls account generate-proof <validator.addr> <bls/vote.addr> --datadir <node> --blspassword <node>/bls/create-password.txt --chain-id 860621`
> **CLI 必须用多行密码文件 `create-password.txt`**（`GetPassPhraseWithList` 按行消费，行数不够会挂住而不是报错）；运行期才用单行无换行的 `password.txt`。这是 `multi.sh:130-149` 已经踩过的坑。

### 2.5 启动前的前置门（G0，任何一条不过就不启动）

| 门 | 命令 | 期望 |
| --- | --- | --- |
| G0-1 编译 | `go build -o build/bin/geth ./cmd/geth && go build -o build/bin/bootnode ./cmd/bootnode` | 两个二进制都存在 |
| G0-2 合约产物 | `ls ../bsc-genesis-contract/out/BSCValidatorSet.sol/*.json` | 存在 |
| G0-3 单元测试 | `go test ./consensus/router/... ./consensus/parlia/... ./core/systemcontracts/pgp/... ./core/vm/ -run 'DoubleSign\ | Lookahead\ | PGP'` | 全绿（作为回归基线） |
| G0-4 genesis 形状 | `python3 -c 'import json;g=json.load(open(".../genesis.json"));print(g["config"]["feynmanTime"],g["config"]["keplerTime"],g["config"]["legacy"]["parliaGenesisCabinets"],len(g["config"]["legacy"]["producers"]))'` | 输出 `0 0 <cabinets> <seats>`（B 为 `0 0 5 6`） |
| G0-5 BLS proof | `cast call 0x66 <msgHash‖proof‖voteAddr>`（prepare.sh 生成） | 返回 `0x01` |
| G0-6 余额 | `cast balance <validator> --rpc-url ...` | ≥ 2100 ETH |
| G0-7 DPoS 预置 | 每节点 `geth/network/dpos/peers.json` 存在且 `Addresses` 条数 = seats-1 | 硬门（6 生产者组网的前提） |


---

## 3. 测试用例

### T1 分叉平滑交接（Run A，~6 min）

**步骤**

```
cd bsc
export PGP_STAKING_HOME=$HOME/.pgp-staking/fork
T0=$(( ($(date +%s)+120+299)/300*300 )); TS=$(printf '0x%x' "$T0")
CABINETS=6 FEYNMAN=1 GENESIS_TIMESTAMP=$TS BALANCE_WEI=0x21e19e0c9bab2400000 \
DEPLOY_HEIGHT=20 OMIT_SYSTEM_ALLOC=1 BLS_VOTE=1 \
  ./scripts/parlia-devnet/multi.sh init 6 40
KEEPALIVE=1 ./scripts/parlia-devnet/multi.sh start
./scripts/parlia-devnet/multi.sh wait 40
./scripts/staking-devnet/run.sh register      # T2：必须在首个 breathe 前完成
./scripts/parlia-devnet/multi.sh wait 105
```

**观测与预期**

| 观测点 | 预期 | 判定 |
| --- | --- | --- |
| 启动日志 | 每节点出现 `PBFT: producers connected ... peers=6 need=6`，且 head 在 T0+30s 内越过 1 | 硬门（6 生产者组网收敛） |
| block 1..39 `miner` | 全部 ∈ 6 个 EVM 地址；不同地址 ≥4；按 6 块周期轮转 | 硬门（PBFT 轮转真的在工作） |
| block 39 `extra` | PBFT 布局（vanity+confirm）；按 `143+132×票数` 解，票数 ≥4（6 席的 confirm 阈值） | 记录并核对 |
| block 20 之后 `eth_getCode(0x1000/0x2002)` | 非空 | 硬门（D 块注入生效） |
| block 40 `extra` 长度 | **97**（32 vanity + 65 seal；非 epoch 块无验证人段） | 硬门 |
| block 40 `miner` | ∈ 6 个验证人 | 硬门 |
| block 40..70 不同 `miner` 数 | ≥ 4 | 硬门（证明 Parlia 轮转接管） |
| 6 个节点 `block 40/70` hash | 完全一致 | 硬门（无分叉） |
| `getMiningValidators()` | 6 个地址 | 硬门 |
| 首个 breathe（理想 100） | 选举系统交易出现，`getMiningValidators()` 与注册集合一致（幂等） | 硬门（Feynman 生效且未改变集合） |
| 日志 | `Created pgp-chain height router forkHeight=40 parlia=true`；无 `Fatal`/连续 `Signed recently` | 硬门 |


**预算**：T0 前等待 ≤420s（对齐 300 的整数倍）+ 分叉 120s + 注册 30s + 观测 60s ≈ 6–8 min。

---

### T2 质押注册（所有 run 的公共前置，~2 min）

**目的**：把 6（或 4）个验证人登记进 StakeHub，满足 F4 的硬约束，并为双签/掉线罚没建立 `consensusToOperator` 映射。

**步骤**（分叉之后立即执行，head ≥ 40：`createValidator` 的 BLS proof 走 0x66 预编译，而 legacy 段的预编译集里没有它）

```
# Run B 用可区分的质押额，让选举排序确定可解释：V0..V5 = 2600/2500/2400/2300/2200/2100 ETH
VALUES=(2600 2500 2400 2300 2200 2100)   # A/C/D 全用 2100
for i in 0..5; do
  # prepare.sh 已产出 calldata 与 proof
  cast send --unlocked --from <addr_i> --value ${VALUES[$i]}ether --gas-limit 5000000 \
    0x0000000000000000000000000000000000002002 \
    "createValidator(address,bytes,bytes,(uint64,uint64,uint64),(string,string,string,string))" \
    <addr_i> <vote_i> <proof_i> "(0,5000,5000)" "(\"Node$i\",\"\",\"\",\"\")" \
    --rpc-url http://127.0.0.1:$((18560+i))
done
```

**预期与判定**

| 观测点 | 预期 | 判定 |
| --- | --- | --- |
| 每笔 receipt | `status=1`，gas ≈ 2–4M（含部署 StakeCredit 代理） | 硬门 |
| `getValidatorElectionInfo(0,0).totalLength` | 6（D 场景 4） | **第一硬门（G-B1）** |
| 每人的 `votingPower` | 等于注册的 msg.value（2600e18…2100e18），1 ETH lock 也计入池子 | 硬门（排序可解释） |
| `ValidatorCreated` 事件 | 每个 operator 一条，voteAddress 与 `<dir>/bls/vote.addr` 一致 | 硬门（三处票地址一致） |
| head 到达首个 breathe 前 15 块仍未全部成功 | **立即 `multi.sh stop`**，否则该 breathe 块必停链 | 强制 |


> 三处票地址必须同源：genesis `parliaGenesisVoteAddrs`、`createValidator` 的 `voteAddress`、节点 `--vote` 用的 BLS 钱包，全部读取 `<datadir>/bls/vote.addr`（`multi.sh:157-165` 已是这个口径）。

---

### T3 选举与 top-N 截断（Run B，~22 min）

**参数**：6 候选人 / 5 席 / Feynman 开 / breathe 300s / T0 对齐 → 块时间 T0+3n，breathe 落在 100、200、300，epoch 落在 200、400。

**序列**

| 高度 | 动作 | 观测 |
| --- | --- | --- |
| 41–60 | T2 注册 6 人（分叉之后：0x66 预编译只在 Feynman 预编译集里） | G-B1 |
| 40 | 分叉 | T1 的门快速复跑一遍 |
| **100** | 第 1 次 breathe | 块体内出现 `to=0x1000` + selector `0x1e4c1524` 的系统交易；`getMiningValidators()` 从"创世顺序的前 5"改为"按票力降序的前 5"：**V0..V4 在列，V5(2100) 出局** |
| **200** | 第 2 次 breathe + epoch | `python3 scripts/parlia-devnet/epoch-report.py --port 18560 --height 200` 解出 **5** 个地址（这是 breathe@100 的排名，因为 header 在 Prepare 阶段读 parent state） |
| **203** | 快照切到 header 200 的 5 席（6 席时 checkLen=3） | `[203,~299]` 的 `miner` ⊆ header200 的 5 个地址；**V5 0 块** |
| 205–220 | `delegate`：给 V5 **+1000 ETH**（2100→3100） | `election-info` 排序翻转：V5 升到第 1，V4(2200) 掉到第 6；header200 已固定，不受影响 |
| **300** | 第 3 次 breathe | 选举写入 S2={V0..V3,V5}；`getMiningValidators()` 立即反映 |
| **402** | 快照切到 header 400 的 5 席（此时 checkLen=2） | `[402,430]` 的 `miner` ⊆ header400 的 5 个地址，且**集合与 header200 不同**；V4 0 块，V5 ≥2 块 |


> 为什么质押要在 203 之后改：这样 header200 的集合必然来自"改质押之前"的选举，header400 的集合必然来自"改之后"的选举。若把 delegate 放在第一次选举之前，两次切换会塌缩成一次，G-B5 就失去意义。该顺序不依赖 breathe 落在 100 还是 75。

> 注意：**203 之前 6 个验证人都会出块**。分叉锚点快照用的是 `parliaGenesisValidators`（6 人），合约层的 5 席要到 203 才进入共识。所以看到 V5 在 40–202 之间出块是正常的，不要据此判失败。

**判定标准**

| 编号 | 断言 | 说明 |
| --- | --- | --- |
| G-B2 | breathe 高度 ≤104；若明显偏小（如 <80）记录实测并继续，后续判断不依赖固定高度 | 出块慢于 3s 会让 breathe 提前，属正常 |
| G-B3 | `header[200]` 地址集合 == `getMiningValidators@202` | 合约层与共识层一致 |
| G-B4 | `[203,299]` 出块者集合 == header200 集合 | 集合切换真的生效 |
| G-B5 | `header[400]` 集合 != `header[200]` 集合 | 质押变化真的改变了选举结果 |
| G-B6 | `[402,430]` 出块者集合 == header400 集合 | 第二次切换生效 |
| G-B7 | 全程 head 单调、6 节点 hash 一致、无 `Fatal` | 无分叉/停链 |


**预算**：T0+300 首次选举，T0+402*3s≈1206s 第二次切换 ≈ 21 min。

> 关键认知（要写进报告，不是缺陷）：**质押变更到出块人变化之间有一个 epoch 的延迟**（≤200+checkLen 块，约 10 分钟）。这是 BSC Parlia 的既有语义，不是本链的 bug。

---

### T4 双签罚没（Run C，~12 min）

**前置**：T2 完成（`consensusToOperator(T) != 0`），否则合约报 `validator not migrated`。

**步骤**

1. `expect.py producers 40 head` 选出**目标验证人 T**：在 40 之后出过块，取它最近一次出块高度 H（保证 `H+28800 >= head`）。
2. 生成证据：

```
go run ./scripts/staking-devnet/doublesign emit \
--rpc http://127.0.0.1:18560 --height H \
--keystore $MULTI_HOME/nodeT/keystore --password-file $MULTI_HOME/nodeT/password.txt \
--chain-id 860621 --out evidence.json
```

工具内部：`HeaderByNumber(H)` → 用 T 的 keystore 解出私钥 → 校验 `SealHash(h1)` 恢复出的地址 == `h1.Coinbase`（不等即退出码 2）→ 复制 h1 只改 `GasLimit+1` → `crypto.Sign(types.SealHash(h2, chainId))` → 写回最后 65 字节 → 输出 `header1/header2/precompileInput`。

3. 预演验签（无副作用）：

```
cast call 0x0000000000000000000000000000000000000068 $(jq -r .precompileInput evidence.json)
# 期望 52 字节，前 20 字节 == T
```

4. 提交：

```
cast send --unlocked --from <任一已解锁验证人，建议非 T> \
0x0000000000000000000000000000000000001001 \
"submitDoubleSignEvidence(bytes,bytes)" $(jq -r .header1 evidence.json) $(jq -r .header2 evidence.json) \
--rpc-url http://127.0.0.1:18560
```

**预期与判定**

| 观测点 | 预期 | 判定 |
| --- | --- | --- |
| receipt | `status=1` | 硬门 |
| `0x2002` 日志 | `ValidatorSlashed(T, jailUntil, 200e18, 0)`（DoubleSign=0） | 硬门 |
| `0x1000` 日志 | `validatorFelony(T, amount)` | 硬门 |
| `getValidatorBasicInfo(T)` | `jailed=true`，`jailUntil = block.timestamp + 30 days` | 硬门 |
| `getValidatorElectionInfo` 中 T | `votingPower = 0`（jailed） | 硬门 |
| `isCurrentValidator(T)` | `false` | 硬门 |
| `getMiningValidators().length` | 5 | 硬门（felony 立即驱逐） |
| T 的票力（若可读） | 2100e18 → 1900e18（烧 200） | 记录 |
| 下一个 epoch 块之后 | 出块者集合不再含 T（快照切换） | 记录（共识层延迟，不是失败） |
| `unjail(T)` | revert `SelfDelegationNotEnough`（1900 < 2000） | 硬门（负向） |
| 重复提交同一证据 | revert `AlreadySlashed` | 硬门（幂等） |


> 注意：`felony` 只把 T 从 `currentValidatorSet` 移除，**快照里的 T 仍合法**，所以 T 在下一个 epoch 前可能还出块。不要断言"立刻停产"。

---

### T5 掉线升级到 felony（Run D，~33 min）

**参数**：4 席 / Feynman 开 / breathe 2592000s（30 天，保证测试窗口内没有 breathe、`clean()` 不跑）。

**步骤**

| 高度 | 动作 | 观测 |
| --- | --- | --- |
| 41–45 | T2 注册 4 人（分叉之后；不注册则 150 分时只驱逐、不烧币不 jail） | receipts |
| ~50 | `run.sh stop-node X`（X ≠ 控制节点） | datadir LOCK 释放；其余 3 节点继续出块 |
| 每 4 块 | 轮询 `indicators(X)` | `count` 单调 +1 |
| count=50（≈块 250） | `validatorMisdemeanor(X, amount)` | 计数**不清零**；不进入 maintenance（`maxNumOfMaintaining=0`） |
| count=150（≈块 650） | `validatorFelony(X, amount)` + `ValidatorSlashed(X, jailUntil, 10e18, 1)`（DownTime=1） | `jailed=true`、`isCurrentValidator=false`、`getMiningValidators()=3`、head 继续增长 30 块 |


**判定标准**

| 编号 | 断言 |
| --- | --- |
| G-D1 | 停机后前 10 块内 `count > 0`（否则 `slash()` 没被调用） |
| G-D2 | `count` 单调不减；出现 -37 的台阶 → 判失败（说明 breathe 间隔没隔离） |
| G-D3 | 50 分时 `validatorMisdemeanor` 在同一块出现 |
| G-D4 | 150 分时 `validatorFelony` 与 `ValidatorSlashed` 在同一块出现 |
| G-D5 | 罚没后 head 继续增长 ≥30 块，3 席仍能出块 |


**预算**：150×4 块×3s = 30 min，加上停机与观测 ≈ 33 min。可后台无人值守。

---

### T6 负向与边界（Run B/C 上顺带做，各 1–2 分钟）

| 编号 | 场景 | 期望 |
| --- | --- | --- |
| N1 | 普通账户调 `SlashIndicator.slash(v)` | revert（`onlyCoinbase`） |
| N2 | 从 coinbase 地址用非零 gasPrice 调 `slash`（需该验证人正好出块；否则先命中 `onlyCoinbase`） | revert（`onlyZeroGasPrice`），用 `debug_traceCall` 取 revert reason |
| N3 | 双签证据：两个 header 高度不同 / parentHash 不同 / 签名相同 | 预编译 revert（`invalid double sign evidence`） |
| N4 | 对未注册验证人提交证据 | revert `validator not migrated` |
| N5 | 重复提交已监禁者的证据 | revert `AlreadySlashed`（不重复烧币） |
| N6 | 被监禁者提前 `unjail` | 若票力 <2000 → `SelfDelegationNotEnough`；若 jail 未到期 → `JailTimeNotExpired` |
| N7 | `editConsensusAddress` 当日重复调用 | revert `UpdateTooFrequently`（`valInfo.updateTime + 1 days`） |


---

## 4. 测试顺序与依赖

```
G0 前置门（构建/产物/单测/genesis 形状/BLS proof/余额/DPoS 预置）
        │
        ▼
T1 分叉交接（Run A：6 个 legacy 生产者 → 6 席 Parlia）  分叉不通，后面全部无意义
        │
        ├──► T2 质押注册（A/B/C/D 每个 run 的分叉后，首个 breathe 前必须完成）
        │        │
        │        ├──► T3 选举 / top-N（Run B）──► 依赖 breathe 300 + cabinets(5) < 候选人(6)
        │        ├──► T4 双签（Run C）──────────► 依赖 consensusToOperator
        │        ├──► T5 掉线（Run D）──────────► 依赖注册（烧币/jail 路径）
        │        └──► T6 负向（Run B/C）────────► 依赖注册与已部署合约
        │
        └──► 回归：go test ./consensus/... ./core/vm/... + forge test
```

**必须先做 T1**：分叉失败时，T2 之后所有"分叉后行为"都不成立；T1 同时验证 6 个 legacy 生产者的 DPoS 组网，这是后面每个 run 都要复用的能力。
**T2 是 T3/T4/T5/T6 的共同前置**：不只是为了选举，`consensusToOperator` 是双签罚没和 `downtimeSlash` 的硬前提。
**T3 必须在 T4 之前**：T3 验证"正常选举能改变集合"，T4 才有参照证明"被罚没的人是被选举/驱逐拿掉的"，而不是别的原因。
**T5 最后做**：耗时最长且需要停机，放在其他 run 都通过之后，避免占着机器影响排查。

**建议执行节奏**：A（6–8 min）→ B（22 min）→ C（12 min）→ D（33 min，可挂后台）。总计约 **75–85 分钟**，不含失败重跑。

---

## 5. 全局判定标准（一次 run 是否算通过）

| 级别 | 条件 |
| --- | --- |
| **PASS** | 该 run 的全部硬门通过；head 单调；所有节点 hash 一致；无 `Fatal`/panic；报告含参数表、门禁实测值、出块者分布、关键 tx/事件 |
| **PASS with deviation** | 仅时间相位漂移（breathe 高度偏离 [98,104]）或 epoch 切换高度按公式重算后仍成立；报告注明实测值 |
| **FAIL** | 任一分叉/停链；注册未在首个 breathe 前完成；集合切换未生效；罚没事件缺失或金额/时长不符；重复罚没 |
| **BLOCKED** | 环境门（G0）不过；或发现新的实现缺陷导致用例无法执行（此时先记录，不修改生产代码） |


---

## 6. 风险与回退

| 风险 | 触发 | 缓解 |
| --- | --- | --- |
| 首个 breathe 块空集合停链 | 注册未在首个 breathe 前完成 | G-B1 硬门 + 失败立即 `stop`；重跑时把 B 调到 600s 重来 |
| `feynmanTime` 写成未来值 | 手工改 genesis | 生成器直接拒绝非 0；G0-4 校验 |
| 各节点 breathe 间隔不一致 | 手工 `start-node` 漏传 | `devnet.env` 由 init 落盘，start 只读它；启动后逐节点 grep 命令行 |
| `clean()` 吃掉掉线计数 | D 场景误用 300s | D 固定 2592000s；G-D2 单调性门 |
| 集合切换"看不见" | 在 breathe 后立刻断言出块人 | 观察点按 203/402 公式；用"出块者集合"而非单块 `miner` |
| 余额不足 2001 | 用了 multi.sh 默认 100 ETH | `BALANCE_WEI=1e22`；G0-6 |
| 只靠 bootnode、不预置 DPoS | devp2p 已连通，但 DPoS 地址簿为空；`producers connected` 要等 ~40s，块时间被 `now` 顶掉、breathe 相位前移 | 预置 `peers.json`（G0-7）；把 `producers connected` 作为 T1 首门；漂移按 G-B2 记录 |
| 6 生产者 DPoS 组网不收敛 | legacy 段卡在高度 0/1；日志 `producers connected peers<6` | `seed-peers.py` 预置（G0-7）；`--pbft.net.port` 每节点不同；`--pbft.net.maxnodeperhost=$SEATS`；60s 内不收敛则回退 `LEGACY_PRODUCERS=1` 重跑 |
| 6 生产者下某节点启动失败 | confirm 不足 4 票，legacy 段停 | T1 先跑"head 越过 1 且 6 节点 hash 一致"的门，再继续 |
| PBFT 段交易不打包 | 在 head<40 时发注册 | 所有写操作等 `head>=F`；receipt 带超时重试 |
| BLS 票地址三处不一致 | 重新生成钱包 | 三处统一读 `<dir>/bls/vote.addr`；G0-5 预演 0x66 |
| 强杀导致重封被判双签 | `stop-node` 用 `kill -9` | 复用 `multi.sh` 的 LOCK 轮询，超时只告警 |
| 证据过期/重复 | 取证块太旧或重复提交 | 取证块选 `head-5` 内；每轮只提交一次；G-T4 负向门 |
| 历史状态不可查（非 archive） | 事后用历史块号 `eth_call` | 观测点到达时立即用 `latest` 采样；报告阶段只读事件与区块体 |


---

## 7. 本次不覆盖（明确边界）

| 项 | 原因 |
| --- | --- |
| 恶意投票 / finality violation 罚没 | `enableMaliciousVoteSlash` 默认 false，需治理提案开启；本次只在 Foundry 层跑 `SlashIndicator.t.sol` 的 `testMaliciousVoteSlash` |
| `unbondPeriod`(7 天) 后的 `claim` | 链上时间不够；只验证"解绑后票力立即下降 + 请求进入队列 + `claimable=0`" |
| PBFT 拜占庭仲裁人/视图切换 | 6 生产者只验"正常轮转 + 4/6 confirm 阈值"，不注入作恶仲裁人；视图切换属于 `scripts/pbft-devnet/` 的既有测试范围 |
| ELA SPV 同步与跨链 | 无 ELA 链，`--chapel` 不启用 |
| 生产配置/系统合约镜像变更 | 本方案只改 devnet genesis 与 devnet 脚本；`pgp-fork-v4` 镜像与 `params.PGPChainConfig` 不动 |


---

## 8. 交付物

| 产物 | 路径 |
| --- | --- |
| 方案文档 | 本文件（执行阶段同步一份到 `bsc/docs/test/staking-posa-test-plan.md`） |
| 脚手架 | `bsc/scripts/staking-devnet/{run.sh,lib.sh,prepare.sh,stake.sh,expect.py,report.py,doublesign/main.go,README.md}` |
| 补丁 | `gen-genesis.py`（feynman/kepler/cabinets）、`multi.sh`（CABINETS/BALANCE/GENESIS_TS/BREATHE/start-node/stop-node/devnet.env） |
| 运行产物 | `$PGP_STAKING_HOME/<run>/report/<run>.md` + `raw/*.json`（参数表、门禁实测、出块者分布、breathe/epoch 高度、合约快照、tx/事件） |
| 回归 | `go test ./consensus/router/... ./consensus/parlia/... ./core/vm/...` + `forge test`（记录已知被 `Deployer.sol` 未调 init 阻塞的用例） |


---

## 9. 执行前最后确认清单

- [ ] 接受 F1：目标 2 测的是 StakeHub 路径，不是自定义 election 合约
- [ ] 接受 F3/F4：devnet genesis 用 `feynmanTime=0 + keplerTime=0`，且注册必须先于首个 breathe 块
- [ ] 接受 F6：6 个 legacy 生产者（DPoS 端口 20679–20684，预置 `peers.json`），失败时可用 `LEGACY_PRODUCERS=1` 回退
- [ ] 知悉 `keplerTime=0` 会关闭每块 1/16 的系统奖励（`parlia.go:2117`），这是现代 BSC 语义
- [ ] 确认 4 个 run 的 datadir 与端口不冲突，同一时刻只跑一个
- [ ] 确认机器能同时跑 6 个 geth + 6 个 DPoS 监听（内存/磁盘），D 场景可挂后台
- [ ] 确认可以接受 B 场景约 10 分钟的"选举→出块人变化"延迟（这是协议语义，不是失败）