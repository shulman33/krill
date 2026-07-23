# wake-bench

Runnable companion to `../wake-path-benchmark.md`. Provision an instance, copy
this directory over, run the numbered scripts in order, ship `results/` back.
Every number the economics model depends on comes out of this directory.

## Provision (tier 1 — GCP nested virt; no physical servers)

```bash
gcloud compute instances create wake-bench \
  --zone=us-central1-a \
  --machine-type=n2-standard-16 \
  --enable-nested-virtualization \
  --local-ssd=interface=NVME \
  --local-ssd=interface=NVME \
  --image-family=ubuntu-2404-lts-amd64 --image-project=ubuntu-os-cloud \
  --boot-disk-size=50GB          # ≈ $1/hr; delete the instance when done
# (n2-standard-16 requires Local SSDs in counts of [2, 4, 8, ...] — hence the
#  repeated flag. The boot-disk I/O WARNING is ignorable: nothing hot lives there.)

# on the instance: put a Local SSD at /srv (NOT the boot disk — it's network storage)
lsblk -o NAME,SIZE,MODEL         # two 375G local SSDs; use the first
mkfs.ext4 -F /dev/nvme0n1 && mkdir -p /srv && mount /dev/nvme0n1 /srv
ls -l /dev/kvm                   # must exist
```

**The tier rule:** nested virt is strictly slower than metal, so latency-gate
PASSes here are conclusive; FAILs mean ESCALATE (re-run that gate on an EC2
`.metal` spot instance, e.g. `i3.metal` — re-snapshot there first, snapshots
don't cross machines). G3 (dedupe) is conclusive on any tier. Regular EC2
instances cannot run this at all — Nitro has no nested virtualization.

```
gcloud compute scp --recurse wake-bench root@wake-bench:~ --zone=us-central1-a
ssh into the instance, cd wake-bench
```

## Day 1 — setup and single-VM latency (gates G1, G2, G5)

```bash
./00-host-setup.sh                     # packages, governor, fio baseline
./01-install-firecracker.sh            # pinned FC release + CI kernel
./02-build-guests.sh                   # python + node rootfs images

for i in $(seq 10); do ./10-boot-and-snapshot.sh python --boot-only; done   # G5 baseline
./10-boot-and-snapshot.sh python       # boot, warm, balloon, snapshot
./11-bench-resume.sh python 100 warm   # G1: p99 <= 300 ms
./11-bench-resume.sh python 50 cold    # G2: p99 <= 1.5 s

for i in $(seq 10); do ./10-boot-and-snapshot.sh node --boot-only; done
./10-boot-and-snapshot.sh node
./11-bench-resume.sh node 100 warm
./11-bench-resume.sh node 50 cold
```

## Day 2 — storage footprint and dedupe (gate G3)

```bash
./20-build-variants.sh 10              # 10 app variants, one shared base image
./21-dedupe-report.sh 10               # borg stats, mem vs disk separately
```

## Day 3 — the wake storm (gate G4)

```bash
./30-storm.sh python 50                # 50 concurrent resumes, one snapshot
```

Then fill in `RESULTS-TEMPLATE.md` and make the call against the gates in
`../wake-path-benchmark.md`.

## Requirements

- `/dev/kvm` — a GCP nested-virt VM (tier 1) or any bare metal (tier 2).
  Regular EC2 instances won't work; EC2 `.metal` will.
- Local-SSD-class storage mounted at `/srv` (the setup script warns if `/srv`
  is on EBS / Persistent Disk — network disks invalidate the latency gates).
- Ubuntu 24.04, run everything as root.
- Nothing else running on the instance during timed runs.

## Notes that will save you an afternoon

- **Snapshots are valid only on the exact Firecracker + kernel pair recorded
  in `results/ENV.txt`.** Re-run `01` after any upgrade and re-snapshot.
- **Every resume gets a pristine rootfs copy** (`lib.sh:fresh_rootfs`). The
  memory snapshot embeds filesystem state that must match the disk exactly —
  skipping this corrupts results silently.
- **The storm uses one mount namespace per VM** to bind a private rootfs copy
  over the drive path baked into the snapshot — Firecracker's load API cannot
  re-path drives.
- `DEBUG=1 ./10-boot-and-snapshot.sh python` keeps the serial console in
  `results/serial-*.log` when a guest won't come up.
- All timing happens on the host. Guest clocks jump on resume; never measure
  inside the VM.
