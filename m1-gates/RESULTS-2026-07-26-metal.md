# M1 gate results — 2026-07-26, METAL (tier 2)

Environment: Hetzner EX44-1-LTD in FSN1 (`krill-fsn1`, i5-13500, 14 cores / 20
threads, 64 GB DDR4, RAID1 across both 512 GB NVMe, resync complete), Ubuntu
24.04.4 host kernel 6.8.0-134-generic, `systemd-detect-virt: none`, Firecracker
v1.16.1, guest kernel firecracker-ci/v1.14 vmlinux-6.1.155, performance governor
pinned. krilld from commit 9bd13c6 + this file. Idle timeout 15 s. Prior tier-1
run: `RESULTS-2026-07-23-nested.md`.

**A1 re-run on metal: PASS, p99 90 ms against the 300 ms gate — 3.3× headroom,
and 3.3× faster than the same gate on nested virt (298 ms).**

Only A1 was re-run. A2 (unattended freeze), A3 (write safety) and A4 (ten-app
fleet) are correctness gates, not latency gates: the tier rule's escalation
logic does not apply to them and their 2026-07-23 PASSes stand. Re-running them
against the current binary is regression testing, not gate work — noted as
available, not done.

## A1 three ways: what the data plane and Phase 7 cost

A1 was written in M1, before the data plane existed, so **the pre-registered
configuration is the first row** and it is the one that carries the verdict.
The other two rows use the same script, guest, host and binary, and exist to
price what has been added since.

| Configuration | p50 | p99 | n | vs the 300 ms gate |
|---|---|---|---|---|
| `--data-plane=false` (**the A1 gate**) | 76 ms | **90 ms** | 100 | **PASS**, 3.3× headroom |
| data plane on, objstore = local fsstore | 88 ms | 95 ms | 50 | +5 ms at p99 |
| data plane on, objstore = **GCS** (production) | 258 ms | **285 ms** | 50 | passes by **15 ms** |

```
dataplane-off  n=100  min=72  p50=76  p90=79  p99=90   max=91   mean=76.5  (ms)
fsstore        n=50   min=82  p50=88  p90=92  p99=95   max=101  mean=87.8  (ms)
gcs            n=50   min=231 p50=258 p90=276 p99=285  max=292  mean=259.6 (ms)
```

⚠ **The production configuration passes A1 with 5% margin.** Every GCS round
trip on the wake path is ~45 ms from FSN1 to europe-west3, and the wake path
takes three of them. One added round trip, a slower region, or an ordinary bad
minute on the network breaks the project's headline latency gate. Two levers
are already identified and un-taken (ROADMAP): `PrepareWake`'s stream load is
redundant with `SealTakeover`'s, and caching manifest generations would let the
seal skip its read. Either buys back ~45 ms.

Note the data plane itself is nearly free — **5 ms at p99** against a local
store. The 195 ms is not the data plane; it is the distance to the object store.

## Where the milliseconds live (metal vs nested, same instrumentation)

The daemon logs `acquire_ms` (spawn VMM + snapshot/load + readiness probe) vs
`proxy_ms` (first proxied request) per wake:

```
              acquire_ms      proxy_ms       total p99
nested        41-49           211-256        298 ms
metal         24-33           43-46          90 ms
```

**This answers the open question M1 left behind.** The ~200 ms post-resume
guest-userspace tax was overwhelmingly a nested-virtualization artifact: same
guest image, same FastAPI/uvicorn/sqlite code, same krilld, same instrumented
boundary — only the tier changed, and it fell to ~44 ms (≈4.8× smaller).
krilld's own machinery also tightened, 41–49 ms → 24–33 ms.

The tax has not vanished: 44 ms is still ~half of a 90 ms wake, and the bench's
simpler python guest resumes to first-200 in 22 ms on this same box
(`../wake-bench/RESULTS-2026-07-26-metal.md`, G1). So roughly 20 ms of
guest-userspace warmup remains attributable to the heavier guest. The M1
hypothesis that snapshot-time guest state matters is neither confirmed nor
refuted here; it is now a ~20 ms question rather than a ~200 ms one, which
demotes it well below M4 on the backlog.

## Notes

- Run with `KRILL_DATA=/srv/krill-gates`, a scratch data dir, so the gate
  daemon never touched the production registry, app disks, or object store.
  The GCS row wrote to a scratch prefix (`gs://krill-fsn1-objstore/gate-scratch`),
  never the production one, and it was deleted afterwards.
- The gate scripts had to be taught to coexist with a production box first:
  `00-setup.sh` used to `pkill -x krilld`, which under `Restart=always` starts a
  second daemon fighting over ports, taps and the data dir. See "Running the
  gate suites on this box" in `../SERVER-SETUP.md`, and commit 9bd13c6.
- Each A1 cycle costs ~7.8 s of wall clock, almost all of it the deliberate
  balloon reclaim during freeze (`BalloonSettle` 5 s + `DeflateSettle` 1 s).
  The matrix is 200 cycles ≈ 23 minutes. Defaults were kept even though
  `--snapshot-balloon=false` would have cut it to a third, because A1's
  2026-07-23 run used defaults and comparability is the whole point.
- Raw per-run latencies on the box: `results/a1-{dataplane-off,fsstore,gcs}.txt`;
  full driver log `/root/a1-matrix.log`.
