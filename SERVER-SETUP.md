# SERVER-SETUP — the Hetzner EX44 (Krill's permanent home)

*Runbook for provisioning the production box from bare delivery to a
verified krilld deploy. Written 2026-07-26 for the EX44-1-LTD in FSN1
(i5-13500, 64 GB DDR4, 2×512 GB NVMe, Ubuntu 24.04, RAID1). Steps are
sequential; each phase ends with a "verify" you must not skip.*

**EXECUTED 2026-07-26 on 46.4.64.187 — Phases 0–8 complete and verified.
Phase 9 (the doorman and first exposure) is WRITTEN BUT NOT YET RUN.**
Corrections found during the real run are folded into the phases below and
marked ⚠. The five that would block a rebuild: the **ESP is mandatory**
(Phase 1), `installimage` is **not on `$PATH` non-interactively** (Phase 1),
`/srv/fc` **must be created by hand** (Phase 4), the example app is
**`guestbook`, not `hello`** (Phase 6), and **the object store must be copied
before `--objstore` is repointed** or every app's `/data` is wiped (Phase 7).

**Posture for this box (until M4 ships):** nothing listens publicly.
SSH (port 22) is the only open inbound port. The router and admin API
both bind loopback; you reach them through an SSH tunnel. There is no
auth in front of apps yet — the doorman is M4 — so exposing port 8080
to the internet before then would let anyone use (and wake, and bill)
every app on the box. **`krill.run` now resolves to this box (Phase 8)
and changes none of that** — the name points at a closed door, and
`curl http://<app>.krill.run/` must time out until M4 says otherwise.

---

## Phase 0 — before/at delivery

Hetzner emails when the server is live (usually minutes for stock
hardware). You already did the two order-time things that matter:
**Rescue System** selected as the OS and your SSH public key
(`~/.ssh/id_ed25519.pub`, generated 2026-07-26, comment
`samshulman6@gmail.com`) attached. The email gives you the server's
IP. Note it; below it's `$SRV`.

On the Mac, add an SSH config block now — every later step assumes it:

```
# ~/.ssh/config
Host krill
    HostName <SERVER-IP>
    User root
    # admin API + router, tunneled (Phase 6):
    LocalForward 9091 127.0.0.1:9091
    LocalForward 8080 127.0.0.1:8080
```

## Phase 1 — installimage: RAID1 + Ubuntu 24.04

⚠ **This box boots in UEFI mode** (`/sys/firmware/efi` exists), so the
partition list **must** contain exactly one EFI System Partition mounted at
`/boot/efi`, ≥256M. Without it installimage hard-fails validation
(`functions.sh:1396`: `UEFI == 1 && espcount != 1`) — in the menu that drops
you back into the editor; unattended it just prints `Cancelled.` The original
version of this runbook omitted the ESP and could not have worked as written.

⚠ **Prefer the unattended path.** The interactive menu needs a `TERM` the
rescue system's terminfo knows — Ghostty (`xterm-ghostty`) dies with
`Error opening terminal`. The `/autosetup` file is the mechanism Hetzner
Robot itself uses, and it sidesteps the menu, the editor, and the prompt:

```bash
ssh krill          # lands in the rescue system (no password — key auth)
cat > /autosetup <<'EOF'
DRIVE1 /dev/nvme0n1
DRIVE2 /dev/nvme1n1
SWRAID 1              # software RAID across both NVMe drives
SWRAIDLEVEL 1         # RAID1 (mirror) — the objstore lives on this box
BOOTLOADER grub
HOSTNAME krill-fsn1
PART /boot/efi esp  256M   # mandatory on UEFI — see above
PART swap     swap  8G
PART /boot    ext3  1G
PART /        ext4  all    # one big root; /srv/krill lives on it (real NVMe, fine)
IMAGE /root/images/Ubuntu-2404-noble-amd64-base.tar.zst
SSHKEYS_URL /root/.ssh/authorized_keys   # take over the rescue system's key
EOF

# ⚠ installimage is NOT on $PATH in a non-interactive ssh session:
TERM=xterm /root/.oldroot/nfs/install/installimage
```

It echoes the config, gives a 20-second abort window, then runs 16 steps to
`INSTALLATION COMPLETE` (~2 min on this hardware). Do **not** reach for `-a`:
that means "automatic mode" and demands its own `-i`/`-c` arguments, which
the `/autosetup` path already covers. Then `reboot`.

installimage also disables the root password and SSH root password login on
its own, so Phase 2's first two `sed`s are belt-and-braces — still run them,
they also set `PermitRootLogin prohibit-password`.

**Verify:** `ssh-keygen -R <SERVER-IP>` on the Mac first — installimage
generates fresh host keys, so the rescue key you trusted in Phase 0 is gone
and SSH will otherwise refuse with a MITM warning. Then `ssh krill` again
(~50 s to come back) and:

```bash
cat /proc/mdstat          # both md arrays [UU] — mirror healthy
ls /dev/kvm               # exists — the assumption everything stands on
systemd-detect-virt       # prints "none" — first true metal tier
lsb_release -d            # Ubuntu 24.04
```

`systemd-detect-virt = none` is worth savoring: every latency number
so far carried a nested-virt asterisk. This box is tier 2.

## Phase 2 — host hardening (10 minutes, do it before anything else)

```bash
# SSH: keys only, no passwords, root-with-key is fine for a solo box
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
systemctl reload ssh

# Inbound firewall: SSH only. 8080/9091 stay loopback — no rules needed.
apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y ufw
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw --force enable

# Keep security patches automatic (this box runs for months, unlike the
# throwaway bench VMs where 00-host-setup.sh disabled this).
apt-get install -y unattended-upgrades
```

