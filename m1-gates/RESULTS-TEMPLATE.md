# M1 gate results — <date>, <tier: GCP nested / metal>

Environment: instance type, zone, kernel, Firecracker version, krilld commit.

| Gate | Verdict | Headline number |
|---|---|---|
| A1 warm wake p99 | PASS/FAIL | p99 = ___ ms (gate ≤ 300) |
| A2 unattended freeze | PASS/FAIL | VMM procs ___ → ___, woke again: y/n |
| A3 write safety | PASS/FAIL | ___/100 cycles, integrity ___, gapless ___ |
| A4 ten-app fleet | PASS/FAIL | ___/10 frozen resident, ___/10 identity-verified |

## A1 distribution

(paste percentiles line)

## A4 fleet wake distribution

(paste percentiles line)

## Anomalies / notes

-
