#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
verify-unison.py — PBFT 多节点「视图一致 / finality 追平」验收门禁（只读）

用途
    在一个已经在跑的 N 节点 PBFT 私链上，判定它是否处于「健康一致」状态。
    专门用来卡住 2026-10-31 那次「高度卡在 6~8、10 分钟才出一个块」的故障复发。

设计约束（重要）
    * 本脚本严格只读：只读日志文件、只读 geth IPC/RPC。
    * 绝不执行 stop / start / kill / restart。
    * 绝不写任何节点的 datadir、logs 目录或仓库内文件。全部结果只打到 stdout。
      （所以它可以在「链正在被人盯着看」的时候安全地跑。）
      --selftest 例外：它会写一个临时目录（tempfile.mkdtemp），跑完自己删掉，
      不碰任何真实 datadir。

================================================================================
使用约定 —— 每次改动本文件之前，先把这一节读完
================================================================================

    0) **夹具必须先能红。**
       任何一条门禁，在拿它去验链之前，先用一个「已知会挂」的输入跑一遍，
       确认它真的会红；红了，再拿它去验链。

       这不是洁癖，是 2026-10-31 那天付过学费的：我在一条已经彻底死掉 6.5 分钟
       的链上跑门禁，六条里 G1/G4/G6 全绿、只有 G3 红。假如当时 G3 也碰巧因为
       别的原因（比如末块刚好新鲜）放行，整套工具就会在一条死链上开绿灯，
       而我不会有任何察觉 —— 那次是运气好，不是工具有效。
       **一个没有被证明过会红的门禁，等于没有门禁。**

       固化成命令：`./verify-unison.py --selftest`
       改完必跑，**全项 PASS** 才算改对。这一条同时覆盖「改了解析正则」和
       「改了判据阈值」两种改动 —— 后者最危险，因为脚本不会报错，只会静默放水。

    0.1) 判据只能由「真实事故归档」来定，不能由「看起来合理」来定。
       本文件里每一条阈值后面都跟着一串「实测：死链 X -> FAIL / 健康 Y -> PASS」。
       没有这串数的判据不许写进来。

================================================================================
门的编制（2026-10-31 二次改判，别再改回去）
================================================================================

    真正参与放行判定、影响退出码的只有三条：
        G2  finality 追平
        G3  TOTAL-STALL（出块停滞）   <-- 唯一能抓「链已经死掉」的一条
        G5  replay 归零

    G1 / G4 / G6 降级成断言宏 ASSERT-QUIET：照旧解析、照旧打印计数与诊断，
    但**不参与 PASS/FAIL、不影响退出码**。

    为什么降级 —— 拿 fix3 的 h158 归档（末 400 秒彻底停滞，零出块）回放，结果是：

        G1 = PASS（122 轮）   G4 = PASS（16 个窗口）   G6 = PASS（7 个窗口）   G3 = FAIL

    六条门禁里**只有 G3 对「链已经完全死掉」有反应**。G1/G4/G6 探的是「脏」
    （锚点分叉、双当值、视图爬升），而死锁时视图转得又稳又齐，它们当然给 PASS。
    它们没写错，是量纲和「链还活着」不对齐。
    让它们继续当门禁，唯一后果就是：3~5 个绿灯把 1 个红灯稀释掉，
    读报告的人以为「大部分都过了」。降级之后报告里只剩 3 个真门，红就是红。

为什么是这几条门禁（每条都对应一次真实事故，不是拍脑袋）
    G1 视图一致
        事故：2026-10-31 22:01:37，值班出块人 node5 在 viewOffset=0 出块，
              node2/node3/node4 在 viewOffset=1，三台同毫秒以
              `have different view offset` 拒收，导致该轮只有 3 票（quorum 需 4）。
        证据：node2/3/4 该轮 `acc_vote` 发送数 = 0；node5 `acceptVotes count` 停在 3。
        判据：a) 同一轮六台 viewoffset 相差 <= 1（ticker 相位天然差一跳，不能要求相等）；
              b) 不许「只涨不回」——够 quorum 台在采样期内 VO 从未回落且首末差 >= 2，
                 就是活锁的形状（死链末段 VO 一路堆到 23，142 秒没出过块）。
        盲区说明：22:01:37 那一刻实测差恰好是 1，链却随后停了 605 秒。
        所以 G1 只是快筛，真正的牙齿在 G3/G4/G5。

    G2 finality 追平
        事故：同上，22:13:16 实测 head 全为 10，但 finishedheight = 9/8/9/10/9/9，
              node1 落后 2 个块且持续了 10 分钟。
        判据：head - finishedheight <= 1，且 blockNumber == headheight。
        == 0 不是正确判据：FinishedProposal 只在「本节点有 confirm 且 sealHash 匹配」
        时推进（engine.go:447-452），刚出的块 confirm 还在路上，head-fin == 1 是设计内正常态。

    G3 出块停滞 TOTAL-STALL（唯一的生死判据）
        事故两起，判据因此改过三版：
        ① 22:01:37 那轮 node5 的 `acceptVotes count` 停在 3，够不到
           majoritycount=4，于是那轮只有 3 票，出不了块。
        ② 23:31:17 落 h158 之后再也没出过块，直到 23:37:47 被停机 —— 6.5 分钟。
           这一段的形状是：视图照转（353 条 viewStartTime）、提案照来
           （114 条 ProcessProposal start）、票只到 1~3（203 条），
           quorum 一次没到（`acceptVotes count: 4` 共 0 条）。
        判据演进（前两版都被真实日志打回，别再改回去）：
          初版「每台必须出现过 count >= 4」—— 方向是反的。死链六台的 count
            最大都到过 4（放水），健康 devnet 六台最大只有 2（误报）。
          二版「末高度**首次**出现距末行 <= 60s」—— 能抓，但阈值只覆盖
            「当下正在停滞」，抓不到「已经停滞过一段、刚恢复」，而且 60s 太短，
            和设计 6s/块只差 10 倍，一次慢块就可能误伤。
          现版（本文件当前）—— **窗口内块高必须单调增长，任一段 >= 90s 零推进即 FAIL**。
            窗口取最近 300s（理由见 STALL_WINDOW_SEC 常量处）。
            与二版的区别：不看「末块新不新鲜」，看「整段曲线里有没有洞」，
            所以「刚恢复但中间卡了 6 分钟」这种也会被抓。
        关键前提（这条不成立，判据就是错的）：
            `[--AppendDposBlock--], height N` **不是「出了新块」**，是「块进池子」
            （dpos/blockpool.go:161，调用点 network_listener.go:178 / blockpool.go:83）。
            卡住时它每一轮都把同一个高度重打一遍：死链 node0 h9 被重打 172 次，
            22:14:05 还在打 136 秒前的 h11。
            所以取高度时必须按 height 去重、只认**首次**出现的时间，否则
            「重打」会伪装成「推进」，整条判据失效。详见 gate_stall 注释。

    G1 视图一致 / G4 视图窗口一致 / G6 当值一致
        **已降级为 ASSERT-QUIET，不参与放行。** 三条探的都是「脏」不是「死」，
        死锁时全部给 PASS（见文件头「门的编制」）。判据说明保留在各自函数注释里，
        供人读报告时判断「脏到什么程度」，但它们不再有否决权。

    G4 视图窗口一致（核心）
        事故：这是 G1/G3 的根因。锚点 = viewStartTime - viewOffset*6s。
              故障期六台分成两派：{node0,node1,node5} 锚点 79297(22:01:37)、
              {node2,node3,node4} 锚点 79287(22:01:27)，整整差 10 秒。
              10 mod 6 = 4，两派 viewStartTime 永远错开 4s/2s —— 这就是「永不收敛」的算术原因。
        判据：末 20 条视图行内，各节点必须存在共同的 viewStartTime（且不过期 60s）。
        为什么不比锚点而是比 viewStartTime（四种写法都被真实日志打回过，详见 gate_anchor 注释）：
          - 健康 devnet 的**锚点差是 18s（3 个槽），死链只有 10s**，锚点差越小反而越死；
            因为 viewoffset 会补偿陈旧锚点：viewStart = 锚点 + VO*6s。
          - 「锚点集合相等」会误报（devnet node3 只有 4 条 view 行，天然是子集）。
          - 「全历史交集非空」会放水（死链六台在 21:59、22:11:37 都对过，3:3 僵 15 分钟照样过）。
        死链实测：22:14:01(node0,1,3) vs 22:14:05(node2,4,5)，末 20 条 viewStart 交集 = ∅ -> FAIL。
        健康 devnet 实测：20:46:19 被四台共同经历 -> PASS。

    G5 replay 归零
        事故：同上。`replaying a proposal that arrived ahead of our view` 只出现在
              「滞后侧」：node1=20、node5=16，而超前侧 node2/3/4 = 0；
              健康对照 devnet 四台全为 0。
        判据：六台 replay 行计数必须全为 0。修完若还有某台在重放，说明锚点仍在分叉。

    G6 当值一致 / 双当值
        由来：值班人 = producers[(dutyIndex + viewoffset) % N]（dpos/producers.go:261-272）。
              所以「谁当值」不只取决于 dutyIndex，还叠加了各台不同的 viewoffset。
              两个节点只要环位落到自己头上就会同时当值、各出一块、互相拒收 ——
              这比「vo 不相等」更贴近致命后果。
              team-lead 在修好 P0-A 之前那次实测：node3 vo=12、node5 vo=28，
              16 % 6 = 4，两个环位各自命中一个持钥人，于是两人同时 onDuty=true。
        判据：最近 10 个「>= quorum 台有视图行」的 6s 窗口里，不允许出现
              「最大环位支持 < quorum」的窗口（即不允许够 quorum 台认不出同一个当值人）。
              不是「六台环位必须相同」：健康 devnet 的 node3 全程 duty=0、vo 爬到 6，
              环位一直差 1，按「六台相同」会判死一个还在 6 秒出块的链。
        实测：死链 144 个窗口里 122 个不满足 quorum，末段 10 个里 4 个是 3:3 双当值；
              健康 devnet 6 个窗口 0 违反。

    附带统计（不设门禁，只记录，供人判断）
        * `have different view offset` 次数
        * `alreadyExistVote` 次数
        * 出块间隔 P50 / P95 —— 口径是 `[--AppendDposBlock--], height N` 行
          （共识层真正落块的那一刻），合并全部节点、按 height 取最早一次。
          注意别和 `Imported new chain segment` 混用：两者块号会差一位、
          分母也不同（前者 6 台各记一次，后者含追块），P95 会得出 395s / 610s 两个数。
        * `alreadyExistVote` 次数（单独列，不并进 G3 的票数判据）

退出码
    0  真门禁（G2/G3/G5）全部 PASS
    1  真门禁至少一项 FAIL
    2  判定不完整 —— 有门禁 SKIP（--logs-only 缺 IPC、日志窗口不足 300s、
       日志已停更、部分节点 IPC 采集失败…）
       注意：2 也是非 0，CI 里同样会挂，符合「不能确认通过就算没过」的原则。
       **SKIP 不是豁免。** 一条判不了的链，和一条判红的链，在放行意义上是同一档：
       都不能上线。

用法示例
    # 六节点，日志在 <datadir>/../logs/geth.log
    ./verify-unison.py \\
        --datadirs ~/.pgp-private/node0/data,~/.pgp-private/node1/data,... \\
        --rpc 8600,8601,8602,8603,8604,8605

    # 链已经停了，只拿冻结的日志复盘（G2 会 SKIP，退出码 2）
    ./verify-unison.py --logs-only --datadirs /nonexistent,... \\
        --log-dir /tmp/pbft-forensics-221535

    # 日志文件名不是默认布局时，逐台指定
    ./verify-unison.py --datadirs ... --log node0=/path/a.log --log node1=/path/b.log

    # 改完本文件第一件事：证明夹具能红（不需要 --datadirs）
    ./verify-unison.py --selftest
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Dict, List, Optional, Sequence, Tuple

# 视图槽长度。协议里 changeViewTime = viewStartTime + 6s（日志里 6 秒恒定）。
VIEW_SLOT_SEC = 6

# ---- G3 TOTAL-STALL 的三个数，每个都有来处，别凭感觉调 ----

# 一段「完全零推进」达到多长判 FAIL。
# 90s 的来处：
#   * 设计出块周期 3s（prepare.go:160 header.Time = parent.Time + period），
#     实测稳态 P50 = 10.01s（6.0s 空洞未修，见交付说明；fix3 全段与 fix5 全段同为 10.0s）。
#   * 实测健康跑的最慢单块间隔 12.75s。90s = 7 倍。
#   * 实测死锁的停滞长度 390s（h158，23:31:17 -> 23:37:47，零出块）。90s = 1/4.3。
#   7 倍余量 + 能覆盖 1/4 个真实事故，两头都不紧。
STALL_SEG_SEC = 90.0

# 判据窗口长度。低于这个长度，G3 不许给 PASS（只能给 SKIP）。
# 300s 的来处：
#   * 必须 > 3 × STALL_SEG_SEC。窗口边界会截断停滞段：若窗口恰好 2×90s=180s，
#     一段真实的 170s 停滞被切在窗口中间，量出来只有 85s，判不出来 —— 假绿。
#     300s / 90s = 3.33，容得下 3 个完整判据段，截断最多吃掉 1/3。
#   * 必须 ≥ 真实事故里「最短的一次致命停滞」。已知最短的是 390s（h158），
#     300s 略短于它，所以判据抓的是「仍在进行或刚结束不久」的停滞，
#     抓不到「300s 以前发生过、之后已恢复」的旧账 —— 那是出块间隔 P95 的活，不是本门的活。
#   * 出块数换算：300s / 10s = 30 个块。30 个点足够看出「单调增长」有没有洞。
STALL_WINDOW_SEC = 300.0