**Verify:** from the Mac, `nc -zv $SRV 22` succeeds and
`nc -zv $SRV 8080` times out.

## Phase 3 — packages + performance governor

Same set the gate boxes used (`wake-bench/00-host-setup.sh`), minus the
bench-only tooling, plus the governor — which finally works, because
the CPU is yours now:

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  curl git jq sqlite3 docker.io e2fsprogs iproute2 util-linux \
  linux-tools-common "linux-tools-$(uname -r)"
cpupower frequency-set -g performance    # no longer a no-op — real metal
systemctl enable --now docker
```

⚠ **`cpupower frequency-set` does not survive a reboot** — and Phase 5
reboots the box. A benchmark run after that reboot would silently be on
`powersave`, quietly poisoning the first metal-tier numbers the project has.
Pin it:

```bash
cat > /etc/systemd/system/cpu-governor.service <<'EOF'
[Unit]
Description=Pin CPU governor to performance (benchmark integrity)
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/usr/bin/cpupower frequency-set -g performance
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now cpu-governor.service
```

**Verify:** `docker run --rm hello-world` works; and — the check that
actually generalizes, since `cpupower frequency-info` reports the *driver's*
view — `cat /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor | sort -u`
prints exactly `performance`. Re-run it after the Phase 5 reboot.

## Phase 4 — Firecracker v1.16.1 + guest kernel

Use the existing pinned installer — **pin the version**; all M1–M3
results are on v1.16.1 and snapshots never cross a Firecracker/kernel
boundary:

```bash
# on the Mac: copy the repo over (includes wake-bench/)
rsync -az --exclude .git --exclude results-nested-2026-07-23 \
  ~/Coding/My-Software-Projects/ycombinator/ krill:/root/krill/

# on the server:
mkdir -p /srv/fc /srv/krill        # ⚠ see below
cd /root/krill/wake-bench
FC_VERSION=v1.16.1 ./01-install-firecracker.sh
```

⚠ **`01-install-firecracker.sh` does not create `/srv/fc`.** On the bench
boxes `00-host-setup.sh` did it, and this runbook deliberately skips that
script. Without the `mkdir` the firecracker binary installs fine and then the
kernel fetch dies on `curl: (23) Failure writing output to destination` —
a confusing error for a missing directory.

That installs `/usr/local/bin/firecracker` and fetches the CI guest
kernel (`vmlinux-6.1.x`, the one with `CONFIG_IP_PNP` that makes
iproute2-less images deployable) to `/srv/fc/vmlinux`.

Old snapshots do NOT come along. Different CPU, different tier — the
version-lock gotcha says they'd be invalid anyway. Every app re-freezes
fresh on this box; the golden rootfs images rebuild via deploy.

**Verify:** `firecracker --version` prints v1.16.1;
`ls -la /srv/fc/vmlinux` exists and is >20 MB.

## Phase 5 — krilld binaries + systemd unit

Build linux binaries on the Mac and install them:

```bash
# Mac:
cd ~/Coding/My-Software-Projects/ycombinator
make krilld-linux krill-linux fencetool-linux
rsync -az bin/ krill:/root/krill/bin/

# server:
for b in krilld krill fencetool; do
  install -m 0755 /root/krill/bin/$b-linux-amd64 /usr/local/bin/$b
done
```

Then a real unit instead of the gates' `nohup` (crash reconcile at
startup is built in — `Restart=always` is safe and correct):

```bash
cat > /etc/systemd/system/krilld.service <<'EOF'
[Unit]
Description=krilld — Krill host agent
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
ExecStart=/usr/local/bin/krilld \
  --data-dir /srv/krill \
  --kernel /srv/fc/vmlinux \
  --listen 127.0.0.1:8080 \
  --admin 127.0.0.1:9091 \
  --base-host krill.run \
  --idle-timeout 60s
Restart=always
RestartSec=2
# root: krilld creates tap devices, runs mkfs.ext4, and drives docker
User=root
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now krilld
```

Two flags deliberately differ from the daemon defaults:

- `--listen 127.0.0.1:8080` — the default is `:8080` (all
  interfaces). **Loopback until the doorman exists.** Flipping this to
  public IS the M4 launch step, and it happens together with TLS + auth.
- `--base-host krill.run` — the real domain, bought 2026-07-26 (**Phase 8**).
  The daemon's *default* is still `krill.local`, which is correct for a box
  with no DNS; this box overrides it so deploys print real URLs. Note this
  flag is **cosmetic**: the router reads only the first DNS label of the
  `Host` header (`appName`, `internal/router/router.go`) and never checks the
  suffix, so changing it cannot break routing — and the gate suites can keep
  sending `Host: <app>.krill.local` forever. Suffix pinning is M4's job.

Defaults doing the right thing already: data plane + sync-ack on, cell-gen 1,
registry backups every 24 h keeping 14. The objstore default
(`file:///srv/krill/objstore`, on the RAID1 mirror) is only right until
**Phase 7** moves it to GCS — do that phase before the box holds anything you
would miss.

**Verify:** `systemctl status krilld` active;
`curl -s http://127.0.0.1:9091/healthz` returns OK. Reboot the box once
(`reboot`) and confirm krilld comes back on its own.

## Phase 6 — first deploy, end to end (from the Mac)

```bash
ssh krill                      # in one terminal: opens the tunnels and stays up
# in another terminal on the Mac:
export KRILL_ADMIN=http://127.0.0.1:9091
cd ~/Coding/My-Software-Projects/ycombinator
go build -o bin/krill ./cmd/krill       # the Makefile only cross-builds for linux
./bin/krill deploy m2-gates/examples/guestbook
```

