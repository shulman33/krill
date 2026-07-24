# M3 gate results — <date>, <tier: nested | metal>

Instance: <shape, zone, image>. Firecracker <version>, kernel <version>.
krilld built from commit `<sha>`. Objstore: <file:///… | gs://…>.

| Gate | Verdict | Numbers |
|---|---|---|
| C1 durability | PASS/FAIL | rows recovered <n>/200; rebuild logged: yes/no |
| C2 fencing | PASS/FAIL | stale append/register fenced; seals <n>, monotone |
| C3 PITR | PASS/FAIL | branch digest match; parent diff empty; roundtrip ok |
| C4 spec-as-oracle | PASS/FAIL | <n> positive seeds clean; violations found at seeds <a>/<b>/<c> |
| C-info wake tax | — | p50 <n> ms, max <n> ms (A1 baseline p99 298 ms) |

## Raw results

Attach `results/` contents (c1.txt, c2*.json, c3*.json, c4.txt,
c-info-wakes.txt).

## Anomalies and findings

<anything unexpected: daemon log excerpts, rebase events during gates,
latency outliers, GCS-vs-fsstore differences>
