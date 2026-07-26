#!/usr/bin/env bash
# F5 — The builder is a guest, and a hostile build stays inside it.
#
# The hostile context arrives THROUGH THE DOORMAN on an edit-plane link, as a
# non-Sam capability. Over the tunnel is fine and correct: the threat is the
# build context, not the packet's origin. What must not happen is reaching
# `docker build` by calling the admin API directly — that path is Sam's and
# proves nothing.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

HOSTILE=${HOSTILE_APP:-hostile}
CONTEXT="$(cd "$(dirname "$0")" && pwd)/examples/hostile"
[ -d "$CONTEXT" ] || fail "F5: $CONTEXT is missing"
[ "$(id -u)" = "0" ] || note "not root: the host-inspection half will be partial"

echo "== F5: baseline — what exists on the host before the build =="
BEFORE_TAPS=$(ip -o link show 2>/dev/null | awk -F': ' '/krillb/{print $2}' | tr '\n' ' ')
BEFORE_FC=$(pgrep -c firecracker 2>/dev/null || echo 0)
note "builder taps: ${BEFORE_TAPS:-none}; firecracker processes: $BEFORE_FC"
ls -la /srv/krill/ > "$RESULTS_DIR/f5-host-before.txt" 2>/dev/null || true
sha256sum /etc/krill/*.json 2>/dev/null > "$RESULTS_DIR/f5-creds-before.txt" || true

echo
echo "== F5: the app must exist before it can be shared for edit =="
if ! admin "/v1/apps/$HOSTILE" >/dev/null 2>&1; then
  note "$HOSTILE is not deployed; creating it from the SAME context as Sam first"
  note "(the gate is about the SECOND deploy, which arrives over the network)"
  krill deploy "$CONTEXT" --name "$HOSTILE" --no-verify >/dev/null 2>&1 \
    || note "the operator-path deploy failed too; continuing — the network path is what is gated"
fi

echo
echo "== F5: deploy the hostile context through the doorman, on an edit link =="
read -r SID LINK <<<"$(share "$HOSTILE" edit "F5 gate")"
TOK=$(share_token "$LINK")
note "edit share $SID"

TARBALL="$RESULTS_DIR/f5-context.tar.gz"
tar -czf "$TARBALL" -C "$CONTEXT" .
START=$(date +%s)
CODE=$(curl -s -o "$RESULTS_DIR/f5-deploy-response.json" -w '%{http_code}' \
  --max-time 1200 -X POST \
  -H "Host: $HOSTILE.$BASE_HOST" \
  -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/gzip' \
  --data-binary "@$TARBALL" \
  "$DOORMAN_PUB/_krill/edit/deploy?verify=false")
ELAPSED=$(( $(date +%s) - START ))
note "HTTP $CODE in ${ELAPSED}s"

case "$CODE" in
  200|201)
    ISOLATED=$(jq -r '.isolated // false' "$RESULTS_DIR/f5-deploy-response.json")
    [ "$ISOLATED" = "true" ] \
      || fail "F5 FAIL: a network-arriving deploy built on the HOST (isolated=$ISOLATED)"
    pass "the build ran inside a microVM (isolated=true), $(jq -r '.build_secs' "$RESULTS_DIR/f5-deploy-response.json")s"
    ;;
  503)
    note "refused: $(jq -r .error "$RESULTS_DIR/f5-deploy-response.json" 2>/dev/null)"
    fail "F5: no isolated builder is configured. That refusal is the CORRECT
  fail-closed behavior and is not itself a pass — configure --build-vm-image
  and --build-vm-kernel (m4-gates/builder-image/README.md) and re-run."
    ;;
  422)
    note "the build failed inside the VM: $(jq -r .stage "$RESULTS_DIR/f5-deploy-response.json" 2>/dev/null)"
    note "$(jq -r '.build_log // ""' "$RESULTS_DIR/f5-deploy-response.json" 2>/dev/null | tail -20)"
    fail "F5: the hostile context did not build. It MUST build successfully —
  a context that fails to build proves nothing, because the probes never ran."
    ;;
  *) fail "F5: unexpected HTTP $CODE" ;;
esac
revoke_share "$SID" >/dev/null || true

echo
echo "== F5: what the build found =="
# The app serves its own probe report, which is why the evidence is readable
# without ssh.
krill wake "$HOSTILE" >/dev/null 2>&1 || true
curl -s --max-time 90 -H "Host: $HOSTILE.$BASE_HOST" http://127.0.0.1:8080/ \
  > "$RESULTS_DIR/f5-probe-report.txt" || note "could not fetch the probe report"
sed -n '1,80p' "$RESULTS_DIR/f5-probe-report.txt" 2>/dev/null || true

echo
echo "== F5: nothing the build reached should be real =="
FAILED=0
check_absent() { # <pattern> <description>
  if grep -qiE "$1" "$RESULTS_DIR/f5-probe-report.txt" 2>/dev/null; then
    echo "✗ the build reached: $2" >&2; FAILED=1
  else
    pass "unreachable: $2"
  fi
}
check_absent 'private_key|BEGIN [A-Z ]*PRIVATE KEY|service_account' "the host's GCS credentials"
check_absent '"name":|subnet_idx|snapshot_valid'                    "krilld's admin API"
check_absent 'SQLite format 3'                                      "the registry / another app's database"
check_absent 'WROTE /srv/krill|WROTE /host-root-pwned|WROTE THE CONTEXT DISK' "a writable host path"
check_absent 'SMTP (25|465|587|2525) OPEN'                          "SMTP"
check_absent 'krilld|firecracker|krill-doorman'                     "host processes"
[ "$FAILED" = "0" ] || fail "F5 FAIL: see the probe report above"

echo
echo "== F5: nothing outlived the build =="
AFTER_TAPS=$(ip -o link show 2>/dev/null | awk -F': ' '/krillb/{print $2}' | tr '\n' ' ')
[ "$AFTER_TAPS" = "$BEFORE_TAPS" ] \
  || fail "F5 FAIL: builder taps survived the deploy (before: '${BEFORE_TAPS:-none}', after: '$AFTER_TAPS')"
pass "no builder tap left behind"
LEFTOVER=$(ls -d /srv/krill/build/buildvm-* 2>/dev/null | wc -l | tr -d ' ')
[ "$LEFTOVER" = "0" ] || fail "F5 FAIL: $LEFTOVER builder scratch dir(s) survived"
pass "no builder scratch left behind"
for f in /srv/krill/PWNED /host-root-pwned /srv/krill/build/PERSIST; do
  [ -e "$f" ] && fail "F5 FAIL: the build persisted $f"
done
pass "nothing persisted onto the host"
if [ -s "$RESULTS_DIR/f5-creds-before.txt" ]; then
  sha256sum /etc/krill/*.json 2>/dev/null > "$RESULTS_DIR/f5-creds-after.txt" || true
  diff "$RESULTS_DIR/f5-creds-before.txt" "$RESULTS_DIR/f5-creds-after.txt" >/dev/null \
    || fail "F5 FAIL: the host's credentials changed during the build"
  pass "host credentials unmodified"
fi

echo
echo "== F5: a hung build is killed by the host's clock =="
note "not run automatically — it costs a full --build-timeout."
note "To run it: add 'RUN sleep 3600' to a copy of the hostile Dockerfile,"
note "deploy it the same way, and confirm the deploy returns a timeout error"
note "and that no firecracker process or krillb* tap survives."

echo
echo "== F5: a normal deploy still works, and how long it takes =="
START=$(date +%s)
krill deploy examples/watchlist --name "${APP}" --json >/dev/null 2>&1 \
  && pass "normal deploy still works ($(( $(date +%s) - START ))s — record it beside the M2/metal numbers)" \
  || note "the normal deploy did not complete; investigate before recording a PASS"

echo
pass "F5 PASS"
