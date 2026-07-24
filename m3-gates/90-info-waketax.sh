#!/usr/bin/env bash
# C-info — wake-latency tax of the data plane (informational, not a gate).
# 30 freeze/wake cycles through the router with the data plane on; compare
# p50/p99 against the A1 numbers from m1-gates on the same box. The delta
# is the cost of: epoch mint + takeover seal CAS + shipper start (+ the
# sync-ack precise scan on the request itself).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

krill delete waketax >/dev/null 2>&1 || true
krill deploy examples/ledger --name waketax --json | jq -e '.ready == true' >/dev/null || fail "C-info: deploy failed"
route waketax POST "/add?k=1&v=x" >/dev/null

: > "$RESULTS_DIR/c-info-wakes.txt"
for i in $(seq 1 30); do
  freeze_app waketax
  t0=$(now_ms)
  route waketax GET / >/dev/null
  t1=$(now_ms)
  echo $((t1 - t0)) >> "$RESULTS_DIR/c-info-wakes.txt"
done

sort -n "$RESULTS_DIR/c-info-wakes.txt" | awk '
  { v[NR] = $1 }
  END {
    p50 = v[int(NR*0.50 + 0.5)]; p99 = v[NR]
    printf "C-info: %d wakes with data plane + sync-ack: p50=%dms max=%dms\n", NR, p50, p99
    printf "compare: m1-gates A1 measured p99 298ms on this instance shape\n"
  }' | tee "$RESULTS_DIR/c-info.txt"
krill delete waketax >/dev/null 2>&1 || true
