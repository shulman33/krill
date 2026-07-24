#!/usr/bin/env bash
# M3 gate setup. Prereqs: wake-bench/00-host-setup.sh + 01-install-firecracker.sh
# already ran (firecracker + /srv/fc/vmlinux + docker), and the repo was copied
# to this box with linux binaries in bin/:
#   make krilld-linux krill-linux fencetool-linux
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

for b in krilld krill fencetool; do
  [ -x "../bin/$b-linux-amd64" ] || { echo "FATAL: ../bin/$b-linux-amd64 missing — 'make ${b}-linux' locally, then copy the repo"; exit 1; }
  install -m 0755 "../bin/$b-linux-amd64" "/usr/local/bin/$b"
done
[ -f "$KERNEL" ] || { echo "FATAL: $KERNEL missing — run wake-bench/01-install-firecracker.sh first"; exit 1; }
command -v docker >/dev/null || { echo "FATAL: docker missing — run wake-bench/00-host-setup.sh"; exit 1; }
command -v jq >/dev/null || apt-get install -y -qq jq

echo "== starting krilld with the data plane on ($OBJSTORE) =="
start_krilld
echo "krilld up (pid $(cat /run/krilld.pid), log $KRILLD_LOG). Run ./c1-durability.sh next."
