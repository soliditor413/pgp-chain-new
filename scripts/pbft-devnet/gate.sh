#!/bin/sh
# gate.sh <run-dir> [options] -- judge a finished devnet run.
#
# Reads a run directory produced by run.sh (logs/geth.log and
# status-before-stop.txt) and prints one verdict per gate. It never starts,
# stops or writes to a node.
#
# Exit status: 0 all gates pass, 1 at least one gate failed, 2 an input could
# not be read (treated as a failure, never as a pass).
#
# Every gate prints the command that produced its number, so a reader can rerun
# one line without trusting this script. See README.md, "gate.sh: judging a
# run", for why each gate exists and what it cannot see.

set -u

PROG=${0##*/}
EXPECT_RACE=0
BINARY=
EXPECT_INTERVAL=3

SEAL_PAT='Successfully seal and write new block'
FORK_PAT='FinishedProposal received fork block'
SHUTDOWN_PAT='Blockchain stopped'
EXTRA_BASE=143
EXTRA_PER_VOTE=132

usage() {
    cat <<EOF
usage: $PROG <run-dir> [options]

  <run-dir>            a devnet home holding logs/geth.log and
                       status-before-stop.txt (run.sh start / stop writes both)

  --binary PATH        also judge the binary that produced the run: print its
                       sha256, its vcs stamp when it has one, and whether it
                       carries the race detector
  --expect-race        with --binary: fail when that binary has no race
                       detector, i.e. when a "-race run" was not one
  --interval N         expected block period in seconds (default $EXPECT_INTERVAL)
  -h, --help           this text

exit: 0 pass, 1 a gate failed, 2 an input could not be read
EOF
}

while [ $# -gt 0 ]; do
    case $1 in
        --binary)      [ $# -ge 2 ] || { usage >&2; exit 2; }; BINARY=$2; shift 2 ;;
        --expect-race) EXPECT_RACE=1; shift ;;
        --interval)    [ $# -ge 2 ] || { usage >&2; exit 2; }; EXPECT_INTERVAL=$2; shift 2 ;;
        -h|--help)     usage; exit 0 ;;
        -*)
            printf '%s: unknown option %s\n' "$PROG" "$1" >&2
            usage >&2
            exit 2
            ;;
        *)
            if [ -n "${RUN_DIR:-}" ]; then
                printf '%s: extra argument %s\n' "$PROG" "$1" >&2
                exit 2
            fi
            RUN_DIR=$1
            shift
            ;;
    esac
done

[ -n "${RUN_DIR:-}" ] || { usage >&2; exit 2; }
[ -d "$RUN_DIR" ] || { printf 'CANNOT %s is not a directory\n' "$RUN_DIR" >&2; exit 2; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/pgp-gate.XXXXXX") || { printf 'CANNOT mktemp\n' >&2; exit 2; }
trap 'rm -rf "$TMP"' EXIT
trap 'rm -rf "$TMP"; exit 2' INT TERM

FAILED=0
PASSED=0
NA=0
GATE_TOTAL=25

# Every verdict is tagged with the section it came from, so the accounting at the end
# can be per section instead of a single total. A total alone can be balanced by two
# compensating errors -- one gate looping out an extra verdict while another drops one
# -- and the sum still looks right. That is the same shape of bug as the "42 blocks"
# report: every number correct, the set of numbers wrong.
: >"$TMP/slots.txt"

sec()  { printf '\n== %s\n' "$*"; SEC=$1; }
cmt()  { printf '   cmd   %s\n' "$*"; }
ok()   { printf '   PASS  %s\n' "$*"; PASSED=$((PASSED + 1)); _tag; }
na()   { printf '   n/a   %s\n' "$*"; NA=$((NA + 1));       _tag; }
bad()  { printf '   FAIL  %s\n' "$*"; FAILED=$((FAILED + 1)); _tag; }

# A verdict must never change its own exit status just because it was untagged; the
# accounting block below runs with SEC empty and its own bad() calls must still be
# ordinary successes to the shell.
_tag() { if [ -n "$SEC" ]; then printf '%s\n' "$SEC" >>"$TMP/slots.txt"; fi; return 0; }
SEC=""
info() { printf '   ..    %s\n' "$*"; }
cannot() { printf '   CANNOT %s\n' "$*" >&2; exit 2; }

# grep's own failure must never be read as "no match": -c exits 1 for no match
# and >1 for a real error, and only the second one is a broken gate.
#
# It returns 2 instead of exiting: it is always called as n=$(count ...), and an
# exit inside a command substitution only ends the subshell -- the caller would
# carry on with an empty value. Callers must write:  n=$(count ...) || cannot ...
count() {
    _p=$1; _f=$2; _l=$3
    if grep -cF -- "$_p" "$_f" >"$TMP/c" 2>"$TMP/e"; then
        :
    else
        _rc=$?
        [ "$_rc" -eq 1 ] || {
            printf 'grep exited %s while counting %s: %s\n' "$_rc" "$_l" "$(head -1 "$TMP/e")" >&2
            return 2
        }
        printf '0\n' >"$TMP/c"
    fi
    tr -d ' \n' <"$TMP/c"
}

# Guards a value that came out of a command substitution before it reaches a
# numeric test. An empty or non-numeric value here means the number was not
# read, which is a broken gate, not a zero.
need_num() {
    case ${2:-} in
        ''|*[!0-9]*) printf '   CANNOT %s is not a number: [%s]\n' "$1" "${2:-}" >&2; exit 2 ;;
    esac
}

