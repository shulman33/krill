#!/usr/bin/env bash
# Phase 0: packages, performance governor, fio disk baseline. Run as root.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh

[ -e /dev/kvm ] || { echo "FATAL: /dev/kvm missing — this must be bare metal with KVM."; exit 1; }
[ "$(id -u)" = 0 ] || { echo "FATAL: run as root."; exit 1; }

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  curl git jq sqlite3 borgbackup fio docker.io e2fsprogs iproute2 util-linux
# best-effort: cpupower lives in a kernel-versioned package
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  linux-tools-common "linux-tools-$(uname -r)" 2>/dev/null || true

systemctl disable --now unattended-upgrades 2>/dev/null || true
# no-op on cloud VMs (the physical CPU isn't yours to govern) — harmless
cpupower frequency-set -g performance 2>/dev/null || echo "WARN: could not set performance governor (expected on cloud VMs)"

# Record which tier this is: nested virt (tier 1) or bare metal (tier 2).
# The tier rule: latency PASSes on nested are conclusive; FAILs mean escalate to metal.
VIRT=$(systemd-detect-virt 2>/dev/null || echo unknown)
if [ "$VIRT" = "none" ]; then TIER="metal"; else TIER="nested (under $VIRT)"; fi
echo "tier: $TIER"

mkdir -p "$FC_DIR" "$SNAP_DIR" "$STORM_DIR"

# /srv must be local-SSD-class storage — network disks (EBS / Persistent Disk)
# silently invalidate G1/G2 and the fio baseline.
SRV_DEV=$(findmnt -n -o SOURCE --target /srv 2>/dev/null || true)
SRV_MODEL=$(lsblk -no MODEL "$SRV_DEV" 2>/dev/null | head -1 || true)
case "$SRV_MODEL" in
  *"Elastic Block Store"*|*PersistentDisk*)
    echo "WARNING: /srv is on network storage ($SRV_MODEL) — mount a Local SSD / instance-store NVMe there instead." ;;
esac
ROOT_DEV=$(findmnt -n -o SOURCE --target / 2>/dev/null || true)
[ -n "$SRV_DEV" ] && [ "$SRV_DEV" = "$ROOT_DEV" ] && \
  echo "WARNING: /srv is on the boot disk — on cloud instances that's network storage; mount the Local SSD at /srv."

echo "== fio 4k randread baseline (30s) — know the NVMe ceiling =="
fio --name=randread --filename="$FC_DIR/fio.test" --size=4G --rw=randread \
    --ioengine=libaio --bs=4k --iodepth=32 --direct=1 --runtime=30 --time_based \
    --group_reporting \
  | tee "$RESULTS_DIR/fio-baseline.txt"
rm -f "$FC_DIR/fio.test"

{ echo "== ENV =="; echo "tier: $TIER"; echo "/srv device: ${SRV_DEV:-?} (${SRV_MODEL:-?})";
  date -u; uname -a; lscpu | sed -n '1,12p'; free -h; } \
  > "$RESULTS_DIR/ENV.txt"
echo "Phase 0 done. Tier: $TIER. Baseline in results/fio-baseline.txt, env in results/ENV.txt."
