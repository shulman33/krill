#!/usr/bin/env bash
# B4 — one MCP tool call, end to end: a scripted stdio JSON-RPC client
# (standing in for Claude Code) calls deploy; the single tool result carries
# URL + ready; the app answers through the router; logs works the same way.
set -euo pipefail
cd "$(dirname "$0")"; source env.sh; source lib.sh

MCP_SERVER=../mcp/dist/index.js
[ -f "$MCP_SERVER" ] || fail "B4: $MCP_SERVER missing — run 00-setup.sh"
krill delete gbmcp >/dev/null 2>&1 || true

echo "== B4: tools/call deploy over stdio =="
out=$(node mcp-drive.mjs "$MCP_SERVER" deploy \
  "{\"directory\":\"$(pwd)/examples/guestbook\",\"name\":\"gbmcp\"}")
echo "$out" > "$RESULTS_DIR/b4-deploy.txt"
echo "$out" | grep -q "URL: " || fail "B4: tool result has no URL"
echo "$out" | grep -q "Ready: yes" || fail "B4: tool result not ready: $out"

route gbmcp GET / | jq -e '.app == "guestbook"' >/dev/null || fail "B4: app not serving through the router"

echo "== B4: tools/call logs over stdio =="
logs=$(node mcp-drive.mjs "$MCP_SERVER" logs '{"name":"gbmcp"}')
[ -n "$logs" ] || fail "B4: logs tool returned nothing"
echo "$logs" > "$RESULTS_DIR/b4-logs.txt"

krill delete gbmcp >/dev/null 2>&1 || true
pass "B4 MCP one-tool-call deploy"
