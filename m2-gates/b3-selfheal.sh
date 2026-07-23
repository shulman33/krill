#!/usr/bin/env bash
# B3 — the self-heal loop: a broken app explains itself through the deploy
# response and the logs tool, structured; a fixed redeploy comes back ready.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

krill delete brokenapp >/dev/null 2>&1 || true
echo "== B3: deploy examples/broken (import-time NameError) =="

set +e
out=$(krill deploy examples/broken --name brokenapp --json)
rc=$?
set -e
echo "$out" > "$RESULTS_DIR/b3-deploy.json"
[ $rc -ne 0 ] || fail "B3: deploy of a crashing app must exit nonzero"
echo "$out" | jq -e '.ready == false' >/dev/null || fail "B3: ready must be false"

# The deploy response itself carries the traceback, structured.
echo "$out" | jq -e '.errors | map(.kind) | index("python_traceback") != null' >/dev/null \
  || fail "B3: deploy response has no python_traceback (errors: $(echo "$out" | jq -c .errors))"
echo "$out" | jq -r '.errors[] | select(.kind=="python_traceback") | .message' | grep -q "NameError" \
  || fail "B3: traceback message lost the NameError"

# The logs tool returns the same, with the offending file+line in the tail.
logs=$(krill logs brokenapp --json)
echo "$logs" > "$RESULTS_DIR/b3-logs.json"
echo "$logs" | jq -e '.errors | map(.kind) | index("python_traceback") != null' >/dev/null \
  || fail "B3: logs endpoint missing the structured traceback"
echo "$logs" | jq -r '.errors[] | select(.kind=="python_traceback") | .detail' | grep -q 'app.py' \
  || fail "B3: traceback detail lost the offending file"

# Fix (deploy the working app to the same name) → ready.
out=$(krill deploy examples/guestbook --name brokenapp --json)
echo "$out" | jq -e '.ready == true' >/dev/null || fail "B3: fixed redeploy not ready"
route brokenapp GET / | jq -e '.app == "guestbook"' >/dev/null || fail "B3: fixed app not serving"

krill delete brokenapp >/dev/null 2>&1 || true
pass "B3 self-heal (structured traceback in deploy response + logs tool)"
