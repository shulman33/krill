# M3 gate results — 2026-07-23, nested (tier 1)

Instance: n2-standard-16, us-east1-c, 2× Local SSD (NVMe, /srv), Ubuntu
24.04, nested virt under GCP. Firecracker v1.16.1, kernel
firecracker-ci/v1.14 vmlinux-6.1.155 (the exact M1/M2 pair). krilld built
from commit `11abd2d` (+ this results commit). Objstore:
`file:///srv/krill/objstore` (fsstore). Data plane + sync-ack on, defaults.
Total hardware cost ≈ $0.20 (about 12 minutes of instance time).

**All three hardware gates PASSED on the first run. Combined with C4
(10,000 sim seeds + the three negative fence configs, PASSED locally at
full budget), M3 is ACCEPTED.**

| Gate | Verdict | Numbers |
|---|---|---|
| C1 durability | **PASS** | 200/200 acked rows recovered; digest byte-identical (`0a713da7…`); stream head 232 before = 232 after; "data disk rebuilt from object store head=232" in the daemon log — the E6 path, on the record |
| C2 fencing | **PASS** | stale-append FENCED (g1.c0 < g1.c1), stale-register FENCED; manifest byte-identical across both attempts; 3 takeover seals, epochs monotone; CheckManifest (I1/I3) clean at head=29, cur_epoch=g1.c3 |
| C3 PITR | **PASS** | branch s1 @ LSN 105 served exactly phase A (digest match); parent s0 byte-identical modulo the branch list; restore back via `--from-stream s0` @ 233 recovered phase A+B exactly (branch s2) |
| C4 spec-as-oracle | **PASS** (local, full budget) | 10,000 positive seeds, zero violations; GatewayFencing off → PT-1 @ seed 0; ReplayOnRestore off → WriterAtHead @ seed 0; RegistrationFencing off → PT-9 I3 forgery @ seed 1428 |
| C-info wake tax | — | 30 freeze/wake cycles through the router with data plane + sync-ack: **p50 205 ms, max 253 ms** — inside A1's 300 ms gate even with epoch mint + takeover seal CAS + shipper start + the sync-ack precise scan on every request |

## The C1 sequence, verbatim

200 rows acked through the router (sync-ack holding each response until
durable), freeze, `kill -9 krilld`, then `rm` of data.ext4 + disk.ext4 +
ship.json + snap/ — everything app-local except the objstore directory and
the registry (the control plane). Restart, one `curl`:
count/sum/digest identical. The daemon log shows the wake demoted to a
cold boot after the rebuild, exactly as designed.

## Findings

1. **First-wake reconcile always rebuilds once.** A freshly installed app
   has a data disk but no cursor file, so `PrepareWake` sees
   `cursor_stream="" != "s0"` and rebuilds an empty disk before the first
   boot (`head=0` rebuild lines in the log). Harmless and correct, but it
   costs one mkfs per first wake; writing an initial cursor at install
   would shave it. Not done now — the conservative path is the one C1
   depends on.
2. **Per-request segments.** With sync-ack on, each acked write ships as
   its own one-frame segment (head 232 ≈ 200 writes + schema + probes).
   Fine at small-software QPS; group-commit batching across concurrent
   requests is the obvious lever if it ever matters, and GC of old
   segments past a checkpoint is already queued for M5.
3. **The wake tax barely moved.** p50 205 ms / max 253 ms vs the M1 A1
   p99 of 298 ms on the same shape — the data plane's wake-path additions
   (mint, seal CAS on fsstore, shipper start, per-request precise scan)
   are noise next to the ~200 ms guest-userspace tax that still dominates.

## Raw data

`results-nested-2026-07-23/`: c1.txt, c2*.json + c2.txt, c3*.json +
c3.txt, c-info-wakes.txt (30 samples), krilld-log-excerpts.txt.
