#!/usr/bin/env bash
# B1 — one-command deploy of a real app.
# A plain app dir (Dockerfile + app.py, nothing krill-specific) becomes a
# running microVM app with a URL, in one command, ≤240s cold cache / ≤90s warm.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

krill delete guestbook >/dev/null 2>&1 || true

cold="cold"
docker image inspect python:3.12-slim >/dev/null 2>&1 && cold="warm"
echo "== B1: krill deploy examples/guestbook (docker cache: $cold) =="

t0=$(now_ms)
out=$(krill deploy examples/guestbook --name guestbook --json)
t1=$(now_ms)
elapsed_s=$(( (t1 - t0) / 1000 ))
echo "$out" > "$RESULTS_DIR/b1-deploy.json"

ready=$(echo "$out" | jq -r .ready)
url=$(echo "$out" | jq -r .url)
[ "$ready" = "true" ] || fail "B1: deploy did not report ready (see $RESULTS_DIR/b1-deploy.json)"
[ -n "$url" ] && [ "$url" != "null" ] || fail "B1: no URL in deploy response"

body=$(route guestbook GET /)
echo "$body" | jq -e '.app == "guestbook" and .version == "v1"' >/dev/null \
  || fail "B1: router response wrong: $body"

budget=240; [ "$cold" = "warm" ] && budget=90
echo "B1: deploy→ready in ${elapsed_s}s ($cold cache, budget ${budget}s); url=$url"
[ "$elapsed_s" -le "$budget" ] || fail "B1: ${elapsed_s}s over the ${budget}s budget"
{ echo "cache=$cold elapsed_s=$elapsed_s budget_s=$budget url=$url"; } > "$RESULTS_DIR/b1.txt"
pass "B1 one-command deploy (${elapsed_s}s, $cold cache)"
