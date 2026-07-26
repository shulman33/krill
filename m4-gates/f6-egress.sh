#!/usr/bin/env bash
# F6 — Egress: apps stay silent, builders reach exactly one thing.
#
# The probes run FROM INSIDE a guest, because that is the only place the
# question means anything. The hostile example serves /runtime.json for
# exactly this: the same set of destinations, asked from an ordinary app.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

PROBE=${PROBE_APP:-hostile}
[ "$(id -u)" = "0" ] || note "not root: the ruleset and counter checks will be skipped"

echo "== F6: the ruleset that is actually loaded =="
if nft list table inet krill > "$RESULTS_DIR/f6-ruleset.txt" 2>/dev/null; then
  pass "inet krill is loaded ($(wc -l < "$RESULTS_DIR/f6-ruleset.txt") lines)"
  for want in 'app egress denied' 'guest->guest denied' 'smtp denied' \
              'guest->host denied' 'builder->everything else denied' '169.254.0.0/16'; do
    grep -q "$want" "$RESULTS_DIR/f6-ruleset.txt" \
      && pass "rule present: $want" \
      || fail "F6: the loaded ruleset has no '$want' rule"
  done
else
  note "cannot read nftables from here; run this on the box as root"
fi

echo
echo "== F6 (1): from an APP guest =="
admin "/v1/apps/$PROBE" >/dev/null 2>&1 || fail "F6: $PROBE is not deployed (see f5-builder.sh)"
RUNTIME=$(curl -s --max-time 120 -H "Host: $PROBE.$BASE_HOST" \
  http://127.0.0.1:8080/runtime.json) || fail "F6: could not reach the probe app"
printf '%s\n' "$RUNTIME" > "$RESULTS_DIR/f6-app-probes.json"
printf '%s\n' "$RUNTIME" | jq .

blocked() { # <key> <description>
  local v
  v=$(printf '%s' "$RUNTIME" | jq -r --arg k "$1" '.[$k] // "missing"')
  case "$v" in
    OPEN|1*|2*|[0-9]*.[0-9]*) echo "✗ an app guest reached $2 ($v)" >&2; return 1 ;;
    *) pass "app guest cannot reach $2 ($v)" ;;
  esac
}
BAD=0
blocked admin_api_127     "the admin API on loopback"          || BAD=1
blocked admin_api_gateway "the admin API via its gateway"      || BAD=1
blocked ssh_gateway       "sshd on the host"                   || BAD=1
blocked other_guest       "another app's guest"                || BAD=1
blocked metadata          "the cloud metadata address"         || BAD=1
blocked smtp_25           "SMTP 25"                            || BAD=1
blocked smtp_587          "SMTP 587"                           || BAD=1
blocked https_arbitrary   "arbitrary HTTPS"                    || BAD=1
blocked dns_resolve       "DNS resolution"                     || BAD=1
[ "$BAD" = "0" ] || fail "F6 FAIL: see above"

echo
echo "== F6: the product still works — inbound is unaffected =="
for a in "$APP" "$OTHER_APP"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 120 \
    -H "Host: $a.$BASE_HOST" http://127.0.0.1:8080/ || true)
  case "$code" in
    2*|3*) pass "$a still answers inbound through the router ($code)" ;;
    *) fail "F6 FAIL: the baseline broke inbound routing to $a ($code)" ;;
  esac
done

echo
echo "== F6 (2): from a BUILDER VM =="
note "the builder's own reachability is proved by F5: the hostile context's"
note "build-time probes ran inside a builder VM and found nothing, while the"
note "build itself pulled its base image — which is the registry allowance"
note "working. Read results/f5-probe-report.txt's BUILD-TIME section."
if [ -s "$RESULTS_DIR/f5-probe-report.txt" ]; then
  if grep -q '=== BUILD-TIME PROBES' "$RESULTS_DIR/f5-probe-report.txt"; then
    pass "the build-time probe report is present"
  fi
else
  note "no F5 report yet; run f5-builder.sh first"
fi

echo
echo "== F6 (3): the rate limit engages, and is observable =="
if [ "$(id -u)" = "0" ] && nft list table inet krill >/dev/null 2>&1; then
  nft list table inet krill | grep -E 'counter packets|limit rate' \
    > "$RESULTS_DIR/f6-counters-before.txt" || true
  note "counters captured before"
  note "To drive it: with --app-egress on, run a flood from a guest"
  note "(e.g. 'ping -f' or a tight curl loop) and diff the 'over limit' counter."
  nft list table inet krill | grep -A1 'over limit' || true
  note "A counter that moves is the observation F6 asks for; a rule that exists"
  note "is not. Record both numbers in the results file."
else
  note "skipped (needs root on the box)"
fi

echo
pass "F6 PASS for the default posture (apps silent). Record whether the rate"
pass "limit was actually driven past its threshold, or only asserted present."