⚠ There is no `examples/hello`. The real app dirs are
`m2-gates/examples/guestbook`, `m2-gates/examples/broken` (the B3
deliberately-failing app), and `m3-gates/examples/ledger`.

Instead of a second terminal you can background the tunnel with a control
socket — note the socket path must be **short**, or ssh fails with
`unix_listener: path ... too long for Unix domain socket`:

```bash
ssh -f -N -o ExitOnForwardFailure=yes -o ControlMaster=yes -o ControlPath=~/.ssh/cm-krill krill
ssh -O exit -o ControlPath=~/.ssh/cm-krill krill      # tear down when done
```

The deploy response verifies by waking the app once, so a URL in the
output means the whole path worked: tar → docker build → mkfs.ext4 →
boot → probe. Then exercise the router and a sleep/wake cycle:

```bash
curl -H "Host: guestbook.krill.run" http://127.0.0.1:8080/   # through the tunnel
krill freeze guestbook && sleep 1
time curl -H "Host: guestbook.krill.run" http://127.0.0.1:8080/   # wake-on-request
krill status guestbook
```

To click URLs in a browser, use the loopback wildcard from Phase 8:
`http://guestbook.local.krill.run:8080/` resolves to `127.0.0.1` in public
DNS, so with the tunnel up it works with no `/etc/hosts` line per app. (It
routes identically because the router only reads the first label.)

⚠ **Verified broken on Sam's home network, 2026-07-26 — expect this.** The
gateway there (`2600:4040:a4c9:c900::1`) answers `NOERROR` with an *empty*
answer section for any public name resolving into loopback/private space:
**DNS-rebinding protection.** It is not specific to this zone — the
well-known `localtest.me` is stripped identically, while `ledger.krill.run`
resolves fine through the same resolver. Diagnose in one line:

```bash
dig +short ledger.local.krill.run            # empty  → your resolver strips it
dig +short ledger.local.krill.run @1.1.1.1   # 127.0.0.1 → the record is fine
```

The scoped fix — send **only** `krill.run` to a resolver that doesn't
rebind-protect, leaving the rest of the Mac's DNS alone:

```bash
sudo mkdir -p /etc/resolver
printf 'nameserver 1.1.1.1\n' | sudo tee /etc/resolver/krill.run
# `dig` bypasses /etc/resolver by design — verify with the SYSTEM resolver:
dscacheutil -q host -a name ledger.local.krill.run
curl -m 5 -s http://ledger.local.krill.run:8080/
```

Blunter alternatives: point the Mac's DNS at `1.1.1.1` globally, or fall back
to a per-app `127.0.0.1 guestbook.krill.run` line in `/etc/hosts`. All of this
is tunnel-era only — it evaporates when M4 serves these names for real.

⚠ **Time the wake on the host, never through the tunnel.** A `curl` from the
Mac to FSN1 carries ~100 ms of transatlantic RTT that has nothing to do with
the wake path — the same host-side-timing rule as the benchmarks (CLAUDE.md).
Use the tunnel to prove the path works; `ssh krill` and curl `127.0.0.1:8080`
to measure it.

**Verify:** the wake curl returns 200 in roughly ≤300 ms and
`krill status` shows the FROZEN→ACTIVE transition happened. Optionally
run the real gates: `m1-gates/a1-warm-wake.sh` gives this box its own
A1 latency distribution — the first metal-tier numbers in the project.

⚠ **Wait for the RAID resync before recording any benchmark.** A fresh
installimage leaves md2 resyncing for ~40 min (`/proc/mdstat`); numbers taken
during it are contaminated. Informal 2026-07-26 result taken *during* resync,
recorded as a smoke test and explicitly NOT as A1: five freeze/wake cycles at
**~100–120 ms end to end**, `last_wake_ms` **49**, with the data plane and
sync-ack on. For comparison, nested virt was A1 p99 298 ms with ~45 ms of
daemon machinery — so the ~200 ms guest-userspace tax appears to be roughly
4× smaller on metal. That is a hypothesis from five samples, not a result.

## Phase 7 — durability beyond the mirror

**EXECUTED 2026-07-26.** RAID1 survives a dead disk, not a dead server, a
fat-fingered `rm`, or a Hetzner account problem. Both moves below are live:
the object store is GCS, and the registry ships to the same bucket nightly.

Four things about the original version of this phase were wrong, and the
corrected steps are what follows:

⚠ **The registry database is `/srv/krill/krill.db`, not `registry.db`.**

⚠ **The GCS backend could not authenticate on this box.** Its auth chain was
`KRILL_GCS_TOKEN` → GCE metadata server → `gcloud`; a Hetzner box is none of
those. "Put a service-account JSON on the box" had nothing to read it.
Service-account auth (RSA-signed JWT → OAuth2 jwt-bearer exchange, stdlib
only) now exists in `internal/objstore/gcsauth.go`, resolved from
`KRILL_GCS_CREDENTIALS` or `GOOGLE_APPLICATION_CREDENTIALS`.

⚠ **"Apps re-seed the new store on next wake" is backwards, and destructive.**
The object store is the arbiter of record (E4). Pointing `--objstore` at an
empty bucket does not migrate history — it *declares every stream empty*, and
`PrepareWake` then faithfully rebuilds each data disk to match, wiping `/data`.
**The record must be copied before the pointer moves** (step 3 below).

⚠ **The nightly `sqlite3 .backup` shell script is replaced by the daemon.**
`sqlite3` shelling out would need its own copy of the credentials and a
`gsutil` that isn't installed; krilld already holds an authenticated object
store. It now takes the snapshot itself (`VACUUM INTO`, a single transaction
against the live DB — `cp` would race the `-wal` file), gzips it, and ships it
with a sidecar manifest that records the cell generation.

