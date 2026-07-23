#!/usr/bin/env bash
# Phase 3.1: boot a guest, time cold boot (G5 baseline), warm it, balloon-shrink,
# snapshot, hole-punch the mem file.
#
# usage: ./10-boot-and-snapshot.sh <python|node> [--boot-only]
#   --boot-only : time boot-to-first-200 and exit (loop it 10x for the G5 baseline)
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

GUEST=${1:?usage: $0 <python|node> [--boot-only]}
MODE=${2:-snapshot}
GOLDEN="$FC_DIR/app-$GUEST.golden.ext4"
LIVE="$FC_DIR/app-$GUEST.ext4"        # this exact path gets baked into the snapshot
SNAP="$SNAP_DIR/$GUEST"
SOCK=$API_SOCK_DEFAULT

setup_tap
fresh_rootfs "$GOLDEN" "$LIVE"

FC_PID=$(launch_fc "$SOCK")
configure_vm "$SOCK" "$LIVE"

t0=$(now_ms)
fc_api "$SOCK" PUT /actions '{"action_type":"InstanceStart"}'
wait_ping
t1=$(now_ms)
echo "$((t1 - t0))" >> "$RESULTS_DIR/cold-boot-$GUEST.txt"
echo "boot-to-first-200: $((t1 - t0)) ms"

if [ "$MODE" = "--boot-only" ]; then
  stop_fc "$FC_PID"
  exit 0
fi

warm_guest
snapshot_vm "$SOCK" "$SNAP"
stop_fc "$FC_PID"

fallocate -d "$SNAP/mem"
{
  echo "== snapshot footprint: $GUEST =="
  echo "mem apparent: $(du -h --apparent-size "$SNAP/mem" | cut -f1)"
  echo "mem actual:   $(du -h "$SNAP/mem" | cut -f1)   (after balloon + fallocate -d)"
  echo "rootfs golden: $(du -h --apparent-size "$GOLDEN" | cut -f1) apparent / $(du -h "$GOLDEN" | cut -f1) actual"
} | tee -a "$RESULTS_DIR/footprint-$GUEST.txt"
echo "Snapshot ready in $SNAP. Now run: ./11-bench-resume.sh $GUEST 100 warm"
