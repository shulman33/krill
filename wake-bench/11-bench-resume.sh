#!/usr/bin/env bash
# Phase 3.3: the resume benchmark. Measures BOTH the snapshot/load API time and
# the number that matters — time to first HTTP 200 through the full stack.
#
# usage: ./11-bench-resume.sh <python|node> <iterations> [warm|cold]
#   warm : NVMe page cache left hot (gate G1: p99 <= 300 ms)
#   cold : caches dropped before every iteration (gate G2: p99 <= 1.5 s)
#
# Every iteration restores a PRISTINE rootfs copy before the clock starts —
# a memory snapshot embeds filesystem state that must match the disk exactly
# (§3.2 of wake-path-benchmark.md). Skipping that silently corrupts results.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

GUEST=${1:?usage: $0 <python|node> <iterations> [warm|cold]}
N=${2:?iterations required}
MODE=${3:-warm}
GOLDEN="$FC_DIR/app-$GUEST.golden.ext4"
LIVE="$FC_DIR/app-$GUEST.ext4"
SNAP="$SNAP_DIR/$GUEST"
SOCK=$API_SOCK_DEFAULT
OUT="$RESULTS_DIR/resume-$GUEST-$MODE.txt"

[ -f "$SNAP/vmstate" ] || { echo "FATAL: no snapshot at $SNAP — run ./10-boot-and-snapshot.sh $GUEST first."; exit 1; }
setup_tap
: > "$OUT"

for i in $(seq 1 "$N"); do
  fresh_rootfs "$GOLDEN" "$LIVE"                 # untimed, pristine disk per run
  [ "$MODE" = cold ] && drop_caches
  FC_PID=$(launch_fc "$SOCK")
  t0=$(now_ms)
  resume_vm "$SOCK" "$SNAP"
  t1=$(now_ms)
  wait_ping
  t2=$(now_ms)
  stop_fc "$FC_PID"
  echo "$((t1 - t0)) $((t2 - t0))" >> "$OUT"
  printf '\r  %d/%d' "$i" "$N" >&2
done
echo >&2

echo "== $GUEST / $MODE  (col 1 = load API, col 2 = first HTTP 200) =="
echo "-- snapshot/load API --"; percentiles "$OUT" 1
echo "-- first HTTP 200    --"; percentiles "$OUT" 2
echo "Raw data: $OUT"
echo "Sanity: if col2 ~= col1 the poll loop is broken; if warm p50 < 10 ms the VM never actually paused."
