# Shared configuration for the wake-path benchmark. Sourced by every script.
# Edit here, nowhere else.

FC_DIR=/srv/fc                 # kernel, rootfs images
SNAP_DIR=/srv/snaps            # snapshot output (vmstate + mem per guest)
STORM_DIR=/srv/storm           # per-VM rootfs copies for Phase 5
RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/results"

KERNEL="$FC_DIR/vmlinux"
GUEST_IP=172.16.0.2
HOST_IP=172.16.0.1
TAP=tap0
TAP_MAC="02:FC:AC:10:00:01"     # deterministic host-side MAC — restored guests' ARP caches must stay valid
GUEST_MAC="06:00:AC:10:00:02"

MEM_MIB=512                    # guest RAM
VCPUS=1
BALLOON_MIB=384                # inflate target before snapshot (reclaims free pages)
WARM_PINGS=20                  # requests before snapshotting
API_SOCK_DEFAULT=/tmp/fc-bench.sock

mkdir -p "$RESULTS_DIR"
