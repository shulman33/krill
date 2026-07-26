#!/usr/bin/env bash
# F7 — Exposure: the door opens, and only the door.
#
# ⚠ RUN THIS FROM THE MAC, NOT FROM THE BOX. The outside view is the point:
# a port scan from localhost proves nothing about what the internet can reach.
#
# This is the last gate of the milestone. Everything F1-F3, F5 and F6 asserted
# was true before it ran and must remain true after — the ordering rule exists
# because a green F4 obtained by exposing the router early is a failed
# milestone, not an early one.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

IP=${BOX_IP:-46.4.64.187}
APP_UNDER_TEST=${1:-$APP}
require curl

echo "== F7 (3): the port scan, with the expectation inverted for exactly two =="
# Phase 8's "all four closed" assertion is superseded HERE and nowhere else.
scan() { # <port> <want: open|closed>
  local port=$1 want=$2 state
  if nc -z -G 5 -w 5 "$IP" "$port" >/dev/null 2>&1; then state=open; else state=closed; fi
  if [ "$state" = "$want" ]; then
    pass "port $port is $state (expected)"
  else
    fail "F7 FAIL: port $port is $state, expected $want"
  fi
}
scan 80  open      # redirects to 443
scan 443 open      # the doorman, behind Caddy
scan 8080 closed   # krilld's router: it never un-loopbacks
scan 9091 closed   # krilld's admin API
scan 9092 closed   # the doorman's control API
scan 9090 closed   # the doorman's own listener
scan 22  open      # ssh

echo
echo "== F7 (1): a real wildcard certificate =="
CERT=$(echo | openssl s_client -servername "$APP_UNDER_TEST.$BASE_HOST" \
  -connect "$APP_UNDER_TEST.$BASE_HOST:443" 2>/dev/null | openssl x509 -noout -text 2>/dev/null) \
  || fail "F7: no certificate served for $APP_UNDER_TEST.$BASE_HOST"
printf '%s\n' "$CERT" > "$RESULTS_DIR/f7-cert.txt"
printf '%s' "$CERT" | grep -q "DNS:\*\.$BASE_HOST" \
  || fail "F7: the certificate does not cover *.$BASE_HOST"
pass "certificate covers *.$BASE_HOST"
ISSUER=$(printf '%s' "$CERT" | awk -F'Issuer: ' '/Issuer:/{print $2; exit}')
note "issuer: $ISSUER"
case "$ISSUER" in
  *STAGING*|*Fake*|*Pretend*) fail "F7: this is an ACME STAGING certificate — a browser will warn, and F4 FAILs on any warning" ;;
esac
NOTAFTER=$(printf '%s' "$CERT" | awk -F'Not After : ' '/Not After/{print $2; exit}')
note "expires: $NOTAFTER"

# A browser must be happy without flags: that is what F4 depends on.
curl -sS --max-time 20 -o /dev/null "https://$APP_UNDER_TEST.$BASE_HOST/" \
  || fail "F7: curl could not complete a TLS handshake without extra flags"
pass "TLS validates against the system trust store"

echo
echo "== F7 (3): 80 redirects to 443 =="
LOC=$(curl -s -o /dev/null -D - --max-time 20 "http://$APP_UNDER_TEST.$BASE_HOST/" \
  | tr -d '\r' | awk 'BEGIN{IGNORECASE=1} /^location:/{print $2; exit}')
case "$LOC" in
  https://*) pass "80 → $LOC" ;;
  *) fail "F7: http did not redirect to https (Location: '${LOC:-none}')" ;;
esac

echo
echo "== F7: the admin API is unreachable from the internet, by two mechanisms =="
# The firewall must not be the only thing keeping it private: the listener
# bindings and ufw have to agree.
for port in 9091 9092 9090 8080; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 6 "http://$IP:$port/healthz" || echo 000)
  [ "$code" = "000" ] || fail "F7 FAIL: something answered on $IP:$port (HTTP $code)"
done
pass "nothing answers on 8080/9090/9091/9092 from outside"
note "ALSO CHECK ON THE BOX: 'ss -ltnp' must show these bound to 127.0.0.1,"
note "not 0.0.0.0. ufw agreeing with the bindings is the assertion; ufw alone is not."

echo
echo "== F7: every F1-F3 result still holds against the PUBLIC listener =="
echo "  Re-run with DOORMAN_PUB pointed at the public name:"
echo
echo "    DOORMAN_PUB=https://$APP_UNDER_TEST.$BASE_HOST SCHEME=https ./f3-planes.sh"
echo
echo "  A gate that passed on loopback and fails in the open is an F7 FAIL."

echo
echo "== F7 (4): renewal is unattended =="
note "not automatable in one run. Do this:"
note "  1. Caddy renews at 2/3 of the certificate lifetime, on its own timer."
note "  2. Force one now:  caddy reload --force  (or delete the cert from"
note "     /var/lib/caddy/.local/share/caddy/certificates and reload)"
note "  3. Confirm from OFF-BOX that the new certificate serves, with no"
note "     human touching the Cloudflare token."
note "  4. Confirm the token has NO TTL — a TTL is a silent renewal failure"
note "     60-90 days out (SERVER-SETUP.md Phase 8, gotcha 4)."

echo
pass "F7 PASS for everything scriptable — record the renewal check by hand"
