# Wake-path benchmark — results

Fill in as you go. Numbers without the environment block are worthless in
three months. Gates and decision matrix: `../wake-path-benchmark.md`.

```
ENVIRONMENT
  tier: ............... nested (GCP n2-standard-16) | metal (i3.metal / other)
  instance: ........... (machine type, zone, CPU model, RAM, /srv device model)
  os/kernel: .......... firecracker: .......... guest kernel: ..........
  fio baseline: ....... IOPS 4k randread, p99 .......... µs
  (source: results/ENV.txt, results/fio-baseline.txt)

RUNS                              p50        p99        n      notes
  cold boot, python (G5 base)     ..... ms   ..... ms   10
  warm resume, python (G1)        ..... ms   ..... ms   100
  warm resume, node (G1)          ..... ms   ..... ms   100
  cold resume, python (G2)        ..... ms   ..... ms   50
  cold resume, node (G2)          ..... ms   ..... ms   50
  storm x50, python (G4)          ..... ms   ..... ms   50     errors: ...

STORAGE (G3)
  mem file, apparent / actual:    512 MiB / ..... MiB   (after balloon + fallocate -d)
  rootfs golden:                  ..... MiB
  dedupe, mem / disk:             .....x / .....x       (results/dedup-*.txt)
  stored bytes per app, net:      ..... MB   -> gate <= 250 MB

DENSITY (bonus)
  host RAM used, before storm:    ..... MB
  host RAM used, 50 resident:     ..... MB   (naive reservation would be +25,600 MB)

VERDICTS   (each: PASS(nested) | PASS(metal) | ESCALATE | FAIL(metal) — a bare
            FAIL may only come from a metal run; see the tier rule)
  G1 [ ]  G2 [ ]  G3 [ ]  G4 [ ]  G5 [ ]
  decision: ..........
```
