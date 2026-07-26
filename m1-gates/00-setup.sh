#!/usr/bin/env bash
# M1 gate setup. Prereqs: wake-bench/00-host-setup.sh and 01-install-firecracker.sh
# already ran on this box (firecracker binary + /srv/fc/vmlinux + docker), and
# a linux/amd64 krilld binary sits next to this script (make krilld-linux
# locally, scp it over).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

[ -x ./krilld ] || { echo "FATAL: ./krilld binary missing — build with 'make krilld-linux' and scp it here"; exit 1; }
[ -f "$KERNEL" ] || { echo "FATAL: $KERNEL missing — run wake-bench/01-install-firecracker.sh first"; exit 1; }
command -v jq >/dev/null || apt-get install -y -qq jq

echo "== building the gate guest image =="
docker build -q -t krill-gate-guest guest/
mkdir -p "$IMAGES_DIR"
cid=$(docker create krill-gate-guest)
dd if=/dev/zero of="$GOLDEN" bs=1M count=1024 status=none
mkfs.ext4 -q -F "$GOLDEN"
mkdir -p /mnt/krill-rootfs
mount -o loop "$GOLDEN" /mnt/krill-rootfs
docker export "$cid" | tar -xC /mnt/krill-rootfs
umount /mnt/krill-rootfs
docker rm "$cid" >/dev/null
echo "   golden: $GOLDEN ($(du -h --apparent-size "$GOLDEN" | cut -f1) apparent)"

# A systemd-managed krilld is the production daemon, not a leftover: pkilling
# it just makes systemd restart it (Restart=always) and then two daemons fight
# over the same ports, taps and data dir. Refuse, and say what to do.
if systemctl is-active --quiet krilld 2>/dev/null; then
  echo "FATAL: krilld is running under systemd on this box." >&2
  echo "  systemctl stop krilld     # and start it again when the gates finish" >&2
  echo "  Then re-run with a scratch data dir so production state is untouched:" >&2
  echo "  KRILL_DATA=/srv/krill-gates ./00-setup.sh" >&2
  exit 1
fi

echo "== starting krilld =="
echo "   data-dir $KRILL_DATA, idle-timeout $IDLE_TIMEOUT, extra flags: ${KRILLD_EXTRA_FLAGS:-(none)}"
install -m 0755 ./krilld /usr/local/bin/krilld
pkill -x krilld 2>/dev/null && sleep 1 || true
# shellcheck disable=SC2086  # KRILLD_EXTRA_FLAGS is deliberately word-split
nohup /usr/local/bin/krilld \
  --data-dir "$KRILL_DATA" \
  --kernel "$KERNEL" \
  --idle-timeout "$IDLE_TIMEOUT" \
  $KRILLD_EXTRA_FLAGS \
  >>"$KRILLD_LOG" 2>&1 &
echo $! > /run/krilld.pid
sleep 1
curl -sf "$ADMIN/healthz" >/dev/null || { echo "FATAL: krilld admin API not answering; see $KRILLD_LOG"; exit 1; }
echo "krilld up (pid $(cat /run/krilld.pid), log $KRILLD_LOG). Run ./a1-warm-wake.sh next."