# Fixed-string grep into a file; the caller then works on the file, so no
# verdict ever depends on the exit status of a pipeline.
keep() {
    if grep -F -- "$1" "$2" >"$3" 2>"$TMP/e"; then
        :
    else
        _rc=$?
        [ "$_rc" -eq 1 ] || cannot "grep exited $_rc while extracting $1"
        : >"$3"
    fi
}

LOG=$RUN_DIR/logs/geth.log
SNAP=$RUN_DIR/status-before-stop.txt

sec "G0 inputs"
cmt "test -s $LOG"
cmt "test -s $SNAP"
if [ -r "$LOG" ] && [ -s "$LOG" ]; then ok "log readable and non-empty"; else cannot "cannot read a non-empty $LOG"; fi
if [ -r "$SNAP" ] && [ -s "$SNAP" ]; then ok "chain-head snapshot readable and non-empty"; else cannot "cannot read a non-empty $SNAP"; fi
LOG_LINES=$(wc -l <"$LOG" | tr -d ' ')
need_num "log line count" "$LOG_LINES"
info "log lines: $LOG_LINES"
[ "$LOG_LINES" -gt 0 ] || cannot "log is empty"

sec "G1 this is a geth log and the run ended, not a fragment"
cmt "grep -cF 'INFO [' $LOG"
cmt "grep -cF '$SHUTDOWN_PAT' $LOG"
N_INFO=$(count 'INFO [' "$LOG" 'INFO lines') || cannot "could not count INFO lines"
need_num "INFO line count" "$N_INFO"
if [ "$N_INFO" -gt 0 ]; then ok "log carries $N_INFO geth log lines"; else bad "no 'INFO [' lines: this is not a geth log, or a different format"; fi
N_SHUT=$(count "$SHUTDOWN_PAT" "$LOG" 'shutdown marker') || cannot "could not count the shutdown marker"
need_num "shutdown marker count" "$N_SHUT"
if [ "$N_SHUT" -gt 0 ]; then ok "run ended: '$SHUTDOWN_PAT' seen"; else bad "no '$SHUTDOWN_PAT': the log is a fragment, the node was killed, or the file was truncated"; fi

