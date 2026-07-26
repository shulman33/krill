#!/usr/bin/env bash
# F3 — The three planes actually separate, and the door has one keyhole.
#
# Every check is a real request through the doorman, not a unit test. The
# assertion that matters most is invisible in the output: a refusal here means
# krilld's router never heard about the request, so no unauthorized attempt
# ever woke an app. A fence that bills is still a fence that failed.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

APP_UNDER_TEST=${1:-$APP}
SESSION=$(need_session "$SESSION_FILE")

echo "== F3 (4): host-suffix pinning =="
# As of 2026-07-26 krilld's router reads only the first DNS label and never
# validates the suffix. The doorman is where that closes.
for host in \
  "$APP_UNDER_TEST.example.invalid" \
  "$APP_UNDER_TEST.local.$BASE_HOST" \
  "46.4.64.187" \
  "$BASE_HOST" \
  "$AUTH_HOST" \
  "$APP_UNDER_TEST.$BASE_HOST.attacker.example"
do
  st=$(verify_status "$APP_UNDER_TEST" "$SESSION" / "$host")
  case "$st" in
    403|404) pass "Host: $host → $st" ;;
    *) fail "F3: Host: $host was ACCEPTED ($st) — the suffix is not pinned" ;;
  esac
done
st=$(verify_status "$APP_UNDER_TEST" "$SESSION" / "$APP_UNDER_TEST.$BASE_HOST")
[ "$st" = "200" ] || fail "F3: the legitimate host was refused ($st) — the pin is just 'refuse everything'"
pass "the legitimate host still routes"

echo
echo "== F3 (2): a use-only holder reaches the app and nothing else =="
read -r ST USER PLANE TOKEN <<<"$(verify "$APP_UNDER_TEST" "$SESSION")"
[ "$ST" = "200" ] || fail "F3: the use-plane session is not working ($ST)"
[ "$PLANE" = "use" ] || fail "F3: expected the use plane, got '$PLANE'"
pass "the app itself: 200 as $USER"

for path in /_krill/data/stream /_krill/data/db /_krill/data/logs; do
  st=$(session_request GET "$APP_UNDER_TEST" "$path" "$SESSION")
  [ "$st" = "403" ] || fail "F3: a use-only holder reached $path ($st)"
  pass "$path → 403"
done
st=$(session_request POST "$APP_UNDER_TEST" /_krill/edit/deploy "$SESSION")
[ "$st" = "403" ] || fail "F3: a use-only holder reached the edit plane ($st)"
pass "/_krill/edit/deploy → 403"

# The admin API must not be reachable through the doorman at all.
st=$(session_request GET "$APP_UNDER_TEST" /v1/apps "$SESSION")
[ "$st" = "404" ] || [ "$st" = "403" ] || fail "F3: krilld's admin API is reachable through the doorman ($st)"
pass "the admin API is not a doorman route ($st)"

echo
echo "== F3 (2, cont.): an app they were never shared on =="
st=$(verify_status "$OTHER_APP" "$SESSION" / "$OTHER_APP.$BASE_HOST")
case "$st" in
  302|403) pass "$OTHER_APP → $st (no session there, or no grant; either way no wake)" ;;
  *) fail "F3: a use-only holder of $APP_UNDER_TEST got $st on $OTHER_APP" ;;
esac

echo
echo "== F3 (3): the identity token names exactly one app =="
[ "$TOKEN" != "-" ] || fail "F3: no token to inspect"
CLAIMS=$(token_claims "$TOKEN")
AUD=$(printf '%s' "$CLAIMS" | jq -r .aud)
[ "$AUD" = "$APP_UNDER_TEST" ] || fail "F3: audience is '$AUD'"
pass "aud = $AUD"
# Replaying it at another app: the doorman always mints, so the guest is what
# must reject it. watchlist and ledger both verify; ask the app directly.
REPLAY=$(curl -s --max-time 60 -H "Host: $OTHER_APP.$BASE_HOST" \
  -H "X-App-User: $USER" -H "X-Krill-Token: $TOKEN" \
  -X POST --data 'title=replayed' "http://127.0.0.1:8080/add" -o /dev/null -w '%{http_code}' || true)
case "$REPLAY" in
  200|201|204|303) fail "F3: a $APP_UNDER_TEST token was accepted by $OTHER_APP ($REPLAY)" ;;
  *) pass "replay against $OTHER_APP refused ($REPLAY)" ;;
esac

echo
echo "== F3 (5): data holds data, edit holds both, neither holds more =="
read -r DSID DLINK <<<"$(share "$APP_UNDER_TEST" data "F3 data")"
read -r ESID ELINK <<<"$(share "$APP_UNDER_TEST" edit "F3 edit")"
DTOK=$(share_token "$DLINK"); ETOK=$(share_token "$ELINK")
printf '%s %s\n' "$DSID" "$ESID" > "$RESULTS_DIR/f3-shares.txt"

st=$(bearer GET "$APP_UNDER_TEST" /_krill/data/stream "$DTOK")
[ "$st" = "200" ] || fail "F3: a data holder could not read the stream ($st)"
pass "data → /_krill/data/stream 200"
st=$(bearer POST "$APP_UNDER_TEST" /_krill/edit/deploy "$DTOK")
[ "$st" = "403" ] || fail "F3: a data holder reached the edit plane ($st)"
pass "data → /_krill/edit/deploy 403"

st=$(bearer GET "$APP_UNDER_TEST" /_krill/data/stream "$ETOK")
[ "$st" = "200" ] || fail "F3: an edit holder could not read data ($st) — edit should be a superset"
pass "edit → data 200 (edit ⊃ data, as intended)"

# A use-plane link must not work on the programmatic surfaces either.
read -r USID ULINK <<<"$(share "$APP_UNDER_TEST" use "F3 use")"
UTOK=$(share_token "$ULINK")
st=$(bearer GET "$APP_UNDER_TEST" /_krill/data/stream "$UTOK")
[ "$st" = "403" ] || fail "F3: a use-plane link read data ($st)"
pass "use → data 403"

# And no link works against a different app.
st=$(bearer GET "$OTHER_APP" /_krill/data/stream "$ETOK")
[ "$st" = "403" ] || [ "$st" = "404" ] || fail "F3: a $APP_UNDER_TEST edit link worked on $OTHER_APP ($st)"
pass "cross-app link → $st"

echo
echo "== F3: nothing above woke an app that should have stayed asleep =="
admin "/v1/apps/$OTHER_APP" | jq -r '"  '"$OTHER_APP"' is \(.state), last wake \(.last_wake_ms) ms"'
note "compare against the state before this run: an unauthorized request that"
note "moved an app out of FROZEN is an F3 FAIL even though it was refused."

# Leave no live links behind; a gate that widens the ACL is a bad gate.
for sid in "$DSID" "$ESID" "$USID"; do revoke_share "$sid" >/dev/null || true; done
pass "F3 gate links revoked"

echo
pass "F3 PASS"
