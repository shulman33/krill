# M1 gate results — 2026-07-23, GCP nested virt (tier 1)

Environment: n2-standard-16, us-east1-c (us-east1-b was capacity-exhausted
this day), Local SSD at /srv, Ubuntu 24.04 host kernel 6.17.0-1021-gcp,
Firecracker v1.16.1, guest kernel firecracker-ci/v1.14 vmlinux-6.1.155,
krilld from this repo at the commit tagged by this file. Idle timeout 15 s.
Instance cost ≈ $2 for the full session including debugging.

**All four pre-registered gates PASS. M1 accepted.**

| Gate | Verdict | Headline number |
|---|---|---|
| A1 warm wake p99 | **PASS** | p99 = 298 ms (gate ≤ 300) over 100 wakes |
| A2 unattended freeze | **PASS** | VMM procs 1 → 0, RAM freed, snapshot valid, woke again |
| A3 write safety | **PASS** | 100/100 cycles, integrity_check ok, seq ledger gapless |
| A4 ten-app fleet | **PASS** | 10/10 frozen resident, 10/10 identity-verified (kernel-assigned IP) |

## A1 distribution (end-to-end through the router, freeze → HTTP 200)

```
n=100  min=258  p50=269  p90=281  p99=298  max=308  mean=271.2  (ms)
```

## A4 fleet wake distribution (cold-ish: freshly registered fleet)

```
n=10  min=312  p50=325  p90=333  p99=333  max=351  mean=326.5  (ms)
```

## Where the milliseconds live (instrumented breakdown)

The daemon logs `acquire_ms` (supervisor wake: spawn VMM + snapshot/load +
readiness probe) vs `proxy_ms` (first proxied request) per wake:

```
acquire_ms ≈ 41–49          proxy_ms ≈ 211–256        (every wake, both modes)
```

tcpdump on the tap puts the boundary precisely: the guest **kernel**
completes the TCP handshake and ACKs the HTTP request bytes ~13 ms after
snapshot/load, but guest **userspace** (uvicorn/python) produces the
response ~200 ms later. Follow-up requests to the ACTIVE app take ~3 ms.

So: krilld's machinery costs ~45 ms of the 298 ms p99; the other ~200 ms is
a post-resume guest-userspace tax, python-specific, constant across cycles.

Falsified hypothesis, worth keeping: it is NOT balloon page-cache eviction —
running with `--snapshot-balloon=false` leaves the tax unchanged (~230 ms),
and it does not scale with pause duration (2 s and 8 s pauses tax the same).
The wake-bench comparison point (115 ms p99 end-to-end in bash) suggests
snapshot-time guest state matters: bench snapshots were taken immediately
after 20 warm pings; krilld snapshots capture a guest that has gone idle.
Attacking this tax (e.g. a pre-pause warmup request, uvloop, or a non-python
reference guest) is backlog — the gate passes with it included.

## The balloon knob (added during this run)

`--snapshot-balloon` (default true) toggles balloon reclaim before pause:

| Mode | Freeze duration | Stored mem file | First-wake latency |
|---|---|---|---|
| balloon on | ~8 s | ~69 MB class | ~270 ms |
| balloon off | ~2 s | 101 MB | ~270 ms (unchanged) |

Balloon reclaim is latency-free on the wake path for this guest — default
stays on; the knob exists for freeze-latency-sensitive setups.

## Anomalies / notes

- First run of the harness died silently: freeze-after-request raced the
  router's in-flight accounting (409) under `curl -sf` + `set -e`. Fixed by
  making `freeze_app` poll for the FROZEN postcondition. Lesson recorded in
  lib.sh.
- The gate guest initially 500'd on every DB endpoint: sqlite3 connections
  are thread-bound and FastAPI runs sync endpoints in a threadpool. Found
  via serial console on a debug app (`console=ttyS0` in per-app boot_args —
  the registry supporting per-app boot args paid for itself immediately).
- Daemon restart mid-session confirmed Reconcile: FROZEN apps stayed FROZEN
  and restored fine afterwards (snapshot + disk pairing preserved); a
  snapshot taken pre-restart restored post-restart in 53 ms.
- A2's MemAvailable delta (~100 MiB) is noisy host-wide accounting; the
  process-gone check is the real assertion (a microVM's RAM cannot outlive
  its process).
- Raw data in `results-nested-2026-07-23/`.