### 1. The bucket and a scoped service account (from the Mac)

```bash
P=ycombinator-503223
gcloud services enable iam.googleapis.com --project $P
gcloud storage buckets create gs://krill-fsn1-objstore \
  --project=$P --location=europe-west3 \
  --default-storage-class=STANDARD \
  --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets update gs://krill-fsn1-objstore --versioning
# versioning without an expiry bills forever; 30 days is the recovery window:
gcloud storage buckets update gs://krill-fsn1-objstore --lifecycle-file=- <<'EOF'
{"rule":[{"action":{"type":"Delete"},
          "condition":{"daysSinceNoncurrentTime":30,"isLive":false}}]}
EOF

gcloud iam service-accounts create krill-fsn1 --project=$P \
  --display-name="Krill host agent (krill-fsn1, Hetzner EX44)"
# Bucket-scoped, object-scoped: this key cannot touch IAM, other buckets, or
# this bucket's configuration. ⚠ Retry it — the SA takes a few seconds to
# become visible to the storage API ("does not exist" on the first try).
gcloud storage buckets add-iam-policy-binding gs://krill-fsn1-objstore \
  --member=serviceAccount:krill-fsn1@$P.iam.gserviceaccount.com \
  --role=roles/storage.objectAdmin

gcloud iam service-accounts keys create /tmp/krill-gcs.json \
  --iam-account=krill-fsn1@$P.iam.gserviceaccount.com
```

`europe-west3` (Frankfurt) is the closest well-connected region to FSN1
(Falkenstein). Region choice is load-bearing — see the latency note in step 5.

### 2. Install the key, prove it works, then trust it

```bash
ssh krill 'install -d -m 0700 /etc/krill'
scp /tmp/krill-gcs.json krill:/etc/krill/gcs.json
ssh krill 'chmod 0600 /etc/krill/gcs.json && rm -f /tmp/krill-gcs.json'
rm /tmp/krill-gcs.json          # the Mac does not need a copy

# From the Mac (or anywhere with the key and a Go toolchain): hold the REAL
# bucket to the same contract the fakes pass — auth, conditional PUT, stale-CAS
# rejection, list, delete. A fake proves nothing about IAM.
KRILL_GCS_TEST_BUCKET=krill-fsn1-objstore \
KRILL_GCS_CREDENTIALS=/etc/krill/gcs.json \
  go test -run TestGCSLive -v ./internal/objstore/
```

### 3. Move the record, THEN the pointer (krilld stopped)

```bash
# On the Mac: the new binaries carry objstore-copy, backups, and durability.
make krilld-linux krill-linux fencetool-linux && rsync -az bin/ krill:/root/krill/bin/

ssh krill
# SIGTERM freezes every app, so final checkpoints land in the fsstore first:
systemctl stop krilld
for b in krilld krill fencetool; do
  install -m 0755 /root/krill/bin/$b-linux-amd64 /usr/local/bin/$b
done
export KRILL_GCS_CREDENTIALS=/etc/krill/gcs.json
krill objstore-copy --from file:///srv/krill/objstore \
                    --to gs://krill-fsn1-objstore/krill --dry-run
krill objstore-copy --from file:///srv/krill/objstore \
                    --to gs://krill-fsn1-objstore/krill
```

The copy writes manifests last (an interrupted run never leaves a manifest
pointing at absent segments), skips objects already present byte-for-byte (so
re-running is a cheap resume), and refuses to overwrite a *differing* object
without `--force` — the guard against aiming it at a live store.

**Keep `/srv/krill/objstore` after the cutover.** It costs nothing and it is
the rollback.

### 4. The unit: `--objstore` plus the credential

Add to `[Service]` in `/etc/systemd/system/krilld.service`:

```ini
Environment=KRILL_GCS_CREDENTIALS=/etc/krill/gcs.json
```

and to the `ExecStart` continuation:

```
  --objstore gs://krill-fsn1-objstore/krill \
  --registry-backup-interval 24h \
  --registry-backup-keep 14
```

Then `systemctl daemon-reload && systemctl start krilld`.

Backups need no destination flag: they default to the data-plane object store,
under `_control/registry/` — a prefix app deletion's purge (`apps/<name>/`)
cannot reach. `--registry-backup-store` exists for the one genuinely useful
split posture: **data plane on the local fsstore (fast wakes), registry shipped
to GCS anyway.**

**Verify:**

```bash
journalctl -u krilld -b | grep -E 'data plane on|registry backups on'
# preflight_ok=true, and the log names the identity it authenticated as
krill durability     # live CAS round trip + the backups that actually landed
krill backup         # force one now; prints key, sizes, sha256, cell-gen
```

`preflight_ok` is not a ping. At startup krilld runs a full conditional-PUT
cycle — create, read back, and a **stale CAS that must be rejected** — because
a store that accepts writes while ignoring preconditions would pass any
liveness check and then silently fail to fence a zombie writer. It is logged
loudly and deliberately non-fatal: under `Restart=always`, exiting would
crash-loop through a transient GCS outage instead of staying up to serve
`/healthz` and admin.

Results on this box, 2026-07-26 (all verified):

- `preflight_ok=true`, auth as `krill-fsn1@ycombinator-503223…`, across a
  **full reboot** — the key is readable unattended at boot.
- 5 rows written to `ledger` through the router; `data.ext4` **and** the ship
  cursor then deleted; the next wake rebuilt `/data` from GCS alone and
  returned the identical digest (`f4fd2a5c…`, count 5, sum 15). That is the
  box-died case, executed.
- Two registry snapshots in the bucket with sidecars, ~800 B gzipped from a
  24 KB DB, `restore_cell_gen: 2` recorded in each.

