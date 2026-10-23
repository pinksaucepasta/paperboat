#!/bin/bash
# Data-plane load for the PLAN 3b scale matrix, run ON an enrolled client machine against one target machine of
# the same test account (two throwaway containers; never other lanes' machines). Each step pushes past the host's
# bound and records, per attempt, whether it succeeded or failed and with which message, so the matrix can say
# whether the limit is graceful (clear error at the bound) or not (hangs, vague errors, failures below the bound).
#
#   PB=~/.local/bin/pb TARGET=<machine name> tools/loadtest/dataplane.sh [ssh|exec|send|all]
#
# Output: one summary line per step, and per-attempt logs under $OUT (default ./dataplane-<time>).
set -uo pipefail
PB=${PB:-pb}; TARGET=${TARGET:?target machine name}; STEP=${1:-all}
OUT=${OUT:-./dataplane-$(date +%H%M%S)}; mkdir -p "$OUT"

# run_parallel NAME COUNT TIMEOUT -- command... : starts COUNT copies at once, waits, summarises.
run_parallel() {
  local name=$1 count=$2 limit=$3; shift 4
  local started=$(date +%s%N)
  for i in $(seq 1 "$count"); do
    ( s=$(date +%s%N); timeout "$limit" "$@" >"$OUT/$name.$i.out" 2>&1; rc=$?
      echo "$rc $(( ($(date +%s%N) - s) / 1000000 ))" >"$OUT/$name.$i.rc" ) &
  done
  wait
  local ok=0 hung=0 failed=0
  for i in $(seq 1 "$count"); do
    read -r rc took <"$OUT/$name.$i.rc"
    case $rc in 0) ok=$((ok+1));; 124) hung=$((hung+1));; *) failed=$((failed+1));; esac
  done
  local messages=$(for i in $(seq 1 "$count"); do read -r rc _ <"$OUT/$name.$i.rc"; [ "$rc" != 0 ] && tail -1 "$OUT/$name.$i.out"; done | sort | uniq -c | sort -rn | head -3 | tr '\n' ';')
  local slowest=$(cut -d' ' -f2 "$OUT"/$name.*.rc | sort -n | tail -1)
  printf '%-6s attempts=%d ok=%d failed=%d hung=%d wall=%dms slowest=%sms errors: %s\n' \
    "$name" "$count" "$ok" "$failed" "$hung" "$(( ($(date +%s%N) - started) / 1000000 ))" "$slowest" "${messages:-none}"
}

# Host bounds (cli/internal/hostruntime/config DefaultResources): 32 concurrent execs, 32 managed SSH streams,
# 2 concurrent transfers. Each step uses 40 so the last 8 must fail clearly and quickly.
case $STEP in ssh|all)
  run_parallel ssh 40 90 -- "$PB" ssh "$TARGET" -- -o BatchMode=yes sleep 20 ;;
esac
case $STEP in exec|all)
  run_parallel exec 40 90 -- "$PB" exec "$TARGET" -- sleep 20 ;;
esac
case $STEP in send|all)
  head -c $((10 << 20)) /dev/urandom >"$OUT/payload.bin"
  run_parallel send 6 300 -- "$PB" send "$OUT/payload.bin" --to "$TARGET" ;;
esac
