# Shared helpers for the wake-path benchmark. Sourced after env.sh.
# All timing happens on the host (never inside the guest — clocks jump on resume).

now_ms() { date +%s%3N; }

fc_api() { # <sock> <method> <path> [json-body]
  local sock=$1 method=$2 path=$3 body=${4:-}
  local out code
  out=$(curl -s -w '\n%{http_code}' --unix-socket "$sock" -X "$method" \
        "http://localhost$path" -H 'Content-Type: application/json' \
        ${body:+--data "$body"})
  code=${out##*$'\n'}
  case "$code" in
    2*) return 0 ;;
    *)  echo "fc_api $method $path -> HTTP $code: ${out%$'\n'*}" >&2; return 1 ;;
  esac
}

wait_sock() { # <sock> — wait up to ~2s for the API socket to appear
  local s=$1 i
  for i in $(seq 1 2000); do [ -S "$s" ] && return 0; sleep 0.001; done
  return 1
}

launch_fc() { # <sock> -> echoes firecracker pid. DEBUG=1 keeps serial output.
  local sock=$1 pid
  rm -f "$sock"
  if [ "${DEBUG:-0}" = 1 ]; then
    firecracker --api-sock "$sock" >"$RESULTS_DIR/serial-$$.log" 2>&1 &
  else
    firecracker --api-sock "$sock" >/dev/null 2>&1 &
  fi
  pid=$!
  wait_sock "$sock" || { echo "FATAL: firecracker API socket never appeared" >&2; return 1; }
  echo "$pid"
}

configure_vm() { # <sock> <rootfs-path>
  local sock=$1 rootfs=$2
  local args="reboot=k panic=1 pci=off quiet loglevel=1 random.trust_cpu=on init=/init.sh"
  [ "${DEBUG:-0}" = 1 ] && args="console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on init=/init.sh"
  fc_api "$sock" PUT /machine-config \
    "{\"vcpu_count\":$VCPUS,\"mem_size_mib\":$MEM_MIB,\"smt\":false}"
  fc_api "$sock" PUT /boot-source \
    "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"$args\"}"
  fc_api "$sock" PUT /drives/rootfs \
    "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$rootfs\",\"is_root_device\":true,\"is_read_only\":false}"
  fc_api "$sock" PUT /network-interfaces/eth0 \
    "{\"iface_id\":\"eth0\",\"guest_mac\":\"$GUEST_MAC\",\"host_dev_name\":\"$TAP\"}"
  fc_api "$sock" PUT /balloon \
    '{"amount_mib":0,"deflate_on_oom":true,"stats_polling_interval_s":1}'
}

setup_tap() { # idempotent host-side tap for single-VM phases
  if ! ip link show "$TAP" >/dev/null 2>&1; then
    ip tuntap add "$TAP" mode tap
    # deterministic MAC: the guest's snapshot embeds its ARP entry for the
    # gateway; a random tap MAC breaks every restored guest's replies for
    # ~30s (ARP reachable_time) until its cache expires
    ip link set "$TAP" address "$TAP_MAC"
    ip addr add "$HOST_IP/24" dev "$TAP"
    ip link set "$TAP" up
  fi
  # Pin the guest's neighbor entry. Without this, iterations race ARP against
  # the restore: the kernel's who-has retry is exactly 1s, which shows up as a
  # bimodal base/base+1000ms distribution. (Production wake paths must do the
  # equivalent — never let ARP resolution race a resuming guest.)
  ip neigh replace "$GUEST_IP" lladdr "$GUEST_MAC" dev "$TAP" nud permanent
}

wait_ping() { # [netns] — busy-poll until /ping answers 200
  local pfx=()
  [ -n "${1:-}" ] && pfx=(ip netns exec "$1")
  until "${pfx[@]}" curl -s -o /dev/null --max-time 0.2 "http://$GUEST_IP:8000/ping"; do :; done
}

warm_guest() {
  local i
  for i in $(seq 1 "$WARM_PINGS"); do
    curl -s -o /dev/null "http://$GUEST_IP:8000/ping"
  done
}

snapshot_vm() { # <sock> <snap-dir> — balloon-shrink, pause, full snapshot
  local sock=$1 dir=$2
  mkdir -p "$dir"
  rm -f "$dir/vmstate" "$dir/mem"
  fc_api "$sock" PATCH /balloon "{\"amount_mib\":$BALLOON_MIB}"
  sleep 5                                   # let the guest reclaim
  fc_api "$sock" PATCH /balloon '{"amount_mib":0}'
  sleep 1                                   # never pause mid-inflation
  fc_api "$sock" PATCH /vm '{"state":"Paused"}'
  fc_api "$sock" PUT /snapshot/create \
    "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$dir/vmstate\",\"mem_file_path\":\"$dir/mem\"}"
}

resume_vm() { # <sock> <snap-dir> — load + resume in one call
  local sock=$1 dir=$2
  fc_api "$sock" PUT /snapshot/load \
    "{\"snapshot_path\":\"$dir/vmstate\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$dir/mem\"},\"resume_vm\":true}"
}

stop_fc() { kill "$1" 2>/dev/null || true; wait "$1" 2>/dev/null || true; }

fresh_rootfs() { cp --sparse=always "$1" "$2"; }   # CRITICAL: pristine disk per resume (§3.2 of the plan)

drop_caches() { sync; echo 3 > /proc/sys/vm/drop_caches; }

image_to_ext4() { # <docker-image> <out.ext4> [size_mb=1024]
  local image=$1 out=$2 size=${3:-1024} cid
  cid=$(docker create "$image")
  dd if=/dev/zero of="$out" bs=1M count="$size" status=none
  mkfs.ext4 -q -F "$out"
  mkdir -p /mnt/wake-rootfs
  mount -o loop "$out" /mnt/wake-rootfs
  docker export "$cid" | tar -xC /mnt/wake-rootfs
  umount /mnt/wake-rootfs
  docker rm "$cid" >/dev/null
}

percentiles() { # <file> <column>
  awk -v c="$2" '{print $c}' "$1" | sort -n | awk '
    { a[NR] = $1; s += $1 }
    END {
      if (NR == 0) { print "  no data"; exit 1 }
      p50 = int(NR * 0.50); if (p50 < 1) p50 = 1
      p90 = int(NR * 0.90); if (p90 < 1) p90 = 1
      p99 = int(NR * 0.99); if (p99 < 1) p99 = 1
      printf "  n=%d  min=%d  p50=%d  p90=%d  p99=%d  max=%d  mean=%.1f  (ms)\n",
        NR, a[1], a[p50], a[p90], a[p99], a[NR], s / NR
    }'
}