# 日志多久没更新，就认为「这台进程已经不在写日志了」。
# 用 mtime 而不是日志里最后一行的时间，理由：进程被 kill 后日志不会再有新行，
# 但文件里最后一行的时间戳会一直停在死亡那一刻 —— 拿它算「停滞多久」会越算越小，
# 甚至会算出「0 秒前刚出块」，把一条死链报成绿的。
# 90s 的来处：本链空闲时每 6s 也必有一条 viewStartTime（视图 tick 恒在），
# 90s = 15 个 tick 没动静，只可能是进程停了或日志被截断。
LOG_STALE_SEC = 90.0

# ---------------------------------------------------------------------------
# 日志解析正则
# ---------------------------------------------------------------------------

# INFO [10-31|22:00:07.543] viewStartTime: 2026-10-31 22:00:07 +0800 CST
#   changeViewTime 2026-10-31 22:00:13 +0800 CST nowTime: ...
#   offsetTime: 543ms force: false viewOffset 3 dutyIndex 0
RE_VIEW = re.compile(
    r"\[(?P<ldate>\d\d-\d\d)\|(?P<ltime>\d\d:\d\d:\d\d\.\d+)\]\s+"
    r"viewStartTime:\s+(?P<vdate>\d{4}-\d\d-\d\d)\s+(?P<vtime>\d\d:\d\d:\d\d)\s+\+0800\s+CST\s+"
    r"changeViewTime\s+(?P<cdate>\d{4}-\d\d-\d\d)\s+(?P<ctime>\d\d:\d\d:\d\d)\s+\+0800\s+CST\s+"
    r"nowTime:.*?"
    r"offsetTime:\s+(?P<off>[0-9.]+)(?P<offu>ms|µs|us|s)\s+"
    r"force:\s+(?P<force>true|false)\s+viewOffset\s+(?P<vo>\d+)"
    r"(?:\s+dutyIndex\s+(?P<duty>\d+))?"
)

# INFO [10-31|22:01:37.473] [--AppendDposBlock--], height 9
RE_APPEND = re.compile(r"\[(?P<ldate>\d\d-\d\d)\|(?P<ltime>\d\d:\d\d:\d\d\.\d+)\]\s+\[--AppendDposBlock--\],\s+height\s+(?P<h>\d+)")

RE_ACCEPT_COUNT = re.compile(r"acceptVotes count:\s*(\d+)")

# 任意一行日志的行首时间戳  [10-31|22:14:05.123]
RE_LINE_TS = re.compile(r"\[(?P<ldate>\d\d-\d\d)\|(?P<ltime>\d\d:\d\d:\d\d\.\d+)\]")

# 这些是全小写/大小写混合都出现过，统一 lower() 后比较
NEEDLE_DIFF_VIEW = "have different view offset"
NEEDLE_DUP_VOTE = "alreadyexistvote"
NEEDLE_REPLAY = "replaying a proposal"

# ---- 死锁指纹（2026-10-31 h158 事故）----
#
# 事故：fix3 跑在 h158 上，23:31:17 落块之后**再也没出过块**，直到 23:37:47 被停机，
# 整整 6.5 分钟。归档日志（geth-before-20261031-233832.log）里这一段是：
#
#   PBFT: height already has a confirm, refusing to propose height=159     × 39
#   PBFT: proposal was rejected by the producer set          height=159     × 19
#   acceptVotes count: 4                                                    ×  0
#   FinishedProposal                                                        ×  0
#   ProcessProposal start                                                   × 114（提案还在来）
#   viewStartTime:                                                          × 353（视图照转）
#   acceptVotes count: 1/2/3                                                × 203（票只到 1~3）
#
# 也就是：h159 的 **confirm 有了、块没有**。Seal 侧因为「已经有 confirm」拒绝再提案，
# 收方因为「不在出块人集合里」拒绝，票永远凑不到 4，视图每 6 秒空转一次。
#
# 关键判据意义：**这两条在健康跑里恒为 0**（fix5 跑 h159~h204 全程 0 条），
# 所以它不是噪声，是这类死锁的专属指纹。这里只当 G3 的「归因信息」打印，
# **不参与 PASS/FAIL** —— 偶发一条不该判死，而停滞本身已经由 G3 的硬判据抓到了。
# 但 G3 一旦 FAIL，这两个数要能直接告诉人「是这条路，不是别的原因」。
NEEDLE_CONFIRM_NO_BLOCK = "already has a confirm"
NEEDLE_PROPOSAL_REJECTED = "rejected by the producer set"
# 看门狗（watchdog）真的动手了没有。这一行是 ConfirmBlock 的自救分支：
# 一个高度收着 confirm 却永远等不到块时，30s / 5 个视图之后主动丢掉那条 confirm，
# 否则它会把后续每一个提案都堵死。
# 为什么必须单独计：kou-dou-ma-2 的判据是「`dropping a confirm` 出现、且链继续走 = 修好了」。
# 反过来，**停滞发生但这条计数为 0**，说明看门狗根本没被触发过 ——
# 那种 FAIL 就不是「看门狗救回来了」，而是「压根没走到看门狗」，归因完全不同。
NEEDLE_DROPPING_CONFIRM = "dropping a confirm whose block never arrived"
RE_DROPPED = re.compile(r"height=(\d+)\s+held=([0-9.]+)s\s+views=(\d+)")
RE_STUCK_HEIGHT = re.compile(r"height[= ]\s*(\d+)")

# 「块真的落地」的标记。`[--AppendDposBlock--]` 是「块进池子」，一个高度会打 2/4/6 条
# （同一个块体被投递两轮、每轮 2~3 行同毫秒），两轮之间正好差一个 6s 视图槽。
# 实测（2026-11-01 00:23~00:32，probe6，node0，55 个高度）：
#     每条高度的 Append 行数分布 = {2 条: 23 个, 4 条: 25 个, 6 条: 8 个}
#     同一高度相邻两条 Append 的间隔 = [0.00, 6.28, 0.00] / [0.00, 5.55, 0.00]
# 所以：
#   * 「某高度被 Append 了几次」在**健康跑里就是 2/4/6**，不是 1。拿它当「重打」会误判；
#     真卡住时是 44 / 172 这种量级。
#   * 量出块间隔要用 `Imported new chain segment`（commit），不能用 Append 首次出现：
#     同一窗口内 Append 口径 P50=11.55s / 最大 21.70s，commit 口径 P50=11.91s / 最大 12.50s。
#     Append 口径的尾巴是被「首轮 vs 次轮」的 6s 抖动撑出来的假尾巴。
NEEDLE_IMPORT = "imported new chain segment"
RE_IMPORT = re.compile(r"Imported new chain segment\s+number=(\d+)")


def _to_dt(date_str: str, time_str: str) -> datetime:
    return datetime.strptime(f"{date_str} {time_str}", "%Y-%m-%d %H:%M:%S")


_YDAY_CACHE: Dict[str, int] = {}


def _abs_secs(ldate: str, ltime: str) -> float:
    """把日志前缀的 MM-DD|HH:MM:SS.mmm 转成「自年初起的秒」（年内第几天*86400 + 当日秒）。

    为什么不用「当日秒」（第一版就是这么写的，被真机打回）：
      1. **跨零点会倒挂**。23:59:59 = 86399 直接跳到 00:00:01 = 1，差 -86398s。
         实测后果：出块间隔「最小值 = -86382.97s」、时间轴首尾显示「h251..h250」、
         末高度「首现距今 86394.8s」—— G3 判据整段失效，而脚本一声不响。
      2. **六台起点不同就合不到一起**。新起的那轮，有的节点日志从 23:53 开始，
         有的从 00:01 开始；各自算「当日秒」再合并，跨零点那台会比别人多整整一天。

    用「年内天数 * 86400 + 当日秒」一次解决两个问题：同一时刻六台算出的值相同，
    跨零点自动多一天，单调递增。

    日志前缀只有 MM-DD 没有年份，统一按 2000 年（闰年）折算。
    这个值只用来排序和求差、不参与任何绝对时刻的换算，所以 2/29 不会造成问题。

    局限：跨年的日志（12-31 -> 01-01）会倒挂。单轮跑不到跨年，暂不处理。
    """
    yday = _YDAY_CACHE.get(ldate)
    if yday is None:
        try:
            mm, dd = (int(x) for x in ldate.split("-"))
            yday = (datetime(2000, mm, dd) - datetime(2000, 1, 1)).days
        except ValueError:
            yday = 0
        _YDAY_CACHE[ldate] = yday
    hh, mm_, ss = ltime.split(":")
    return yday * 86400 + int(hh) * 3600 + int(mm_) * 60 + float(ss)


# ---------------------------------------------------------------------------
# 单节点采集
# ---------------------------------------------------------------------------


@dataclass
class NodeSpec:
    name: str
    datadir: str
    rpc_port: Optional[int] = None
    ipc: str = ""
    geth: str = ""
    log_path: str = ""


@dataclass
class NodeSnapshot:
    name: str
    ok: bool = False
    error: str = ""
    block: Optional[int] = None
    vo: Optional[int] = None
    fin: Optional[int] = None
    head: Optional[int] = None
    rpc_block: Optional[int] = None
    rpc_error: str = ""
    majority: Optional[int] = None
    # 复核记录：单次抓拍 head-fin > 1 时，隔 3s 再读一次，把前后两次读数都留在这里。
    # 不加这个字段就会把一次瞬态当成「节点落后 2 个块」报出去（实测有假阳性）。
    recheck: str = ""


@dataclass
class LogStats:
    name: str
    path: str
    exists: bool = False
    error: str = ""
    # (log_abs_secs, anchor_epoch, viewStartTime_epoch, viewOffset, dutyIndex)
    #   dutyIndex 可能缺失（老版本日志没有这个字段）-> None
    #   anchor      = viewStartTime - viewOffset*6s（这一轮视图网格的原点）
    #   viewStart   = 该行打印的 viewStartTime，即「当时站在哪个视图窗口」
    #   两个都要：G4 的硬判据用 viewStart（结果层），诊断用 anchor（原因层，P0-A 靶心）
    view_events: List[Tuple[float, int, int, int, Optional[int]]] = field(default_factory=list)
    accepts: List[int] = field(default_factory=list)
    diff_view: int = 0
    dup_vote: int = 0
    replays: int = 0
    # height -> 最早出现的时间（当日秒）
    first_height_at: Dict[int, float] = field(default_factory=dict)
    # 最后一条 [--AppendDposBlock--] 的（当日秒, 高度）；以及日志最后一行的当日秒
    # G3 用它判「光收票不出块」：末块距末行多远。时间轴统一走 _abs_secs（自年初起的秒），
    # 这样跨零点不会倒挂、六台起点不同也能对齐。
    last_append_secs: Optional[float] = None
    last_append_height: int = -1
    last_line_secs: Optional[float] = None
    # height -> 该高度的 `[--AppendDposBlock--]` 行被打印了几次。
    # 关键：这行**不是「出了新块」**，卡住时每一轮都会把当前高度重打一遍
    # （死链 node0 h9 重打 172 次、node5 170 次；健康 devnet 最多 4 次）。
    append_counts: Dict[int, int] = field(default_factory=dict)
    # height -> 该高度**落地**（Imported new chain segment）的时刻，取最早。
    # 为什么另记一份：Append 行是「块进池子」，一个高度会打 2/4/6 条、跨两个 6s 槽，
    # 拿它量间隔会把 6s 抖动算进出块周期里（实测 P95 17.86s），
    # 换成落地口径才是真周期（同窗口 P95 12.41s）。见 NEEDLE_IMPORT 处的注释。
    commit_at: Dict[int, float] = field(default_factory=dict)
    # ---- 死锁指纹（见 NEEDLE_CONFIRM_NO_BLOCK 处的长注释）----
    # confirm_no_block: `height already has a confirm, refusing to propose` 的行数
    # proposal_rejected: `proposal was rejected by the producer set` 的行数
    # stuck_heights:     height -> 前一条出现的次数（用来直接指出是哪个高度卡住了）
    confirm_no_block: int = 0
    proposal_rejected: int = 0
    stuck_heights: Dict[int, int] = field(default_factory=dict)
    # `dropping a confirm whose block never arrived` 的行数 = 看门狗动手次数。
    # 只当诊断：它出现**不是**坏事（说明有人在自救），它不出现而链又停了才是坏事。
    dropping_confirm: int = 0
    # 被看门狗丢掉的那几个高度，以及丢掉时已经拖了多久 / 转了几个视图。
    dropped_heights: List[Tuple[int, float, int]] = field(default_factory=list)
    # 这份日志属于哪一轮。取 symlink 解开后的文件名（geth-<RUN_TS>.log）。
    # 为什么必须有：start-all.sh 现在是「每轮一个文件名 + geth.log 做软链」，
    # 一旦有两轮 start-all 并发跑（或某台的软链没被更新），六台的 geth.log
    # 就会指向不同轮次。此时把它们的时间轴合并，等于把两段历史焊在一起 ——
    # 实测见过：五台指向 00:16:39 那轮、node2 还指着 23:55:58 那轮，
    # 合并后凭空造出「末段 95 秒零推进」，G3 报了一个不存在的停滞。
    run_tag: str = ""
    # 「恢复次数」= 带 force: true 的 viewStartTime 行数。
    # force=true 只有三个调用点，全部在区块 commit 之后（dispatcher.go:348 FinishedProposal /
    # :410 ResetConsensus / :444 epoch transition），所以这一行的 anchor 参数就是
    # 「刚 commit 那个块的 header.Time」，这条计数约等于「成功出块并重锚了几次」。
    # force=false 只有一个来源：consensusview.go:286 的超时推进（TryChangeView），
    # 也就是「这一轮没人收齐票，白转了一格」。
    # 两个数放在一起看才有效：force_true 涨 = 链在推进；只有 force_false 涨 = 空转。
    force_true: int = 0


