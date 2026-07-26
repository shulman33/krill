#!/usr/bin/env bash
# Preflight for the M4 gates. Changes nothing that matters; refuses loudly
# when the box is not in a state where a gate result would mean anything.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

require curl; require jq

echo "== krilld =="
admin /healthz >/dev/null || fail "krilld's admin API is not answering at $ADMIN
  (on the box: systemctl status krilld; from the Mac: bring the SSH tunnel up)"
pass "krilld admin API up"
apps=$(admin /v1/apps | jq -r '.[].name' | tr '\n' ' ')
note "apps: ${apps:-none}"

echo "== krill-doorman =="
curl -sf "$DOORMAN/healthz" >/dev/null || fail "krill-doorman is not answering at $DOORMAN
  (systemctl status krill-doorman; the tunnel must forward 9092 as well as 9091)"
status=$(dm /v1/status)
printf '%s\n' "$status" > "$RESULTS_DIR/doorman-status.json"
pass "doorman up"
note "base host: $(printf '%s' "$status" | jq -r .base_host), auth host: $(printf '%s' "$status" | jq -r .auth_host)"
note "identity key: $(printf '%s' "$status" | jq -r .identity_key)"

# F2 cannot pass without this, and finding out during the revoke step wastes
# a browser sign-in.
if [ "$(printf '%s' "$status" | jq -r .revoke_durable)" != "true" ]; then
  fail "the doorman has NO object store: every revoke will be refused, so F2 cannot pass.
  Set --objstore on the krill-doorman unit and restart it."
fi
pass "revocation is durable at $(printf '%s' "$status" | jq -r '.objstore // "the configured object store"')"

if [ "$(printf '%s' "$status" | jq -r '.owners | length')" = "0" ]; then
  note "no --owners configured: Sam will need a share link to reach his own apps"
fi

echo "== the guest identity contract =="
pub=$(printf '%s' "$status" | jq -r .identity_pub)
onbox=$(cat "$DOORMAN_STATE/identity.pub" 2>/dev/null | tr -d '\n' || true)
if [ -n "$onbox" ] && [ "$onbox" != "$pub" ]; then
  fail "$DOORMAN_STATE/identity.pub does not match the running doorman's key.
  krilld bakes that file into every guest's boot args, so every app would reject every token."
fi
[ -n "$onbox" ] && pass "identity.pub matches the running key" || note "identity.pub not readable from here (fine if running off-box)"

echo "== apps the gates need =="
for a in "$APP" "$OTHER_APP"; do
  if admin "/v1/apps/$a" >/dev/null 2>&1; then
    pass "$a deployed ($(admin "/v1/apps/$a" | jq -r .state))"
  else
    fail "$a is not deployed. Deploy it first:
    krill deploy m4-gates/examples/watchlist --name watchlist
    krill deploy m3-gates/examples/ledger    --name ledger
  (decision #10a: watchlist should be deployed BY AN AGENT THROUGH THE MCP
   SERVER — 'agent-written apps deploy in one call' is the pitch, and doing it
   by hand makes the README a little less true.)"
  fi
done

echo "== isolated builder (F5) =="
if journalctl -u krilld -n 200 --no-pager 2>/dev/null | grep -q "isolated builder on"; then
  pass "krilld reports an isolated builder"
else
  note "could not confirm an isolated builder from the journal."
  note "F5 needs --build-vm-image and --build-vm-kernel; see m4-gates/builder-image/README.md."
fi

echo "== egress baseline (F6) =="
if nft list table inet krill >/dev/null 2>&1; then
  pass "the nftables table 'inet krill' exists"
  nft -a list table inet krill > "$RESULTS_DIR/egress-ruleset.txt" 2>/dev/null || true
else
  note "no 'inet krill' table visible from here (need root on the box). F6 will say more."
fi

echo
pass "preflight done — run ./f1-identity.sh next (it is the only gate needing a human)"