### 5. ⚠ What this costs: GCS is now on the wake path

A GCS round trip from FSN1 to europe-west3 measures **~45 ms**. The wake path
does three of them (stream load, takeover-seal read, seal CAS) and each acked
write does two to four (segment PUTs + a manifest CAS). Informal, host-side,
N=10, RAID resync complete, **not a gate result**:

| | fsstore (earlier, informal) | GCS (2026-07-26) |
|---|---|---|
| warm wake, end to end | ~100–120 ms | p50 **246 ms**, p99 280 ms |
| daemon-reported `last_wake_ms` | 49 | **195** |
| acked write (sync-ack) | not measured | p50 **188 ms**, p99 421 ms |
| read, no write | — | 4–7 ms |

So off-box durability costs roughly **+150 ms of wake**, which is more than
the entire metal-tier gain over nested virt. That is the honest trade, and it
is the right default: D1 promises a write is durable when it is acked, and a
promise kept on the dying host's own disk is not kept. Three known levers, none
taken yet (see ROADMAP): `PrepareWake`'s `CreateStream` load is redundant with
`SealTakeover`'s (−1 round trip), manifest generations could be cached so the
seal skips its read (CAS still catches staleness), and segment+manifest
group-commit batching was already on the M3 findings list.

If latency ever wins over durability, the fallback is one flag:
`--objstore file:///srv/krill/objstore --registry-backup-store gs://…`.

### 6. The restore drill (write it down before you need it)

On a fresh box, after Phases 1–5:

```bash
install -d -m 0700 /etc/krill      # then copy the key back in
export KRILL_ADMIN=http://127.0.0.1:9091 KRILL_GCS_CREDENTIALS=/etc/krill/gcs.json
# 1. The registry (app catalog + epoch mint). Newest key from `krill durability`:
krill objstore-copy --from gs://krill-fsn1-objstore/krill --to file:///tmp/restore \
                    --prefix _control/registry/
gunzip -c /tmp/restore/_control/registry/<newest>.db.gz > /srv/krill/krill.db
# 2. E1: a restored mint re-issues counters it already handed out. The sidecar
#    JSON next to the snapshot names the minimum safe generation
#    (restore_cell_gen). Bump --cell-gen in the unit to at least that, and a
#    single new generation fences every pre-loss epoch at once.
# 3. Start krilld pointed at the SAME bucket. Every app's data disk is absent,
#    so the first wake rebuilds each one from its stream — which is exactly the
#    path proven in step 4 above.
```

Apps' rootfs images do not live in the object store: redeploy them (`krill
deploy`), which is also how they were created.

## Phase 8 — the name (`krill.run`, bought 2026-07-26)

Done **before** M4, deliberately: DNS propagation, TLD choice and DNS-provider
choice all gate the doorman, and each is the kind of thing that stalls a
milestone when discovered mid-build. **This phase changes no posture** — the
name points at a closed door. Registrar and DNS: **Cloudflare** (at-cost, and
its DNS API is what the M4 wildcard certificate will need).

⚠ **TLD constraint, not taste:** `.dev`, `.app` and `.page` are HSTS-preloaded,
so browsers refuse plain HTTP on them unconditionally — that would have killed
tunnel-era `http://…:8080` testing before the doorman exists. `.run` is
Identity Digital, not preloaded (verified against hstspreload.org).

The zone, all records **DNS only** (grey cloud — Cloudflare's proxy can't reach
8080, and M4 wants raw client behavior arriving at our own edge):

| Name | Type | Value | Why |
|---|---|---|---|
| `*.krill.run` | A | `46.4.64.187` | the app wildcard — `<app>.krill.run` |
| `krill.run` | A | `46.4.64.187` | apex; a wildcard does not match the bare name |
| `*.local.krill.run` | A | `127.0.0.1` | tunnel-era browsing — ⚠ needs `/etc/resolver/krill.run`, see Phase 6 |
| `krill.run` | MX | `0 .` | null MX (RFC 7505): this domain sends/receives no mail |
| `krill.run` | TXT | `v=spf1 -all` | anti-spoofing — the name will appear in links sent to other people |
| `_dmarc.krill.run` | TXT | `v=DMARC1; p=reject; adkim=s; aspf=s` | `p=reject` is inherited by subdomains |

The mail lockdown is posture, not decoration: `krill.run` is about to start
appearing in share links, and an unprotected domain is trivially spoofable.
Reverse it only if the platform ever sends mail — and then per the egress note
below, through a transactional HTTPS API, never SMTP from this IP.

Verify from a resolver that isn't yours:

```bash
dig +short NS krill.run @1.1.1.1                   # cloudflare NS = delegation landed
dig +short anything.krill.run @1.1.1.1             # → 46.4.64.187 (wildcard)
dig +short ledger.local.krill.run @1.1.1.1         # → 127.0.0.1
dig +short TXT _dmarc.krill.run @1.1.1.1
```

Then set `--base-host krill.run` in the unit (Phase 5) and **re-prove the
posture from the Mac** — a name that resolves must still reach nothing:

```bash
nc -vz -w 5 46.4.64.187 8080     # must fail
nc -vz -w 5 46.4.64.187 9091     # must fail
curl -m 5 http://ledger.krill.run/ ; echo "exit=$?"   # must time out, not serve
```

**Staged for M4, already done:** a Cloudflare API token scoped to
`Zone:DNS:Edit` **+ `Zone:Zone:Read`** (both required by
`caddy-dns/cloudflare`; `DNS:Edit` alone cannot resolve the zone ID) on the
`krill.run` zone only, no TTL and no client-IP filter. A TTL here would mean
silent certificate-renewal failure 60–90 days out; the IP filter is deferred
because this box has an IPv6 /64 and a v4-only filter would 403 any request
that egresses over v6 — add it after ACME works, listing both addresses.
At M4 the token lands as `/etc/krill/cloudflare.env`, mode `0600`, referenced
by `EnvironmentFile=` (never inline in a unit file — those are world-readable).

