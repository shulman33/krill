#!/usr/bin/env bash
# C1 — durability: acked writes survive total local-state loss.
# Deploy the ledger app, ack 200 rows through the router (sync-ack on, the
# D1 default), freeze, then destroy EVERYTHING local to the app — data disk,
# rootfs disk, FC snapshot, ship cursor — with krilld SIGKILLed. The object
# store alone must bring all 200 rows back (E6: fresh epoch, takeover seal,
# checkpoint + WAL-delta replay).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

echo "== C1: deploy ledger =="
krill delete ledger >/dev/null 2>&1 || true
out=$(krill deploy examples/ledger --name ledger --json)
echo "$out" | jq -e '.ready == true' >/dev/null || fail "C1: deploy not ready: $out"

echo "== C1: acking 200 rows through the router =="
for i in $(seq 1 200); do
  route ledger POST "/add?k=$i&v=val-$i" >/dev/null
done
before=$(ledger_digest ledger)
echo "before: $before"
[ "$(echo "$before" | cut -d' ' -f1)" = 200 ] || fail "C1: expected 200 rows before the kill"

echo "== C1: freeze, SIGKILL krilld, destroy all local app state =="
freeze_app ledger
head_before=$(stream_head ledger)
pkill -9 -x krilld; sleep 1
rm -f  "$KRILL_DATA/apps/ledger/data.ext4" \
       "$KRILL_DATA/apps/ledger/disk.ext4" \
       "$KRILL_DATA/apps/ledger/ship.json"
rm -rf "$KRILL_DATA/apps/ledger/snap"
ls -la "$KRILL_DATA/apps/ledger/" || true

echo "== C1: restart krilld, wake through the router =="
start_krilld
after=$(ledger_digest ledger)
echo "after:  $after"

[ "$before" = "$after" ] || fail "C1: state diverged across total local loss (before='$before' after='$after')"
grep -q "data disk rebuilt from object store" "$KRILLD_LOG" || fail "C1: no rebuild in the daemon log — did the wake really take the E6 path?"
head_after=$(stream_head ledger)
{ echo "rows=200 before='$before' after='$after' head_before=$head_before head_after=$head_after"; } > "$RESULTS_DIR/c1.txt"
pass "C1 durability: 200/200 acked rows recovered from the object store alone"
