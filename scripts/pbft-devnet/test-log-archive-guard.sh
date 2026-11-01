#!/bin/bash
# test-log-archive-guard.sh
#
# 验 start-all.sh 里的 link_latest_log 在「归档(mv)失败」时不会销毁上一轮日志。
#
# 为什么要有这个脚本 —— 一个真实的教训：
#   最初的红/绿验证是「把 logs 目录 chmod 500 让 mv 失败」。但实测：
#       mv  rc=13  EACCES
#       ln  rc=1   Permission denied
#   **mv 失败的时候 ln -sfn 也失败**（同一个目录，权限要求一样），所以哪怕跑的是
#   改前那版 `mv ... && echo ...`，文件也活下来了 —— 那个夹具根本区分不了新旧代码。
#   它证明的是「目录不可写时谁都删不掉文件」，跟守卫有没有救回现场毫无关系。
#
#   唯一能造出「mv 失败、但 ln -sfn 仍能 unlink 真实文件」的办法，是把 mv 覆盖成
#   一个必失败的 shell 函数。真机上这种组合来自 EIO / ENAMETOOLONG / 被别的进程
#   抢先删掉，不能按需复现，只能用桩。
#
# 而且要「先证明夹具能红」：本脚本第 3 个用例会把函数里的 `return 1` 改成 `:`
# （等于回退成没有守卫），再跑同一套断言，**它必须失败**。如果不失败，
# 说明这个夹具是假的，第 2 个用例的通过毫无意义。
#
# 用法：
#   ./test-log-archive-guard.sh                       # 用默认路径
#   ./test-log-archive-guard.sh /path/to/start-all.sh
#
# 退出码：0 全过 / 1 有失败 / 2 跳过（找不到脚本或函数）

set -u

SRC="${1:-/Users/apple/Desktop/private/start-all.sh}"
FN="$(mktemp -t linkfn)"
MUT="$(mktemp -t linkfn-mut)"
TMPROOT="$(mktemp -d -t archive-guard)"
cleanup() { rm -rf "$TMPROOT" "$FN" "$MUT"; }
trap cleanup EXIT

pass=0
fail=0
ok()   { echo "[PASS] $*"; pass=$((pass+1)); }
bad()  { echo "[FAIL] $*"; fail=$((fail+1)); }

if [ ! -f "$SRC" ]; then
  echo "[SKIP] 找不到 $SRC"
  exit 2
fi

# 从真实脚本里抽出函数原文（不是手抄副本 —— 手抄的副本改了源码也不会跟着变）
awk '/^link_latest_log\(\) \{/,/^\}/' "$SRC" > "$FN"
if [ ! -s "$FN" ]; then
  echo "[SKIP] $SRC 里没找到 link_latest_log() 函数体"
  exit 2
fi

# 变异版本：把 return 1 换成 :，等于回退成「失败也不返回」的老行为
sed 's/^\([[:space:]]*\)return 1[[:space:]]*$/\1:/' "$FN" > "$MUT"
if cmp -s "$FN" "$MUT"; then
  echo "[SKIP] 变异没生效（函数里找不到独立的 return 1），无法证明夹具能红"
  exit 2
fi

# run_case <函数文件> <normal|mvfail> <用例目录>
# 打印：rc / geth.log 是软链还是真文件 / 内容
run_case() {
  local fnfile="$1" mode="$2" dir="$3"
  mkdir -p "$dir"
  printf 'PRECIOUS-EVIDENCE\n' > "$dir/geth.log"
  (
    export RUN_TS=20260101-000000
    # shellcheck disable=SC1090
    source "$fnfile"
    if [ "$mode" = "mvfail" ]; then
      mv() { echo "  (stub-mv) 模拟归档失败" >&2; return 1; }
    fi
    link_latest_log "$dir" "geth-20260101-000000.log" "geth.log"
    echo "__RC__=$?"
  ) > "$dir/.out" 2>&1
  RC=$(sed -n 's/^__RC__=//p' "$dir/.out")
}

echo "=================================================================="
echo "link_latest_log 归档守卫验证   源码: $SRC"
echo "=================================================================="

# ---- 用例 1：正常路径必须照旧工作 ----
d="$TMPROOT/normal"; run_case "$FN" normal "$d"
content=$(cat "$d/geth.log" 2>/dev/null)
arch=$(cat "$d"/geth-before-*.log 2>/dev/null)
if [ "$RC" = "0" ] && [ -L "$d/geth.log" ] && [ "$arch" = "PRECIOUS-EVIDENCE" ]; then
  ok "正常路径：归档成功、内容完好、别名指向本轮文件（rc=0）"
else
  bad "正常路径：rc=$RC 别名=$(readlink "$d/geth.log" 2>/dev/null || echo 非软链) 归档内容=[$arch]"
fi

# ---- 用例 2：mv 失败时不许销毁现场（这就是这条守卫的全部意义）----
d="$TMPROOT/mvfail"; run_case "$FN" mvfail "$d"
if [ "$RC" = "1" ] && [ ! -L "$d/geth.log" ] && [ -f "$d/geth.log" ] \
   && [ "$(cat "$d/geth.log")" = "PRECIOUS-EVIDENCE" ]; then
  ok "mv 失败：rc=1、geth.log 仍是真实文件、内容 PRECIOUS-EVIDENCE 完好"
else
  bad "mv 失败：rc=$RC 是软链=$([ -L "$d/geth.log" ] && echo 是 || echo 否) 内容=[$(cat "$d/geth.log" 2>/dev/null)]"
fi

# ---- 用例 3：夹具必须能红（变异成「没有守卫」，同一断言必须失败）----
d="$TMPROOT/mutant"; run_case "$MUT" mvfail "$d"
if [ -L "$d/geth.log" ] || [ "$(cat "$d/geth.log" 2>/dev/null)" != "PRECIOUS-EVIDENCE" ]; then
  ok "夹具能红：去掉守卫后现场被销毁（说明用例 2 的通过是有意义的）"
else
  bad "夹具不能红：去掉守卫后文件仍然完好 —— 说明用例 2 无论代码对不对都会绿，是假测试"
fi

echo "------------------------------------------------------------------"
if [ "$fail" -gt 0 ]; then
  echo "结论：❌ $fail 项失败 —— 归档守卫不可信"
  exit 1
fi
echo "结论：✅ $pass/$pass 全过 —— mv 失败不销毁现场，且夹具本身能红"
exit 0