## Phase 9 — the doorman, and then the door (M4)

*Not yet executed. Written alongside the M4 code so the sequence exists before
anyone is tempted to improvise it at 1am.*

**The order below is the milestone's PT-3 and is not negotiable.** Steps 1–6
change nothing an outsider can reach; F1–F3, F5 and F6 are all provable
through the tunnel while 80/443 are still closed. Only step 7 opens the door,
and F4 and F7 run after it. A green F4 obtained by exposing the router early is
a failed milestone, not an early one.

**One consequence worth saying out loud: the router never un-loopbacks.** Caddy
binds 443 and proxies to `127.0.0.1:8080`. The "expose the router" step that
every earlier plan treated as the last dangerous commit simply never happens —
a risk deleted rather than sequenced.

```
internet → Caddy :443 ──forward_auth──> krill-doorman :9090 (unprivileged)
                    └──(only on 200)──> krilld router :8080 (loopback, root)
```

### 1. A Google OAuth client

console.cloud.google.com → APIs & Services → Credentials → **OAuth client ID**,
type *Web application*.

- **Authorized redirect URI:** `https://auth.krill.run/_krill/auth/callback` —
  exactly one, because Google matches redirect URIs exactly and a wildcard of
  app subdomains is not possible. This is why the doorman has a single auth
  host and hands sessions to app hosts over a one-time code.
- **Scopes: `openid`, `email`, `profile` and nothing else.** Those are
  non-sensitive, so the client needs no Google verification review. ⚠ This is
  the mitigation for the known unknown recorded in the 2026-07-26 session
  summary: F4 FAILs on "a security warning of any kind", and an unverified
  client asking for sensitive scopes shows an interstitial. **Confirm before
  building the flow, not the week F4 runs**: with a Testing-status client you
  are limited to explicitly-added test users, which would also fail F4.

```bash
install -d -m 0750 -o root -g krill-doorman /etc/krill
cat > /etc/krill/doorman.env <<'EOF'
KRILL_GOOGLE_CLIENT_ID=<...>.apps.googleusercontent.com
KRILL_GOOGLE_CLIENT_SECRET=<...>
EOF
chmod 0640 /etc/krill/doorman.env
chown root:krill-doorman /etc/krill/doorman.env
```

Secrets go in an `EnvironmentFile`, never inline in a unit — unit files are
world-readable.

### 2. The doorman's own user, state, and object store

It runs **unprivileged**. krilld is root (taps, `mkfs.ext4`, docker) and the
internet-facing OAuth surface must not live inside it.

```bash
useradd --system --home /var/lib/krill-doorman --shell /usr/sbin/nologin krill-doorman
install -d -m 0700 -o krill-doorman -g krill-doorman /var/lib/krill-doorman
install -m 0755 krill-doorman-linux-amd64 /usr/local/bin/krill-doorman
```

Its object store is where revocations become durable. ⚠ **Give it its own GCS
prefix, and its own readable copy of the credentials** — the doorman's
recovery story and the registry's are opposite (the registry is safe to roll
back because `cell_gen` fences it; a rolled-back revocation silently
un-revokes a share), which is why they do not share a database and should not
share a prefix either.

```bash
install -m 0640 -o root -g krill-doorman /etc/krill/gcs.json /etc/krill/gcs-doorman.json
```

### 3. The doorman unit

```ini
# /etc/systemd/system/krill-doorman.service
[Unit]
Description=krill-doorman (M4 front door)
After=network-online.target krilld.service
Wants=network-online.target

[Service]
User=krill-doorman
Group=krill-doorman
EnvironmentFile=/etc/krill/doorman.env
Environment=KRILL_GCS_CREDENTIALS=/etc/krill/gcs-doorman.json
ExecStart=/usr/local/bin/krill-doorman \
  --state-dir      /var/lib/krill-doorman \
  --listen         127.0.0.1:9090 \
  --control        127.0.0.1:9092 \
  --krilld-admin   http://127.0.0.1:9091 \
  --base-host      krill.run \
  --auth-host      auth.krill.run \
  --scheme         https \
  --owners         samshulman6@gmail.com \
  --objstore       gs://krill-fsn1-objstore/doorman
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/krill-doorman

[Install]
WantedBy=multi-user.target
```

Verify **before** anything is exposed:

```bash
systemctl enable --now krill-doorman
curl -s 127.0.0.1:9092/v1/status | jq '{base_host, auth_host, revoke_durable, identity_key}'
# revoke_durable MUST be true. If it is false, every revoke will be refused
# and F2 cannot pass — which is the intended failure, not a bug to work around.
```

### 4. Teach krilld about the doorman's key, the builder VM, and egress

Three additions to `/etc/systemd/system/krilld.service` (⚠ read the live file
first — it carries Phase 7's `--objstore gs://…` and backup flags; a backup
was left at `krilld.service.bak-2026-07-26`):

```
  --identity-pubkey-file /var/lib/krill-doorman/identity.pub \
  --route-suffixes       krill.run,krill.local \
  --build-vm-image       /srv/fc/builder.ext4 \
  --build-vm-kernel      /srv/fc/vmlinux-builder \
  --build-isolation      untrusted \
  --egress-build-allow   deb.debian.org,security.debian.org,pypi.org,files.pythonhosted.org
```

