#!/usr/bin/env bash
# M2 gate setup. Prereqs: wake-bench/00-host-setup.sh + 01-install-firecracker.sh
# already ran (firecracker + /srv/fc/vmlinux + docker), and the repo was copied
# to this box with linux binaries in bin/ (make krilld-linux krill-linux).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

KRILLD_BIN=../bin/krilld-linux-amd64
KRILL_BIN=../bin/krill-linux-amd64
[ -x "$KRILLD_BIN" ] || { echo "FATAL: $KRILLD_BIN missing — 'make krilld-linux' locally, then copy the repo"; exit 1; }
[ -x "$KRILL_BIN" ]  || { echo "FATAL: $KRILL_BIN missing — 'make krill-linux' locally, then copy the repo"; exit 1; }
[ -f "$KERNEL" ] || { echo "FATAL: $KERNEL missing — run wake-bench/01-install-firecracker.sh first"; exit 1; }
command -v docker >/dev/null || { echo "FATAL: docker missing — run wake-bench/00-host-setup.sh"; exit 1; }
command -v jq >/dev/null || apt-get install -y -qq jq

echo "== node for the MCP server (B4) =="
if ! command -v node >/dev/null || [ "$(node -e 'console.log(process.versions.node.split(".")[0])')" -lt 18 ]; then
  curl -fsSL https://deb.nodesource.com/setup_20.x | bash - >/dev/null
  apt-get install -y -qq nodejs
fi
node --version

echo "== building the MCP server =="
( cd ../mcp && npm install --no-audit --no-fund --loglevel=error && npm run build )

echo "== installing binaries + starting krilld =="
install -m 0755 "$KRILLD_BIN" /usr/local/bin/krilld
install -m 0755 "$KRILL_BIN" /usr/local/bin/krill
pkill -x krilld 2>/dev/null && sleep 1 || true
nohup /usr/local/bin/krilld \
  --data-dir "$KRILL_DATA" \
  --kernel "$KERNEL" \
  --idle-timeout "$IDLE_TIMEOUT" \
  >>"$KRILLD_LOG" 2>&1 &
echo $! > /run/krilld.pid
sleep 1
curl -sf "$ADMIN/healthz" >/dev/null || { echo "FATAL: krilld admin API not answering; see $KRILLD_LOG"; exit 1; }
echo "krilld up (pid $(cat /run/krilld.pid), log $KRILLD_LOG). Run ./b1-deploy.sh next."
