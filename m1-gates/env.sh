# Shared configuration for the M1 acceptance gates. Sourced by every script.
ADMIN=http://127.0.0.1:9091
ROUTER=http://127.0.0.1:8080
KRILL_DATA=/srv/krill
IMAGES_DIR=$KRILL_DATA/images
GOLDEN=$IMAGES_DIR/gate.golden.ext4
KERNEL=/srv/fc/vmlinux              # installed by wake-bench/01-install-firecracker.sh
IDLE_TIMEOUT=15s                    # krilld flag; a2 waits this out
RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/results"
KRILLD_LOG=/var/log/krilld.log
mkdir -p "$RESULTS_DIR"
