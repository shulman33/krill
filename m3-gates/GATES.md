# M3 acceptance gates — pre-registered

*Written 2026-07-23, BEFORE any M3 code exists. Same discipline as the
benchmark and M1/M2: the pass/fail criteria are frozen here first, so the
implementation can't quietly bend the test to fit what got built.*

M3 is the data plane: rules E1–E6 as code. Host-side SQLite WAL tailing,
epoch stamping, segment shipping to object storage with manifest CAS,
seal-on-takeover, fenced checkpoint registration, restore = checkpoint +
WAL-delta replay, PITR as branching. The TLA+ spec becomes the test oracle
via a deterministic-simulation harness.

Terminology: "checkpoint" below is the protocol object (a full SQLite
database image at an exact WAL commit boundary, registered in the stream
manifest under E5) — not a Firecracker memory snapshot. LSN = frame index
in the app's logical WAL stream, starting at 1.

## C1 — Durability: acked writes survive total local-state loss

D1/D2 live. Local NVMe is a cache; the object store is the record.

1. Deploy the example app (SQLite at `/data/app.db`, `synchronous=FULL`,
   WAL mode) and write 200 rows through its HTTP API, recording each row
   only after its HTTP response completes (= acked).
2. Quiesce (stop writing), then freeze the app (or wait for the janitor).
3. Simulate host-local storage loss: stop krilld with SIGKILL, then delete
   the app's `data.ext4`, `disk.ext4` AND its Firecracker snapshot files.
   The object store (fsstore dir / GCS bucket) is NOT touched.
4. Restart krilld, curl the app.

**PASS:** the app serves, and every one of the 200 acked rows is present
(count and checksum match). The wake log shows the E6 path: fresh epoch,
takeover seal, rebuild from checkpoint + WAL-delta replay.
**FAIL:** any acked row missing, or the app serves pre-loss stale data.

With `-sync-ack` enabled (the D1 default), the acked set must be exactly
recoverable even when step 2 is skipped and the kill happens mid-write-load:
rows whose HTTP response completed are present; rows never acked may be
absent. (Torn tail rows that were durable but unacked are allowed to be
present — D1 bounds loss, not extra durability.)

## C2 — Fencing: stale epochs cannot write, register, or seal

E3 and E5 live, plus the takeover seal (the PT-9 commitment: the seal is a
fenced gateway *write*, never elided).

1. Deploy the app; write a few rows so the stream has segments.
2. Using the gate tool (a small Go utility linked against the dataplane
   packages), attempt against the app's live stream:
   a. a WAL-segment append stamped with an epoch strictly below the
      manifest's current epoch (PT-1's zombie push);
   b. a checkpoint registration stamped with a stale epoch (PT-2/PT-9).
3. Wake the app at least twice more (freeze between wakes) so real
   takeovers happen.

**PASS:** (a) and (b) are both rejected with fencing errors and the
manifest is byte-identical before/after each rejected attempt; the manifest
after step 3 shows strictly monotone epochs with a seal record for every
takeover; the epoch sequence in the shipped WAL history is non-decreasing
(I1 read off the real manifest).
**FAIL:** any stale write mutates the manifest, or epochs regress.

## C3 — PITR is branching, never truncation

D4/PT-8 live.

1. Write phase A (100 rows), note the stream head LSN `L_A` (and wall time).
2. Write phase B (100 more rows).
3. `krill restore <app> --at <L_A>` (and once via `--at-time`).
4. The app now serves phase A only. Verify the parent stream's manifest is
   unchanged: same head, same segments, same checkpoints (byte-compare the
   manifest minus the branch registry).
5. Restore again to the parent's head: phase B is back — nothing was ever
   truncated.

**PASS:** branch serves exactly phase A; parent manifest unchanged; branch
manifest records `parent = (stream, L_A)`; second restore recovers all 200
rows.
**FAIL:** any parent-history mutation, or phase B unrecoverable.

## C4 — The spec is the oracle: I1–I4 under fault injection

The deterministic-simulation harness (pure Go, runs anywhere — this gate
needs no hardware). The harness drives the REAL dataplane code (manifest
CAS, gateway accept/reject/seal, registration, replay) with N virtual
hosts over an in-memory object store, using a seeded scheduler that
interleaves hosts at protocol-action granularity. Pause is a first-class
schedule (a host that isn't scheduled IS paused — same modeling choice as
the spec); crashes, restarts, lease expiry, and object-store partitions
are injected at every step.

After every step the harness asserts the spec's invariants on the real
manifest + durable history: I1 EpochsMonotoneInWAL, I2 AckedDurable +
WriterAtHead, I3 SnapshotsOnHistory, plus OneInstancePerEpoch and
CurEpochBounded.

1. Positive: ≥ 10,000 seeded schedules with all three fences on.
2. Negative, mirroring the TLC configs one at a time:
   - GatewayFencing off → must violate WriterAtHead (PT-1);
   - ReplayOnRestore off → must violate WriterAtHead (the E6 bug);
   - RegistrationFencing off → must violate SnapshotsOnHistory (PT-9,
     the slow waker).

**PASS:** zero violations across all positive seeds; each negative config
finds its violation within the same seed budget. A fence that can't fail
when disabled is a fence the harness no longer exercises — the negative
runs are as load-bearing as the positive one.
**FAIL:** any positive violation, or any negative config that stays green.

## C-info — wake-latency tax of the data plane (informational, not a gate)

A1's budget was p99 ≤ 300 ms through the router. With the data plane on
(seal-on-takeover CAS + shipper start on every wake), re-run a 100-wake
loop and record the delta vs the M1 number on the same box. Informational
because the fsstore CAS is local I/O; the number that matters arrives with
a real object store (GCS) and is recorded separately with `-objstore gs://`.

## Where they run

C4: anywhere (`go test`). C1–C3 + C-info: the GCP nested-virt dev box
(same recipe as m1-gates/m2-gates; scripts in this directory, numbered).
Tier rule unchanged: latency PASSes on nested are conclusive; only C-info
has a latency component and it is informational.
