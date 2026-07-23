# Shared helpers for the M1 gates. Sourced after env.sh.
# All timing happens on the host — guest clocks jump across resume.

now_ms() { date +%s%3N; }

admin() { # <method> <path> [json] — control-API call, prints body, fails on non-2xx
  local method=$1 path=$2 body=${3:-}
  curl -sf -X "$method" "$ADMIN$path" \
    -H 'Content-Type: application/json' ${body:+--data "$body"}
}

app_state() { # <app> — current lifecycle state
  admin GET "/v1/apps/$1" | jq -r .state
}

route() { # <app> <method> <path> — request through the wake-on-request router
  local app=$1 method=$2 path=$3
  curl -sf -X "$method" -H "Host: $app.krill.local" --max-time 60 "$ROUTER$path"
}

register_app() { # <name> — register against the gate golden image (idempotent)
  local name=$1
  if admin GET "/v1/apps/$name" >/dev/null 2>&1; then return 0; fi
  admin POST /v1/apps \
    "{\"name\":\"$name\",\"golden\":\"$GOLDEN\",\"vcpus\":1,\"mem_mib\":512,\"guest_port\":8000}" >/dev/null
}

freeze_app() { # <name> — drive to FROZEN, retrying through transient 409s.
  # A freeze issued immediately after a request can race the router's
  # in-flight accounting (409 busy), or find the idle janitor's own freeze
  # already running (state SNAPSHOTTING). Both resolve in seconds; what the
  # gate needs is the postcondition, so poll for it.
  local name=$1 i
  for i in $(seq 1 30); do
    admin POST "/v1/apps/$name/freeze" >/dev/null 2>&1 || true
    [ "$(app_state "$name")" = FROZEN ] && return 0
    sleep 0.5
  done
  echo "FATAL: $name never reached FROZEN" >&2
  return 1
}

wait_state() { # <app> <state> <timeout_s> — poll until the app reaches a state
  local app=$1 want=$2 timeout=$3 t0
  t0=$(date +%s)
  while :; do
    [ "$(app_state "$app")" = "$want" ] && return 0
    [ $(( $(date +%s) - t0 )) -ge "$timeout" ] && { echo "FATAL: $app never reached $want" >&2; return 1; }
    sleep 0.5
  done
}

percentiles() { # <file> <column>
  awk -v c="$2" '{print $c}' "$1" | sort -n | awk '
    { a[NR] = $1; s += $1 }
    END {
      if (NR == 0) { print "  no data"; exit 1 }
      p50 = int(NR * 0.50); if (p50 < 1) p50 = 1
      p90 = int(NR * 0.90); if (p90 < 1) p90 = 1
      p99 = int(NR * 0.99); if (p99 < 1) p99 = 1
      printf "  n=%d  min=%d  p50=%d  p90=%d  p99=%d  max=%d  mean=%.1f  (ms)\n",
        NR, a[1], a[p50], a[p90], a[p99], a[NR], s / NR
    }'
}

pctl() { # <file> <column> <p> — one raw percentile value, for gate assertions
  awk -v c="$2" '{print $c}' "$1" | sort -n | awk -v p="$3" '
    { a[NR] = $1 } END { i = int(NR * p / 100); if (i < 1) i = 1; print a[i] }'
}

gate() { # <name> <ok-bool> <detail>
  local name=$1 ok=$2 detail=$3
  if [ "$ok" = 1 ]; then echo "== GATE $name: PASS — $detail =="
  else echo "== GATE $name: FAIL — $detail =="; return 1; fi
}
