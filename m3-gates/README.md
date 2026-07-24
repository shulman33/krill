# m3-gates — runnable C1–C4 acceptance gates for the data plane

The criteria were frozen in `GATES.md` **before** any M3 code existed —
read that first. This directory makes them executable.

## What runs where

- **C4 (`c4-sim.sh`)** — pure Go, no hardware: run it on the dev laptop.
  10,000 seeded schedules of the real gateway/manifest/replay code, plus
  the three negative fence configs reproducing the TLC counterexamples.
- **C1–C3 + C-info** — the GCP nested-virt dev box, same recipe as
  m1-gates/m2-gates (see `wake-bench/README.md` for provisioning).

## Dev-box run order

```bash
# locally:
make krilld-linux krill-linux fencetool-linux
# copy the repo (with bin/) to the box, then on the box as root:
cd m3-gates
./00-setup.sh          # installs binaries, starts krilld with the data plane on
./c1-durability.sh     # acked writes survive total local-state loss
./c2-fencing.sh        # stale epochs rejected, manifest untouched, monotone seals
./c3-pitr.sh           # restore = branch; parent stream byte-identical
./90-info-waketax.sh   # informational: wake latency with data plane + sync-ack
```

Results land in `results/`; transcribe into `RESULTS-TEMPLATE.md` and
commit as `RESULTS-<date>-nested.md` (m1/m2 convention).

## Notes

- The gates run krilld with `--objstore file:///srv/krill/objstore`
  (fsstore). The C1 kill deliberately deletes everything app-local EXCEPT
  that directory and the registry — the registry is the control plane
  (epoch mint), not the data record. To run against real GCS instead:
  `--objstore gs://bucket/prefix` (auth via the GCE metadata server).
- `fencetool` (m3-gates/fencetool) is linked against the internal
  dataplane packages: it speaks the real wire format, not a mock of it.
  Run its probes only while the app is FROZEN — the fsstore CAS is
  process-local, and a quiescent app is what makes cross-process probing
  sound.
- The ledger example app is deliberately krill-ignorant: plain Python
  stdlib + SQLite at `/data/app.db` with `synchronous=FULL`. Its digest
  endpoint is what the gates compare across kills and restores.