sec "G2 seal accounting (the denominator)"
keep "$SEAL_PAT" "$LOG" "$TMP/seal.txt"
SEALED=$(wc -l <"$TMP/seal.txt" | tr -d ' ')
need_num "sealed block count" "$SEALED"
sed -n 's/.*number=\([0-9][0-9,]*\).*/\1/p' "$TMP/seal.txt" | tr -d ',' >"$TMP/seal_h.txt"
cmt "grep -F '$SEAL_PAT' $LOG | wc -l"
cmt "sed -n 's/.*number=\\([0-9][0-9,]*\\).*/\\1/p'  （取高度）"
info "sealed blocks: $SEALED"
if [ "$SEALED" -gt 0 ]; then ok "sealed $SEALED block(s)"; else bad "no sealed blocks: wrong literal, wrong file, or the run never sealed"; fi
if [ "$SEALED" -gt 0 ]; then
    FIRST=$(head -1 "$TMP/seal_h.txt")
    LAST=$(tail -1 "$TMP/seal_h.txt")
    cmt "sed -n ... | head -1   /   ... | tail -1"
    info "first sealed block: $FIRST   highest sealed block: $LAST"
    if [ "$FIRST" = "1" ]; then ok "run starts at genesis (first sealed block is 1)"; else bad "INVALID RUN: first sealed block is $FIRST, not 1 -- counts describe a partial run and must not be compared with another"; fi
    if [ "$LAST" = "$SEALED" ]; then ok "highest block ($LAST) equals the count ($SEALED), so the count is the whole run"; else bad "highest block $LAST != count $SEALED: blocks are missing or the counter is wrong"; fi
    GAPS=$(awk 'NR > 1 && $1 != prev + 1 { n++ } { prev = $1 } END { print n + 0 }' "$TMP/seal_h.txt")
    need_num "gap count" "$GAPS"
    need_num "first sealed block" "$FIRST"
    need_num "highest sealed block" "$LAST"
    if [ "$GAPS" -eq 0 ]; then ok "block numbers are contiguous"; else bad "$GAPS gap(s) in the sealed sequence"; fi
    SNAP_HEAD=$(sed -n 's/.*eth_blockNumber = 0x[0-9a-f][0-9a-f]* (\([0-9][0-9]*\)).*/\1/p' "$SNAP" | head -1)
    cmt "sed -n 's/.*eth_blockNumber = 0x[0-9a-f]* (\\([0-9]*\\)).*/\\1/p' $SNAP"
    info "chain head recorded in the snapshot: ${SNAP_HEAD:-none}"
    if [ -n "$SNAP_HEAD" ]; then
        if [ "$LAST" -ge "$SNAP_HEAD" ]; then ok "log reaches the recorded head ($LAST >= $SNAP_HEAD)"; else bad "log stops at block $LAST but the node recorded head $SNAP_HEAD: the log is truncated"; fi
    else
        bad "snapshot has no parseable eth_blockNumber: cannot cross-check the log against the chain"
    fi
else
    na "first sealed block: not evaluated, the run sealed nothing"
    na "highest block against the count: not evaluated"
    na "contiguity of block numbers: not evaluated"
    na "log against the recorded head: not evaluated"
fi

sec "G3 failure markers (each must be 0)"
cmt "grep -cF '<marker>' $LOG   （逐个）"
for marker in 'DATA RACE' 'fatal error' 'panic:' 'concurrent map' 'seal timed out' 'ERROR'; do
    n=$(count "$marker" "$LOG" "$marker") || cannot "could not count marker '$marker'"
    need_num "count of '$marker'" "$n"
    if [ "$n" -eq 0 ]; then ok "'$marker' -> 0"; else bad "'$marker' -> $n line(s)"; fi
done
info "one-sided: each of these reads 0 when nothing matches, so a typo in a"
info "marker reads exactly like a clean run. A planted-marker fixture is what"
info "pins the literals -- see README, 'gate.sh: judging a run'."

sec "G4 FinishedProposal 1:1 against sealed blocks"
keep "$FORK_PAT" "$LOG" "$TMP/fork.txt"
FORK=$(wc -l <"$TMP/fork.txt" | tr -d ' ')
need_num "fork line count" "$FORK"
grep -oE 'fork block ?height ?[0-9]+' "$TMP/fork.txt" 2>/dev/null | awk '{ gsub(/[^0-9]/, ""); print }' >"$TMP/fork_h.txt"
cmt "grep -cF '$FORK_PAT' $LOG"
cmt "grep -oE 'fork block ?height ?[0-9]+' | awk '{gsub(/[^0-9]/,\"\"); print}'"
info "fork lines: $FORK   sealed: $SEALED"
if [ "$FORK" -eq "$SEALED" ]; then ok "1:1 ($FORK : $SEALED)"; else bad "MISMATCH ($FORK fork vs $SEALED sealed)"; fi
if [ -s "$TMP/fork_h.txt" ] && [ -s "$TMP/seal_h.txt" ]; then
    if cmp -s "$TMP/fork_h.txt" "$TMP/seal_h.txt"; then
        ok "fork heights and sealed heights are the same sequence, element by element"
    else
        bad "fork and sealed height sequences differ: $(diff "$TMP/fork_h.txt" "$TMP/seal_h.txt" 2>/dev/null | grep -c '^[<>]') differing line(s)"
    fi
    FSUM=$(awk '{ s += $1 } END { print s + 0 }' "$TMP/fork_h.txt")
    SSUM=$(awk '{ s += $1 } END { print s + 0 }' "$TMP/seal_h.txt")
    info "height sums: fork $FSUM   sealed $SSUM"
    if [ "$FSUM" = "$SSUM" ]; then ok "height sums equal ($FSUM)"; else bad "height sums differ: $FSUM vs $SSUM"; fi
