# Shared configuration for the M1 acceptance gates. Sourced by every script.
#
# Every value honors an environment override, because these gates were written
# for a throwaway bench box and now have to run on one that hosts production:
# point KRILL_DATA at a scratch directory and the gate daemon never touches the
# real registry, app disks, or object store.
#
#   KRILL_DATA=/srv/krill-gates KRILLD_EXTRA_FLAGS='--data-plane=false' ./00-setup.sh
#
ADMIN=${ADMIN:-http://127.0.0.1:9091}
ROUTER=${ROUTER:-http://127.0.0.1:8080}
KRILL_DATA=${KRILL_DATA:-/srv/krill}
IMAGES_DIR=$KRILL_DATA/images
GOLDEN=$IMAGES_DIR/gate.golden.ext4
KERNEL=${KERNEL:-/srv/fc/vmlinux}   # installed by wake-bench/01-install-firecracker.sh
IDLE_TIMEOUT=${IDLE_TIMEOUT:-15s}   # krilld flag; a2 waits this out
# Extra krilld flags for the gate daemon. A1's latency is only comparable
# across tiers when the data plane is configured the same way it was on the
# tier being compared to — so state it explicitly rather than inheriting
# whatever krilld's defaults have become since the last run.
KRILLD_EXTRA_FLAGS=${KRILLD_EXTRA_FLAGS:-}
RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/results"
KRILLD_LOG=/var/log/krilld.log
mkdir -p "$RESULTS_DIR"