- **`--identity-pubkey-file`** is how a guest with no outbound network can
  still verify who is calling it: krilld appends `krill_idkey=<b64>` to every
  guest's kernel command line, and the generated init exports it. The file is
  world-readable on purpose and rewritten by the doorman at every start; a
  stale copy would make every app reject every token, which `m4-gates/00-setup.sh`
  checks for.
- **`--route-suffixes`** is defense in depth. The doorman pins the suffix
  unconditionally; this closes the same hole at the loopback router. Include
  `krill.local` or the gate suites — which correctly send `krill.local` —
  stop routing.
- **`--egress-build-allow`** is the deliberate widening: "the registry and
  nothing else" is the right posture and almost every real Dockerfile also
  runs `apt-get` or `pip install`. Leave it empty if you would rather find out
  the hard way which images need it.

Then build the builder image once (`m4-gates/builder-image/README.md` has the
open kernel question, which is the one thing here likely to need iteration):

```bash
sudo m4-gates/builder-image/build.sh /srv/fc/builder.ext4
systemctl restart krilld
journalctl -u krilld -n 40 | grep -E 'isolated builder|egress baseline'
```

### 5. Caddy, against ACME **staging**, serving nothing

Deliberately before the doorman is wired in: this is the only piece with an
external dependency that can fail in ways you do not control, and Let's
Encrypt rate-limits **per registered domain per week** — a handful of botched
production attempts costs you `krill.run` for seven days.

```bash
apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf https://dl.cloudsmith.io/public/caddy/xcaddy/gpg.key \
  | gpg --dearmor -o /usr/share/keyrings/caddy-xcaddy-archive-keyring.gpg
# Caddy needs the cloudflare DNS module compiled in — the stock binary has no
# DNS-01 provider, and ACME can only issue *.krill.run over DNS-01.
apt-get install -y golang-go
GOBIN=/usr/local/bin go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy build --with github.com/caddy-dns/cloudflare --output /usr/local/bin/caddy
/usr/local/bin/caddy list-modules | grep dns.providers.cloudflare   # must print
```

The token from Phase 8 lands now:

```bash
install -d -m 0750 -o root -g caddy /etc/krill
printf 'CF_API_TOKEN=%s\n' '<token>' > /etc/krill/cloudflare.env
chmod 0640 /etc/krill/cloudflare.env && chown root:caddy /etc/krill/cloudflare.env
```

`/etc/caddy/Caddyfile` — see `deploy/Caddyfile` in the repo for the annotated
version; the shape is:

```caddyfile
{
    # STAGING FIRST. Delete this line only once a staging cert has issued.
    acme_ca https://acme-staging-v02.api.letsencrypt.org/directory
    email samshulman6@gmail.com
}

*.krill.run, krill.run {
    tls { dns cloudflare {env.CF_API_TOKEN} }
    respond "krill" 404
}
```

```bash
ufw allow 80/tcp && ufw allow 443/tcp    # ⚠ this is the first inbound opening
systemctl enable --now caddy
journalctl -u caddy -f | grep -i certificate     # watch DNS-01 complete
```

⚠ **If DNS-01 fails with something that reads like a generic auth error**, the
token is almost certainly missing `Zone:Zone:Read` — `DNS:Edit` alone cannot
resolve the zone ID (Phase 8, gotcha 3).

### 6. Wire the doorman in, still on staging

Replace the `respond` with the real thing (annotated in `deploy/Caddyfile`):

```caddyfile
*.krill.run, krill.run {
    tls { dns cloudflare {env.CF_API_TOKEN} }

    # Never let a client supply its own identity headers.
    request_header -X-App-User
    request_header -X-App-User-Id
    request_header -X-App-User-Name
    request_header -X-App-Plane
    request_header -X-Krill-Token

    # The doorman owns /_krill/* on every host: the OAuth callback, the share
    # links, JWKS, logout. forward_auth cannot serve these — they are the flow
    # itself, not a check on it.
    handle /_krill/* {
        reverse_proxy 127.0.0.1:9090
    }

    handle {
        forward_auth 127.0.0.1:9090 {
            uri /_krill/auth/verify
            copy_headers X-App-User X-App-User-Id X-App-User-Name X-App-Plane X-Krill-Token
        }
        # Reached ONLY on a 200 above. A redirect to Google, a 403 or a 404
        # is returned to the browser verbatim and never touches krilld — which
        # is what makes "no unauthorized request wakes an app" structural.
        reverse_proxy 127.0.0.1:8080 {
            # The guest must not see the session cookie. It is host-only
            # already, so this is belt and braces on an untrusted guest.
            header_up Cookie "(^|; ?)(__Host-)?krill_app=[^;]*;?" ""
        }
    }
}
```

Now run the tunnel-era gates — **all of F1, F2, F3, F5 and F6** — before
touching the staging line. They do not need the public listener.

### 7. Production certificate, then F4 and F7

Only once every earlier gate is green:

```bash
sed -i '/acme_ca/d' /etc/caddy/Caddyfile
systemctl reload caddy
journalctl -u caddy -n 50 | grep -i 'certificate obtained'
```

Then, from the Mac (**not** from the box — the outside view is the point):

```bash
cd m4-gates && BOX_IP=46.4.64.187 ./f7-exposure.sh
```

⚠ **Phase 8's port scan is superseded here and nowhere else**: 80 and 443 must
now answer, while 8080, 9090, 9091 and 9092 must still refuse. Everything else
Phase 8 asserted still holds.

F4 last: `krill share watchlist --plane use`, send the link over iMessage with
no instructions, and watch.

## Running the gate suites on this box (they assume they own the machine)

`m1-gates/` and `wake-bench/` were written for throwaway bench VMs where
nothing else ran. This box hosts production, and two collisions bite:

