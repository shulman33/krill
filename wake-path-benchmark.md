# Wake-Path Benchmark: Validating the Sleeping Cloud's Core Bet

**Goal.** Convert the one number the entire unit-economics model rests on from a guess into a measurement: *how fast, how small, and how densely can Firecracker snapshot/restore wake a realistic agent-written app?*

**Budget.** Two-tier cloud, hourly billing, no physical servers and no monthly rental: ~$25 of GCP nested-virt instance time covers everything, plus a few hours of EC2 `.metal` spot (~$10–30) **only if** a latency gate fails in tier 1. ~3 working days.

**Output.** A filled-in results table (bottom of this doc) and a commit / pivot / investigate decision against five gates.

**Companion scripts.** `wake-bench/` in this repo contains every script referenced below, syntax-checked and ready to `scp -r wake-bench root@BOX:` — day 1 on the box is pure copy-paste. See `wake-bench/README.md` for the run order.

**Status: tier 1 RUN 2026-07-23; tier 2 (METAL) RUN 2026-07-26 — ALL FIVE GATES PASS, no open benchmark gates remain.**

Tier 1 (`wake-bench/RESULTS-2026-07-23-nested.md`, ~$2.50 of GCP): warm resume **p99 115 ms** (G1 ✓), cold **432 ms** (G2 ✓), **69 MB stored/app** after dedupe (G3 ✓, 3.6× headroom), **14.5×** resume-vs-boot (G5 ✓), storm linear to N=25 then CPU-bound at N=50 (G4 = ESCALATE per the tier rule). **Decision: commit to the microVM architecture.**

Tier 2 (`wake-bench/RESULTS-2026-07-26-metal.md`, the Hetzner EX44): **G4 CLOSED — p99 109 ms at N=50 with zero errors**, a 39× improvement that confirms the tier-1 escalation was aggregate-CPU-bound on nested VM exits and not an architectural ceiling. G1 and G5 re-measured for free: warm resume **p99 26 ms** (11.5× headroom), resume-vs-boot **42×**. G2 and G3 stand at tier 1, where a PASS is conclusive.

---

## The gates (decide these before measuring anything)

These map 1:1 to assumptions in the economics model. Write the verdicts down *after* the runs, not during.

| Gate | Claim being tested | Pass threshold | Model assumption it protects |
|------|-------------------|----------------|------------------------------|
| **G1** | Warm resume is invisible to users | First HTTP 200 after resume: **p99 ≤ 300 ms** (NVMe, warm page cache) | "Wake-on-request feels like a slow request, not an outage" |
| **G2** | Cold resume is tolerable | First HTTP 200: **p99 ≤ 1.5 s** (page cache dropped — proxy for the warm-storage tier) | Cold-tier UX; router hold-and-replay budget |
| **G3** | Idle apps are cheap to store | Stored bytes per app (mem + rootfs, after balloon + hole-punch + dedupe across 10 apps): **≤ 250 MB** | The 220 MB default → ⅓¢/mo idle app |
| **G4** | Monday 9 am doesn't melt a host | 50 concurrent resumes of distinct VMs: **p99 ≤ 1 s, zero errors** | Peak-factor sizing (model needs only ~2 wakes/s/host; this is ~10× margin) |
| **G5** | Snapshots beat cold boots | Resume p50 at least **5× faster** than full cold boot p50 | Justifies the snapshot machinery existing at all |

