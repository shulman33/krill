#!/usr/bin/env bash
# C4 — the spec is the oracle. Pure Go, runs anywhere with a Go toolchain
# (typically the dev laptop, not the KVM box): 10,000 seeded schedules of
# the REAL gateway/manifest/replay code under crash/partition/pause
# injection with all fences on must violate nothing; each fence disabled in
# turn must produce its TLC counterexample (PT-1, the E6 bug, PT-9).
set -euo pipefail
cd "$(dirname "$0")/.."

SEEDS=${KRILL_SIM_SEEDS:-10000}
echo "== C4: positive + three negative configs, $SEEDS seeds each =="
KRILL_SIM_SEEDS=$SEEDS go test ./internal/dataplane/sim/ \
  -run 'TestPositiveInvariants|TestNegativeGatewayFencing|TestNegativeReplayOnRestore|TestNegativeRegistrationFencing' \
  -v -timeout 60m 2>&1 | tee m3-gates/results/c4.txt | grep -E '^(=== RUN|--- (PASS|FAIL)|ok|FAIL)'

grep -q -- '--- FAIL' m3-gates/results/c4.txt && { echo "== FAIL: C4 =="; exit 1; }
echo "== PASS: C4 — $SEEDS clean positive seeds; all three disabled fences produced violations =="
