#!/usr/bin/env bash
# C2 — fencing: stale epochs cannot write, register, or seal.
# fencetool speaks the data plane's own wire format and attempts a zombie's
# two moves (stale segment append = PT-1, stale checkpoint registration =
# PT-2/PT-9) against the live stream. Both must be rejected with the
# manifest byte-identical; real takeovers must leave a monotone epoch trail
# with a seal record each.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

echo "== C2: deploy ledger + a few writes =="
krill delete ledger >/dev/null 2>&1 || true
krill deploy examples/ledger --name ledger --json | jq -e '.ready == true' >/dev/null || fail "C2: deploy failed"
for i in $(seq 1 5); do route ledger POST "/add?k=$i&v=x" >/dev/null; done
freeze_app ledger   # quiescent stream for the probe

echo "== C2: stale-epoch attempts =="
fencetool -app ledger dump > "$RESULTS_DIR/c2-manifest-before.json"
fencetool -app ledger stale-append   || fail "C2: stale append was not fenced"
fencetool -app ledger stale-register || fail "C2: stale registration was not fenced"
fencetool -app ledger dump > "$RESULTS_DIR/c2-manifest-after.json"
diff "$RESULTS_DIR/c2-manifest-before.json" "$RESULTS_DIR/c2-manifest-after.json" >/dev/null \
  || fail "C2: a fenced attempt mutated the manifest"

echo "== C2: two real takeovers (wake/freeze cycles) =="
route ledger GET / >/dev/null; freeze_app ledger
route ledger GET / >/dev/null; freeze_app ledger

echo "== C2: invariants off the real manifest =="
fencetool -app ledger check || fail "C2: CheckManifest failed"
m=$(fencetool -app ledger dump)
seals=$(echo "$m" | jq '.seals | length')
[ "$seals" -ge 3 ] || fail "C2: expected >=3 takeover seals, found $seals"
echo "$m" | jq -e '[.seals[].epoch] as $e | $e == ($e | sort)' >/dev/null || fail "C2: seal epochs not monotone"
echo "$m" | jq -e '[.segments[].epoch] as $e | $e == ($e | sort)' >/dev/null || fail "C2: segment epochs not monotone (I1)"
echo "$m" > "$RESULTS_DIR/c2-manifest-final.json"
{ echo "seals=$seals stale_append=FENCED stale_register=FENCED manifest_unchanged=yes"; } > "$RESULTS_DIR/c2.txt"
pass "C2 fencing: zombie writes rejected, manifest untouched, $seals monotone takeover seals"
