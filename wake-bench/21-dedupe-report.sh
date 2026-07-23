#!/usr/bin/env bash
# Phase 4b: content-defined-chunking dedupe stats via borg, measured SEPARATELY
# for memory snapshots and rootfs images — they dedupe very differently (ASLR
# ruins memory dedupe; shared base layers make disk dedupe great) and the split
# tells you where the engineering effort goes.
#
# usage: ./21-dedupe-report.sh [N=10]
# Gate G3: (mem deduplicated + disk deduplicated) / N  <=  250 MB per app.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh

N=${1:-10}
export BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes
export BORG_RELOCATED_REPO_ACCESS_IS_OK=yes

# separate repos so each class's "deduplicated size" is self-contained
REPO_MEM=/srv/dedup-mem
REPO_DISK=/srv/dedup-disk
[ -d "$REPO_MEM" ]  || borg init --encryption=none "$REPO_MEM"
[ -d "$REPO_DISK" ] || borg init --encryption=none "$REPO_DISK"

echo "== memory snapshots (${N}x mem files) =="
borg create --stats "$REPO_MEM::mem-$(date +%s)" "$SNAP_DIR"/var-*/mem \
  2>&1 | tee "$RESULTS_DIR/dedup-mem.txt"

echo ""
echo "== rootfs images (${N}x golden ext4) =="
borg create --stats "$REPO_DISK::disk-$(date +%s)" "$FC_DIR"/var-*.golden.ext4 \
  2>&1 | tee "$RESULTS_DIR/dedup-disk.txt"

echo ""
echo "Read the 'This archive:' rows above: Original vs Deduplicated size."
echo "Gate G3 arithmetic: (mem dedup bytes + disk dedup bytes) / $N  <=  250 MB/app."
echo "Also expected: disk dedupes hard (shared base layers), memory poorly (ASLR)."
