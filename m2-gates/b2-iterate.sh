#!/usr/bin/env bash
# B2 — iterate: redeploy replaces the app; snapshots never serve stale code;
# app data resets on redeploy (asserted, because it's the documented M2
# contract, not an accident).
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

# Precondition: guestbook v1 up (B1). Write a row, then freeze → valid v1 snapshot.
echo "== B2: iterate on guestbook =="
route guestbook GET / | jq -e '.version == "v1"' >/dev/null || fail "B2: precondition — run b1 first"
route guestbook POST /guests -H 'Content-Type: application/json' --data '{"name":"alice"}' >/dev/null
route guestbook GET / | jq -e '.guests == ["alice"]' >/dev/null || fail "B2: seed write lost"
freeze_app guestbook

# v2 source: same app, bumped marker.
v2dir=$(mktemp -d)
cp -r examples/guestbook/. "$v2dir/"
sed -i 's/VERSION = "v1"/VERSION = "v2"/' "$v2dir/app.py"
grep -q 'VERSION = "v2"' "$v2dir/app.py" || fail "B2: sed failed"

t0=$(now_ms)
out=$(krill deploy "$v2dir" --name guestbook --json)
t1=$(now_ms)
elapsed_s=$(( (t1 - t0) / 1000 ))
echo "$out" > "$RESULTS_DIR/b2-deploy.json"
echo "$out" | jq -e '.ready == true and .created == false' >/dev/null \
  || fail "B2: redeploy not ready/created-flag wrong"

# THE assertion: the first response after redeploy is v2 — the v1 snapshot
# (which had alice in it) must never answer. And the data reset is explicit.
body=$(route guestbook GET /)
echo "$body" | jq -e '.version == "v2"' >/dev/null || fail "B2: stale code served: $body"
echo "$body" | jq -e '.guests == []' >/dev/null || fail "B2: old data survived redeploy (M2 contract says it must not): $body"

# Freeze → wake again: the NEW snapshot pairs with the NEW disk.
freeze_app guestbook
route guestbook GET / | jq -e '.version == "v2"' >/dev/null || fail "B2: post-freeze wake served stale code"

echo "B2: redeploy→ready in ${elapsed_s}s (budget 90s warm)"
[ "$elapsed_s" -le 90 ] || fail "B2: ${elapsed_s}s over the 90s budget"
{ echo "elapsed_s=$elapsed_s"; } > "$RESULTS_DIR/b2.txt"
rm -rf "$v2dir"
pass "B2 iterate (${elapsed_s}s, snapshot invalidation + data reset verified)"