else
    bad "cannot extract heights from one side, so the two sequences cannot be compared"
    na "height sums: not comparable"
fi

sec "G5 chain head snapshot: extra length and block period"
cmt "sed -n 's/.*extra=\\([0-9]*\\)B.*/\\1/p' $SNAP"
cmt "sed -n 's/.*interval=\\([0-9]*\\)s.*/\\1/p' $SNAP"
awk '/extra=/ && /votes=/ {
        e = ""; v = ""
        if (match($0, /extra=[0-9]+B/)) { e = substr($0, RSTART + 6, RLENGTH - 7) }
        if (match($0, /votes=[0-9]+/))  { v = substr($0, RSTART + 6, RLENGTH - 6) }
        if (e != "" && v != "") printf "%s %s\n", e, v
     }' "$SNAP" >"$TMP/extra.txt"
if [ -s "$TMP/extra.txt" ]; then
    awk -v base="$EXTRA_BASE" -v per="$EXTRA_PER_VOTE" -v vf="$TMP/viol.txt" '
        { e = $1; v = $2; want = base + per * v; n++
          if (e != want) print "extra=" e "B with votes=" v ", expected " want " (" base " + " per "*" v ")" > vf
          if (v + 0 < 1) print "votes=" v ", expected at least 1" > vf
          if (v + 0 > mv) mv = v + 0 }
        END { printf "%d %d\n", n, mv > sv }' sv="$TMP/extra_stats.txt" "$TMP/extra.txt" || cannot "awk failed while checking the extra layout"
    if [ -s "$TMP/viol.txt" ]; then
        # One verdict slot, not one per bad block. The count goes in the message and
        # the detail goes to info lines: a gate that emits a variable number of
        # verdicts cannot be accounted for, and a run with 300 bad blocks would read
        # as 300 failed gates rather than one failed gate.
        NVIOL=$(wc -l <"$TMP/viol.txt" | tr -d ' ')
        need_num "extra-layout violation count" "$NVIOL"
        while IFS= read -r line; do info "$line"; done <"$TMP/viol.txt"
        bad "$NVIOL block(s) violate the extra length or vote-count floor; first: $(head -1 "$TMP/viol.txt")"
    else
        ok "extra length is $EXTRA_BASE + $EXTRA_PER_VOTE*votes on every inspected block, votes >= 1"
    fi
    read -r N_INSPECTED MAX_VOTES <"$TMP/extra_stats.txt" || cannot "cannot read the extra statistics"
    info "blocks inspected: $N_INSPECTED   max votes: $MAX_VOTES"
else
    bad "snapshot has no 'extra=...B' + 'votes=...' line: cannot verify the extra layout"
fi
GOT_INT=$(sed -n 's/.*interval=\([0-9][0-9]*\)s.*/\1/p' "$SNAP" | head -1)
info "interval reported by the snapshot: ${GOT_INT:-none}s, expected ${EXPECT_INTERVAL}s"
if [ -n "$GOT_INT" ]; then
    if [ "$GOT_INT" = "$EXPECT_INTERVAL" ]; then ok "block period matches"; else bad "block period is ${GOT_INT}s, expected ${EXPECT_INTERVAL}s"; fi
else
    bad "snapshot has no parseable interval=Ns"
fi

sec "G6 intervals between seals (reported, not judged)"
cmt "awk 'match(\$0, /[0-9][0-9]:[0-9][0-9]:[0-9][0-9]\\.[0-9][0-9][0-9]/)' <seal lines>   （取日志时间戳）"
awk 'match($0, /[0-9][0-9]:[0-9][0-9]:[0-9][0-9]\.[0-9][0-9][0-9]/) {
        t = substr($0, RSTART, 12)
        h = substr(t, 1, 2); m = substr(t, 4, 2); s = substr(t, 7, 2); ms = substr(t, 10, 3)
        print ((h * 60 + m) * 60 + s) * 1000 + ms
     }' "$TMP/seal.txt" >"$TMP/ts.txt"
