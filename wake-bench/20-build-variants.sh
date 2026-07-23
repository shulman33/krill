#!/usr/bin/env bash
# Phase 4a: build N python-app variants sharing the same base image, boot + warm +
# snapshot each. This reproduces the production distribution (many apps, one base)
# so Phase 4b can measure real dedupe ratios.
#
# usage: ./20-build-variants.sh [N=10]
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

N=${1:-10}
EXTRA_PKGS=(requests pyyaml click)   # every 3rd variant gets one extra dep, rotating
SOCK=$API_SOCK_DEFAULT
setup_tap

for i in $(seq 1 "$N"); do
  echo "== variant $i/$N =="
  work=$(mktemp -d)
  cp -r guests/python/. "$work/"

  # perturb the app: unique route per variant
  {
    echo ""
    echo ""
    echo "@app.get(\"/v$i\")"
    echo "def v$i():"
    echo "    return {\"variant\": $i}"
  } >> "$work/app.py"

  # every 3rd variant: one extra pip package (varies the site-packages layer)
  if (( i % 3 == 0 )); then
    pkg=${EXTRA_PKGS[$(( (i / 3 - 1) % ${#EXTRA_PKGS[@]} ))]}
    sed -i "s/fastapi uvicorn/fastapi uvicorn $pkg/" "$work/Dockerfile"
    echo "   (+ extra package: $pkg)"
  fi

  docker build -q -t "guest-var-$i" "$work" >/dev/null
  img="$FC_DIR/var-$i.ext4"
  image_to_ext4 "guest-var-$i" "$img"
  cp --sparse=always "$img" "$FC_DIR/var-$i.golden.ext4"
  rm -rf "$work"

  # boot + warm + snapshot (sequential; all share tap0)
  FC_PID=$(launch_fc "$SOCK")
  configure_vm "$SOCK" "$img"
  fc_api "$SOCK" PUT /actions '{"action_type":"InstanceStart"}'
  wait_ping
  warm_guest
  snapshot_vm "$SOCK" "$SNAP_DIR/var-$i"
  stop_fc "$FC_PID"
  fallocate -d "$SNAP_DIR/var-$i/mem"
done
echo "Phase 4a done: $N variants snapshotted under $SNAP_DIR/var-*. Now run ./21-dedupe-report.sh $N"
