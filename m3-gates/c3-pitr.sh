#!/usr/bin/env bash
# C3 — PITR is branching, never truncation.
# Phase A (100 rows), note the head LSN; phase B (100 more); restore to the
# phase-A point → the app serves A only and the parent stream s0 is
# byte-identical (modulo the informational branch list). Then restore BACK
# to s0's head (--from-stream s0) → phase B returns: nothing was truncated.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

echo "== C3: deploy ledger =="
krill delete ledger >/dev/null 2>&1 || true
krill deploy examples/ledger --name ledger --json | jq -e '.ready == true' >/dev/null || fail "C3: deploy failed"

echo "== C3: phase A (100 rows) =="
for i in $(seq 1 100); do route ledger POST "/add?k=$i&v=A" >/dev/null; done
digestA=$(ledger_digest ledger)
lsnA=$(stream_head ledger)
echo "phase A: $digestA at LSN $lsnA"

echo "== C3: phase B (100 more rows) =="
for i in $(seq 101 200); do route ledger POST "/add?k=$i&v=B" >/dev/null; done
digestAB=$(ledger_digest ledger)
freeze_app ledger
headS0=$(stream_head ledger)
echo "phase A+B: $digestAB at LSN $headS0"

fencetool -app ledger -stream s0 dump | jq 'del(.branches)' > "$RESULTS_DIR/c3-s0-before.json"

echo "== C3: restore to the phase-A point (LSN $lsnA) =="
krill restore ledger --at-lsn "$lsnA" | tee "$RESULTS_DIR/c3-restore1.json" | jq -e '.stream == "s1"' >/dev/null \
  || fail "C3: restore did not land on branch s1"
gotA=$(ledger_digest ledger)
[ "$gotA" = "$digestA" ] || fail "C3: branch state != phase A (got '$gotA', want '$digestA')"

echo "== C3: parent stream s0 untouched =="
fencetool -app ledger -stream s0 dump | jq 'del(.branches)' > "$RESULTS_DIR/c3-s0-after.json"
diff "$RESULTS_DIR/c3-s0-before.json" "$RESULTS_DIR/c3-s0-after.json" >/dev/null \
  || fail "C3: branching mutated the parent stream"

echo "== C3: restore BACK to s0's head (phase B recoverable forever) =="
freeze_app ledger 2>/dev/null || true
krill restore ledger --at-lsn "$headS0" --from-stream s0 >/dev/null
gotAB=$(ledger_digest ledger)
[ "$gotAB" = "$digestAB" ] || fail "C3: could not restore back to phase A+B (got '$gotAB', want '$digestAB')"

{ echo "lsnA=$lsnA headS0=$headS0 digestA_ok=yes parent_untouched=yes roundtrip_ok=yes"; } > "$RESULTS_DIR/c3.txt"
pass "C3 PITR: branch served phase A, parent unchanged, phase B recovered from s0"
