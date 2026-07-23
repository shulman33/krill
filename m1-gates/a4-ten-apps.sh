#!/usr/bin/env bash
# GATE A4: 10 different apps resident as snapshots on one host, any of them
# wakeable. Identity is proven, not assumed: each app reports its own
# kernel-assigned IP (unique per subnet), so a routing mixup cannot pass.
#
# usage: ./a4-ten-apps.sh [apps=10]
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh
N=${1:-10}
OUT="$RESULTS_DIR/a4-wakes.txt"
: > "$OUT"

echo "== registering + freezing $N apps =="
for i in $(seq 1 "$N"); do
  app="gate-fleet-$i"
  register_app "$app"
  route "$app" GET /ping >/dev/null       # cold boot
  route "$app" POST /write >/dev/null     # give it distinct state
  freeze_app "$app"
  printf '\r  %d/%d frozen' "$i" "$N" >&2
done
echo >&2

frozen=$(admin GET /v1/apps | jq '[.[] | select(.name | startswith("gate-fleet-")) | select(.state == "FROZEN")] | length')
echo "   resident frozen fleet: $frozen/$N"

echo "== waking all $N in shuffled order, verifying identity =="
ok=1
for i in $(seq 1 "$N" | shuf); do
  app="gate-fleet-$i"
  expected_subnet=$(admin GET "/v1/apps/$app" | jq -r .subnet_idx)
  t0=$(now_ms)
  route "$app" GET /ping >/dev/null
  t1=$(now_ms)
  who=$(route "$app" GET /whoami | jq -r .ip)
  echo "$((t1 - t0))" >> "$OUT"
  if [ "$who" != "172.16.$expected_subnet.2/30" ]; then
    echo "   IDENTITY FAIL: $app answered as $who, expected subnet $expected_subnet"
    ok=0
  fi
  count=$(route "$app" GET /ping | jq -r .count)
  [ "$count" = 1 ] || { echo "   STATE FAIL: $app count=$count, want its own 1"; ok=0; }
  printf '  %-14s %4d ms  ip=%s count=%s\n' "$app" "$((t1 - t0))" "$who" "$count"
done

echo "== A4 wake latency across the fleet =="
percentiles "$OUT" 1
[ "$frozen" = "$N" ] || ok=0
gate A4 "$ok" "$frozen/$N frozen simultaneously, every app woke with its own identity and state"
