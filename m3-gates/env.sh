# Shared configuration for the M3 gates. Sourced by every script.
ADMIN=http://127.0.0.1:9091
ROUTER=http://127.0.0.1:8080
KRILL_DATA=/srv/krill
OBJSTORE="file://$KRILL_DATA/objstore"   # C1's survivor: never deleted by the gates
KERNEL=/srv/fc/vmlinux                   # installed by wake-bench/01-install-firecracker.sh
IDLE_TIMEOUT=15s
RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/results"
KRILLD_LOG=/var/log/krilld.log
mkdir -p "$RESULTS_DIR"
