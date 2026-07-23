# Wake-path benchmark — results, tier 1 (nested), 2026-07-23

Raw data: `results-nested-2026-07-23/`. Gates and tier rule: `../wake-path-benchmark.md`.

```
ENVIRONMENT
  tier: nested (GCP, systemd-detect-virt: google)
  instance: n2-standard-16 (Intel, 16 vCPU / 64 GB), us-east1-b,
            /srv on 375 GB Local SSD (nvme_card), 2nd Local SSD unused
  os/kernel: Ubuntu 24.04   firecracker: v1.16.1   guest kernel: CI 6.1.155
  fio baseline: 179k IOPS 4k randread (libaio, QD32), p99 371 µs
                (QD1 latency: avg 126 µs)

RUNS                              p50        p99        n      notes
  cold boot, python (G5 base)     1649 ms    1717 ms*   10     *max, n too small for p99
  warm resume, python (G1)        114 ms     115 ms     50     max 116 — 3 ms total spread
  warm resume, node (G1)          90 ms      92 ms      50
  cold resume, python (G2)        355 ms     432 ms     30     caches dropped each iter
  cold resume, node (G2)          276 ms     306 ms     30
  storm, python (G4)   N=5        156 ms     157 ms     5      linear scaling with N:
                       N=10       296 ms     316 ms     10     ~30-35 ms aggregate CPU
                       N=25       680 ms     890 ms     25     per VM; superlinear
                       N=50       4145 ms    4206 ms    50     thrash at N=50 (16 vCPU)
  all storm runs: zero errors. 50 wakes in 4.1 s = ~12 wakes/s on one small
  nested host; the economics model needs ~2 wakes/s/host sustained.

STORAGE (G3)
  mem file, apparent / actual:    512 MiB / 104 MiB (python), 113 MiB (node)
  rootfs golden (actual):         164 MiB (python), 295 MiB (node)
  dedupe (borg, 10 variants):     mem  5.37 GB -> 364 MB   disk 10.74 GB -> 327 MB
  stored bytes per app, net:      69 MB   -> gate <= 250 MB, 3.6x headroom

DENSITY (bonus)
  host RAM, 50 clones resident:   +622 MB total = ~12 MB per VM
  naive reservation would be:     +25,600 MB  -> ~41x denser via shared page cache

VERDICTS   (tier rule: nested PASSes conclusive; nested FAILs = ESCALATE)
  G1 [PASS(nested)]  warm p99 115 ms vs 300 ms gate — 2.6x headroom
  G2 [PASS(nested)]  cold p99 432 ms vs 1500 ms gate — 3.5x headroom
  G3 [PASS]          69 MB/app vs 250 MB gate (tier-independent)
  G4 [ESCALATE at N=50; PASS(nested) at N<=25]
                     4.2 s at N=50 is aggregate-CPU-bound and nested-virt exits
                     multiply exactly this workload; N=25 already passes the 1 s
                     gate on 16 nested vCPUs. Re-run N=50 on i3.metal (48+ real
                     cores, cheap exits) before recording any FAIL. Throughput
                     already exceeds the model's requirement by ~6x even here.
  G5 [PASS(nested)]  1649 ms boot / 114 ms resume = 14.5x vs 5x gate

  decision: COMMIT to the microVM architecture. The single number the economics
  model rests on is real: ~114 ms warm wake under *nested* virtualization, with
  storage per idle app at 69 MB — 3x better than the model assumed. The only
  open item is the N=50 burst on real metal, which is an hour of i3.metal spot
  if we ever care; the sustained-rate requirement is already beaten 6x.
```

## Two findings that changed the harness (and the production design)

Both were discovered because results looked wrong, and both are production
requirements in disguise:

1. **Never let ARP race a resuming guest.** First packets arrive before the
   guest can answer who-has; the kernel's ARP retry is exactly 1 s, producing a
   bimodal base/base+1000 ms latency split that flips run to run with neighbor
   cache expiry. Fix: pin a permanent neighbor entry for the guest's fixed MAC
   (`ip neigh replace ... nud permanent`). Production wake path: same rule.

2. **Tap MACs must be deterministic.** The snapshot embeds the guest's ARP
   entry for its gateway. Restore it behind a tap with a different (random)
   MAC and every guest reply goes to a dead address for ~30 s until the
   guest's ARP cache expires (`reachable_time`). This cost the first storm run
   a flat 30 s per VM. Fix: fixed, locally administered tap MAC everywhere.
   Production: MAC assignment is part of the snapshot contract, exactly like
   the drive path — deterministic or restored guests are network-deaf.

## Cost of this entire benchmark

One n2-standard-16 + 2 Local SSD in us-east1-b for ~2.5 hours ≈ **$2.50**,
plus a zone hunt (us-central1 a/b/c/f were all out of this shape; us-east1-b
had it).
