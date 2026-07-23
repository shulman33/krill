#!/usr/bin/env bash
# Phase 5: the Monday-9am wake storm. N VMs resume the SAME snapshot at once,
# each in its own network namespace (so all guests can be 172.16.0.2) and its
# own mount namespace (so each opens a private rootfs copy at the path the
# snapshot expects — Firecracker's load API cannot re-path drives).
#
# Gate G4: p99 first-200 <= 1 s across N=50, zero errors.
# Bonus: host RSS with all N resident — the shared-page-cache density number.
#
# usage: ./30-storm.sh <python|node> [N=50]
# Requires: ./10-boot-and-snapshot.sh <guest> already ran on this box.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

GUEST=${1:?usage: $0 <python|node> [N=50]}
N=${2:-50}
GOLDEN="$FC_DIR/app-$GUEST.golden.ext4"
LIVE="$FC_DIR/app-$GUEST.ext4"          # the drive path baked into the snapshot
SNAP="$SNAP_DIR/$GUEST"
OUT="$RESULTS_DIR/storm-$GUEST-$N.txt"
PIDFILE="$STORM_DIR/fc-pids"

[ -f "$SNAP/vmstate" ] || { echo "FATAL: no snapshot at $SNAP — run ./10-boot-and-snapshot.sh $GUEST first."; exit 1; }
[ -f "$LIVE" ] || { echo "FATAL: $LIVE missing (bind-mount target) — run phase 3 first."; exit 1; }

: > "$OUT"; : > "$PIDFILE"
free -m > "$RESULTS_DIR/storm-free-before.txt"

echo "== pre-stage (untimed): $N rootfs copies + $N netns =="
for i in $(seq 1 "$N"); do
  cp --sparse=always "$GOLDEN" "$STORM_DIR/rootfs-$i.ext4"
  ip netns delete "fc$i" 2>/dev/null || true
  ip netns add "fc$i"
  ip netns exec "fc$i" ip link set lo up
  ip netns exec "fc$i" ip tuntap add "$TAP" mode tap
  # same deterministic MAC in every netns — restored guests' ARP caches expect it
  ip netns exec "fc$i" ip link set "$TAP" address "$TAP_MAC"
  ip netns exec "fc$i" ip addr add "$HOST_IP/24" dev "$TAP"
  ip netns exec "fc$i" ip link set "$TAP" up
  # pin the guest's MAC so ARP never races the restore (see lib.sh:setup_tap)
  ip netns exec "fc$i" ip neigh replace "$GUEST_IP" lladdr "$GUEST_MAC" dev "$TAP" nud permanent
done

storm_one() {  # resume VM i, record first-200 ms; leave the VM RUNNING
  local i=$1 sock="/tmp/fc-storm-$i.sock" t0 t2
  rm -f "$sock"
  # netns for the guest's tap; private mount ns to bind our copy over $LIVE
  ip netns exec "fc$i" unshare -m bash -c "
    mount --make-rprivate / &&
    mount --bind '$STORM_DIR/rootfs-$i.ext4' '$LIVE' &&
    exec firecracker --api-sock '$sock'
  " >/dev/null 2>&1 &
  echo "$!" >> "$PIDFILE"
  wait_sock "$sock" || { echo "vm$i: API socket never appeared" >&2; return 1; }
  t0=$(now_ms)
  resume_vm "$sock" "$SNAP" || { echo "vm$i: load failed" >&2; return 1; }
  # enter the netns ONCE and poll inside it — per-poll `ip netns exec` forks
  # (fork + setns per attempt x N VMs) become a self-inflicted CPU storm that
  # pollutes the very latency being measured
  ip netns exec "fc$i" bash -c \
    "until curl -s -o /dev/null --max-time 0.2 http://$GUEST_IP:8000/ping; do sleep 0.02; done"
  t2=$(now_ms)
  echo "$((t2 - t0))" >> "$OUT"
}

echo "== firing $N concurrent resumes =="
pids=()
for i in $(seq 1 "$N"); do storm_one "$i" & pids+=($!); done
FAILED=0
for p in "${pids[@]}"; do wait "$p" || FAILED=$((FAILED + 1)); done

# all VMs still resident: capture the density number before killing anything
free -m > "$RESULTS_DIR/storm-free-resident.txt"

echo "== results =="
percentiles "$OUT" 1
echo "  errors: $FAILED (gate G4 requires 0)"
awk '/^Mem:/{print "  host RAM used, before: "$3" MB"}' "$RESULTS_DIR/storm-free-before.txt"
awk -v n="$N" -v m="$MEM_MIB" '/^Mem:/{printf "  host RAM used, %d resident: %s MB   (naive would be +%d MB)\n", n, $3, n*m}' \
  "$RESULTS_DIR/storm-free-resident.txt"

echo "== cleanup =="
while read -r p; do kill "$p" 2>/dev/null || true; done < "$PIDFILE"
sleep 1
for i in $(seq 1 "$N"); do
  ip netns delete "fc$i" 2>/dev/null || true
  rm -f "$STORM_DIR/rootfs-$i.ext4" "/tmp/fc-storm-$i.sock"
done
rm -f "$PIDFILE"
echo "Done. Raw latencies: $OUT"