⚠ **`m1-gates/00-setup.sh` used to `pkill -x krilld`.** Under
`Restart=always` that just makes systemd start a new one, and then two daemons
fight over ports 8080/9091, the tap devices, and the data dir. The script now
**refuses to run while the unit is active** and tells you to stop it.

⚠ **`wake-bench` wants `172.16.0.1/24` on `tap0`; krilld already has
`172.16.0.1/30` on `krill0`.** Same address, and the bench's `/24` covers every
app subnet. The `/30` is more specific, so packets for the bench guest
(`172.16.0.2`) route out `krill0` — which is `linkdown` — and the bench dies at
its warm-up pings. krilld's taps **persist across freeze**, so stopping the
daemon is not enough: delete them.

Deleting `krill0`/`krill1` is safe and reversible. Host-side MACs are
deterministic by design (`internal/network`, the same scheme the bench uses)
precisely so a re-created tap keeps restored guests' ARP caches valid.
**Verified 2026-07-26:** after deleting both taps and running the bench, both
apps woke from their existing snapshots with no rebuild and no fence, and
`ledger`'s rows were intact.

```bash
# --- before any gate run ---
systemctl stop krilld
for t in krill0 krill1; do ip link delete "$t" 2>/dev/null || true; done

# --- A1 on metal, with a SCRATCH data dir so production state is untouched ---
cd /root/krill/m1-gates
install -m 0755 /root/krill/bin/krilld-linux-amd64 ./krilld
# Three runs make the tiers comparable and price Phase 7 honestly:
#   1. data plane OFF   → comparable to the 2026-07-23 nested A1 numbers
#   2. data plane + fsstore → comparable to the M3 gate numbers (p50 205 ms)
#   3. data plane + GCS  → the real production configuration
KRILL_DATA=/srv/krill-gates KRILLD_EXTRA_FLAGS='--data-plane=false' ./00-setup.sh
./a1-warm-wake.sh
# then, for runs 2 and 3, rm -rf /srv/krill-gates and repeat with
#   KRILLD_EXTRA_FLAGS='--objstore file:///srv/krill-gates/objstore'
#   KRILLD_EXTRA_FLAGS='--objstore gs://krill-fsn1-objstore/gate-scratch'
# (a scratch PREFIX, never the production one — gate apps must not land in it)

# --- G4, the last open benchmark gate (no krilld involved; drives FC directly) ---
cd /root/krill/wake-bench
./30-storm.sh python 50        # snapshot already staged, see below

# --- after ---
pkill -x krilld; ip link delete tap0 2>/dev/null || true
systemctl start krilld
curl -s -H "Host: ledger.krill.run" http://127.0.0.1:8080/   # sanity
```

**Already staged on this box (2026-07-26), so the timed runs are one command
each:** `02-build-guests.sh` has run (python + node ext4 images in `/srv/fc`),
and `10-boot-and-snapshot.sh python` has produced `/srv/snaps/python/`
(`mem` 104 MB actual after balloon, `vmstate` 14 KB). **Untimed prep, but one
real number fell out of it: cold boot-to-first-200 on metal is 904 ms**, versus
~1.67 s implied by the nested G5 ratio — about 1.8× faster. Not a gate result.

`00-host-setup.sh` stays **unrun** on this box, deliberately: it disables
unattended-upgrades and is written for a machine you throw away.

## Notes & gotchas specific to this box

- ⚠ **`guestbook` is not data-plane-backed.** It writes
  `/var/lib/guestbook.db` — on the rootfs, not the `/data` disk — so its
  stream head is permanently LSN 0 and nothing about it is shipped anywhere.
  Its data survives freeze/wake because the rootfs file survives, which reads
  like durability and isn't: a redeploy or a dead box loses it. The 2026-07-26
  "durability round-trip passed" note in Phase 6 means less than it sounds.
  **Verify the data plane with `m3-gates/examples/ledger`** (`/data/app.db`),
  and check `krill stream <app>` for a non-zero `head_lsn` before believing
  any app is covered.
- **Guests have no outbound internet, on purpose (for now).** krilld
  creates the tap and pins the ARP entry; nothing configures NAT. Apps
  can serve requests but can't call external APIs. When some app
  legitimately needs egress, add the masquerade rule and the egress
  baseline (metadata drop is N/A on Hetzner — no credential-bearing
  metadata service — but port-587 block + rate limits, per the posture
  map; Hetzner already blocks outbound 25/465 provider-side) in the
  same change. Never masquerade without the baseline: Hetzner
  null-routes abusive sources, and one host = total outage. Any future
  email feature goes through a transactional API (SES/Resend, HTTPS),
  never direct SMTP from this IP.
- **Docker + ufw:** krilld never publishes container ports (docker is
  build-only here), so Docker's iptables games don't open anything. If
  `docker ps` ever shows a port-published container, something is wrong.
- **The ~200 ms guest-userspace wake tax travels with the guest, not
  the host** — expect metal wakes near A1's shape, not magically at the
  115 ms bench number, until the snapshot-while-hot lever is pulled.
- **Hourly billing:** the box bills per hour with no setup fee. A
  botched install is not precious — `installimage` again from rescue,
  or cancel/reorder, costs almost nothing.
- **Subnets cap at ~255 apps** (/30 allocation) — the box's
  architectural ceiling, far beyond friends-scale.

## What this box is still waiting for (do NOT pre-empt)

- Public 80/443, TLS, and un-loopbacking the router: all land together as
  part of **M4** (doorman first, exposure second). Wildcard DNS and the
  ACME-capable API token are **already done** (Phase 8) — deliberately
  ahead of M4, because they gate it and nothing else.
- Builder isolation + full egress policy: per the ROADMAP posture map,
  required before anyone untrusted can deploy or edit-share.
