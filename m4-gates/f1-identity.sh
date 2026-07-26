#!/usr/bin/env bash
# F1 — Identity at the edge: a stranger gets in, and the app knows who they are.
#
# The only gate with a human in the middle, because the human IS the test:
# "complete Google sign-in as an identity that has never touched this app"
# cannot be scripted without becoming a different test. So this script does
# the halves a script can do — mint the link, watch the ACL for the claim,
# then assert everything downstream of it — and asks for exactly one paste.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

APP_UNDER_TEST=${1:-$APP}

echo "== F1: mint a share link =="
read -r SHARE_ID LINK <<<"$(share "$APP_UNDER_TEST" use "F1 gate")"
pass "share $SHARE_ID created"
echo
echo "  Open this in a browser profile with NO session for this deployment,"
echo "  and sign in as an identity that has never touched this app:"
echo
echo "    $LINK"
echo
echo "  (F1 FAILs if the flow asks the recipient to create anything beyond"
echo "   signing in with Google.)"
echo

echo "== F1: waiting for the claim to land on the ACL =="
EMAIL=$(wait_for_claim "$APP_UNDER_TEST" "$SHARE_ID" 600) \
  || fail "F1: nobody claimed $SHARE_ID within 10 minutes"
pass "claimed by $EMAIL"
grants_for "$APP_UNDER_TEST" > "$RESULTS_DIR/f1-acl.json"

echo
echo "== F1: the session, for the gates that follow =="
echo "  In that browser's dev tools, copy the value of the cookie"
echo "  '$(cookie_name)' on $APP_UNDER_TEST.$BASE_HOST and paste it here."
echo "  (F2 and F3 reuse it; it is a real credential — results/ is gitignored.)"
printf '  session: '
read -r SESSION
[ -n "$SESSION" ] || fail "F1: no session pasted"
printf '%s' "$SESSION" > "$SESSION_FILE"

echo
echo "== F1: what the app is told about its caller =="
read -r STATUS USER PLANE TOKEN <<<"$(verify "$APP_UNDER_TEST" "$SESSION")"
[ "$STATUS" = "200" ] || fail "F1: the doorman refused the signed-in session (HTTP $STATUS)"
pass "forward_auth returns 200"

[ "$USER" = "$EMAIL" ] \
  || fail "F1: X-App-User is '$USER' but the ACL says '$EMAIL'"
pass "X-App-User = $USER"
[ "$PLANE" = "use" ] || fail "F1: plane is '$PLANE', expected 'use'"

[ "$TOKEN" != "-" ] || fail "F1: no X-Krill-Token — the app would be trusting a bare header"
CLAIMS=$(token_claims "$TOKEN")
printf '%s\n' "$CLAIMS" > "$RESULTS_DIR/f1-token-claims.json"
[ "$(printf '%s' "$CLAIMS" | jq -r .aud)" = "$APP_UNDER_TEST" ] \
  || fail "F1: the token's audience is not $APP_UNDER_TEST"
[ "$(printf '%s' "$CLAIMS" | jq -r .email)" = "$EMAIL" ] \
  || fail "F1: the token names someone other than the claimant"
pass "X-Krill-Token verifies as $EMAIL, audience $APP_UNDER_TEST, expiring $(printf '%s' "$CLAIMS" | jq -r .exp)"

echo
echo "== F1: the app's own view (it must VERIFY, not trust) =="
# watchlist exposes /whoami precisely so this assertion does not depend on
# reading HTML. An app that reported verified=false here would be an app
# trusting the header, which F1 explicitly does not accept.
WHO=$(curl -s --max-time 60 -H "Host: $APP_UNDER_TEST.$BASE_HOST" \
        -H "X-App-User: $USER" -H "X-Krill-Token: $TOKEN" \
        "http://127.0.0.1:8080/whoami" || true)
if printf '%s' "$WHO" | jq -e . >/dev/null 2>&1; then
  printf '%s\n' "$WHO" > "$RESULTS_DIR/f1-app-whoami.json"
  if [ "$(printf '%s' "$WHO" | jq -r .verified)" = "true" ]; then
    pass "the guest verified the token itself: $(printf '%s' "$WHO" | jq -r .email)"
  else
    fail "F1: the guest could NOT verify the token: $(printf '%s' "$WHO" | jq -r .why)
  (check that krilld runs with --identity-pubkey-file and the app was woken since)"
  fi
else
  note "the app has no /whoami; skipping the guest-side check (watchlist has one)"
fi

echo
pass "F1 PASS — record the wake latency the recipient actually saw, and any words they said"