if [ -s "$TMP/ts.txt" ]; then
    awk 'NR > 1 { d = $1 - prev; print d; if (d < 0) neg++ } { prev = $1 }
         END { if (neg > 0) exit 3 }' "$TMP/ts.txt" >"$TMP/dt.txt"
    RC=$?
    if [ "$RC" -eq 3 ]; then
        bad "log timestamps go backwards: this log mixes two runs, or crossed midnight"
    elif [ -s "$TMP/dt.txt" ]; then
        info "first interval (start -> block 1, includes unlocking): $(head -1 "$TMP/dt.txt")ms"
        awk -v want=$((EXPECT_INTERVAL * 1000)) 'NR > 1 {
                 s += $1; n++
                 if (min == "" || $1 < min) min = $1
                 if ($1 > max) max = $1
                 d = $1 - want; if (d < 0) d = -d
                 if (d > 30) big++
             }
             END { if (n > 0) printf "   ..    steady state, %d intervals: mean %.1fms min %dms max %dms; %d deviate more than 30ms\n", n, s / n, min, max, big + 0
                   else printf "   ..    no steady-state interval to report\n" }' "$TMP/dt.txt"
        # This slot is 'ok' for "the intervals were read", not for "the intervals are
        # good": no threshold is applied. It exists so the section always accounts for
        # exactly one verdict, whether or not there was anything wrong to report.
        ok "intervals were computed and reported (no threshold applied here)"
    else
        bad "cannot compute any interval: fewer than two seal timestamps"
    fi
else
    bad "cannot read seal timestamps"
fi

sec "G7 WARN accounting: every warning is classified, and the parts add up"
cmt "grep -F 'WARN' $LOG | awk '{sub(/^WARN \\[[^]]*\\] /,\"\"); gsub(/[0-9][0-9,]*/,\"N\"); print}' | sort | uniq -c | sort -rn"
keep 'WARN' "$LOG" "$TMP/warn.txt"
WARNS=$(wc -l <"$TMP/warn.txt" | tr -d ' ')
need_num "WARN line count" "$WARNS"
awk '{ sub(/^WARN \[[^]]*\] /, ""); gsub(/[0-9][0-9,]*/, "N"); print }' "$TMP/warn.txt" >"$TMP/warn_t.txt"
awk '{ c[$0]++ } END { for (k in c) printf "%d\t%s\n", c[k], k }' "$TMP/warn_t.txt" | sort -rn >"$TMP/warn_c.txt"
awk '{ s = substr($0, index($0, "\t") + 1); printf "   ..    %6d  %s\n", $1, substr(s, 1, 72) }' "$TMP/warn_c.txt"
WSUM=$(awk '{ s += $1 } END { print s + 0 }' "$TMP/warn_c.txt")
need_num "classified WARN total" "$WSUM"
info "WARN lines: $WARNS   classified total: $WSUM"
if [ "$WARNS" -eq "$WSUM" ]; then ok "every WARN line falls into a printed template"; else bad "classified $WSUM of $WARNS WARN lines: the breakdown dropped some"; fi
WFORK=$(awk '$2 == "FinishedProposal" { s += $1 } END { print s + 0 }' "$TMP/warn_c.txt")
need_num "fork-template WARN count" "$WFORK"
info "fork-template WARNs: $WFORK   fork lines from G4: $FORK"
if [ "$WFORK" -eq "$FORK" ]; then ok "fork WARN count matches G4"; else bad "fork WARN count $WFORK differs from G4's $FORK"; fi
NONFORK=$((WARNS - WFORK))
info "non-fork WARNs: $NONFORK -- read them, they are not required to be zero"

