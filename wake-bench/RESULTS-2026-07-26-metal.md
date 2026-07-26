# Wake-path benchmark — results, tier 2 (METAL), 2026-07-26

Raw data: `results/` on the box (`storm-python-50.txt`, `resume-python-warm.txt`).
Gates and tier rule: `../wake-path-benchmark.md`. Prior tier-1 run:
`RESULTS-2026-07-23-nested.md`.

**G4 is closed. The 2026-07-23 ESCALATE was a nested-virt artifact, not an
architectural ceiling: p99 went 4206 ms → 109 ms on the same code.**

Only the gates that needed metal were re-run. Per the tier rule a nested PASS
is conclusive, so G2 and G3 stand as recorded on 2026-07-23; G1 and G5 were
re-run anyway because their metal numbers were cheap and they price the
guest-userspace tax that M1 left open.

```
ENVIRONMENT
  tier: metal (Hetzner EX44-1-LTD, FSN1; systemd-detect-virt: none)
  instance: i5-13500 (14 cores / 20 threads), 64 GB DDR4,
            /srv on /dev/md2 = ext4 on RAID1 across both 512 GB NVMe
  os/kernel: Ubuntu 24.04.4, host 6.8.0-134-generic
             firecracker: v1.16.1   guest kernel: CI 6.1.155 (unchanged from tier 1)
  fio baseline: 333k IOPS 4k randread (libaio, QD32), p99 145 µs, avg 94 µs
                (tier 1 was 179k / p99 371 µs — 1.9x the IOPS, 2.6x tighter tail)
  governor: performance, pinned by cpu-governor.service across reboots
  RAID1 resync: COMPLETE before every timed run below

RUNS                              p50        p99        n      notes
  cold boot, python (G5 base)     931 ms     945 ms*    10     *max; n too small for p99
  warm resume, python (G1)        22 ms      26 ms      100    max 66; load API alone p50 8 ms
  storm x50, python (G4)          84 ms      109 ms     50     errors: 0; min 51, max 117

  tier-1 comparison, same scripts and same guest:
  cold boot          1649 ms -> 931 ms    (1.8x)
  warm resume         114 ms ->  22 ms    (5.2x)
  storm N=50         4145 ms ->  84 ms    (49x at p50, 39x at p99)

STORAGE (G3) — not re-run; tier-independent and PASSed on 2026-07-23
  mem file, apparent / actual:    512 MiB / 104 MiB (python) — identical on metal
  stored bytes per app, net:      69 MB   -> gate <= 250 MB

DENSITY (bonus) — 50 clones of ONE snapshot, which is what G4 models
  host RAM used, before storm:    1067 MB
  host RAM used, 50 resident:     1518 MB   (+451 MB total = ~9 MB per VM)
  buff/cache over the same span:  2091 MB -> 10561 MB (reclaimable; 50 private
                                  rootfs copies at 164 MB actual dominate it)
  MemAvailable:                   63013 MB -> 62563 MB  (-450 MB for 50 VMs)
  naive reservation would be:     +25,600 MB  -> ~57x denser (tier 1 saw ~41x)
  Mechanism: the snapshot's memory is file-backed, so all 50 VMs map the same
  /srv/snaps/python/mem and it stays in page cache instead of becoming 50x
  anonymous RAM. This is the same-snapshot case; G3's 69 MB/app is the number
  to quote for 50 DIFFERENT apps.

VERDICTS   (tier rule: a bare FAIL may only come from metal — this is metal)
  G1 [PASS(metal)]   warm p99 26 ms vs 300 ms gate — 11.5x headroom
  G2 [PASS(nested)]  unchanged; conclusive at tier 1 (p99 432 ms vs 1500 ms)
  G3 [PASS]          unchanged; tier-independent
  G4 [PASS(metal)]   p99 109 ms vs 1000 ms gate — 9.2x headroom, 0 errors.
                     RESOLVES the 2026-07-23 ESCALATE. 50 concurrent resumes all
                     served inside 117 ms of wall clock — a burst of ~430
                     wakes/s for clones of one snapshot, against a model that
                     needs ~2 wakes/s/host sustained. The tier-1 diagnosis was
                     correct: 16 nested vCPUs were aggregate-CPU-bound on VM
                     exits, and 20 real threads with cheap exits are not.
  G5 [PASS(metal)]   931 ms boot / 22 ms resume = 42x vs 5x gate (tier 1: 14.5x)

  decision: all five benchmark gates now PASS, with a metal verdict wherever
  one was required. No open benchmark gates remain in the project.
```

## What metal changed, and what it didn't

The wake path got **5.2× faster** (114 → 22 ms warm resume) and the storm got
**39× faster at p99**. Both are larger than the usual nested-virt penalty
because both are exit-heavy: a resume is a burst of page faults and MMIO, and
the storm multiplies that by 50 against a fixed core count.

What did **not** change: the memory footprint per snapshot (104 MiB actual
after balloon, identical to tier 1) and therefore the economics model's storage
input. G3 needs no metal re-run and got none.

## Caveats worth keeping

- **`/srv` is the RAID1 root, not a dedicated device.** The bench warns when
  `/srv` shares the boot disk because on cloud instances that means network
  storage; here it is real NVMe, so the warning does not apply — but writes
  carry mirror amplification, and 333k read IOPS is a mirrored-read number.
- **The storm resumes 50 clones of one snapshot.** That is precisely the
  Monday-9am scenario G4 was written for, and precisely NOT a claim about 50
  distinct apps; the page-cache sharing that produces the 57× density figure
  does not apply across different snapshots.
- **G4's pre-stage is untimed** (50 sparse rootfs copies + 50 netns). The gate
  measures resume-to-first-200 under concurrency, not provisioning.
- The G5 baseline is n=10, so its "p99" is a max. Same convention as tier 1.