**The tier rule (read before recording any verdict).** Tier 1 runs under nested virtualization, which is *strictly slower* than bare metal — every VM exit is handled twice (guest → your VM → Google's hypervisor). That makes latency verdicts one-sided:

- A latency gate that **passes** in tier 1 passes conclusively — real hardware only improves it.
- A latency gate that **fails** in tier 1 is **inconclusive**. Record it as ESCALATE, never FAIL, and re-run just that gate on tier 2 metal.
- **G3 is byte counting** — no latency involved, conclusive on any tier.
- A FAIL may only ever be written down from a metal run.

**Decision matrix:**
- All pass (any tier) → commit to the microVM architecture; next spike is the MCP deploy wrapper.
- G1 fails *on metal* → try Phase 6 (UFFD lazy restore) and hugepages before concluding; if still failing, revisit isolates-first hybrid.
- G3 fails → content-addressed dedupe moves from "phase 2" to "before launch," and re-run the economics model with the measured number.
- G4 fails *on metal* → wake admission control and jitter get promoted to MVP scope.

---

## Phase 0 — Provision compute (~30 minutes, no physical servers)

The only hard requirement is `/dev/kvm` plus local-SSD-class scratch storage. What that means per provider:

- **Regular EC2 instances: impossible.** Nitro does not support nested virtualization at all — no `/dev/kvm`, full stop. AWS's answer is `.metal` instances (which is literally what Lambda runs Firecracker on).
- **GCP: ordinary VMs work** with `--enable-nested-virtualization` (Intel N2/C2). This is tier 1.
- **EC2 `.metal`: real bare metal, hourly + spot.** This is tier 2, used only to re-run latency gates that failed under nested virt.

### Tier 1 — GCP nested-virt instance (run everything here first)

```bash
gcloud compute instances create wake-bench \
  --zone=us-central1-a \
  --machine-type=n2-standard-16 \
  --enable-nested-virtualization \
  --local-ssd=interface=NVME \
  --local-ssd=interface=NVME \
  --image-family=ubuntu-2404-lts-amd64 --image-project=ubuntu-os-cloud \
  --boot-disk-size=50GB
# ≈ $1/hr (16 vCPU / 64 GB + 2× 375 GB Local SSD — this machine shape requires
# Local SSDs in counts of [2, 4, 8, ...]; the second one just comes along).
# gcloud will WARN about the 50 GB boot disk's I/O — ignorable: nothing
# performance-sensitive lives on the boot disk.
# ZONE_RESOURCE_POOL_EXHAUSTED is common for this shape — capacity is zonal and
# transient; loop over zones (checking gcloud's EXIT CODE, not its output) until
# one sticks. On 2026-07-23 all of us-central1 was out; us-east1-b had it.
# Delete when done: gcloud compute instances delete wake-bench --zone=<the-zone>
```

SSH in and put a **Local SSD** at `/srv` — this is the "local NVMe" every phase assumes. The boot disk is a network-attached Persistent Disk and must not hold snapshots; putting `/srv` there silently invalidates G1/G2 and the fio baseline:

```bash
lsblk -o NAME,SIZE,MODEL            # two 375G Local SSD devices; use the first
mkfs.ext4 -F /dev/nvme0n1
mkdir -p /srv && mount /dev/nvme0n1 /srv
ls -l /dev/kvm                      # must exist — if missing, the nested-virt flag didn't take
grep -cm1 vmx /proc/cpuinfo         # ≥ 1
```

### Tier 2 — EC2 metal spot (only for latency gates that failed tier 1)

`i3.metal` (512 GB RAM, 8× 1.9 TB NVMe instance store; spot commonly ~$1.5–2/hr) or `c5d.metal`/`m5d.metal`. Ubuntu 24.04 AMI, mount an instance-store NVMe at `/srv`, run the identical scripts. Remember: **snapshots do not transfer between tiers** — different CPUs; re-run Phase 3.1 to re-snapshot on the metal box before benching.

*(Prefer a dedicated physical box anyway? Hetzner AX52 at ~€64/mo works identically and is the cheapest option if the box will live longer than this benchmark.)*

### Setup and disk baseline (both tiers)

`wake-bench/00-host-setup.sh` installs packages, records the tier (via `systemd-detect-virt`) and environment into `results/ENV.txt`, warns if `/srv` looks like network storage, and runs the fio baseline:

```bash
fio --name=randread --filename=/srv/fio.test --size=4G --rw=randread \
    --bs=4k --iodepth=32 --direct=1 --runtime=30 --time_based --group_reporting
```

Record IOPS and p99 latency in the results table — the point is knowing your ceiling so you can tell "Firecracker is slow" from "the disk is slow." Expect roughly 150–200k 4k read IOPS per GCP Local SSD device, 400–700k on metal NVMe. (The `cpupower` governor step is a harmless no-op on cloud VMs — the physical CPU is Google's to manage.)

> **Rule for the whole benchmark:** nothing else runs on this instance during timed runs. No monitoring agents, no unattended-upgrades (`systemctl disable --now unattended-upgrades`).

---

## Phase 1 — Firecracker install and first boot (1–2 hours)

1. **Install a pinned release** (record the exact version — snapshots are only valid across identical Firecracker + kernel versions):

   ```bash
   ARCH="$(uname -m)"
   REL="https://github.com/firecracker-microvm/firecracker/releases"
   VER=$(basename $(curl -fsSLI -o /dev/null -w %{url_effective} ${REL}/latest))
   curl -L ${REL}/download/${VER}/firecracker-${VER}-${ARCH}.tgz | tar -xz
   install release-${VER}-${ARCH}/firecracker-${VER}-${ARCH} /usr/local/bin/firecracker
   firecracker --version   # write this down
   ```

2. **Fetch a guest kernel.** Use the CI kernels from the Firecracker getting-started guide (the S3 bucket path changes per release line — copy the current URL from [the quickstart doc](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md) if this 404s):

   ```bash
   mkdir -p /srv/fc && cd /srv/fc
   # pattern: https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/<ci-ver>/x86_64/vmlinux-6.1.<patch>
   curl -fsSL -o vmlinux <current-6.1.y-url-from-quickstart>
   ```

3. **Smoke-test** with the quickstart's Ubuntu rootfs to confirm a VM boots at all before building custom guests. Don't skip this; it isolates "my rootfs is broken" from "my setup is broken" later.

---

## Phase 2 — Build the two representative guests (2–3 hours)

Two workloads, chosen because they're what agents actually generate: Python/FastAPI + SQLite, and Node/Express + SQLite. Build them as Docker images, then export to ext4 — no systemd, a shell script as PID 1.

1. **App A (Python).** `guest-a/app.py`:

   ```python
   import sqlite3, os
   from fastapi import FastAPI

   app = FastAPI()
   db = sqlite3.connect("/data.db")
   db.execute("CREATE TABLE IF NOT EXISTS hits (ts TEXT DEFAULT CURRENT_TIMESTAMP)")

   @app.get("/ping")
   def ping():
       db.execute("INSERT INTO hits DEFAULT VALUES"); db.commit()
       n = db.execute("SELECT COUNT(*) FROM hits").fetchone()[0]
       return {"hits": n}
   ```

   The `/ping` handler does a real SQLite write+read on purpose — a 200 proves the *full stack* resumed (network, runtime, database), not just the VMM.

   `guest-a/Dockerfile`:

   ```dockerfile
   FROM python:3.12-slim
   RUN apt-get update && apt-get install -y --no-install-recommends iproute2 && rm -rf /var/lib/apt/lists/*
   RUN pip install --no-cache-dir fastapi uvicorn
   COPY app.py /app.py
   COPY init.sh /init.sh
   RUN chmod +x /init.sh
   ```

   `guest-a/init.sh` (PID 1 — mounts, network, app; no init system):

   ```sh
   #!/bin/sh
   mount -t proc proc /proc
   mount -t sysfs sys /sys
   ip addr add 172.16.0.2/24 dev eth0
   ip link set eth0 up
   ip route add default via 172.16.0.1
   exec python3 -m uvicorn app:app --host 0.0.0.0 --port 8000
   ```

2. **App B (Node).** Same shape: `node:22-slim`, `express` + `better-sqlite3`, same `/ping` semantics, same `init.sh` with `exec node /app.js`.

3. **Docker image → ext4 rootfs:**

   ```bash
   cd guest-a && docker build -t guest-a .
   CID=$(docker create guest-a)
   dd if=/dev/zero of=/srv/fc/app-a.ext4 bs=1M count=1024
   mkfs.ext4 -q /srv/fc/app-a.ext4
   mkdir -p /mnt/rootfs && mount -o loop /srv/fc/app-a.ext4 /mnt/rootfs
   docker export "$CID" | tar -xC /mnt/rootfs
   umount /mnt/rootfs && docker rm "$CID"
   cp /srv/fc/app-a.ext4 /srv/fc/app-a.golden.ext4   # pristine copy — see the gotcha in Phase 3
   ```

4. **Host-side network** (single-VM phases; the storm phase uses netns instead):

   ```bash
   ip tuntap add tap0 mode tap
   ip addr add 172.16.0.1/24 dev tap0
   ip link set tap0 up
   ```

---

## Phase 3 — Single-VM resume latency: G1, G2, G5 (day 2 morning)

### 3.1 Boot, warm, snapshot

Start Firecracker and configure over its unix-socket API:

```bash
rm -f /tmp/fc.sock && firecracker --api-sock /tmp/fc.sock &

api() { curl -s --unix-socket /tmp/fc.sock -X "$1" "http://localhost$2" \
        -H 'Content-Type: application/json' -d "$3"; }

api PUT /machine-config       '{"vcpu_count":1,"mem_size_mib":512,"smt":false}'
api PUT /boot-source          '{"kernel_image_path":"/srv/fc/vmlinux",
                                "boot_args":"reboot=k panic=1 pci=off quiet loglevel=1 init=/init.sh"}'
api PUT /drives/rootfs        '{"drive_id":"rootfs","path_on_host":"/srv/fc/app-a.ext4",
                                "is_root_device":true,"is_read_only":false}'
api PUT /network-interfaces/eth0 '{"iface_id":"eth0","guest_mac":"06:00:AC:10:00:02","host_dev_name":"tap0"}'
api PUT /balloon              '{"amount_mib":0,"deflate_on_oom":true,"stats_polling_interval_s":1}'
api PUT /actions              '{"action_type":"InstanceStart"}'
```

**Measure cold boot while you're here (G5 baseline):** time from `InstanceStart` to first `/ping` 200. Do 10 iterations. This is the number snapshots must beat by 5×.

**Warm the guest** — 20× `curl 172.16.0.2:8000/ping` so imports are loaded and hot paths are in the guest's page cache. Then shrink and snapshot:

```bash
api PATCH /balloon '{"amount_mib":384}'   # reclaim free guest pages → zeros in the mem file
sleep 5
api PATCH /balloon '{"amount_mib":0}'
api PATCH /vm      '{"state":"Paused"}'
mkdir -p /srv/snaps/a
api PUT /snapshot/create '{"snapshot_type":"Full",
                           "snapshot_path":"/srv/snaps/a/vmstate",
                           "mem_file_path":"/srv/snaps/a/mem"}'
kill %1
fallocate -d /srv/snaps/a/mem             # punch holes where pages are zero
du -h --apparent-size /srv/snaps/a/mem && du -h /srv/snaps/a/mem   # apparent vs actual
```

### 3.2 The critical correctness gotcha: restore rootfs with the snapshot

A memory snapshot embeds the guest's page cache and dirty buffers **as of snapshot time**, which correspond to the rootfs **as of snapshot time**. If resume #1 writes to the disk and resume #2 loads the same memory image against the mutated disk, the ext4 journal and the in-memory FS state disagree — silent corruption. So: **every resume gets a fresh copy of the golden rootfs** (untimed, before the clock starts). This is not benchmark pedantry — it's the production design surfacing early: a snapshot manifest must always pair memory with a copy-on-write disk clone, which is exactly what rule E5's `(epoch, LSN, checkpoint_id)` triple is for.

### 3.3 The bench script

`bench_resume.sh` — measures both the API-level load time and the number that actually matters, time to first HTTP 200:

```bash
#!/usr/bin/env bash
# usage: bench_resume.sh <snap-dir> <golden-rootfs> <live-rootfs> <iterations> [cold]
set -euo pipefail
SNAP=$1; GOLD=$2; LIVE=$3; N=$4; MODE=${5:-warm}
SOCK=/tmp/fc-bench.sock

for i in $(seq 1 "$N"); do
  cp --sparse=always "$GOLD" "$LIVE"                      # untimed: pristine disk per run
  if [ "$MODE" = cold ]; then sync; echo 3 > /proc/sys/vm/drop_caches; fi
  rm -f "$SOCK"
  firecracker --api-sock "$SOCK" >/dev/null 2>&1 &
  FC=$!
  until [ -S "$SOCK" ]; do sleep 0.001; done

  t0=$(date +%s%N)
  curl -s --unix-socket "$SOCK" -X PUT http://localhost/snapshot/load \
    -H 'Content-Type: application/json' \
    -d "{\"snapshot_path\":\"$SNAP/vmstate\",
         \"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$SNAP/mem\"},
         \"resume_vm\":true}" >/dev/null
  t1=$(date +%s%N)
  until curl -s -o /dev/null --max-time 0.2 http://172.16.0.2:8000/ping; do :; done
  t2=$(date +%s%N)

  kill "$FC" 2>/dev/null; wait "$FC" 2>/dev/null || true
  echo "$(( (t1-t0)/1000000 )) $(( (t2-t0)/1000000 ))"    # load-ms  first-200-ms
done
```

Percentiles:

```bash
./bench_resume.sh /srv/snaps/a /srv/fc/app-a.golden.ext4 /srv/fc/app-a.ext4 100 warm > warm-a.txt
awk '{print $2}' warm-a.txt | sort -n | \
  awk '{a[NR]=$1} END {printf "p50 %d ms   p99 %d ms\n", a[int(NR*0.50)], a[int(NR*0.99)]}'
```

### 3.4 Run matrix

| Run | Guest | Mode | Iterations | Gate |
|-----|-------|------|-----------|------|
| 1 | App A (Python) | warm | 100 | G1 |
| 2 | App A | cold (`drop_caches` each iter) | 50 | G2 |
| 3 | App B (Node) | warm | 100 | G1 |
| 4 | App B | cold | 50 | G2 |
| 5 | App A | cold boot, no snapshot | 10 | G5 |

Sanity checks before trusting the numbers: the two columns should differ (if first-200 ≈ load-time, your poll loop is broken); warm p50 for this setup plausibly lands in the 60–250 ms band — a p50 of 5 ms means you measured a VM that never actually paused.

---

## Phase 4 — Storage footprint and dedupe: G3 (day 2 afternoon)

The model assumes ~220 MB stored per idle app, which implies ~4× dedupe on raw bytes. Memory pages and disk blocks dedupe very differently — measure them separately, because the split determines where the engineering effort goes.

1. **Make 10 app variants.** Loop: copy `guest-a/`, perturb `app.py` (different route names, an extra pip package on a few), rebuild, re-export, boot, warm, snapshot. All share the same base image — exactly the production distribution. Script it; it's ~30 lines.

2. **Measure per-app raw footprint** (post-balloon, post-hole-punch):

   ```bash
   for d in /srv/snaps/*/; do echo "$d: mem=$(du -sh $d/mem | cut -f1)"; done
   du -sh /srv/fc/app-*.golden.ext4
   ```

3. **Dedupe across the fleet** with content-defined chunking (borg reports exact dedup stats):

   ```bash
   borg init --encryption=none /srv/dedup-repo
   borg create --stats /srv/dedup-repo::mem-only   /srv/snaps/*/mem       2>&1 | tee dedup-mem.txt
   borg create --stats /srv/dedup-repo::disk-only  /srv/fc/*.golden.ext4  2>&1 | tee dedup-disk.txt
   # "Deduplicated size" vs "Original size" → your ratio, per class
   ```

4. **Expected shape of the result** (verify, don't assume): rootfs dedupes hard (shared base layers → 5–10×); memory dedupes poorly across *different* apps (ASLR randomizes layout) but the balloon + hole-punch step should have already shrunk mem files well below their 512 MiB apparent size. **G3 arithmetic:** `(deduped total bytes) / 10 apps ≤ 250 MB`. If memory dominates the footprint, note it — that's the signal that guest-side page hygiene (balloon timing, `madvise` behavior) matters more than the chunk store.

---

## Phase 5 — The wake storm: G4 (day 3 morning)

Fifty VMs resuming at once, each in its own network namespace so fifty guests can all believe they're `172.16.0.2` without conflict.

1. **Pre-stage (untimed):** 50 rootfs copies, 50 netns each containing a `tap0`:

   ```bash
   for i in $(seq 1 50); do
     cp --sparse=always /srv/fc/app-a.golden.ext4 /srv/storm/rootfs-$i.ext4
     ip netns add fc$i
     ip netns exec fc$i ip tuntap add tap0 mode tap
     ip netns exec fc$i ip addr add 172.16.0.1/24 dev tap0
     ip netns exec fc$i ip link set tap0 up
     ip netns exec fc$i ip link set lo up
   done
   ```

2. **Fire.** Launch all 50 concurrently — each worker is Phase 3's timed body wrapped in `ip netns exec fc$i`, all sharing **one** snapshot's `mem` file (the `File` backend maps it privately, copy-on-write; this is safe and is itself a finding — see step 4). Collect one first-200 latency per VM; while it runs, watch `iostat -x 1` and `vmstat 1` on the host.

3. **Report:** p50/p99 across the 50, error count, and host RAM/IO saturation. Context for the gate: the economics model at defaults needs ~2 wakes/s/host *sustained*; surviving a 50-VM instantaneous burst is roughly 10× margin on the worst correlated spike.

4. **Bonus measurement (density lever):** host RSS with 50 clones resident vs 50 × 512 MiB naive. Because all 50 map the same mem file, clean pages are shared in the host page cache — measure `free -m` before/after. This number feeds the density claims in the economics model directly.

5. **Production caveat to log, not solve today:** 50 resumes of *one* snapshot means 50 guests with identical RNG state, session keys, and `/dev/urandom` pools — the classic snapshot-clone security problem. Production needs VMGenID-triggered reseeding (supported by newer Firecracker + 6.1+ guest kernels). In production each *app* has its own snapshot, so this is only acute for same-app scale-out — but write it in the risk register now.

---

## Phase 6 (stretch, only if G1 failed) — UFFD lazy restore and hugepages

The `File` backend eagerly relies on page cache; the `Uffd` backend serves pages on demand from a userspace handler, which is what a network-tiered warm store would use in production. Firecracker ships an example UFFD handler in its repo (`examples/uffd/`). Re-run the Phase 3 warm matrix with `"backend_type":"Uffd"` and the example handler, and separately with hugepages-backed memory. If lazy restore rescues G1, the production wake path needs the handler as a first-class component (it's also the natural seam for content-addressed chunk fetch). If nothing rescues G1 on local NVMe *on the metal tier*, the microVM bet itself is in question — that's exactly what this benchmark exists to find out for under $50 instead of six months.

---

## Results template

Fill this in as you go. Numbers without the environment block are worthless in three months.

```
ENVIRONMENT
  tier: ............... nested (GCP n2-standard-16) | metal (i3.metal / other)
  instance: ........... (machine type, zone, CPU model, RAM, /srv device model)
  os/kernel: .......... firecracker: .......... guest kernel: ..........
  fio baseline: ....... IOPS 4k randread, p99 .......... µs

RUNS                              p50        p99        n      notes
  cold boot, App A (G5 base)      ..... ms   ..... ms   10
  warm resume, App A (G1)         ..... ms   ..... ms   100
  warm resume, App B (G1)         ..... ms   ..... ms   100
  cold resume, App A (G2)         ..... ms   ..... ms   50
  cold resume, App B (G2)         ..... ms   ..... ms   50
  storm ×50, App A (G4)           ..... ms   ..... ms   50     errors: ...

STORAGE (G3)
  mem file, apparent / actual:    512 MiB / ..... MiB   (after balloon + fallocate -d)
  rootfs golden:                  ..... MiB
  dedupe ratio, mem × disk:       .....× / .....×  (borg)
  stored bytes per app, net:      ..... MB   → gate ≤ 250 MB

DENSITY (bonus)
  host RSS, 50 clones resident:   ..... GB  vs naive 25 GB

VERDICTS   (each: PASS(nested) | PASS(metal) | ESCALATE | FAIL(metal) — a bare
            FAIL may only come from a metal run; see the tier rule)
  G1 [ ]  G2 [ ]  G3 [ ]  G4 [ ]  G5 [ ]
  decision: ..........
```

---

## Gotcha appendix (read before day 2, again when something looks weird)

- **Version lock.** Snapshots are valid only across identical Firecracker versions and guest kernels, and are not portable across CPU vendors, microcode revisions, or **tiers** — never copy snapshots from the GCP instance to the metal box; re-run Phase 3.1 on each machine.
- **The tier rule, again.** Nested-virt numbers are one-sided: passes are conclusive, failures mean escalate to metal. Never mix tier-1 and tier-2 numbers in the same comparison, and never write FAIL from a nested run.
- **Network storage under `/srv` is disqualifying.** Persistent Disk / EBS have network-latency physics; snapshots and the fio baseline must live on Local SSD / instance-store NVMe. `00-host-setup.sh` warns if it detects otherwise.
- **Fresh rootfs per resume** (§3.2). The single most likely way to get corrupted, confusing results.
- **Clocks jump on resume.** kvmclock snaps forward; guest `CLOCK_MONOTONIC` behavior across pause is subtle. Don't measure anything *inside* the guest — all timing happens on the host. (Production echo: this is why PT-3 forbids guest-side lease timers.)
- **TCP doesn't survive.** Any connection open at snapshot time is dead on resume; the bench opens fresh connections.
- **Never let ARP race a resuming guest** *(found the hard way — cost a day-1 mystery)*. If the host's neighbor entry for the guest has expired, the kernel's who-has retry is exactly 1 s, producing a bimodal base/base+1000 ms latency split that flips run to run. Pin it: `ip neigh replace <guest-ip> lladdr <guest-mac> dev tap0 nud permanent`. The production wake path needs the same rule.
- **Tap MACs must be deterministic** *(found the hard way — a flat 30 s per storm VM)*. The snapshot embeds the guest's ARP entry for its gateway; restore behind a tap with a different random MAC and every guest reply goes to a dead address until the guest's ARP cache expires (`reachable_time` ≈ 30 s). Fix: assign the tap a fixed, locally administered MAC everywhere. MAC assignment is part of the snapshot contract, exactly like the drive path.
- **Don't let the measurement harness DoS the measurement.** Fifty pollers each forking `ip netns exec` + `curl` every few ms is a CPU storm that inflates the storm numbers; enter each netns once and loop inside it.
- **`curl --max-time 0.2` in the poll loop matters** — without it a half-open socket during resume can hang the poller and inflate readings.
- **SMT off** (`"smt":false`) for stable latencies; it also mirrors the likely production posture for cross-tenant side-channel reasons.
- **Balloon timing.** Inflate → wait for guest reclaim → deflate → then pause. Pausing mid-inflation snapshots the balloon in a weird state.
- **Poll granularity.** The bash loop resolves ~1–3 ms, fine at a 100–300 ms scale. If results cluster suspiciously at loop granularity, rewrite the poller in Python with `time.monotonic_ns()` before drawing conclusions.

## What this benchmark deliberately does not test

Multi-tenant CPU interference, network-tiered snapshot storage (Phase 6's UFFD handler is the hook for it), gVisor/isolate alternatives, live migration, and everything in Parts I–II of the design doc (fencing is validated by TLA+/simulation, not by this box). One instance at a time, one question: **is the wake path fast and small enough for the economics to be real?**
