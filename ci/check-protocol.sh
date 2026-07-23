#!/usr/bin/env bash
# Model-check the fencing protocol: the positive config must satisfy all seven
# invariants, AND each of the three fence toggles must still produce a
# counterexample when disabled. A fence that can't fail when disabled is a
# fence the model no longer exercises (see CLAUDE.md, "the tripwire").
#
# usage: ci/check-protocol.sh [path/to/tla2tools.jar]
set -euo pipefail
cd "$(dirname "$0")/.."

JAR=${1:-tla2tools.jar}
if [ ! -f "$JAR" ]; then
  echo "== fetching tla2tools.jar =="
  curl -fsSL -o "$JAR" https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
fi

TLC() { # <cfg> — run TLC in an isolated scratch dir, echo output, return TLC's exit code
  local cfg=$1 dir out rc=0
  dir=$(mktemp -d)
  cp FencingProtocol.tla "$dir/"
  cp "$cfg" "$dir/FencingProtocol.cfg"
  out=$(cd "$dir" && java -XX:+UseParallelGC -cp "$OLDPWD/$JAR" tlc2.TLC \
        -workers auto -config FencingProtocol.cfg FencingProtocol.tla 2>&1) || rc=$?
  echo "$out"
  rm -rf "$dir"
  return $rc
}

flip() { # <toggle-name> — emit the cfg with one fence disabled
  sed -E "s/^([[:space:]]*$1[[:space:]]*=[[:space:]]*)TRUE/\1FALSE/" FencingProtocol.cfg
}

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "== positive model: all seven invariants must hold =="
out=$(TLC FencingProtocol.cfg) || { echo "$out"; fail "TLC reported an error on the positive config"; }
grep -q "No error has been found" <<<"$out" || { echo "$out"; fail "positive run did not report a clean pass"; }
grep -Eo "[0-9]+ distinct states found" <<<"$out" | head -1

# Each negative config must violate the exact invariant recorded in
# FencingProtocol.cfg's header comments — not just "some error".
check_negative() { # <toggle> <expected-violated-invariant>
  local toggle=$1 want=$2 out rc=0
  echo "== negative model: $toggle = FALSE must violate $want =="
  flip "$toggle" > "/tmp/neg-$toggle.cfg"
  grep -q "$toggle = FALSE" "/tmp/neg-$toggle.cfg" || fail "could not flip $toggle in FencingProtocol.cfg"
  out=$(TLC "/tmp/neg-$toggle.cfg") && rc=0 || rc=$?
  [ $rc -ne 0 ] || { echo "$out"; fail "$toggle=FALSE produced NO counterexample — the fence is no longer exercised by the model"; }
  grep -q "Invariant $want is violated" <<<"$out" \
    || { echo "$out"; fail "$toggle=FALSE failed, but not by violating $want"; }
  echo "   counterexample found, $want violated (as designed)"
}

check_negative GatewayFencing      WriterAtHead
check_negative ReplayOnRestore     WriterAtHead
check_negative RegistrationFencing SnapshotsOnHistory

echo "== protocol check PASSED: positive model clean, all three fences load-bearing =="