# 一次 IPC 往返取全部需要的字段。majoritycount 放在最后：老版本 pbft.status()
# 没有这个字段时会返回 undefined，解析时降级为「按 2N/3 推算」。
META_EXPR = (
    'eth.blockNumber + "/" + pbft.status().viewoffset + "/"'
    ' + pbft.status().finishedheight + "/" + pbft.status().headheight'
    ' + "/" + pbft.status().majoritycount'
)


def _unquote(s: str) -> str:
    s = s.strip()
    if len(s) >= 2 and s[0] == '"' and s[-1] == '"':
        s = s[1:-1]
        s = s.replace('\\"', '"').replace("\\n", "\n")
    return s


def ipc_exec(geth: str, ipc: str, expr: str, timeout: float) -> Tuple[Optional[str], str]:
    """执行 geth attach --exec，返回 (结果, 错误)。只读操作。"""
    if not geth:
        return None, "找不到 geth 可执行文件（用 --geth 指定）"
    if not os.path.exists(ipc):
        return None, f"IPC 不存在: {ipc}"
    try:
        cp = subprocess.run(
            [geth, "attach", "--exec", expr, ipc],
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        return None, f"IPC 超时 >{timeout}s"
    except Exception as exc:  # noqa: BLE001 - 任何异常都当作采集失败
        return None, f"IPC 调用异常: {exc}"

    out = (cp.stdout or "").strip()
    err = (cp.stderr or "").strip()
    if not out:
        return None, err or f"无输出 (rc={cp.returncode})"
    if out.lower().startswith("fatal"):
        return None, out
    return _unquote(out), ""


def rpc_block_number(port: int, timeout: float) -> Tuple[Optional[int], str]:
    payload = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "eth_blockNumber", "params": []}).encode()
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}", data=payload, headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = json.loads(resp.read().decode())
        return int(body["result"], 16), ""
    except Exception as exc:  # noqa: BLE001
        return None, f"RPC 失败: {exc}"


def snapshot(n: NodeSpec, timeout: float, logs_only: bool) -> NodeSnapshot:
    snap = NodeSnapshot(name=n.name)
    if not logs_only:
        raw, err = ipc_exec(n.geth, n.ipc, META_EXPR, timeout)
        if raw is None:
            snap.error = err
        else:
            parts = [p.strip() for p in raw.split("/")]
            core = parts[:4]
            if len(parts) >= 4 and all(p.isdigit() for p in core):
                snap.block, snap.vo, snap.fin, snap.head = (int(p) for p in core)
                snap.ok = True
                # 第 5 段是 majoritycount，老版本可能是 "undefined"
                if len(parts) >= 5 and parts[4].isdigit():
                    snap.majority = int(parts[4])
            else:
                snap.error = f"无法解析 IPC 输出: {raw!r}"
    if n.rpc_port:
        blk, rerr = rpc_block_number(n.rpc_port, timeout)
        snap.rpc_block = blk
        if rerr:
            snap.rpc_error = rerr
    return snap


