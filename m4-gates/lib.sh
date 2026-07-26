# Shared helpers for the M4 gates. Sourced after env.sh.
#
# The central one is verify(): it makes exactly the call Caddy's forward_auth
# makes, which is the only thing standing between the internet and a wake. If
# it does not return 200, no request reached krilld — so a gate that asserts
# on verify()'s status is asserting on whether an app could have been woken,
# which is what F3 actually cares about.

fail() { echo "✗ $*" >&2; exit 1; }
pass() { echo "✓ $*"; }
note() { echo "  · $*"; }

dm()  { curl -sf "$DOORMAN$1"; }                      # doorman control GET
dmp() { curl -sf -X POST "$DOORMAN$1" -H 'Content-Type: application/json' --data "${2:-{\}}"; }
admin() { curl -sf "$ADMIN$1"; }

# verify <app> [session-cookie] [uri] [extra-host]
# Prints "<status> <X-App-User> <X-App-Plane> <X-Krill-Token>".
verify() {
  local app=$1 session=${2:-} uri=${3:-/} host=${4:-}
  [ -n "$host" ] || host="$app.$BASE_HOST"
  local args=(-s -o /dev/null -D - --max-time 30
    -H "X-Forwarded-Host: $host" -H "X-Forwarded-Uri: $uri"
    -H "X-Forwarded-Method: GET" -H "Accept: text/html")
  [ -n "$session" ] && args+=(-H "Cookie: $(cookie_name)=$session")
  local out status user plane token
  out=$(curl "${args[@]}" "$DOORMAN_PUB/_krill/auth/verify")
  status=$(printf '%s' "$out" | awk 'NR==1{print $2}')
  user=$(header_of "$out" x-app-user)
  plane=$(header_of "$out" x-app-plane)
  token=$(header_of "$out" x-krill-token)
  echo "$status ${user:--} ${plane:--} ${token:--}"
}

verify_status() { verify "$@" | awk '{print $1}'; }

header_of() { printf '%s' "$1" | tr -d '\r' | awk -v h="$2" 'BEGIN{IGNORECASE=1} tolower($1)==h":"{sub(/^[^:]*: */,""); print; exit}'; }

# The cookie name depends on whether the doorman is running with
# --cookie-secure (production) or not (tunnel-era testing).
cookie_name() {
  if [ "${SCHEME}" = "https" ]; then echo "__Host-krill_app"; else echo "krill_app"; fi
}

# share <app> <plane> [label] — prints "<share-id> <link>"
share() {
  local out
  out=$(dmp /v1/shares "{\"app\":\"$1\",\"plane\":\"$2\",\"label\":\"${3:-m4-gate}\",\"created_by\":\"m4-gates\"}") \
    || fail "creating a $2 share for $1 failed"
  echo "$(printf '%s' "$out" | jq -r .share.id) $(printf '%s' "$out" | jq -r .link)"
}

# share_token <link> — the secret out of a share URL, for Authorization: Bearer
share_token() { printf '%s' "$1" | sed 's|.*/_krill/s/||'; }

# revoke_identity <app> <email> / revoke_share <id> / revoke_app <app>
revoke_identity() { dmp /v1/revoke "{\"kind\":\"identity\",\"app\":\"$1\",\"email\":\"$2\",\"by\":\"m4-gates\"}"; }
revoke_share()    { dmp /v1/revoke "{\"kind\":\"share\",\"share\":\"$1\",\"by\":\"m4-gates\"}"; }
revoke_app()      { dmp /v1/revoke "{\"kind\":\"app\",\"app\":\"$1\",\"by\":\"m4-gates\"}"; }

grants_for() { dm "/v1/grants?app=$1"; }

# claimed_by <app> <share-id> — the email that claimed a link, empty if none
claimed_by() {
  grants_for "$1" | jq -r --arg s "$2" '.[] | select(.share_id == $s and .revoked == false) | .email' | head -1
}

# wait_for_claim <app> <share-id> <timeout_s>
wait_for_claim() {
  local app=$1 sid=$2 timeout=$3 t0 email
  t0=$(date +%s)
  while :; do
    email=$(claimed_by "$app" "$sid")
    [ -n "$email" ] && { echo "$email"; return 0; }
    [ $(( $(date +%s) - t0 )) -ge "$timeout" ] && return 1
    sleep 2
  done
}

# bearer <method> <app> <path> <token> [curl args...] — prints the status
bearer() {
  local method=$1 app=$2 path=$3 token=$4; shift 4
  curl -s -o /dev/null -w '%{http_code}' -X "$method" --max-time 60 \
    -H "Host: $app.$BASE_HOST" -H "Authorization: Bearer $token" "$@" \
    "$DOORMAN_PUB$path"
}

# session_request <method> <app> <path> <session> — a browser-shaped request
# at the doorman's own routes (the data and edit planes live there).
session_request() {
  local method=$1 app=$2 path=$3 session=$4; shift 4
  curl -s -o /dev/null -w '%{http_code}' -X "$method" --max-time 60 \
    -H "Host: $app.$BASE_HOST" -H "Cookie: $(cookie_name)=$session" "$@" \
    "$DOORMAN_PUB$path"
}

# token_claims <jwt> — the decoded payload, for the audience assertions
token_claims() {
  local body
  body=$(printf '%s' "$1" | cut -d. -f2)
  # base64url, no padding
  body="${body//-/+}"; body="${body//_//}"
  case $(( ${#body} % 4 )) in 2) body="$body==";; 3) body="$body=";; esac
  printf '%s' "$body" | base64 -d 2>/dev/null
}

need_session() {
  [ -s "$1" ] || fail "no saved session at $1 — run ./f1-identity.sh first (it is the only gate that needs a human)"
  tr -d '\n' < "$1"
}

require() { command -v "$1" >/dev/null || fail "need $1 on PATH"; }
