#!/usr/bin/env bash
# GATE A1: a curl of a FROZEN app returns 200, and warm-wake p99 <= 300 ms
# over 100 wakes — measured end-to-end through the router (Host-header in,
# HTTP 200 out), which is strictly more than the benchmark's 115 ms bash
# path measured.
#
# usage: ./a1-warm-wake.sh [iterations=100]
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh
N=${1:-100}
APP=gate-a1
OUT="$RESULTS_DIR/a1-warm-wake.txt"

register_app "$APP"
echo "== priming: cold boot + first freeze =="
route "$APP" GET /ping >/dev/null
freeze_app "$APP"
: > "$OUT"

echo "== $N freeze -> timed-curl cycles =="
for i in $(seq 1 "$N"); do
  t0=$(now_ms)
  body=$(route "$APP" GET /ping)   # -sf: non-200 kills the run
  t1=$(now_ms)
  echo "$((t1 - t0))" >> "$OUT"
  freeze_app "$APP"
  printf '\r  %d/%d (last %d ms)' "$i" "$N" "$((t1 - t0))" >&2
done
echo >&2

echo "== A1 warm wake through the router (frozen -> HTTP 200) =="
percentiles "$OUT" 1
p99=$(pctl "$OUT" 1 99)
gate A1 "$([ "$p99" -le 300 ] && echo 1 || echo 0)" "p99 = ${p99} ms (gate: <= 300 ms) over $N wakes"