def scan_log(n: NodeSpec, anchor_min_secs: float) -> LogStats:
    st = LogStats(name=n.name, path=n.log_path)
    if not n.log_path or not os.path.exists(n.log_path):
        st.error = f"日志不存在: {n.log_path or '(未解析出路径)'}"
        return st
    st.exists = True
    try:
        st.run_tag = os.path.basename(os.path.realpath(n.log_path))
    except OSError:
        st.run_tag = os.path.basename(n.log_path)
    prev_adj: Optional[float] = None
    try:
        with open(n.log_path, "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                # ---- 先把这一行的时间戳算出来（带跨零点偏移）----
                # 所有用到「当日秒」的地方都必须用 cur，不许各自再算一遍。
                # 为什么：一旦日志跑过 00:00，23:59:59 = 86399 会直接跳到 00:00:01 = 1，
                # 差 -86398s。我第一版就没处理，实测后果是：
                #   出块间隔「最小值 = -86382.97s」、时间轴首尾显示成「h251..h250」、
                #   末高度「首现距今 86394.8s」—— 整条 G3 判据全废，而且不报错，静默乱说。
                lm = RE_LINE_TS.search(line)
                if lm:
                    cur = _abs_secs(lm.group("ldate"), lm.group("ltime"))
                    prev_adj = cur
                    # 「最后一条」而不是「最大一条」。原写法用 `>=` 取最大值，
                    # 跨零点后最大值会永远钉在 86399，末行时间就再也不更新了。
                    st.last_line_secs = cur
                else:
                    cur = prev_adj

                low = line.lower()
                if NEEDLE_DIFF_VIEW in low:
                    st.diff_view += 1
                if NEEDLE_DUP_VOTE in low:
                    st.dup_vote += 1
                if NEEDLE_REPLAY in low:
                    st.replays += 1
                if NEEDLE_CONFIRM_NO_BLOCK in low:
                    st.confirm_no_block += 1
                    m = RE_STUCK_HEIGHT.search(line)
                    if m:
                        hh = int(m.group(1))
                        st.stuck_heights[hh] = st.stuck_heights.get(hh, 0) + 1
                if NEEDLE_PROPOSAL_REJECTED in low:
                    st.proposal_rejected += 1
                if NEEDLE_DROPPING_CONFIRM in low:
                    st.dropping_confirm += 1
                    md = RE_DROPPED.search(line)
                    if md:
                        st.dropped_heights.append(
                            (int(md.group(1)), float(md.group(2)), int(md.group(3)))
                        )

                if "acceptVotes count:" in line:
                    m = RE_ACCEPT_COUNT.search(line)
                    if m:
                        st.accepts.append(int(m.group(1)))

                if "[--AppendDposBlock--]" in line and cur is not None:
                    m = RE_APPEND.search(line)
                    if m:
                        h = int(m.group("h"))
                        if h not in st.first_height_at or cur < st.first_height_at[h]:
                            st.first_height_at[h] = cur
                        st.append_counts[h] = st.append_counts.get(h, 0) + 1
                        st.last_append_secs = cur
                        st.last_append_height = h

                if NEEDLE_IMPORT in low and cur is not None:
                    m = RE_IMPORT.search(line)
                    if m:
                        hh = int(m.group(1))
                        if hh not in st.commit_at or cur < st.commit_at[hh]:
                            st.commit_at[hh] = cur

                if "viewStartTime:" in line:
                    m = RE_VIEW.search(line)
                    if not m:
                        continue
                    try:
                        vstart = _to_dt(m.group("vdate"), m.group("vtime"))
                    except ValueError:
                        continue
                    vo = int(m.group("vo"))
                    vstart_epoch = int(vstart.timestamp())
                    # 锚点 = viewStartTime - viewOffset * 视图槽长
                    anchor = int((vstart - timedelta(seconds=vo * VIEW_SLOT_SEC)).timestamp())
                    duty_raw = m.groupdict().get("duty")
                    duty = int(duty_raw) if duty_raw is not None else None
                    if m.group("force") == "true":
                        st.force_true += 1
                    st.view_events.append((cur if cur is not None else 0.0, anchor, vstart_epoch, vo, duty))
    except Exception as exc:  # noqa: BLE001
        st.error = f"读日志失败: {exc}"
    # anchor_min_secs 仅用于后续窗口筛选，这里不裁剪，保留全量供诊断
    _ = anchor_min_secs
    return st


# ---------------------------------------------------------------------------
# 统计工具
# ---------------------------------------------------------------------------


def percentile(sorted_vals: Sequence[float], p: float) -> float:
    """线性插值分位数（与 numpy 默认一致）。sorted_vals 必须已升序、非空。"""
    if len(sorted_vals) == 1:
        return float(sorted_vals[0])
    k = (len(sorted_vals) - 1) * (p / 100.0)
    lo = int(k)
    hi = min(lo + 1, len(sorted_vals) - 1)
    frac = k - lo
    return float(sorted_vals[lo]) * (1 - frac) + float(sorted_vals[hi]) * frac


def fmt_dur(v: Optional[float]) -> str:
    if v is None:
        return "-"
    if v >= 120:
        return f"{v:.1f}s ({v/60:.1f}min)"
    return f"{v:.2f}s"


def anchor_label(epoch: int) -> str:
    """把锚点 epoch 秒渲染成 HH:MM:SS(当日秒)，便于和日志直接对读。

    日志里锚点长这样：viewStartTime 22:01:37 + viewOffset 0 -> 锚点 22:01:37(79297)。
    """
    dt = datetime.fromtimestamp(epoch)
    secs = dt.hour * 3600 + dt.minute * 60 + dt.second
    return f"{dt.strftime('%H:%M:%S')}({secs})"


# ---------------------------------------------------------------------------
# 门禁
# ---------------------------------------------------------------------------


class Gate:
    """一个门禁 / 一个断言宏的结果容器。

    status 取值：
        PASS / FAIL / SKIP —— 这三种参与放行判定，影响退出码。
        QUIET               —— 断言宏（ASSERT-QUIET）：照旧算、照旧打印，
                               main() 汇总时被排除在 fails/skips 之外。

    为什么要区分 quiet：见文件头「门的编制」。G1/G4/G6 探的是「脏」不是「死」，
    给它们否决权会让绿灯稀释红灯；但它们的诊断信息仍然有用，所以不能删，只能降级。
    """

    def __init__(self, key: str, title: str, quiet: bool = False):
        self.key = key
        self.title = title
        self.quiet = quiet
        self.status = "SKIP"
        # 断言宏降级前的原判定，用来在报告里照实显示「它自己是怎么看的」
        self.declared_status = "SKIP"
        self.details: List[str] = []

    def set(self, status: str, *details: str) -> None:
        self.status = status
        self.declared_status = status
        # 丢掉空串：调用点常有 `"xxx: " + ... if missing else ""` 这种写法，
        # 不滤掉就会在报告里留一排空行，反而看不清真正的判据。
        self.details.extend(d for d in details if d)

    def as_quiet(self) -> "Gate":
        """降级为断言宏：保留 details 和 declared_status，把 status 改成 QUIET。"""
        self.quiet = True
        self.status = "QUIET"
        return self


def gate_view_unison(
    samples: List[Dict[str, Optional[int]]], names: List[str], interval: float, quorum: int
) -> Gate:
    """视图一致门禁（G1）。

    两条判据，缺一不可：

    G1a「同一时刻六台 viewoffset 相差 <= 1」
        不是「完全相等」。理由：changeViewLoop 的 ticker 是每个节点各起一个
        （view.go:51/80），各节点跨过视图边界的瞬间天然差最多一个 tic 相位
        （实测本链 786ms）。任意采样瞬间相差 1 是正常的，要求完全相等会稳定误报。
    但要明说这条的盲区：2026-10-31 22:01:37 那一刻实测差恰好就是 1，链随后停了
    605 秒。所以 **G1a 单独不足以判定健康**，它不是主判据。

    G1b「不许只涨不回」（这条才是 G1 的牙齿）
        活锁的现场特征：没人能收齐票 -> 每次 tick 都只是 VO+1，谁也不 force 重锚。
        死链末段六台 VO = 23/23/22/23/22/22，就是连续 22 次 view 超时堆出来的；
        而健康链每次成功出块都会 force 重锚把 VO 归 0，所以健康链的 VO 一定会「回落」。
        判据：采样期间若某台 VO 从未回落、且首末差 >= 2，判为「只涨不回」；
              「只涨不回」的台数 >= quorum -> FAIL。
        采样跨度必须 >= 12s（2 个视图槽）才有判别力，不足时打印提示不做判定。

    实测（本条为 2026-10-31 事故复盘后新增）：死链 22:11:43 -> 22:14:05 共 142s，
    VO 从 0 一路走到 23、期间无一次成功出块（22:11:37 那次之后），正是 G1b 要抓的形状。
    """
    g = Gate("G1", "视图一致（同轮 viewoffset 差 <= 1，且不许只涨不回）")
    if not samples:
        g.set("SKIP", "无 IPC 采样（--logs-only？）")
        return g

    ok_rounds = 0
    exact_rounds = 0
    per_round = []
    worst = 0
    for row in samples:
        vals = [row.get(n) for n in names]
        if any(v is None for v in vals):
            per_round.append(None)
            continue
        spread = max(vals) - min(vals)
        worst = max(worst, spread)
        if spread == 0:
            exact_rounds += 1
        if spread <= 1:
            ok_rounds += 1
        per_round.append(vals)

    series = " ".join(
        "[" + ",".join("-" if v is None else str(v) for v in vals) + "]" if vals else "[采集失败]"
        for vals in per_round
    )
    g.details.append(f"采样 {len(samples)} 次（间隔 {interval:g}s），每次的 viewoffset 序列：")
    g.details.append("  " + series)

    # ---- G1b：只涨不回 ----
    span = (len(samples) - 1) * interval
    climbers: List[str] = []
    for n in names:
        col = [row.get(n) for row in samples]
        col_live = [v for v in col if v is not None]
        if len(col_live) < 2:
            continue
        fell = any(b < a for a, b in zip(col_live, col_live[1:]))
        delta = col_live[-1] - col_live[0]
        if not fell and delta >= 2:
            climbers.append(f"{n}: {col_live[0]} -> {col_live[-1]}（{len(col_live)} 次采样无一次回落）")
    if span < 2 * VIEW_SLOT_SEC:
        g.details.append(
            f"  采样跨度 {span:g}s < {2 * VIEW_SLOT_SEC}s（2 个视图槽），不足以判定「只涨不回」，跳过该判据"
        )
    elif climbers:
        g.details.append(f"  「只涨不回」{len(climbers)} 台：")
        g.details.extend("    " + c for c in climbers)
    else:
        g.details.append(f"  「只涨不回」0 台（跨度 {span:g}s）")

    g.details.append(
        f"相差 <=1 的轮次：{ok_rounds}/{len(samples)}"
        f"（其中完全相等 {exact_rounds} 轮）；最大相差 {worst}"
    )

    problems = []
    if ok_rounds != len(samples):
        offenders = []
        for idx, vals in enumerate(per_round, 1):
            if vals and (max(vals) - min(vals)) > 1:
                offenders.append(f"第{idx}次 spread={max(vals)-min(vals)} ({min(vals)}~{max(vals)})")
        problems.append(f"仅 {ok_rounds}/{len(samples)} 轮相差 <= 1：" + "; ".join(offenders[:6]))
    # 阈值取 quorum 而不是 1：只有「够 quorum 台一起只涨不回」才说明这一轮没人能收齐票。
    # 单台掉队（健康 devnet 的 node3 就是 VO=3 的掉队者）不影响出块，不该判死。
    if span >= 2 * VIEW_SLOT_SEC and len(climbers) >= quorum:
        problems.append(
            f"{len(climbers)}/{len(names)} 台 viewoffset 只涨不回（>= quorum {quorum}，活锁形状）："
            + "; ".join(climbers[:4])
        )

    if problems:
        g.set("FAIL", *problems)
    elif span < 2 * VIEW_SLOT_SEC:
        g.set(
            "PASS",
            f"{len(samples)}/{len(samples)} 轮相差 <= 1；采样跨度不足 {2 * VIEW_SLOT_SEC}s，"
            "「只涨不回」未判（建议 --interval 2 或更大）",
        )
    else:
        g.set("PASS", f"{len(samples)}/{len(samples)} 轮相差 <= 1，且无活锁爬升（跨度 {span:g}s）")
    return g


def gate_finality(snaps: Dict[str, NodeSnapshot], names: List[str]) -> Gate:
    """finality 追平门禁。

    判据是 head - finishedheight <= 1，不是完全相等。理由（2026-10-31 复核后修正）：
        FinishedProposal 只在「本节点有 confirm 且 sealHash 匹配」时推进
        （engine.go:447-452 的 confirm 门禁，本轮明确不动）。
        所以刚出的那个块，其 confirm 还在路上，head-fin == 1 是设计内的正常态；
        **>= 2 才是异常**（节点落后两个块，说明它根本没在推进）。
    实测依据：22:03:43 六台 head-fin = 1,0,0,0,0,0；
              22:13:16 六台 head-fin = 1,2,1,0,1,1 —— node1 落后 2 且持续了 10 分钟。
    注意：重启/追块阶段 head-fin 会短暂变大，请等链预热后再跑（见 --warmup）。
    另外单次抓拍也会假阳性（head 刚推进、confirm 还在路上），所以 main() 里对
    head-fin > 1 的台会隔 3s 复核一次，只有复核仍 > 1 才判 FAIL。实测依据：
    2026-11-01 00:10 一次抓拍 node1 head-fin=2，连查 12 次全程 <= 1。
    """
    g = Gate("G2", "finality 追平（head - finishedheight <= 1，且 blockNumber == headheight）")
    live = [n for n in names if snaps[n].ok]
    if not live:
        g.set("SKIP", "无可用 IPC 读数（--logs-only？）")
        return g

    rows = []
    bad: List[str] = []       # 确认的逻辑违规 -> FAIL
    missing: List[str] = []   # 采集失败的台 -> SKIP（判不了，不等于通过）
    warming: List[str] = []   # finishedheight=0 的台 -> SKIP（刚重启，还没重建）
    for n in names:
        s = snaps[n]
        if not s.ok:
            # 采集失败**不是** finality 的违规。以前这里往 bad 里塞，结果是
            # 「进程被停掉 / IPC 路径写错」会被报成「finality 追平失败」，
            # 归因完全错，开发会去查一个不存在的共识 bug。现在归 SKIP。
            missing.append(n)
            rows.append(f"  {n:8s} <采集失败：{s.error}>")
            continue
        # finishedheight=0 而 head>0：这不是「落后 349 个块」，是刚重启还没重建 finality。
        # 实测（2026-11-01 00:17，重启后 46s）：三台 fin=0/head=349，
        # 按原判据会打出一排「落后 349 个块」把真正的问题盖住。
        if s.fin == 0 and (s.head or 0) > 0:
            warming.append(n)
            rows.append(f"  {n:8s} fin=0 head={s.head} block={s.block} <刚重启，finishedheight 尚未重建>")
            continue
        blk = s.block if s.block is not None else s.rpc_block
        lag = (s.head - s.fin) if (s.head is not None and s.fin is not None) else None
        tail = ""
        # RPC 与 IPC 是两次独立调用，中间可能刚好出一个块，差 1 是采样竞态不是 bug。
        # 所以这里只当诊断打印，不判 FAIL。（这一条原来写成了判据，在活链上假报过
        # 「RPC block=306 与 IPC block=305 不一致」。）
        if s.rpc_block is not None and s.block is not None and s.rpc_block != s.block:
            tail += f"  [诊断] RPC block={s.rpc_block} / IPC block={s.block}（两次调用之间的出块竞态，不判错）"
        # blockNumber 与 headheight 都取自同一次 IPC 求值，这个不一致才是真异常。
        if s.block is not None and s.head is not None and s.block != s.head:
            bad.append(f"{n}: IPC blockNumber={s.block} != headheight={s.head}")
        if lag is None:
            bad.append(f"{n}: 缺少 head/fin")
        elif lag > 1:
            bad.append(f"{n}: head-fin={lag} (>1，落后 {lag} 个块)")
        elif lag < 0:
            bad.append(f"{n}: head-fin={lag} (<0，finished 超过 head，状态异常)")
        if s.recheck:
            tail += f"  [复核] {s.recheck}"
        rows.append(f"  {n:8s} fin={s.fin} head={s.head} block={blk} head-fin={lag} vo={s.vo}{tail}")
    g.details.extend(rows)
    if bad:
        g.set("FAIL", f"{len(bad)} 处异常", *bad[:8])
    elif missing or warming:
        why = []
        if warming:
            why.append(
                f"{len(warming)} 台 finishedheight=0（{', '.join(warming)}）—— 刚重启，"
                "finality 还没重建，此时 head-fin 巨大但没意义"
            )
        if missing:
            why.append(f"{len(missing)} 台 IPC 采集失败（{', '.join(missing)}）")
        g.set(
            "SKIP",
            "；".join(why) + "。已能判的台均正常，但缺信息就不能算追平 —— 等预热完（--warmup）再跑",
        )
    else:
        g.set("PASS", f"{len(names)} 台 head-fin 均 <= 1，blockNumber == headheight")
    return g


def _merged_heights(stats: Dict[str, LogStats], names: List[str]) -> List[Tuple[int, float]]:
    """把全部节点的「高度 -> 首次出现时间」合并成一条时间轴，按时间升序。

    为什么取「最早」而不是「平均 / 最大」：`[--AppendDposBlock--]` 是「块进池子」
    （dpos/blockpool.go:161），六台各自往自己池子里打一次，谁先收到谁先打。
    「最早」= 全网第一次看见这个块的时刻，这才是「链推进到 h」的时刻。
    取平均会被最慢的那台拖后，把间隔算小；取最大更糟，会把六台的抖动全算进去。
    """
    merged: Dict[int, float] = {}
    for n in names:
        for h, t in stats[n].first_height_at.items():
            if h not in merged or t < merged[h]:
                merged[h] = t
    return sorted(merged.items(), key=lambda kv: kv[1])


def _stall_segments(
    points: List[Tuple[int, float]], w0: float, w1: float
) -> Tuple[List[Tuple[float, str]], List[Tuple[int, float]]]:
    """在窗口 [w0, w1] 内切出全部「零推进段」，返回 (段列表, 窗口内的块)。

    三段都要算，少算任何一段都会放水：
      * 前导段 w0 -> 窗口内第一个块：窗口右对齐截断，这一段可能是「停滞被切在中间」，
        但长度一样是真实的零推进时间，能量就量。
      * 内部段：相邻两块的时间差。
      * 尾段 最后一块 -> w1：这一段就是「此刻已经卡了多久」。
    特例：窗口内**一个块都没有**时，整段 300s 就是一段零推进 —— 这正是 h158 事故
    最后 6.5 分钟的形状，绝不能因为「没有相邻点可比」就返回空、然后就 Pass 掉。
    """
    inside = [(h, t) for h, t in points if w0 <= t <= w1]
    if not inside:
        return [(w1 - w0, "整个窗口内没有任何块")], inside
    segs: List[Tuple[float, str]] = [(inside[0][1] - w0, f"窗口起点 -> h{inside[0][0]}")]
    for (h1, t1), (h2, t2) in zip(inside, inside[1:]):
        segs.append((t2 - t1, f"h{h1} -> h{h2}"))
    segs.append((w1 - inside[-1][1], f"h{inside[-1][0]} -> 此刻"))
    return segs, inside


def gate_stall(
    stats: Dict[str, LogStats],
    names: List[str],
    window_sec: float = STALL_WINDOW_SEC,
    seg_sec: float = STALL_SEG_SEC,
    log_stale_sec: float = LOG_STALE_SEC,
    growth: Optional[int] = None,
) -> Gate:
    """出块停滞门禁（G3 / TOTAL-STALL）—— 全套门禁里**唯一**能抓「链已经死掉」的一条。

    判据（一版，就这一条）：
        取最近 window_sec（300s）作为判据窗口，把「高度 -> 全网首次出现时刻」
        拼成一条时间轴，切成若干「零推进段」（前导 / 相邻块间隔 / 尾段 + 空窗口特例）。
        **任一段 >= seg_sec（90s）即 FAIL。**

    为什么不是「末块距末行 <= 60s」（二版判据，已废弃）：
        * 二版只看尾巴，抓不到「中间卡了 6 分钟、刚恢复」——修完重启一跑就绿了，
          把一次真事故洗掉。fix3 的 h158 正是这种：卡 6.5 分钟后被停机，
          假如当时重启过再跑一次门禁，二版会直接 PASS。
        * 60s 相对设计 6s/块只有 10 倍余量，一次慢块 + 一次 GC 就可能误伤。

    为什么也不是「每台 acceptVotes count 必须到 quorum」（初版判据，已废弃）：
        方向是反的。死链六台的 count 最大都到过 4（放水），
        健康 devnet（4 producer，191 块）六台最大只有 2（误报）。
        原因：这行日志只在「票还没收齐」时才打印，收齐的那一票不写成 count=quorum。

    三个必须写死的前提：
        1. `[--AppendDposBlock--], height N` **不是「出了新块」**，是「块进池子」。
           卡住时它每轮重打同一个高度（死链 node0 h9 重打 172 次）。
           所以时间轴必须按 height 去重、只认首次时刻（见 _merged_heights）。
        2. 日志 mtime 必须新鲜，否则「没新块」和「进程已经死了」分不开。
           进程被 kill 后日志停止写入，最后一行时间戳会永远停在死亡那一刻，
           拿它算「停滞多久」会越算越小，甚至算出「0 秒前刚出块」—— 死链报绿。
           所以日志停更（> log_stale_sec）时**不许给 PASS**，只能 SKIP。
           注意 FAIL 优先于 SKIP：停更之前如果已经量出了 >=90s 的零推进段，
           那仍然是 FAIL（h158 归档复盘就靠这一条出红）。
        3. 时间轴用「当日秒」比较，跨零点会错。本链单轮 < 1 小时，暂不处理；
           真要跨零点跑请分段看。

    退出语义（很重要）：
        FAIL —— 窗口内确实有 >=90s 零推进，链出过或正在出严重问题。
        SKIP —— 判不了（窗口不够长 / 日志停更 / 一个高度都没有）。
                **SKIP 不是豁免**，退出码 2，和 FAIL 一样不放行。
        PASS —— 窗口够长、日志新鲜、最长零推进段 < 90s。只有这种情况才是真绿。

    实测锚点（改判据必须重新量这两组数）：
        * h158 归档（fix3，末 390s 零出块）：窗口内 0 个块 -> 最长段 300s -> FAIL。
        * fix5 健康跑（h159~h228，稳态 P50 10.01s，最慢单块 12.75s）：最长段 < 90s -> PASS。
    """
    g = Gate(
        "G3",
        f"出块停滞 TOTAL-STALL（最近 {window_sec:g}s 内任一段 >= {seg_sec:g}s 零推进即 FAIL）",
    )

    have = [n for n in names if stats[n].exists]
    missing = [n for n in names if not stats[n].exists]
    if not have:
        g.set(
            "SKIP",
            f"{len(names)} 台日志一个都读不到，无法重建出块时间轴 —— 这不是通过，是判不了",
            *[f"{n}: {stats[n].error}" for n in missing][:8],
        )
        return g
    tags: Dict[str, List[str]] = {}
    for n in have:
        tags.setdefault(stats[n].run_tag or "(未知)", []).append(n)
    if len(tags) > 1:
        # 这条是踩出来的：五台 geth.log 指向 00:16:39 那轮、node2 还指着 23:55:58 那轮，
        # 合并时间轴后凭空造出「末段 95 秒零推进」，G3 报了一个根本不存在的停滞。
        # 混轮的时间轴不是「稍微不准」，是能凭空造事故，所以直接判 SKIP 不给判。
        g.set(
            "SKIP",
            "六台日志指向了**不同轮次**，时间轴会被焊在一起 —— 这种输入判不了，"
            "既不敢说健康也不敢说停滞。请用同一轮的文件重跑（--log 逐台指定 "
            "geth-<RUN_TS>.log，别用 geth.log 这个软链）。",
            *[f"{tag} <- {', '.join(ns)}" for tag, ns in sorted(tags.items())],
        )
        return g

    if missing:
        g.details.append(
            f"  注意：{len(missing)} 台无日志（{', '.join(missing)}），时间轴只由其余 "
            f"{len(have)} 台合成。出块是全网事件，少几台不影响判「链有没有在推进」，"
            "但会缩小观测面。"
        )

    # ---- 时间轴与窗口 ----
    points = _merged_heights(stats, have)
    w1s = [stats[n].last_line_secs for n in have if stats[n].last_line_secs is not None]
    if not points or not w1s:
        g.set("SKIP", "日志里没有「落块行 + 行时间戳」，无法重建时间轴")
        return g
    w1 = max(w1s)
    w0_all = points[0][1]
    w0 = max(w0_all, w1 - window_sec)
    segs, inside = _stall_segments(points, w0, w1)
    longest, longest_desc = max(segs, key=lambda kv: kv[0])

    # ---- 日志新鲜度：用 mtime，不用日志里最后一行的时间 ----
    now = time.time()
    mtimes = []
    for n in have:
        if stats[n].path:
            try:
                mtimes.append(os.path.getmtime(stats[n].path))
            except OSError:
                pass
    stale: Optional[float] = (now - max(mtimes)) if mtimes else None

    # ---- 打印：判据的全部输入都摆出来，让人能自己复算 ----
    g.details.append(
        f"  时间轴 h{points[0][0]}..h{points[-1][0]}，{len(points)} 个高度，"
        f"覆盖 {w1 - w0_all:.0f}s；判据窗口 = 最近 {w1 - w0:.0f}s"
        f"（窗口要求 {window_sec:g}s，实得 {len(inside)} 个块）"
    )
    notable = [(d, desc) for d, desc in segs if d >= 20]
    g.details.append(
        f"  零推进段共 {len(segs)} 段，最长 {longest:.1f}s（{longest_desc}），阈值 {seg_sec:g}s"
    )
    g.details.append(
        "  >= 20s 的段："
        + ("  ".join(f"{d:.0f}s[{desc}]" for d, desc in sorted(notable, reverse=True)[:6]) if notable else "无")
    )

    # ---- 每台的末高度 / 重打次数 / 死锁指纹（诊断，不单独判） ----
    for n in names:
        st = stats[n]
        if not st.exists:
            g.details.append(f"  {n:8s} <无日志>")
            continue
        if st.last_append_secs is None:
            g.details.append(f"  {n:8s} <日志里没有任何 [--AppendDposBlock--]>")
            continue
        h_last = st.last_append_height
        first_secs = st.first_height_at.get(h_last, st.last_append_secs)
        age = max(0.0, (st.last_line_secs or first_secs) - first_secs)
        reprint = st.append_counts.get(h_last, 1)
        fp = ""
        if st.confirm_no_block or st.proposal_rejected:
            fp = (
                f"  [死锁指纹] already-has-confirm={st.confirm_no_block}"
                f" proposal-rejected={st.proposal_rejected}"
            )
        if st.dropping_confirm:
            # 看门狗动手是「有人自救」的证据，单独列，别混进死锁指纹里当坏事。
            fp += (
                f"  [看门狗] 动手 {st.dropping_confirm} 次"
                + (
                    "（"
                    + "，".join(f"h{h} 拖 {held:.0f}s/{views} 视图" for h, held, views in st.dropped_heights[:3])
                    + "）"
                    if st.dropped_heights
                    else ""
                )
            )
        g.details.append(
            f"  {n:8s} 末高度 h{h_last} 首现距今 {age:.1f}s"
            f"（该高度 Append 行 {reprint} 条；健康就是 2/4/6，卡住时是 44/172 这种量级）"
            f"  票数 max={max(st.accepts) if st.accepts else '-'}"
            f"（quorum 参考值另见报告头）{fp}"
        )

    if stale is not None:
        g.details.append(
            f"  日志最新 mtime 距今 {stale:.0f}s（阈值 {log_stale_sec:g}s -> "
            + ("新鲜" if stale <= log_stale_sec else "已停更")
            + "）"
        )
    if growth is not None:
        g.details.append(
            f"  活链采样：采样跨度内全链最高块号变化 = {growth:+d}（仅记录，不参与判定 —— "
            "18s 的跨度太短，偶尔 +0 不代表卡死，真判据是上面那条 90s 段）"
        )
    # 看门狗单独报一行：它「没出现」既不是好事也不是坏事，要看当时的链在不在走。
    tot_d_all = sum(stats[n].dropping_confirm for n in have)
    g.details.append(
        f"  看门狗「dropping a confirm」全链合计 {tot_d_all} 次"
        + (
            "（0 = 这段日志里它一次都没动手。链在推进 -> 不需要它；链停了 -> 说明没接上，见下方归因）"
            if tot_d_all == 0
            else "（出现是好事：说明那条「confirm 到了块没到」的路有人自救；要追问的是为什么需要自救）"
        )
    )

    # ---- 判定：FAIL 优先于 SKIP ----
    if longest >= seg_sec:
        tot_c = sum(stats[n].confirm_no_block for n in have)
        tot_r = sum(stats[n].proposal_rejected for n in have)
        tot_d = sum(stats[n].dropping_confirm for n in have)
        hint = ""
        if tot_c or tot_r:
            worst_h: Dict[int, int] = {}
            for n in have:
                for h, c in stats[n].stuck_heights.items():
                    worst_h[h] = worst_h.get(h, 0) + c
            stuck = ",".join(
                f"h{k}x{v}" for k, v in sorted(worst_h.items(), key=lambda kv: -kv[1])[:3]
            )
            hint += (
                f"；归因：全链 {tot_c} 条「height already has a confirm, refusing to propose」"
                f" + {tot_r} 条「rejected by the producer set」"
                + (f"（集中在 {stuck}）" if stuck else "")
                + " —— 这是「confirm 已到、块没到」型死锁（2026-10-31 h158 事故同型），不是慢"
            )
        # 这一条是答应 kou-dou-ma-2 加的：停滞发生了、看门狗却一次没动手，
        # 那就不能把这段停滞说成「看门狗救回来了」。两种归因差得很远：
        #   dropping>0  -> 走到了自救分支，问题在「为什么需要自救」；
        #   dropping==0 -> 压根没走到自救分支，问题在「看门狗挂在哪条路径上」。
        if tot_d == 0:
            hint += (
                "；闸门疑似静音：全链「dropping a confirm」= 0，这段停滞期间看门狗一次都没动手"
                " —— 说明停滞不是被看门狗解开的（或看门狗不在这条代码路径上）。"
                "别把这次 FAIL 当成「看门狗生效」的证据。"
            )
        else:
            drops = sorted(
                ((h, held, views) for n in have for (h, held, views) in stats[n].dropped_heights),
                key=lambda x: x[1],
                reverse=True,
            )[:3]
            hint += (
                f"；看门狗动手 {tot_d} 次"
                + (
                    "，最久的几条："
                    + "，".join(f"h{h} 拖了 {held:.0f}s/{views} 个视图" for h, held, views in drops)
                    if drops
                    else ""
                )
                + "（它出现是好事：有人在自救。要问的是「为什么需要它」）"
            )
        g.set(
            "FAIL",
            f"出现 {longest:.0f}s 零推进段（{longest_desc}），阈值 {seg_sec:g}s{hint}",
        )
        return g

    if stale is not None and stale > log_stale_sec:
        g.set(
            "SKIP",
            f"窗口内最长零推进段 {longest:.0f}s 未达阈值，但日志已 {stale:.0f}s 未更新"
            f"（>{log_stale_sec:g}s）—— 进程可能已停止 / 被正常关掉 / 日志被截断。"
            "这种情况下「最后一段很短」是假象（时间轴在进程死亡那一刻就冻结了），"
            "既不能算停滞，也绝不能算健康。请在有活进程时重跑。",
        )
        return g

    if (w1 - w0) < window_sec:
        g.set(
            "SKIP",
            f"可用时间轴只有 {w1 - w0:.0f}s < 判据窗口 {window_sec:g}s。"
            f"最长零推进段 {longest:.0f}s 虽未达 {seg_sec:g}s 阈值，但短窗口天然放水"
            f"（300s 至少要能容下 3 个 {seg_sec:g}s 的判据段，否则真实停滞会被窗口边界截断）。"
            "请等链跑满窗口后再跑，或把上几轮日志一并指进来。",
        )
        return g

    g.set(
        "PASS",
        f"最近 {w1 - w0:.0f}s 内最长零推进段 {longest:.1f}s < {seg_sec:g}s，"
        f"窗口内 {len(inside)} 个块、{len(segs)} 段全部达标；日志 mtime 新鲜",
    )
    return g


def gate_anchor(
    stats: Dict[str, LogStats], names: List[str], tail_n: int, fresh_sec: float, quorum: int
) -> Gate:
    """视图窗口一致门禁（G4，核心）。

    结论先行：判据落在「同一时刻各节点是否站在同一个视图窗口」，
    不是「锚点集合相同」，也不是「锚点交集非空」，更不是「锚点差 <= 1 个槽」。
    这四种写法我都实测过，前三种、第四种都被真实日志打回：

      * 「集合必须完全相等」—— 误报。健康台架 devnet 的 node3 全程只有 4 条
        view 行，天然是别人的子集（2026-10-31 20:46 实测）。
      * 「全历史交集非空」—— 放水。死链六台在 21:59 与 22:11:37 都共同锚定过，
        交集天然非空，于是那条 3:3 僵死 15 分钟、最后被 SIGKILL 的链会被放过去。
      * 「锚点差 <= 1 个槽 / 最新锚点完全相同」—— 方向是反的。
        健康 devnet 的末条锚点差是 **18s**（3 个槽），死链只有 **10s**。
        小的那个才死了，大的那个活着 —— 所以「锚点差」本身没有判别力。
        原因是 viewoffset 会天然补偿锚点的陈旧：viewStart = 锚点 + VO*6s。
        devnet 的 node3 锚点落后 18s、但 VO=3，viewStart 和别人一模一样。
      * 「锚点 mod 6 相同（同一网格）」—— 也不行。同一台节点一生中会跨多个网格
        （devnet node0 的锚点 mod 6 就有 {1,4} 两个值），不是不变式。

    所以真正该比的是 **viewStart**（每条 view 行打印的 viewStartTime）：
        * 死链末段：22:14:01(node0,1,3) vs 22:14:05(node2,4,5)，差 4s；
          末 20 条视图行的 viewStart 交集 = 空 -> FAIL
        * 健康 devnet 末段：20:46:19 被四台共同经历过，交集 = {20:46:19} -> PASS
    用「末 N 条的交集非空」而不是「末条必须相等」：末条相等会因 ticker 相位
    天然差一跳而误报（devnet 末条 spread 就是 6s，node2 超前一个槽）。

    N 的取法（实测扫描，N=3..60 逐个跑，见交付说明）：
        N <= 20 都能把两条链分开；N >= 30 时死链会因为 22:11:37 那次成功出块
        （六台当时确实对齐过）而变绿。故默认 N=20。
    另用 fresh_sec 硬卡新鲜度：共同视图必须落在「最新一条视图行之前 fresh_sec 秒内」，
    防止拿很久以前的旧对齐交差（死链 22:11:37 距被杀 148s，会被这一条挡掉）。

    附加诊断（打印出来，不单独判 FAIL）：
        末条锚点差、锚点 mod 6 分组。死链两派 22:11:43 与 22:11:53 差 10s，
        10 mod 6 = 4，两派 viewStart 永远错开 4s/2s，这就是「永不收敛」的算术原因。
        P0-A 修好后这里应看到锚点差为 0（或 6 的整数倍，即单纯落后几个槽）。
    """
    g = Gate(
        "G4",
        f"视图窗口一致（末 {tail_n} 条视图行内存在共同 viewStartTime，且不过期 {fresh_sec:g}s）",
    )

    present = [n for n in names if stats[n].view_events]
    missing = [n for n in names if not stats[n].view_events]
    if not present:
        g.set("SKIP", "日志里没有 viewStartTime 行（链可能还没进入视图切换）")
        return g

    newest = max(stats[n].view_events[-1][0] for n in present)

    vs_sets: Dict[str, set] = {}
    last_seen: Dict[int, float] = {}
    for n in names:
        evs = stats[n].view_events
        if not evs:
            g.details.append(f"  {n:8s} <无视图行/无日志>")
            continue
        tail = evs[-tail_n:]
        vs_sets[n] = {x[2] for x in tail}
        for s, _a, vs, _vo, _du in tail:
            if last_seen.get(vs) is None or s > last_seen[vs]:
                last_seen[vs] = s
        last = evs[-1]
        shown = sorted(vs_sets[n])[-3:]
        g.details.append(
            f"  {n:8s} 末{len(tail)}条覆盖 viewStartTime={[anchor_label(v) for v in shown]}"
            f"（共 {len(vs_sets[n])} 个）  末条@+{newest - last[0]:.0f}s "
            f"锚点={anchor_label(last[1])}(mod6={last[1] % VIEW_SLOT_SEC}) VO={last[3]}"
        )
    for n in missing:
        if stats[n].path and not stats[n].exists:
            g.details.append(f"  {n:8s} <{stats[n].error}>")

    if len(vs_sets) < quorum:
        g.set(
            "FAIL",
            f"只有 {len(vs_sets)} 台有视图行 < quorum {quorum}，无法判定视图窗口是否一致",
            "缺日志/无视图行: " + ", ".join(missing) if missing else "",
        )
        return g

    common = set.intersection(*vs_sets.values())
    fresh = {v for v in common if newest - last_seen.get(v, -1e9) <= fresh_sec}

    # 诊断：末条锚点差 + mod 6 分组
    last_anchor = {n: stats[n].view_events[-1][1] for n in vs_sets}
    spread = max(last_anchor.values()) - min(last_anchor.values())
    if spread:
        mods = sorted({a % VIEW_SLOT_SEC for a in last_anchor.values()})
        g.details.append(
            f"  诊断：末条锚点最大差 {spread}s"
            f"（= {spread // VIEW_SLOT_SEC} 个槽 + 余 {spread % VIEW_SLOT_SEC}s），"
            f"锚点 mod 6 = {mods}（{len(mods)} 组）"
            + (
                "  <锚点未统一，靠 viewoffset 补偿，P0-A 尚未生效>"
                if spread % VIEW_SLOT_SEC
                else "  <同网格，只是落后若干槽，属可容忍>"
            )
        )
    else:
        g.details.append("  诊断：末条锚点六台完全相同（零锚点差）")

    if fresh:
        detail = f"共同 viewStartTime={[anchor_label(v) for v in sorted(fresh)]}"
        if common - fresh:
            detail += f"；另有已过期的旧对齐 {[anchor_label(v) for v in sorted(common - fresh)]}"
        g.set("PASS", detail)
        return g

    if common:
        stale = sorted(common)
        age = newest - max(last_seen.get(v, 0) for v in stale)
        g.set(
            "FAIL",
            f"共同视图已过期：最后一次共同 viewStartTime={[anchor_label(v) for v in stale]}"
            f"发生在 {age:.0f}s 前（阈值 {fresh_sec:g}s）",
        )
        return g

    groups: Dict[int, List[str]] = {}
    for n, vss in vs_sets.items():
        key = max(vss)
        groups.setdefault(key, []).append(n)
    desc = " / ".join(
        "{" + anchor_label(k) + "} <- " + ",".join(sorted(v)) for k, v in sorted(groups.items())
    )
    g.set(
        "FAIL",
        f"末 {tail_n} 条内不存在共同 viewStartTime：{len(groups)} 组各自为战",
        desc,
    )
    return g


def gate_duty(
    stats: Dict[str, LogStats], names: List[str], producer_count: int, quorum: int,
    recent: int = 10,
) -> Gate:
    """当值一致 / 双当值检测（G6）。

    由来（2026-10-31 事故 + 修复后新链的实测）：
        值班人由 `GetNextOnDutyProducer(offset=viewOffset)` 决定，
        `dpos/producers.go:261-272`：
            index = (dutyIndex + viewOffset) % len(producers)
        所以「谁当值」不是只看 dutyIndex，而是 **dutyIndex + viewoffset**。
        viewoffset 一台一个（各节点 Running 时长不同），两个节点只要
        `(dutyIndex+vo) % N` 落到自己头上，就会**同时当值**、同时出块、
        各带一票互相拒收 —— 这比「vo 不相等」直接得多。

    实测依据（同一 6s 窗口内，各台自己算出的环位）：
        * 死链末段 10 个窗口：5 个窗口最大支持只有 3 台（< quorum 4），
          形状是 3:3 / {2:3, 1:3}，就是两派各认一个人当值。
          全日志 144 个窗口里 122 个不满足 quorum。
        * 健康 devnet：6 个窗口全部有 ≥3 台落在同一环位（quorum=3），0 违反。

    判据：最近 recent(10) 个「≥ quorum 台有视图行」的 6s 窗口里，
          不允许出现「最大环位支持 < quorum」的窗口。
          为什么不是「六台必须相同」：健康 devnet 的 node3 全程 duty=0、vo 爬到 6，
          环位一直和别人差 1，按「六台相同」会把它判死 —— 但它只是掉队者，
          其余 3 台正好够 quorum，链照样 6 秒一个块。
          **能出块的条件是「够 quorum 台认同一个当值人」，不是「全员一致」。**
    窗口不足 3 个时判 SKIP（判不了就是没过，退出码 2）。

    已知前提：本判据假设 `len(producers)` 等于本次运行的节点数（私有链全部是 producer）。
    接公链/混合集合时要改成读链上 producer 总数，否则环位算错。
    """
    g = Gate("G6", f"当值一致（最近 {recent} 个 6s 窗口内不出现「够 quorum 台认不出同一个当值人」）")

    buckets: Dict[int, Dict[str, int]] = {}
    no_duty: List[str] = []
    for n in names:
        evs = stats[n].view_events
        if not evs:
            continue
        has = False
        for secs, _a, _vs, vo, duty in evs:
            if duty is None:
                continue
            has = True
            b = int(secs // VIEW_SLOT_SEC)
            buckets.setdefault(b, {})[n] = (duty + vo) % producer_count
        if not has:
            no_duty.append(n)

    if no_duty:
        g.details.append(f"  无 dutyIndex 字段（跳过的台）: {', '.join(no_duty)}")
    windows = [(b, m) for b, m in sorted(buckets.items()) if len(m) >= quorum]
    if len(windows) < 3:
        g.set("SKIP", f"只有 {len(windows)} 个 6s 窗口里够 quorum 台有视图行，不足以判定双当值")
        return g

    def dist(m: Dict[str, int]) -> Tuple[Dict[int, int], int]:
        cnt: Dict[int, int] = {}
        for v in m.values():
            cnt[v] = cnt.get(v, 0) + 1
        return cnt, max(cnt.values())

    bad_all = [(b, m) for b, m in windows if dist(m)[1] < quorum]
    tail = windows[-recent:]
    bad_tail = [(b, m) for b, m in tail if dist(m)[1] < quorum]

    g.details.append(
        f"  全日志 {len(windows)} 个窗口里「最大环位支持 < quorum {quorum}」的有 {len(bad_all)} 个"
    )
    for b, m in tail[-6:]:
        cnt, top = dist(m)
        g.details.append(
            f"  @{int(b * VIEW_SLOT_SEC) % 86400 // 3600:02d}:{int(b * VIEW_SLOT_SEC) % 3600 // 60:02d}:"
            f"{int(b * VIEW_SLOT_SEC) % 60:02d} 环位分布={cnt} 最大支持={top}"
            + ("" if top >= quorum else "  <双当值：够 quorum 台认不出同一个当值人>")
        )

    if bad_tail:
        worst = max(bad_tail, key=lambda x: -dist(x[1])[1])
        cnt, top = dist(worst[1])
        g.set(
            "FAIL",
            f"最近 {len(tail)} 个窗口里有 {len(bad_tail)} 个出现双当值",
            f"最差窗口环位分布={cnt}（最大仅 {top} 台一致，需 {quorum}）",
            "成员: " + ", ".join(f"{k}->环位{v}" for k, v in sorted(worst[1].items())),
        )
    else:
        g.set("PASS", f"最近 {len(tail)} 个窗口均有 >= {quorum} 台认出同一个当值人")
    return g


def gate_replay(stats: Dict[str, LogStats], names: List[str]) -> Gate:
    g = Gate("G5", "replay 归零（replaying a proposal 行必须全为 0）")
    rows = []
    bad: List[str] = []       # 真的数出了重放
    missing: List[str] = []   # 没日志 -> 判不了，不是有重放
    for n in names:
        st = stats[n]
        if not st.exists:
            # 这里以前写的是 bad.append("无日志") + 报「存在重放」。
            # 「读不到日志」和「读到了重放」是两件事，混在一起会让人去查一个不存在的 bug。
            missing.append(n)
            rows.append(f"  {n:8s} <无日志，判不了>")
            continue
        rows.append(f"  {n:8s} replay={st.replays}")
        if st.replays != 0:
            bad.append(f"{n}: {st.replays} 行")
    g.details.extend(rows)
    if bad:
        g.set("FAIL", "存在重放", *bad[:8])
    elif missing:
        g.set(
            "SKIP",
            f"{len(missing)} 台没有日志（{', '.join(missing)}），"
            f"已读到的 {len(names) - len(missing)} 台 replay 全为 0 —— 但缺一台就不能算归零",
        )
    else:
        g.set("PASS", f"{len(names)} 台均为 0")
    return g


# ---------------------------------------------------------------------------
# 门禁自测（--selftest）
#
# 「夹具必须先能红」这条约定的执行体。每次改动本文件之后先跑它，
# 10 项全 PASS 才说明：解析还对、判据还能红、窗口不足时不会假装绿、
# 采集失败/刚重启/混轮不会假装红。项数会一直涨 —— 每踩一个新坑就补一项，
# 不补下次还会犯。真正的项数以输出里的 [n/N] 为准，这行只是给人扫一眼的概数。
#
# 为什么必须有这个子命令：门禁脚本最危险的失效方式不是崩，是**静默放水**。
# 改一个正则、调一个阈值，脚本不会报错，只会从此永远给 PASS —— 而没有人会察觉，
# 因为「全绿」看起来正是你想要的结果。
# ---------------------------------------------------------------------------


def _stamp(sec_of_day: float) -> str:
    """把「自 10-31 00:00 起的秒」渲染成日志行首时间戳，超过 86400 自动进到 11-01。"""
    s = int(sec_of_day)
    day, s = divmod(s, 86400)
    date = "10-31" if day == 0 else f"11-{day:02d}"
    return f"[{date}|{s // 3600:02d}:{s % 3600 // 60:02d}:{s % 60:02d}.000]"


def _block_lines(start: float, step: float, n: int, h0: int = 100) -> List[str]:
    out: List[str] = []
    t, h = start, h0
    for _ in range(n):
        out.append(f"INFO {_stamp(t)} [--AppendDposBlock--], height {h}")
        h += 1
        t += step
    return out


def _write_log(path: str, lines: List[str]) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")


def _stats_of(name: str, path: str) -> LogStats:
    return scan_log(NodeSpec(name=name, datadir="/nonexistent", log_path=path), 0.0)


def _run_g3(files: Dict[str, str]) -> Gate:
    return gate_stall({n: _stats_of(n, p) for n, p in files.items()}, list(files))


def run_selftest() -> int:
    tmp = tempfile.mkdtemp(prefix="verify-unison-selftest-")
    rows: List[Tuple[bool, str, str]] = []

    def add(ok: bool, name: str, note: str) -> None:
        rows.append((ok, name, note))

    try:
        # ---- 1. 解析：viewStartTime 行 ----
        view_line = (
            "INFO [10-31|22:00:07.543] viewStartTime: 2026-10-31 22:00:07 +0800 CST   "
            "changeViewTime 2026-10-31 22:00:13 +0800 CST nowTime: 2026-10-31 22:00:07.100 +0800 CST  "
            "offsetTime: 543ms force: false viewOffset 3 dutyIndex 0"
        )
        m = RE_VIEW.search(view_line)
        if not m:
            add(False, "正则 viewStartTime", "RE_VIEW 没匹配上真实格式的视图行")
        else:
            vo, duty = int(m.group("vo")), m.group("duty")
            vstart = _to_dt(m.group("vdate"), m.group("vtime")).timestamp()
            anchor = vstart - vo * VIEW_SLOT_SEC
            ok = vo == 3 and duty == "0" and int(anchor) % 1 == 0
            add(
                ok,
                "正则 viewStartTime",
                f"vo={vo} duty={duty} anchor=viewStart-vo*6s={anchor:.0f}（锚点公式依赖这一条）",
            )

        # ---- 2. 解析：AppendDposBlock 行 ----
        m2 = RE_APPEND.search("INFO [10-31|22:01:37.473] [--AppendDposBlock--], height 9")
        add(
            bool(m2) and m2.group("h") == "9",
            "正则 AppendDposBlock",
            "块进池子行 = 时间轴的数据源，解析不出来整个 G3 都是空的"
            if not m2
            else f"height={m2.group('h')}，时间戳 {m2.group('ltime')}",
        )

        # ---- 3. 健康夹具必须绿 ----
        p_ok = os.path.join(tmp, "healthy.log")
        lines = _block_lines(start=10 * 3600, step=10.0, n=61)  # 600s，每 10s 一块
        lines.append(f"INFO {_stamp(10 * 3600 + 605)} [--AppendDposBlock--], height 160")
        _write_log(p_ok, lines)
        g_ok = _run_g3({"node0": p_ok})
        add(g_ok.status == "PASS", "G3 健康夹具 -> PASS", f"判定={g_ok.status}；{g_ok.details[0].strip()}")

        # ---- 4. 死锁夹具必须红（这就是「夹具能红」的核心一项）----
        p_bad = os.path.join(tmp, "deadlock.log")
        lines = _block_lines(start=11 * 3600, step=10.0, n=31)  # 跑到 11:05:00
        # 之后 420 秒零推进，视图照转、提案照来、confirm 有块没有 —— h158 的形状
        t = 11 * 3600 + 300
        for k in range(42):
            lines.append(
                f"INFO {_stamp(t + k * 10)} PBFT: height already has a confirm, "
                "refusing to propose height=131"
            )
        _write_log(p_bad, lines)
        g_bad = _run_g3({"node0": p_bad})
        attr = "归因" in " ".join(g_bad.details)
        add(
            g_bad.status == "FAIL" and attr,
            "G3 死锁夹具 -> FAIL 且能归因",
            f"判定={g_bad.status}，归因行{'已' if attr else '未'}出现"
            f"（没有归因就只能说「卡了」，说不出「卡在哪条路」）",
        )

        # ---- 5. 窗口不足必须 SKIP，不许绿 ----
        p_short = os.path.join(tmp, "short.log")
        lines = _block_lines(start=12 * 3600, step=10.0, n=7)  # 只有 60s
        lines.append(f"INFO {_stamp(12 * 3600 + 65)} idle")
        _write_log(p_short, lines)
        g_short = _run_g3({"node0": p_short})
        add(
            g_short.status == "SKIP",
            "G3 短窗口 -> SKIP（不许绿）",
            f"判定={g_short.status}；60s 的时间轴不足以否定 90s 的停滞，给 PASS 就是放水",
        )

        # ---- 6. 跨零点时间轴不许倒挂 ----
        # 这一项是补的：2026-11-01 00:0x 在活链上实测，日志跨过 00:00 之后
        # 「出块间隔最小值」被算成 -86382.97s、时间轴首尾显示 h251..h250。
        # 根因是拿「当日秒」当时间轴，86399 直接跳到 1。
        # 这类 bug 不会让脚本崩，只会让它静默乱说 —— 必须固化成一项自测。
        p_mid = os.path.join(tmp, "midnight.log")
        _write_log(p_mid, _block_lines(start=23 * 3600 + 59 * 60, step=10.0, n=13, h0=300))
        st_mid = _stats_of("node0", p_mid)
        pts = _merged_heights({"node0": st_mid}, ["node0"])
        ds = [b[1] - a[1] for a, b in zip(pts, pts[1:])]
        hs = [h for h, _ in pts]
        add(
            bool(ds) and all(abs(d - 10.0) < 0.01 for d in ds) and hs == sorted(hs),
            "跨零点时间轴不倒挂",
            (
                f"{len(pts)} 个高度、{len(ds)} 个间隔，最小 {min(ds):.1f}s、最大 {max(ds):.1f}s"
                f"（倒挂时这里会是 -8 万多秒，实测过 -86382.97s）"
            )
            if ds
            else "一个高度都没解出来",
        )

        # ---- 7. 日志停更时必须 SKIP，绝不许绿 ----
        # 这是我在 fix5 上踩到的真缺陷：链被停掉之后，日志冻结，最后一行时间戳
        # 永远停在死亡那一刻，于是「末块距末行」越算越小，死链反而报绿。
        # 现在的判法：mtime 超过 --log-stale 就不许给 PASS。
        p_stale = os.path.join(tmp, "stale.log")
        _write_log(p_stale, _block_lines(start=13 * 3600, step=10.0, n=61, h0=200))
        old = time.time() - 600
        os.utime(p_stale, (old, old))
        g_stale = _run_g3({"node0": p_stale})
        add(
            g_stale.status == "SKIP",
            "日志停更 -> SKIP（「链死了」和「链被正常关掉」都不许绿）",
            f"判定={g_stale.status}；时间轴看起来一路 10s 一块，但 mtime 已 600s 未更新，"
            "那段「安静」不是健康，是没人写了",
        )

        # ---- 8. 混轮日志必须 SKIP，不许把两段历史焊在一起 ----
        # 实测事故：五台 geth.log 指向 00:16:39 那轮、node2 还指着 23:55:58 那轮，
        # 合并后凭空造出「末段 95 秒零推进」，G3 报了一个不存在的停滞。
        # 这里用「一台健康 + 一台中间有 200s 洞」来复现：焊起来会算出 200s 的假事故，
        # 正确行为是拒绝判（SKIP）。
        p_run_a = os.path.join(tmp, "geth-20260101-140000.log")
        p_run_b = os.path.join(tmp, "geth-20260101-140100.log")
        _write_log(p_run_a, _block_lines(start=14 * 3600, step=10.0, n=61, h0=400))
        _write_log(
            p_run_b,
            _block_lines(start=14 * 3600, step=10.0, n=31, h0=400)
            + _block_lines(start=14 * 3600 + 500, step=10.0, n=31, h0=430),
        )
        g_mix = _run_g3({"node0": p_run_a, "node1": p_run_b})
        add(
            g_mix.status == "SKIP",
            "混轮日志 -> SKIP（不许把两轮时间轴焊在一起）",
            f"判定={g_mix.status}；不做混轮检查时这两份会被当成同一条时间轴"
            "（真机事故里就这么凭空造出了 95s 零推进）",
        )

        # ---- 9. 刚重启不许报「落后 N 个块」----
        # 实测事故（2026-11-01 00:17，fix5 第二次重启后 46s）：三台 fin=0/head=349，
        # 按原判据打出一排「落后 349 个块」，把真正要看的东西全盖住了。
        # finishedheight 是重启后由 confirm 重建的，重建完之前 head-fin 没有意义。
        # 这一项直接喂 NodeSnapshot、不碰 IPC，所以不会给 selftest 增加等待时间。
        def _snap(n: str, fin: Optional[int], head: Optional[int],
                  blk: Optional[int] = None, ok: bool = True, err: str = "") -> NodeSnapshot:
            return NodeSnapshot(
                name=n, ok=ok, error=err, fin=fin, head=head,
                block=head if blk is None else blk,
            )

        g_warm = gate_finality(
            {"node0": _snap("node0", 0, 349), "node1": _snap("node1", 348, 349)},
            ["node0", "node1"],
        )
        add(
            g_warm.status == "SKIP",
            "刚重启（fin=0/head=349）-> SKIP，不许报「落后 349 个块」",
            f"判定={g_warm.status}；这是重启后 finality 还没重建，不是共识违规"
            f"（实测原判据在这里打出一排假落后，把真问题盖住了）",
        )

        # ---- 10. 真落后必须还能红（G2 的「夹具能红」项）----
        # 第 9 项把「fin 小」放宽成 SKIP，放宽过头就会变成永真。
        # 这一项保证：fin 明明有值、却稳定落后 9 个块时，G2 必须 FAIL。
        # 故意让 block == head（=349），这样 FAIL 的唯一来源就是 head-fin，不会混进
        # 「IPC blockNumber != headheight」那条并列判据，红得干净。
        g_lag = gate_finality(
            {"node0": _snap("node0", 340, 349), "node1": _snap("node1", 348, 349)},
            ["node0", "node1"],
        )
        add(
            g_lag.status == "FAIL",
            "真落后（fin=340/head=349）-> FAIL（放宽重启态之后仍能红）",
            f"判定={g_lag.status}；第 9 项放宽了「fin 小」，这一项证明放宽没把门禁变成永真",
        )

        # ---- 11. 停滞时「看门狗动没动手」必须能分开 ----
        # kou-dou-ma-2 的判据是「`dropping a confirm` 出现、且链继续走 = 修好了」。
        # 所以门禁必须能在 FAIL 里区分两件事：
        #   有 dropping  -> 走到了自救分支（问题在「为什么需要自救」）
        #   无 dropping  -> 压根没走到看门狗（问题在「看门狗挂在哪条路径上」）
        # 混成一句话，归因就会指错地方。
        wd_base = _block_lines(start=15 * 3600, step=10.0, n=31, h0=500)  # 到 15:05:00
        wd_stall = [
            f"INFO {_stamp(15 * 3600 + 300 + k * 10)} idle" for k in range(1, 43)
        ]
        p_silent = os.path.join(tmp, "wd-silent.log")
        _write_log(p_silent, wd_base + wd_stall)
        g_silent = _run_g3({"node0": p_silent})
        p_fired = os.path.join(tmp, "wd-fired.log")
        _write_log(
            p_fired,
            wd_base
            + wd_stall
            + [
                f'WARN {_stamp(15 * 3600 + 730)} PBFT: dropping a confirm whose block '
                'never arrived  height=531 held=30.999485122s views=5 reason="..."'
            ],
        )
        g_fired = _run_g3({"node0": p_fired})
        silent_txt = " ".join(g_silent.details)
        fired_txt = " ".join(g_fired.details)
        add(
            g_silent.status == "FAIL"
            and g_fired.status == "FAIL"
            and "闸门疑似静音" in silent_txt
            and "闸门疑似静音" not in fired_txt
            and "看门狗动手 1 次" in fired_txt
            and "h531 拖了 31s/5 个视图" in fired_txt,
            "停滞归因：看门狗 0 次 -> 「闸门疑似静音」；1 次 -> 报出拖了多久",
            f"silent={g_silent.status}/{'静音已标' if '闸门疑似静音' in silent_txt else '静音未标'}，"
            f"fired={g_fired.status}/{'已报出' if '看门狗动手 1 次' in fired_txt else '未报出'}"
            "（两种情况都 FAIL，但指向的代码路径不同）",
        )
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print("=" * 78)
    print("verify-unison.py --selftest   门禁自测（夹具能红才算数）")
    print("=" * 78)
    for i, (ok, name, note) in enumerate(rows, 1):
        print(f"[{'PASS' if ok else 'FAIL'}] {i}/{len(rows)} {name}")
        print(f"        {note}")
    bad_n = sum(1 for ok, _, _ in rows if not ok)
    print("-" * 78)
    if bad_n:
        print(f"结论：❌ 自测 {bad_n}/{len(rows)} 项不过 —— 这个版本的门禁不可信，别拿去验链")
        return 1
    print(f"结论：✅ {len(rows)}/{len(rows)} 项全过 —— 解析可用、判据能红、短窗口不放水")
    return 0


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------


def build_specs(args: argparse.Namespace) -> List[NodeSpec]:
    datadirs = [d.strip() for d in args.datadirs.split(",") if d.strip()]
    ports_raw = [p.strip() for p in (args.rpc or "").split(",") if p.strip()]
    names = [n.strip() for n in args.names.split(",")] if args.names else []
    if not datadirs:
        raise SystemExit("错误：--datadirs 不能为空")
    if names and len(names) != len(datadirs):
        raise SystemExit(f"错误：--names 有 {len(names)} 项，--datadirs 有 {len(datadirs)} 项")
    if ports_raw and len(ports_raw) != len(datadirs):
        raise SystemExit(f"错误：--rpc 有 {len(ports_raw)} 项，--datadirs 有 {len(datadirs)} 项")
    if not names:
        names = [f"node{i}" for i in range(len(datadirs))]

    log_overrides: Dict[str, str] = {}
    for item in args.log or []:
        if "=" not in item:
            raise SystemExit(f"错误：--log 需要 NAME=PATH 形式，收到 {item!r}")
        k, v = item.split("=", 1)
        log_overrides[k.strip()] = v.strip()

    specs: List[NodeSpec] = []
    for i, dd in enumerate(datadirs):
        dd = os.path.abspath(os.path.expanduser(dd))
        name = names[i]
        geth = args.geth or os.path.join(os.path.dirname(dd.rstrip("/")), "geth")
        if not os.path.exists(geth):
            found = shutil.which("geth")
            geth = found or ""

        ipc = args.ipc or os.path.join(dd, "geth.ipc")

        # 日志：优先级 --log 覆盖 > --log-dir > <datadir>/../logs/geth.log
        if name in log_overrides:
            log_path = os.path.abspath(os.path.expanduser(log_overrides[name]))
        elif args.log_dir:
            base = os.path.abspath(os.path.expanduser(args.log_dir))
            cands = [
                os.path.join(base, f"{name}-geth.log"),
                os.path.join(base, name, "logs", "geth.log"),
                os.path.join(base, f"{name}.log"),
            ]
            log_path = next((c for c in cands if os.path.exists(c)), cands[0])
        else:
            log_path = os.path.join(os.path.dirname(dd.rstrip("/")), "logs", "geth.log")

        specs.append(
            NodeSpec(
                name=name,
                datadir=dd,
                rpc_port=int(ports_raw[i]) if ports_raw else None,
                ipc=ipc,
                geth=geth,
                log_path=log_path,
            )
        )
    return specs


def main(argv: Sequence[str]) -> int:
    ap = argparse.ArgumentParser(
        description="PBFT 多节点视图一致 / finality 追平验收门禁（只读）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("--selftest", action="store_true",
                    help="只跑门禁自测（夹具必须能红），不连任何真实节点；改完本文件先跑它")
    ap.add_argument("--datadirs", help="逗号分隔的 datadir 列表（--selftest 时可省略）")
    ap.add_argument("--rpc", help="逗号分隔的 HTTP RPC 端口（与 --datadirs 一一对应，可省略）")
    ap.add_argument("--names", help="逗号分隔的节点名，默认 node0,node1,...")
    ap.add_argument("--geth", help="geth 可执行文件路径，默认 <datadir>/../geth")
    ap.add_argument("--ipc", help="统一覆盖 IPC 路径（单节点时方便）")
    ap.add_argument("--log-dir", help="从该目录找 <name>-geth.log / <name>/logs/geth.log")
    ap.add_argument("--log", action="append", metavar="NAME=PATH", help="逐台覆盖日志路径，可重复")
    ap.add_argument("--logs-only", action="store_true", help="只分析日志，跳过需要 IPC 的 G2")
    ap.add_argument("--warmup", type=float, default=60.0,
                    help="采样前先等待的秒数，默认 60（链刚启动时头 30s 还在刷 "
                         "`have different view offset`，30s 不够；实测需 60s 才安静下来）")
    ap.add_argument("--samples", type=int, default=10, help="采样次数，默认 10（只喂 ASSERT-QUIET）")
    ap.add_argument("--interval", type=float, default=2.0, help="采样间隔秒，默认 2.0（10 次=18s）")
    ap.add_argument("--anchor-tail", type=int, default=20, help="G4 每台回看的视图行数，默认 20")
    ap.add_argument("--stall-window", type=float, default=STALL_WINDOW_SEC,
                    help=f"G3 判据窗口秒数，默认 {STALL_WINDOW_SEC:g}"
                         "（低于这个长度 G3 只给 SKIP，不给 PASS）")
    ap.add_argument("--stall-seg", type=float, default=STALL_SEG_SEC,
                    help=f"G3 单段零推进的 FAIL 阈值秒数，默认 {STALL_SEG_SEC:g}")
    ap.add_argument("--log-stale", type=float, default=LOG_STALE_SEC,
                    help=f"日志 mtime 多久没更新就认为进程已停，默认 {LOG_STALE_SEC:g}")
    ap.add_argument("--anchor-window", type=float, default=60.0, help="G4 共同视图的保鲜期秒，默认 60")
    ap.add_argument("--timeout", type=float, default=15.0, help="单次 IPC/RPC 超时秒，默认 15")
    args = ap.parse_args(argv)

    if args.selftest:
        return run_selftest()
    if not args.datadirs:
        ap.error("--datadirs 必填（除非只跑 --selftest）")

    specs = build_specs(args)
    names = [s.name for s in specs]

    print("=" * 78)
    print("PBFT 多节点验收门禁  verify-unison.py   （只读：仅读日志 + 仅读 IPC/RPC）")
    print("=" * 78)
    for s in specs:
        print(f"  {s.name:8s} datadir={s.datadir}")
        print(f"  {'':8s} ipc={s.ipc}")
        real = ""
        if s.log_path and os.path.exists(s.log_path):
            try:
                rp = os.path.realpath(s.log_path)
                if rp != s.log_path:
                    real = f"   -> {os.path.basename(rp)}"
            except OSError:
                pass
        print(f"  {'':8s} log={s.log_path}" + ("" if os.path.exists(s.log_path) else "   <不存在>") + real)
    print()

    # ---- 采集 ----
    snaps: Dict[str, NodeSnapshot] = {}
    samples: List[Dict[str, Optional[int]]] = []

    block_series: List[Optional[int]] = []
    if not args.logs_only and args.warmup > 0:
        print(f"[warmup] 先等 {args.warmup:g}s 再采样（避开重启过渡态）...", flush=True)
        time.sleep(args.warmup)
    if not args.logs_only:
        for i in range(args.samples):
            row: Dict[str, Optional[int]] = {}
            blks: List[int] = []
            for s in specs:
                sn = snapshot(s, args.timeout, logs_only=False)
                snaps[s.name] = sn  # 最后一次覆盖，用于 G2
                row[s.name] = sn.vo if sn.ok else None
                if sn.ok and sn.block is not None:
                    blks.append(sn.block)
            samples.append(row)
            block_series.append(max(blks) if blks else None)
            if i < args.samples - 1:
                time.sleep(args.interval)
    else:
        for s in specs:
            snaps[s.name] = NodeSnapshot(name=s.name, error="--logs-only 跳过 IPC")

    # ---- G2 的单次抓拍会假阳性，对 head-fin > 1 的台复核一次 ----
    # 实测（2026-11-01 00:10）：一次抓拍看到 node1 fin=304/head=306（head-fin=2），
    # 紧接着连查 12 次全程 <= 1。原因是「head 刚推进、那个高度的 confirm 还在路上」
    # 这一瞬 —— 正是 G2 注释里说的设计内正常态的放大版。
    # 只有隔 3s 复核后仍然 > 1 才算「落后」，否则会把正常出块报成 finality 故障。
    if not args.logs_only:
        by_name = {s.name: s for s in specs}
        for n in names:
            s = snaps.get(n)
            if s is None or not s.ok or s.head is None or s.fin is None:
                continue
            if s.head - s.fin <= 1:
                continue
            # 复核最多 3 次、每次隔 3s。为什么不是 1 次：本链一个块 ~10s，
            # 「head 已经 +2、fin 还在等 confirm」可以持续好几秒才回落。
            # 实测 2026-11-01 00:36：四台同时 head-fin=2、隔 3s 复核仍是 2，
            # 但 90s 后逐台连查全部回落到 0。只复核一次会把这种正常突发判死。
            first = f"{s.fin}/{s.head}"
            for _ in range(3):
                time.sleep(3.0)
                s2 = snapshot(by_name[n], args.timeout, logs_only=False)
                if not s2.ok:
                    break
                s2.recheck = (
                    f"首次 head-fin={s.head - s.fin}（{first}）-> 复核 {s2.fin}/{s2.head}"
                    f"（head-fin={s2.head - s2.fin}）"
                )
                s = s2
                snaps[n] = s2
                if s2.head - s2.fin <= 1:
                    break
                first = f"{first}, 再等后 {s2.fin}/{s2.head}"

    stats: Dict[str, LogStats] = {s.name: scan_log(s, args.anchor_window) for s in specs}

    # 活链时才有的「采样跨度内块高增长」，供 G3 判断链是否真的在推进
    live_growth: Optional[int] = None
    _bs = [b for b in block_series if b is not None]
    if len(_bs) >= 2:
        live_growth = _bs[-1] - _bs[0]

    # majoritycount 优先用链上真值（pbft.status().majoritycount），取不到再按 floor(2N/3) 推算。
    # 实测：6 producer -> 4 票，4 producer -> 2 票（见 scripts/pbft-private/README.md 第 3.1 节）。
    n_nodes = len(specs)
    reported = [s.majority for s in snaps.values() if s.majority is not None]
    if reported:
        majority_est = max(set(reported), key=reported.count)
        majority_src = f"链上报值 majoritycount={majority_est}"
    else:
        majority_est = -(-2 * n_nodes // 3)  # ceil(2N/3)：6 台->4，4 台->3
        majority_src = f"按 floor(2N/3) 推算 = {majority_est}（N={n_nodes}）"

    # ---- 真门禁（参与放行判定）----
    # 只有这三条有否决权。G3 是唯一能抓「链已经死掉」的一条，见文件头「门的编制」。
    real_gates = [
        gate_finality(snaps, names),
        gate_stall(stats, names, args.stall_window, args.stall_seg, args.log_stale, live_growth),
        gate_replay(stats, names),
    ]
    # ---- 断言宏 ASSERT-QUIET（只算不判）----
    # 三条探的都是「脏」不是「死」：死锁时视图转得又稳又齐，它们照样给 PASS。
    # 给它们否决权只会用绿灯稀释红灯；但诊断信息有用，所以留着打印、不给票。
    quiet_gates = [
        gate_view_unison(samples, names, args.interval, majority_est).as_quiet(),
        gate_anchor(stats, names, args.anchor_tail, args.anchor_window, majority_est).as_quiet(),
        gate_duty(stats, names, n_nodes, majority_est).as_quiet(),
    ]
    gates = real_gates + quiet_gates

    # ---- 输出 ----
    print("-" * 78)
    print(f"真门禁（决定放行）：{', '.join(g.key for g in real_gates)}")
    print(f"断言宏（只记录，不参与放行）：{', '.join(g.key for g in quiet_gates)}")
    print("-" * 78)
    print(f"quorum 基准：{majority_src}")
    print()
    for g in gates:
        if g.quiet:
            print(f"[QUIET] {g.key} {g.title}")
            print(f"        ↳ 该断言自己的看法：{g.declared_status}（仅供参考，不影响退出码）")
        else:
            print(f"[{g.status:4s}] {g.key} {g.title}")
        for line in g.details:
            print(f"       {line}")
        print()

    # ---- 附带统计（不设门禁）----
    print("-" * 78)
    print("附带统计（不设门禁，仅记录）")
    print("-" * 78)
    print(f"  {'节点':10s} {'different-view-offset':>22s} {'alreadyExistVote':>17s}"
          f" {'confirm无块':>11s} {'提案被拒':>9s} {'看门狗':>7s} {'恢复次数':>9s} {'空转(force=false)':>17s}")
    for n in names:
        st = stats[n]
        if not st.exists:
            print(f"  {n:10s} {'<无日志>':>22s}")
            continue
        idle = len(st.view_events) - st.force_true
        print(f"  {n:10s} {st.diff_view:>22d} {st.dup_vote:>17d}"
              f" {st.confirm_no_block:>11d} {st.proposal_rejected:>9d}"
              f" {st.dropping_confirm:>7d}"
              f" {st.force_true:>9d} {idle:>17d}")
    _tot_c = sum(stats[n].confirm_no_block for n in names if stats[n].exists)
    _tot_r = sum(stats[n].proposal_rejected for n in names if stats[n].exists)
    _tot_d = sum(stats[n].dropping_confirm for n in names if stats[n].exists)
    if _tot_c or _tot_r:
        print(
            f"  提示：confirm无块={_tot_c} / 提案被拒={_tot_r} 非 0 —— 这是"
            "「confirm 已到、块没到」型死锁的指纹（2026-10-31 h158 事故同型）。"
            "注意：**不是「非 0 就是事故」**。实测 h158 是 44/21 且链永久停住；"
            f"健康跑里也可能是 1/1 且 33 秒自愈（见「看门狗」列——它动手了）。"
            "判严重程度看量级 + 链有没有继续走，别只看是不是 0。"
        )
    if _tot_d:
        print(
            f"  提示：看门狗动手 {_tot_d} 次（dropping a confirm）—— 这是「confirm 到了块没到」"
            "那条路上的自救分支被真的走到了。它出现**不等于**修好了："
            "要看它救完之后链是不是继续推进；而它不出现才要命（说明压根没走到这条路）。"
        )

    merged: Dict[int, float] = {}
    for n in names:
        for h, t in stats[n].first_height_at.items():
            if h not in merged or t < merged[h]:
                merged[h] = t
    heights = sorted(merged)
    deltas: List[float] = []
    for prev, cur in zip(heights, heights[1:]):
        deltas.append(merged[cur] - merged[prev])
    # 落地口径：同样合并六台、按 height 取最早 Imported 时刻。这个才是「真出块周期」。
    merged_commit: Dict[int, float] = {}
    for n in names:
        for h, t in stats[n].commit_at.items():
            if h not in merged_commit or t < merged_commit[h]:
                merged_commit[h] = t
    ch = sorted(merged_commit)
    cd: List[float] = [merged_commit[b] - merged_commit[a] for a, b in zip(ch, ch[1:])]
    if deltas:
        srt = sorted(deltas)
        print()
        print(f"  出块间隔 · 口径A「Append 首次出现」（块进池子，含 0/6s 双轮抖动）")
        print(f"    共 {len(deltas)} 个间隔，高度范围 h{heights[0]}..h{heights[-1]}")
        print(f"    P50 = {fmt_dur(percentile(srt, 50))}")
        print(f"    P95 = {fmt_dur(percentile(srt, 95))}")
        print(f"    最大 = {fmt_dur(srt[-1])}   最小 = {fmt_dur(srt[0])}")
        worst = sorted(zip(deltas, heights[1:]), reverse=True)[:3]
        print("    最慢 3 个间隔：" + ", ".join(f"h{h} +{d:.1f}s" for d, h in worst))
        if cd:
            cs = sorted(cd)
            print(f"  出块间隔 · 口径B「Imported new chain segment」（真落地，**报这个**）")
            print(f"    共 {len(cd)} 个间隔，高度范围 h{ch[0]}..h{ch[-1]}")
            print(f"    P50 = {fmt_dur(percentile(cs, 50))}")
            print(f"    P95 = {fmt_dur(percentile(cs, 95))}")
            print(f"    最大 = {fmt_dur(cs[-1])}   最小 = {fmt_dur(cs[0])}")
            cworst = sorted(zip(cd, ch[1:]), reverse=True)[:3]
            print("    最慢 3 个间隔：" + ", ".join(f"h{h} +{d:.1f}s" for d, h in cworst))
            print(
                "    说明：报口径B。口径A 的上尾被「同一高度两轮 Append 相差 6s」撑大"
                "（实测同一窗口 A 的 P95=15.47s，B 的 P95=12.45s），"
                "但 A 的 P50 可能反而更低，两者不是包含关系，别混着引用。"
            )
        else:
            print("  出块间隔 · 口径B：日志里没有 `Imported new chain segment` 行，无法给落地口径")
    else:
        print("\n  出块间隔：日志里没有 >=2 个高度的 AppendDposBlock 记录，无法计算")

    # ---- 「块进池 -> 落地」的滞后：这是 2026-11-01 01:0x 新加的诊断 ----
    # 为什么值得单独量：G3 只看「零推进段 >= 90s」，抓不到 16~33 秒的滞后。
    # 但实测 h424 就是这么卡了 33.2s：Append（块进池、confirm 也齐）-> 33.2s 后才
    # Imported 进链，中间 3 个视图白转，还把看门狗逼出来了。
    # 同一个度量在 fix5 的日志里也是 4 次 >10s（h304/h334/h343 各 16s、h283 10.1s），
    # 所以它不是插桩版引入的，是一只在的低概率长尾。
    lags: List[Tuple[int, float]] = []
    for h, t0 in merged.items():
        if h in merged_commit:
            lags.append((h, merged_commit[h] - t0))
    if lags:
        lv = sorted(v for _, v in lags)
        slow = sorted(lags, key=lambda kv: -kv[1])[:5]
        print()
        print("  块进池 -> 落地 滞后（Append 首次 -> Imported，全网最早口径）")
        print(f"    可比高度 {len(lags)} 个；P50 = {fmt_dur(percentile(lv, 50))}  "
              f"P95 = {fmt_dur(percentile(lv, 95))}  最大 = {fmt_dur(lv[-1])}")
        print("    最慢 5 个：" + ", ".join(f"h{h} +{v:.1f}s" for h, v in slow))
        over = [x for x in slow if x[1] >= 10.0]
        if over:
            print(
                f"    注意：{len(over)} 个高度 >= 10s。这个滞后期间「块已经在池子里、confirm 也齐」，"
                "但链头不前进 —— 它不是「慢」，是「有块不落链」。"
                "形态上会长得像 h158 死锁（会出现 already-has-confirm / 看门狗动手），"
                "但根因不同：要去查块为什么没被写进链，而不是查视图分叉。"
            )
        else:
            print("    全部 < 10s —— 没有出现「有块不落链」的长尾。")

    # ---- 结论 ----
    print()
    print("=" * 78)
    fails = [g for g in real_gates if g.status == "FAIL"]
    skips = [g for g in real_gates if g.status == "SKIP"]
    noisy = [g for g in quiet_gates if g.declared_status == "FAIL"]
    if fails:
        print(f"结论：❌ 不可交付 —— {len(fails)} 项 FAIL：" + ", ".join(g.key for g in fails))
        if skips:
            print(f"      另有 {len(skips)} 项 SKIP：" + ", ".join(g.key for g in skips))
        if noisy:
            print(f"      断言宏同时报脏：{', '.join(g.key for g in noisy)}（不参与放行，但值得看）")
        print("=" * 78)
        return 1
    if skips:
        print(f"结论：⚠️  判定不完整 —— {len(skips)} 项 SKIP：" + ", ".join(g.key for g in skips))
        print("      不能确认通过 ≠ 通过。请补齐条件（活进程、够长的日志窗口）后重跑。")
        print("=" * 78)
        return 2
    print("结论：✅ 三条真门禁全过 —— 出块在推进（无 >= "
          f"{args.stall_seg:g}s 零推进段）、finality 追平、无重放")
    if noisy:
        print(f"      但断言宏报脏：{', '.join(g.key for g in noisy)} —— "
              "不拦放行，请人工读一遍它说的是什么")
    print("=" * 78)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
