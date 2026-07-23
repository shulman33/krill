#!/usr/bin/env bash
# GATE A2: the idle timeout demotes ACTIVE -> FROZEN unattended, and the RAM
# is actually freed (the VMM process is gone — a microVM's RAM cannot outlive
# its process).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh
APP=gate-a2

register_app "$APP"
echo "== waking $APP =="
route "$APP" GET /ping >/dev/null
[ "$(app_state "$APP")" = ACTIVE ] || { echo "FATAL: not ACTIVE after request"; exit 1; }

fc_procs() { pgrep -fc "fc.sock" || true; }
before_procs=$(fc_procs)
before_avail=$(awk '/MemAvailable/{print $2}' /proc/meminfo)
echo "   firecracker processes: $before_procs; MemAvailable: $((before_avail / 1024)) MiB"

# The whole point: nobody touches the app. IDLE_TIMEOUT + freeze duration
# (balloon settle 5s + deflate 1s + snapshot write) + janitor tick + margin.
wait_s=$(( ${IDLE_TIMEOUT%s} + 40 ))
echo "== hands off for up to ${wait_s}s (idle timeout $IDLE_TIMEOUT + freeze time) =="
wait_state "$APP" FROZEN "$wait_s"

after_procs=$(fc_procs)
after_avail=$(awk '/MemAvailable/{print $2}' /proc/meminfo)
echo "   firecracker processes: $after_procs; MemAvailable: $((after_avail / 1024)) MiB (freed ~$(( (after_avail - before_avail) / 1024 )) MiB)"

ok=1
[ "$after_procs" -lt "$before_procs" ] || ok=0
snap_valid=$(admin GET "/v1/apps/$APP" | jq -r .snapshot_valid)
[ "$snap_valid" = true ] || ok=0

# And it must come back: the demotion is a nap, not a death.
body=$(route "$APP" GET /ping)
echo "   post-freeze request answered: $body"

gate A2 "$ok" "unattended ACTIVE->FROZEN, VMM process gone ($before_procs -> $after_procs), snapshot valid, wakes again"
