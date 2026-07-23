# Shared helpers for the M2 gates. Sourced after env.sh.
# All timing is host-side wall clock — guest clocks jump across resume.

now_ms() { date +%s%3N; }

admin() { # <method> <path> [json] — control-API call, prints body, fails on non-2xx
  local method=$1 path=$2 body=${3:-}
  curl -sf -X "$method" "$ADMIN$path" \
    -H 'Content-Type: application/json' ${body:+--data "$body"}
}

app_state() { # <app> — current lifecycle state
  admin GET "/v1/apps/$1" | jq -r .state
}

route() { # <app> <method> <path> [curl-args...] — request through the router
  local app=$1 method=$2 path=$3; shift 3
  curl -sf -X "$method" -H "Host: $app.krill.local" --max-time 60 "$@" "$ROUTER$path"
}

wait_state() { # <app> <state> <timeout_s> — poll until the app reaches a state
  local app=$1 want=$2 timeout=$3 t0
  t0=$(date +%s)
  while :; do
    [ "$(app_state "$app")" = "$want" ] && return 0
    [ $(( $(date +%s) - t0 )) -ge "$timeout" ] && { echo "FATAL: $app never reached $want (state=$(app_state "$app"))" >&2; return 1; }
    sleep 0.5
  done
}

freeze_app() { # <name> — drive to FROZEN by postcondition, riding out 409s
  local name=$1 i
  for i in $(seq 1 40); do
    admin POST "/v1/apps/$name/freeze" >/dev/null 2>&1 || true
    [ "$(app_state "$name")" = FROZEN ] && return 0
    sleep 0.5
  done
  echo "FATAL: $name never reached FROZEN" >&2
  return 1
}

pass() { echo "== PASS: $* =="; }
fail() { echo "== FAIL: $* ==" >&2; exit 1; }
