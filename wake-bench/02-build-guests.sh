#!/usr/bin/env bash
# Phase 2: build both guest rootfs images (Docker image -> ext4) + golden copies.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

for name in python node; do
  echo "== building guest: $name =="
  docker build -t "guest-$name" "guests/$name"
  image_to_ext4 "guest-$name" "$FC_DIR/app-$name.ext4"
  cp --sparse=always "$FC_DIR/app-$name.ext4" "$FC_DIR/app-$name.golden.ext4"
  echo "   $FC_DIR/app-$name.ext4 (+ .golden)"
done
echo "Phase 2 done."
