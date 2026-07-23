#!/usr/bin/env bash
# Phase 1: install a pinned Firecracker release + a CI guest kernel.
# Pin with FC_VERSION=v1.x.y if you need a specific release; default = latest.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh

ARCH=$(uname -m)
REL=https://github.com/firecracker-microvm/firecracker/releases
VER=${FC_VERSION:-$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$REL/latest")")}

echo "Installing firecracker $VER ($ARCH)"
curl -fsSL "$REL/download/$VER/firecracker-$VER-$ARCH.tgz" | tar -xz -C /tmp
install "/tmp/release-$VER-$ARCH/firecracker-$VER-$ARCH" /usr/local/bin/firecracker
firecracker --version | head -1 | tee -a "$RESULTS_DIR/ENV.txt"

# Guest kernel from the Firecracker CI bucket. The CI folder version does not
# track the release version, so probe a few. If all fail, copy the current URL
# from the getting-started doc and place the file at $KERNEL yourself.
if [ ! -f "$KERNEL" ]; then
  KEY=""
  for CI in v1.14 v1.13 v1.12 v1.11 v1.10; do
    KEY=$(curl -fsS "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/$CI/$ARCH/vmlinux-6.1&list-type=2" \
          | grep -oE "firecracker-ci/$CI/$ARCH/vmlinux-6\.1\.[0-9]+" | sort -V | tail -1) || true
    [ -n "$KEY" ] && break
  done
  [ -n "$KEY" ] || { echo "FATAL: no CI kernel found — fetch manually (see script header)."; exit 1; }
  echo "Fetching kernel: $KEY"
  curl -fsSL -o "$KERNEL" "https://s3.amazonaws.com/spec.ccfc.min/$KEY"
  echo "kernel: $KEY" | tee -a "$RESULTS_DIR/ENV.txt"
fi
echo "Phase 1 done. Versions recorded in results/ENV.txt — snapshots are only valid on THIS exact pair."
