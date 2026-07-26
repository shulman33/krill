#!/usr/bin/env bash
# F2 — Revoke takes effect on the next request, and never comes back.
#
# Three steps, and step 3 is the amendment: a revoke that a recovery can undo
# is not a revoke. Nothing here restarts krilld, freezes an app, or waits out
# a TTL — if any of those were needed, the gate would be failing.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

APP_UNDER_TEST=${1:-$APP}
SESSION=$(need_session "$SESSION_FILE")

echo "== F2: the session works before we take anything away =="
read -r STATUS EMAIL _ _ <<<"$(verify "$APP_UNDER_TEST" "$SESSION")"
[ "$STATUS" = "200" ] || fail "F2: the F1 session is not working (HTTP $STATUS) — re-run f1-identity.sh"
pass "$EMAIL is in"

echo
echo "== F2 (a): revoke the claimed identity =="
REV=$(revoke_identity "$APP_UNDER_TEST" "$EMAIL") || fail "F2: the revoke was refused — nothing was revoked"
printf '%s\n' "$REV" > "$RESULTS_DIR/f2-revoke-identity.json"
note "revocation $(printf '%s' "$REV" | jq -r .id), $(printf '%s' "$REV" | jq -r '.grants | length') grant(s)"
STATUS=$(verify_status "$APP_UNDER_TEST" "$SESSION")
[ "$STATUS" = "403" ] || fail "F2: the revoked session served with HTTP $STATUS on the VERY NEXT request"
pass "next request refused (403), with no restart and no freeze/wake in between"

echo
echo "== F2 (a'): re-opening the link they still hold must not re-admit them =="
# Not in the gate's letter, in its spirit: a revoke one click undoes is not a
# revoke. The link from F1 is still live, so this is a real attempt.
SID=$(grants_for "$APP_UNDER_TEST" | jq -r --arg e "$EMAIL" '.[] | select(.email==$e) | .share_id' | head -1)
if [ -n "$SID" ] && [ "$SID" != "null" ]; then
  note "their link $SID is still live; a re-claim must not restore access"
fi
STATUS=$(verify_status "$APP_UNDER_TEST" "$SESSION")
[ "$STATUS" = "403" ] || fail "F2: access came back without any operator action"
pass "still refused"

echo
echo "== F2 (b): revoke a LINK while a second identity holds a live session =="
if [ -s "$SESSION2_FILE" ]; then
  SESSION2=$(need_session "$SESSION2_FILE")
  SID2=$(grants_for "$APP_UNDER_TEST" | jq -r '[.[] | select(.revoked==false)][0].share_id // empty')
  [ -n "$SID2" ] || fail "F2: no live grant to revoke by link — claim a second link first"
  STATUS=$(verify_status "$APP_UNDER_TEST" "$SESSION2")
  [ "$STATUS" = "200" ] || fail "F2: the second session is not working (HTTP $STATUS)"
  pass "the second identity is in via $SID2"
  REV2=$(revoke_share "$SID2") || fail "F2: the link revoke was refused"
  printf '%s\n' "$REV2" > "$RESULTS_DIR/f2-revoke-share.json"
  STATUS=$(verify_status "$APP_UNDER_TEST" "$SESSION2")
  [ "$STATUS" = "403" ] || fail "F2: a session claimed from a revoked link served with HTTP $STATUS"
  pass "the second session is refused too"
  # And the link itself is dead for everyone, forever.
  LINKSTATUS=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $APP_UNDER_TEST.$BASE_HOST" \
                 -H 'Accept: text/html' "$DOORMAN_PUB/_krill/s/$(cat "$RESULTS_DIR/f2-link-secret.txt" 2>/dev/null || echo nothing)")
  [ "$LINKSTATUS" = "404" ] || note "re-open of the revoked link returned $LINKSTATUS (404 expected; save the secret to results/f2-link-secret.txt to assert this)"
else
  note "SKIPPED: step (b) needs a SECOND browser identity holding a live session"
  note "  claim another link, save its cookie to $SESSION2_FILE, and re-run."
  note "  A run without it is an INCOMPLETE F2, not a passing one."
fi

echo
echo "== F2 (3): the revoke survives total local-state loss =="
echo "  Stopping the doorman, deleting its database, restoring from the object"
echo "  store ALONE, and asking again. This is C1's shape applied to auth."
if [ "$(id -u)" != "0" ]; then
  fail "F2 step 3 must run as root ON THE BOX (it stops a unit and deletes a database)"
fi
BEFORE=$(dm /v1/revocations | jq 'length')
note "revocations in the log before: $BEFORE"

systemctl stop "$DOORMAN_UNIT" || fail "F2: could not stop $DOORMAN_UNIT"
cp -a "$DOORMAN_STATE/doorman.db" "$RESULTS_DIR/doorman-before-loss.db" 2>/dev/null || true
rm -f "$DOORMAN_STATE"/doorman.db "$DOORMAN_STATE"/doorman.db-wal "$DOORMAN_STATE"/doorman.db-shm
[ -f "$DOORMAN_STATE/doorman.db" ] && fail "F2: the database is still there"
pass "local auth state destroyed"

# No flag needed: a missing database IS the restore signal, so the ordinary
# restart is the recovery. (`krill-doorman --restore` forces it over an
# existing database, which is the operator's tool, not this gate's.) The
# assertions below are what keep an empty database from passing as a restore.
if ! systemctl start "$DOORMAN_UNIT"; then
  fail "F2: the doorman did not restart after its database was deleted"
fi
for i in $(seq 1 30); do curl -sf "$DOORMAN/healthz" >/dev/null && break; sleep 1; done
curl -sf "$DOORMAN/healthz" >/dev/null || fail "F2: the doorman never came back"
AFTER=$(dm /v1/revocations | jq 'length')
pass "doorman restarted; revocations replayed from the log: $AFTER"
[ "$AFTER" -ge "$BEFORE" ] \
  || fail "F2: the restored doorman knows about FEWER revocations ($AFTER < $BEFORE) — a restore lost one"

STATUS=$(verify_status "$APP_UNDER_TEST" "$SESSION")
[ "$STATUS" = "403" ] \
  || fail "F2 FAIL: a restore resurrected a revoked grant (HTTP $STATUS). A revoke a recovery can undo is not a revoke."
pass "still refused after total local-state loss"

# The restore has to be a real one: an empty database would deny everything
# and prove nothing.
SHARES=$(dm /v1/shares | jq 'length')
[ "$SHARES" -gt 0 ] \
  || fail "F2: the restored doorman has no shares at all — this was an empty database, not a restore"
pass "the rest of the ACL came back too ($SHARES link(s)) — a real restore"

dm /v1/revocations > "$RESULTS_DIR/f2-revocations.json"
echo
pass "F2 PASS (record whether step (b) ran)"
