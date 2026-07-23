# Shared configuration for the M2 gates. Sourced by every script.
ADMIN=http://127.0.0.1:9091
ROUTER=http://127.0.0.1:8080
KRILL_DATA=/srv/krill
KERNEL=/srv/fc/vmlinux              # installed by wake-bench/01-install-firecracker.sh
IDLE_TIMEOUT=15s                    # short so B2 can exercise freeze/wake quickly
RESULTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/results"
KRILLD_LOG=/var/log/krilld.log
mkdir -p "$RESULTS_DIR"
