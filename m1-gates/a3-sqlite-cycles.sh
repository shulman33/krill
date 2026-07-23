#!/usr/bin/env bash
# GATE A3: 100 sleep/wake cycles of an app doing SQLite writes -> zero
# corruption, zero lost acked-before-sleep writes.
#
# Each cycle: POST /write (the 200 is the ack, sent only after COMMIT),
# assert the running count, freeze, wake via the next write. At the end the
# guest itself proves the ledger: PRAGMA integrity_check ok AND seq 1..N
# gapless — an acked write that vanished would leave a hole.
#
# usage: ./a3-sqlite-cycles.sh [cycles=100]
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh
N=${1:-100}
APP=gate-a3

register_app "$APP"
echo "== $N write -> freeze -> wake cycles =="
for i in $(seq 1 "$N"); do
  count=$(route "$APP" POST /write | jq -r .count)
  [ "$count" = "$i" ] || { echo; echo "FATAL: cycle $i acked count=$count (a previously acked write is missing)"; exit 1; }
  freeze_app "$APP"
  printf '\r  %d/%d' "$i" "$N" >&2
done
echo >&2

verdict=$(route "$APP" GET /verify)
echo "   guest verdict: $verdict"
integrity=$(jq -r .integrity_ok <<<"$verdict")
gapless=$(jq -r .gapless <<<"$verdict")
count=$(jq -r .count <<<"$verdict")

ok=0
[ "$integrity" = true ] && [ "$gapless" = true ] && [ "$count" = "$N" ] && ok=1
gate A3 "$ok" "$N cycles: integrity_check=$integrity, count=$count/$N, gapless=$gapless"