if [ -n "$BINARY" ]; then
    sec "G8 binary provenance"
    cmt "shasum -a 256 $BINARY"
    cmt "go version -m $BINARY | grep -E 'vcs.revision|vcs.modified'"
    cmt "strings -a $BINARY | grep -cF 'WARNING: DATA RACE'"
    if [ -r "$BINARY" ]; then
        info "sha256: $(shasum -a 256 "$BINARY" | cut -d' ' -f1)"
        info "size:   $(wc -c <"$BINARY" | tr -d ' ') bytes"
        if command -v go >/dev/null 2>&1; then
            go version -m "$BINARY" 2>/dev/null | grep -E 'vcs.revision|vcs.modified' >"$TMP/vcs.txt" || : >"$TMP/vcs.txt"
            if [ -s "$TMP/vcs.txt" ]; then
                awk '{ printf "   ..    %s\n", $0 }' "$TMP/vcs.txt"
                if grep -F 'vcs.modified=false' "$TMP/vcs.txt" >/dev/null 2>&1; then ok "binary carries vcs.modified=false"; else bad "binary carries vcs.modified=true (built from a dirty tree) or no such field"; fi
            else
                na "no vcs stamp: built outside a repository root (worktree, archive, clone without .git), so it cannot prove its own source"
            fi
        else
            bad "go is not on PATH, so the vcs stamp cannot be read"
        fi
        if command -v strings >/dev/null 2>&1; then
            strings -a "$BINARY" >"$TMP/str.txt" 2>/dev/null || cannot "strings failed on $BINARY"
            NRACE=$(count 'WARNING: DATA RACE' "$TMP/str.txt" 'race detector string') || cannot "could not read the detector string out of $BINARY"
            need_num "race detector string count" "$NRACE"
            if [ "$NRACE" -gt 0 ]; then ok "binary contains the race detector"; else
                if [ "$EXPECT_RACE" -eq 1 ]; then bad "--expect-race but the binary has no race detector: a run called '-race' was not one"; else na "binary has no race detector, and --expect-race was not given"; fi
            fi
        else
            if [ "$EXPECT_RACE" -eq 1 ]; then bad "--expect-race but strings is not on PATH, so the detector cannot be confirmed"; else na "strings not on PATH, so the detector state is unknown"; fi
        fi
    else
        cannot "cannot read binary $BINARY"
    fi
else
    sec "G8 binary provenance (skipped)"
    na "vcs stamp: no --binary given, the binary that produced this run was not judged"
    na "race detector: no --binary given"
fi

# Every gate declares how many verdict slots it owns. A slot that silently disappears
# when an earlier gate fails looks exactly like a gate that had nothing to judge, and
# the totals of two runs stop being comparable -- which is how the earlier "42 blocks"
# report survived review with every number correct.
#
# The check is per section, not just on the total: two compensating errors (one gate
# looping out an extra verdict, another dropping one) leave the total intact, and the
# total is the only thing a human reads.
SEC=""
# The declared counts live in an array called want_n, not exp: `exp` is a builtin
# function name, and `-v exp=` / `exp[x]` are rejected or silently reinterpreted. This
# exact mistake already cost one wrong report on this project (see README, "a name that
# means something in two layers"). Prefix the name instead of remembering the hazard.
if ! awk 'BEGIN {
         n = split("G0 2 G1 2 G2 5 G3 6 G4 3 G5 2 G6 1 G7 2 G8 2", spec, " ")
         for (i = 1; i < n; i += 2) want_n[spec[i]] = spec[i + 1]
     }
     { got[$1]++ }
     END {
         for (s in want_n) if ((got[s] + 0) != want_n[s])
             printf "gate accounting: section %s emitted %d verdict(s), %d declared\n", s, got[s] + 0, want_n[s]
         for (s in got) if (!(s in want_n))
             printf "gate accounting: section %s emitted %d verdict(s) but is not declared\n", s, got[s]
     }' "$TMP/slots.txt" >"$TMP/acct.txt"; then
    # An accounting check that fails to run must not read as an accounting check that
    # found nothing. Same rule as count(): a broken gate is not a passing gate.
    bad "gate accounting could not be evaluated: the checker itself failed"
fi
if [ -s "$TMP/acct.txt" ]; then
    while IFS= read -r line; do bad "$line"; done <"$TMP/acct.txt"
fi
TOTAL=$(wc -l <"$TMP/slots.txt" | tr -d ' ')
need_num "tagged verdict count" "$TOTAL"
if [ "$TOTAL" -ne "$GATE_TOTAL" ]; then
    bad "gate accounting: $TOTAL verdicts issued by the declared gates, $GATE_TOTAL declared"
fi

printf '\n== verdict\n'
printf '   gates passed: %d   failed: %d   not applicable: %d\n' "$PASSED" "$FAILED" "$NA"
printf '   gates accounted for: %s of %d\n' "$(wc -l <"$TMP/slots.txt" | tr -d ' ')" "$GATE_TOTAL"
if [ "$FAILED" -eq 0 ]; then
    printf '   PASS: this run is usable as evidence\n'
    exit 0
fi
printf '   FAIL: this run is not usable as evidence\n'
exit 1
