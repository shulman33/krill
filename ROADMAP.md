# ROADMAP — The Sleeping Cloud

*Written 2026-07-23; last updated 2026-07-23 (M2 accepted). This is the
cross-session continuity document: a fresh Claude Code session (or a future
Sam) should be able to read CLAUDE.md, then this file, and start productive
work with zero rediscovery. Update the "Current state" and "Next action"
sections at the end of any session that changes them.*

---

## What this project is, and what it is for

**Krill** — a cloud platform for **"small software"** (from YC's Request for
Startups): agent-written apps with ~5 users that deploy in one call, share
like a Google Doc, and scale to zero via Firecracker microVM
snapshot/restore. The metaphor: the ocean runs on uncountable tiny organisms.
(The design docs use "the sleeping cloud" as narrative framing — that stays;
Krill is the product/repo/binary name.)

**The goal, decided explicitly on 2026-07-23: this is a personal project,
built for resume value and for fun — NOT a startup validation exercise.**
Market demand is irrelevant to whether it gets built. This decision has real
engineering consequences:

- Build the **real technology** (own microVM orchestration, own fencing
  protocol, own data plane) — not thin wrappers over Fly/Railway. A wrapper
  was proposed as a demand test and **rejected by Sam**: impressive depth is
  the point.
- Optimize milestones for demoability and learning, not time-to-market.
- Public repo + write-ups are first-class deliverables, not marketing.

## Where we are (verified state, 2026-07-23)

The design phase is **complete and unusually well-verified**. No product code
exists yet. The directory is **not yet a git repository** (fix in M1 step 0).

| Asset | Status |
|---|---|
| Protocol design ("Fencing the Sleeping Cloud", `docs/small-software-cloud-pressure-test.html`) | 9 pressure tests, 7 design commitments, interactive economics model. Artifact URL in CLAUDE.md. |
| `FencingProtocol.tla` + `.cfg` | **Model-checked**: all 7 invariants hold over 26,618 states; each of the 3 fence toggles produces a counterexample when disabled. TLC found PT-9 ("slow waker") after human review missed it. |
| Architecture doc (`docs/sleeping-cloud-architecture.html`) | 6 diagrams, D1–D6, fence pills mark epoch-check enforcement points. |
| Plain-English explainer (`docs/sleeping-cloud-explained.html`) | For non-engineers; analogies mirror the technical docs. |
| Wake-path benchmark (`wake-path-benchmark.md` + `wake-bench/`) | **RAN 2026-07-23 on GCP nested virt (~$2.50 total).** G1 ✓ (warm resume p99 115 ms), G2 ✓ (cold 432 ms), G3 ✓ (69 MB stored/app), G5 ✓ (14.5× vs cold boot), G4 escalate-at-N=50-only (linear to N=25; throughput already 6× requirement). **Decision: COMMIT to the microVM architecture.** Full data: `wake-bench/RESULTS-2026-07-23-nested.md` + `wake-bench/results-nested-2026-07-23/`. **RE-RAN 2026-07-26 ON METAL: G4 CLOSED — p99 109 ms at N=50, 0 errors (was 4206 ms nested), so all five gates now PASS and no open benchmark gate remains.** G1 26 ms, G5 42× on the same box. `wake-bench/RESULTS-2026-07-26-metal.md`. |
| GCP project `ycombinator-503223` | **Clean — nothing billing.** Instance deleted 2026-07-23; full resource sweep confirmed zero instances/disks/IPs/snapshots/buckets/datasets. |

## Decisions made (with what was rejected)

1. **Firecracker microVMs**, not V8 isolates / Wasm / gVisor. Agents write
   arbitrary Python/Node with native deps; full-Linux compat beats density
   ceiling. Now benchmark-validated, not just argued.
2. **SQLite-per-app, WAL shipped by the HOST agent**, never by the guest.
   Guest code is agent-written and untrusted; epoch stamping in guest hands
   is not fencing. (Protocol doc §1.1, load-bearing decision #1.)
3. **Leases route, epochs protect.** The lease is an optimization allowed to
   be wrong; every effect is individually fenced downstream, because on this
   platform arbitrary pause is the normal case (PT-3). The composite epoch is
   `cell_gen ‖ counter`, compared as one integer.
4. **All 3 fences are load-bearing** — E3 (gateway), E5 (snapshot
   registration), E6 (replay-on-restore). E5 was believed defense-in-depth;
   TLC proved it's the ONLY fence on the slow-waker path. Never weaken it.
5. **Two-tier benchmarking**: GCP nested virt first (~$1/hr), EC2 `.metal`
   spot only on escalation. Verdicts are one-sided: nested PASS is
   conclusive, nested FAIL = ESCALATE. Physical-server rental rejected by Sam.
6. **Prototype-on-rented-infra rejected** (2026-07-23): build the real
   platform. See "What this project is for."
7. **Documentation set moves together** — the tripwire in CLAUDE.md. Spec
   first, TLC re-run (positive + 3 negative configs), then both HTML docs,
   then republish to existing artifact URLs.
8. **Commercial posture: keep the verified core, buy the perimeter
   (2026-07-24, with Sam).** The charter is unchanged (resume value first,
   market demand irrelevant) — this decision records which custom code we
   would *defend* if the project ever commercializes, so milestone designs
   stop drifting toward hand-rolling commodity plumbing. The hand-written
   core (snapshot lifecycle, E1–E6, host-side WAL shipping, spec+sim
   harness) has no off-the-shelf equivalent and a stronger verification
   story than most proven software; everything commodity around it gets
   proven components. Consequences: M4 assembles its auth plumbing from
   proven parts, M5's "real lease service" becomes etcd, the ext4+jbd2
   parser retires via a host-served block device, and two **today**-risks
   (not at-scale risks) get closed before anyone untrusted touches the
   platform: the builder's trust boundary and guest egress. Full map in
   "Commercial posture — build vs. buy" below. Rejected: Litestream/LiteFS as data-plane replacements
   (both trust the guest or treat a lease as correctness — violates the
   untrusted-guest model and PT-3).

## M1-gating decisions — RESOLVED with Sam, 2026-07-23

1. **Name: Krill.** Repo `krill`, host-agent binary `krilld`. Sam rejected
   all sleep-themed names ("sleepy" connotes slow/lazy — the system's virtues
   are instant/ready/cheap). Known collision, accepted: NLnet Labs' `krill`
   (an RPKI Certificate Authority — different domain entirely); "Shoal" was
   evaluated as an alternative and is more crowded (GlassFish Shoal,
   shoalstack.com). Rename is cheap if this ever becomes a company.
2. **Language: Go + TypeScript hybrid.** Go for `krilld` and the data plane —
   the two closest prior-art codebases are Go (**litestream** for SQLite WAL
   shipping, **firecracker-go-sdk** / firecracker-containerd for the VMM
   driver). TypeScript only for the M2 MCP server (first-class MCP SDK),
   which talks JSON to krilld's API.
3. **Dev hardware: scripted on-demand GCP nested virt** (~$1/hr while
   working; `wake-bench/README.md` has the exact command; Local SSD dies with
   the instance; this shape needs Local SSDs in counts of 2/4/8; us-central1
   was capacity-starved, us-east1-b worked). ~~Permanent always-on hardware
   (mini-PC vs Hetzner) is deliberately deferred until M4 makes it matter.~~
   **RESOLVED 2026-07-26: Hetzner EX44-1-LTD in FSN1** (i5-13500, 64 GB,
   2×512 GB NVMe RAID1; €59/mo incl. IPv4, hourly-billed, €0 setup) — the
   first true bare-metal tier. Cloud-instances-behind-an-LB was evaluated
   and rejected (nested-virt tax forever, ~5× cost, no real HA — krilld
   hosts are stateful; the router IS the LB). Sam's M1 Mac mini rejected
   (no KVM under macOS; aarch64 would fork every artifact). Provisioning
   runbook: `SERVER-SETUP.md`. M5's second host starts as two nested-KVM
   VMs on this box (free PT-1 live-fire), then an hourly-billed twin EX44
   for a weekend — a standing second box must be earned by demand.

## The milestones

Every milestone ends with something demoable. Pre-register acceptance gates
before building (this discipline made the benchmark trustworthy — keep it).

### M1 — `krilld`, the host agent — ✅ DONE 2026-07-23 (single session)

**All four pre-registered gates PASS on GCP nested virt** (n2-standard-16,
us-east1-c, FC v1.16.1): A1 warm-wake p99 298 ms through the router (gate
≤ 300; krilld's own machinery is ~45 ms of it — see the instrumented
breakdown in `m1-gates/RESULTS-2026-07-23-nested.md`), A2 unattended
freeze with the VMM process gone, A3 100 sleep/wake cycles with a gapless
acked-write ledger, A4 10-app frozen fleet with kernel-assigned-IP identity
checks. Step 0 (git repo + TLC CI with the three negative configs) is in.
GitHub repo creation was permission-blocked in-session — run
`gh repo create krill --public --source=. --push` once, then the CI badges
go live.

One daemon on one KVM host: register apps, boot Firecracker VMs, snapshot on
idle, wake on request.

**Step 0:** `git init`, initial commit of everything, create the public
GitHub repo (public from day one — resume artifact), add a GitHub Action
that runs TLC on `FencingProtocol.cfg` AND asserts the three negative
configs still fail (a CI badge that says "protocol model-checked" is gold).

**Components:**
- **App registry** — apps, rootfs paths, snapshot paths, state. SQLite is
  fine (yes, the platform's own metadata in SQLite is on-brand).
- **Firecracker driver** — create/configure/boot/pause/snapshot/restore via
  the API socket. Port the exact call sequence from `wake-bench/lib.sh`
  (machine-config → boot-source → drive → net → balloon → InstanceStart;
  balloon-inflate → sleep → deflate → pause → snapshot/create; snapshot/load
  with `resume_vm:true`).
- **Lifecycle state machine** — M1 simplification of the doc's machine:
  FROZEN → WAKING → ACTIVE → SNAPSHOTTING → FROZEN. No leases/epochs yet
  (single host, single instance per app enforced by process-local mutex) —
  the fencing machinery arrives in M3 where it means something.
- **Wake-on-request router** — HTTP reverse proxy; unknown-or-frozen app →
  hold the request, trigger wake, replay when the guest answers. This is D2
  in the architecture doc as code.
- **Network manager** — per-app tap with **deterministic MAC** and **pinned
  neighbor entry** (both learned the hard way; see gotchas). Give each app
  its own /30 (e.g. 172.16.N.0/30) instead of the benchmark's
  netns-with-identical-IP trick — simpler for a router that talks to many
  guests at once.
- **Rootfs manager** — golden + CoW copy per resume. NEVER resume a memory
  snapshot against a mutated disk (benchmark doc §3.2).

**Acceptance gates (pre-registered):**
- A1: `curl` of a frozen app returns 200, warm-wake p99 ≤ 300 ms over 100
  wakes on the nested dev box (we measured 115 ms with bash, so the daemon
  has ~2.5× budget for its own overhead).
- A2: idle timeout demotes ACTIVE → FROZEN unattended; RAM is actually freed.
- A3: 100 sleep/wake cycles of an app doing SQLite writes → zero corruption,
  zero lost acked-before-sleep writes.
- A4: 10 different apps resident as snapshots on one host, any wakeable.

### M2 — the deploy path — ✅ DONE 2026-07-23 (single session)

**All four pre-registered gates PASS on GCP nested virt**
(`m2-gates/RESULTS-2026-07-23-nested.md`): B1 one-command deploy of a plain
Dockerfile dir → verified-ready in 22 s cold cache / 4 s warm; B2 redeploy
6 s with stale-snapshot-never-serves and data-reset asserted; B3 broken app
returns its Python traceback structured **in the deploy response** (and via
the logs tool); B4 one MCP `tools/call deploy` → URL + ready end to end.

What was built: server-side build pipeline in krilld
(`POST /v1/apps/{name}/deploy` takes a tar.gz context: docker build →
docker export → `mkfs.ext4 -d`, no loop mounts; generated `/krill-init.sh`
satisfies the network contract and execs the image CMD), deploy responses
that verify by waking the app once, `GET .../logs` with structured error
parsing (`internal/guestlog`), the `krill` CLI, and the TypeScript MCP
server (`mcp/`) with deploy/logs/apps/delete_app tools.

Two contracts worth remembering: **redeploy resets app data** (disk rebuilt
from the new golden — durable data is M3's job), and apps keep their subnet
(and IP) across redeploys. Big hardware finding: the Firecracker CI kernel
has `CONFIG_IP_PNP`, so krilld's `ip=` boot arg configures eth0 before
userspace — **images need no iproute2, arbitrary Dockerfiles deploy
unmodified** (the generated init's `ip`-if-present path is a fallback).

### M3 — the data plane (the crown jewel) — ✅ ACCEPTED 2026-07-23 (single session)

**All four pre-registered gates PASS** (`m3-gates/RESULTS-2026-07-23-nested.md`,
hardware ≈ $0.20): C1 durability — 200/200 acked rows byte-identical after
SIGKILL + deletion of every app-local file, rebuilt from the object store
via the E6 path; C2 fencing — stale append and stale registration rejected
with the manifest byte-identical, 3 monotone takeover seals; C3 PITR —
branch served exactly phase A, parent stream untouched, phase B recovered
via `--from-stream s0`; C4 spec-as-oracle — 10,000 seeds clean, all three
negative fence configs reproduce their TLC counterexamples. C-info: wake
p50 205 ms / max 253 ms WITH the data plane + sync-ack (A1's gate was
300 ms) — the fencing machinery is latency-noise next to the
guest-userspace tax.

Rules E1–E6 as code, built and green under `-race`:

- **`internal/objstore`** — Store with conditional PUT (E4's arbiter):
  fsstore (single-host default), GCS via XML-API
  `x-goog-if-generation-match`, memstore; one contract test over all three.
- **`internal/sqlitewal`** — WAL parse with the cumulative checksum chain
  (both byte orders), salt-change detection, Replay to exact commit
  boundaries — verified against real SQLite files.
- **`internal/ext4`** — read-only ext4 + **jbd2 journal replay**, so the
  host tails a guest's WAL from outside the VM. Fixtures generated by a
  real Linux kernel in docker; the live fixture is proven non-vacuous.
- **`internal/dataplane`** — Epoch (`cell_gen‖counter`, E1; minted in the
  registry), Manifest mutated only by CAS (E4), Gateway
  (AppendSegment/AppendRebase E2/E3 with forward seal, SealTakeover as a
  fenced WRITE per the PT-9 commitment, RegisterCheckpoint E5 at the CAS),
  Shipper (tail → stamp → ship; guest WAL resets become **rebase
  segments** — durability preserved, PITR granularity coarsens), Restore =
  checkpoint + WAL-delta replay across branch lineage (E6), CreateBranch
  (D4). Coordinator wires it into the lifecycle: wake = mint + fenced
  takeover seal (stale wake dies) + rebuild-if-diverged; freeze = final
  ship + E5 checkpoint; gateway fence = zombie killed.
- **`internal/dataplane/sim`** — the spec as oracle: spec-mirrored actions
  at spec atomicity through the REAL gateway/manifest code, I1–I4 (+
  OneInstancePerEpoch, CurEpochBounded) asserted after every step.
  **C4 PASS 2026-07-23: 10,000 positive seeds clean; each negative config
  reproduces its TLC counterexample** (PT-1 @ seed 0, E6 bug @ seed 0,
  PT-9 slow waker @ seed 1428).
- Router **sync-ack** (D1): responses hold until durable; failure = 502,
  never a false ack. Admin `GET .../stream`, `POST .../restore`; CLI
  `krill stream`, `krill restore --at-lsn/--at-time [--from-stream]`.
- **Contract change vs M2:** apps get a second virtio disk mounted at
  `/data` (SQLite contract: `/data/app.db`); **redeploy now preserves app
  data** (B2's reset assertion is historical).

Gates: `m3-gates/GATES.md` was pre-registered before any code; all four
passed (see the heading above and the results file).

### M4 — the doorman (~1–2 weeks)

Edge auth proxy: Google OAuth, signed identity header scoped per app
(`X-App-User` + JWT), share links / domain shares / revoke, the three-plane
ACL (use / edit / data) from the explainer's share sheet. Gate: a
non-technical friend opens a shared link and uses an app with zero setup.
Side effect: if recipients keep returning, the rejected demand test runs
itself for free.

Build-vs-buy for this milestone (decision #8): hand-write only what *is*
the product — the three-plane ACL, share links/revoke, per-app token
scoping (JWT audience = one app), and the integration with the router's
wake hold. The commodity plumbing (OAuth flow, session cookies, JWT/JWKS
handling) comes from proven libraries/components in the
oauth2-proxy/Pomerium mold — auth plumbing is the highest
security-bug-density code there is, and none of it is differentiated.
Dex (OIDC federation) or a WorkOS-class connector is the later path to
enterprise SSO without touching sessions or the ACL. Scope note: the
moment **edit** shares reach people Sam doesn't fully trust, their agents
can push Dockerfiles — builder isolation (posture map below) becomes M4
scope, not dessert. Same trigger for egress: **use** shares mean other
people drive the apps and **edit** shares mean other people's agents
write the code they run — the metadata-IP drop and port-25 block
(today-risk #2 in the posture map) must land no later than the first
share link.

### M5 — dessert tray (pick by joy, any order)

Second host + etcd-backed lease/epoch mint (PT-1 live; decision #8 — no
homegrown lease service, and the fences stay at the storage layer per the
spec: etcd replaces the *mint*, never the checks), host-served data disks
(NBD/ublk/vhost-user-blk) to retire the `internal/ext4` jbd2 parser, UFFD
lazy restore from network storage, content-addressed snapshot dedupe,
VMGenID reseeding, web dashboard, custom domains. (The `i3.metal` hour to
close G4 was never needed — the Hetzner box closed it on 2026-07-26.)

## Commercial posture — build vs. buy (recorded 2026-07-24)

Decision #8 in one table. "Keep" = differentiated, verified, no
off-the-shelf equivalent. "Buy" = commodity with decades of adversarial
hardening we cannot replicate.

| Component | Posture | Proven choice / rationale |
|---|---|---|
| VMM | already bought | Firecracker (runs AWS Lambda) |
| App database | already bought | SQLite in-guest |
| Object store / arbiter | already bought | GCS/S3 conditional PUTs are E4's substrate; never operate our own consensus |
| Wake-on-request snapshot lifecycle | **keep** | The product. Knative cold-starts; firecracker-containerd has no snapshot/wake lifecycle; Modal/CodeSandbox/e2b all built theirs in-house |
| Fencing semantics + WAL shipping (E1–E6, PrepareWake, shipper, gateway) | **keep** | No fit: Litestream trusts the guest, LiteFS treats a lease as correctness (PT-3). Defended by spec + 10k-seed sim, CI-checked negative configs |
| M4 auth plumbing (OAuth, sessions, JWT/JWKS) | **buy** | oauth2-proxy/Pomerium shape; Caddy/Envoy viable as proxy substrate with the wake hold as middleware; Dex / WorkOS-class for SSO federation. Hand-write only ACL + per-app scoping |
| Lease/epoch mint at multi-host | **buy (M5)** | etcd lease revisions — fencing tokens are natively its feature. Checks remain conditional PUTs at storage |
| ext4+jbd2 read-only parser | **replace by rearchitecting (M5)** | No library reads a live guest-written ext4; a host-served block device (NBD/ublk/vhost-user-blk) sees every write and deletes the problem (EBS/Neon shape). Until then the parser stays, guarded by its non-vacuous fixtures |
| Builder trust boundary | **buy — today-risk #1** | Server-side `docker build` of an untrusted context executes attacker instructions during build, on the host, outside any microVM, with a root daemon. Close with rootless BuildKit / kaniko / builds inside a throwaway Krill microVM (dogfood) **before anyone untrusted can deploy** — see the M4 scope note |
| Guest egress controls | **buy — today-risk #2** | Nothing today stops an agent-written app from arbitrary outbound traffic: cloud-metadata SSRF (`169.254.169.254` steals the host's GCP credentials — N/A on Hetzner bare metal, real on any cloud host), spam, abuse under the platform's IP reputation. Kernel netfilter (nftables, proven) per tap: drop link-local/metadata destinations on cloud hosts, port-25/465/587 block + per-app rate limits **no later than the first M4 share link**, an auditing egress proxy later if commercial. Hetzner blocks outbound 25/465 provider-side by default (2026-07: confirmed policy) — our baseline still owns 587, HTTP-API abuse, and rate limits. Note guests currently have NO outbound at all (no NAT configured); the baseline lands together with the first masquerade rule |
| Registry catalog | keep for now | SQLite is fine single-host; Postgres if a control plane ever needs to scale |

## Resume-leverage backlog (parallel track, low effort, high value)

Three blog posts are already written in the session history and just need
prose: (1) "The model checker found the bug my design review missed"
(PT-9/slow-waker story), (2) "Debugging a mystery 1-second latency down to
an ARP retransmit" (the bimodal benchmark story, including the 30-second
tap-MAC sequel), (3) "114 ms microVM wakes, measured for $2.50" (the
two-tier benchmark method). Plus: public repo README that leads with the
CI-model-checked badge and the results table.

## Gotchas a fresh session must not rediscover

Most operational gotchas are already written where they belong — check these
before debugging anything:

- `wake-path-benchmark.md` → Gotcha appendix: ARP-vs-resume race (pin
  neighbor entries), deterministic tap MACs (snapshot embeds the guest's ARP
  cache; random MACs = 30 s deafness), fresh-rootfs-per-resume, version lock
  (snapshots never cross Firecracker/kernel/CPU/tier boundaries), clocks
  jump on resume (never measure inside the guest), don't let pollers DoS the
  measurement.
- `wake-bench/RESULTS-2026-07-23-nested.md` → the two networking findings
  with their production implications.
- `CLAUDE.md` → the tripwire (three protocol artifacts move together), the
  artifact-republish mechanics (existing URLs, `url:` param from new
  sessions), headline-numbers consistency rule.

M1-session gotchas (2026-07-23, hardware-verified — details and raw data in
`m1-gates/RESULTS-2026-07-23-nested.md`):

- **The ~200 ms guest-userspace wake tax.** After snapshot/load the guest
  KERNEL answers TCP in ~13 ms, but python/uvicorn takes ~200 ms to serve
  the first request (then ~3 ms). Constant per wake; NOT the balloon
  (falsified with `--snapshot-balloon=false`); NOT pause-duration-dependent.
  A1 passes with it included, but it's the whole gap to the 115 ms bench
  number — likely lever: what the guest was doing when snapshotted (bench
  snapshots followed 20 warm pings; krilld snapshots an idle guest).
- **Freeze-after-request races in-flight accounting** — drive gates by
  postcondition (poll for FROZEN), never by one freeze call. In the daemon
  this is by design: ErrBusy protects in-flight requests.
- **Subnets are reallocated after delete** — deleting and re-registering an
  app can move it to a different /30; never assume a guest IP across
  re-registration (cost 20 min of phantom-bug chasing: direct curls to a
  stale IP hit a frozen app's tap and ate the full 130 s SYN ladder).
- **sqlite3 connections are thread-bound** and FastAPI sync endpoints run in
  a threadpool — gate guest needed `check_same_thread=False` + a lock. Found
  via per-app `boot_args` with `console=ttyS0` (serial log catches guest
  tracebacks; that registry column earned its keep day one).
- **Balloon tradeoff, measured:** reclaim costs ~6 s of freeze time and
  saves ~32 MB/app stored, with zero wake-latency cost for this guest.
  Default stays on; `--snapshot-balloon=false` exists.

M2-session gotchas (2026-07-23, hardware-verified — details in
`m2-gates/RESULTS-2026-07-23-nested.md`):

- **Kernel `ip=` autoconfig is real and load-bearing.** The FC CI kernel has
  `CONFIG_IP_PNP`; krilld appends `ip=<guest>::<gw>:<mask>::eth0:off` to
  every boot, so guests without iproute2 still meet the network contract.
  If the kernel ever changes, re-run `m2-gates/90-info-noiproute2.sh` —
  a kernel without IP_PNP silently breaks every iproute2-less image.
- **Go's `flag` stops at the first positional arg** — `krill deploy dir
  --json` ignored `--json` and cost the first B1 run. `parseFlexible` in
  cmd/krill is the fix; keep using it for new commands.
- **Redeploy resets app data by design** (B2 asserts it). Do not "fix" this
  before M3 — a redeploy that preserved the old disk would un-pair disk and
  snapshot lineage.
- **`mkfs.ext4 -d` replaces the whole mount/copy/umount dance** — no loop
  devices, nothing to leak on a crashed build. Needs root for ownership
  preservation, which krilld has anyway.

M3-session gotchas (2026-07-23 — the data-plane build; local verification
only, hardware pending):

- **jbd2 journal replay is NOT optional for host-side tailing.** After a
  guest fsync, the freshest ext4 metadata (file size, extents, even the
  dirent of a new file) can exist ONLY as committed journal transactions.
  `internal/ext4` replays the journal by default; the fast poll path skips
  it (bounded staleness, WAL checksums self-validate) but every decision
  point — sync-ack, freeze catch-up, restore — uses the precise view.
- **Fixture vacuity trap:** a `sync` before copying a mounted image
  checkpoints everything and silently makes journal replay untested. The
  live fixture writes fsync-only AFTER the last sync, and
  `TestLiveImageNeedsReplay` fails if a regenerated fixture goes vacuous.
- **Guest-initiated WAL resets are normal, not exceptional.** SQLite's
  default `wal_autocheckpoint=1000` will reset the WAL under the shipper on
  any busy app; the shipper's answer is a rebase segment (full image, one
  LSN). Data is never lost; frame-granular PITR coarsens across the jump.
  The gate app disables autocheckpoint only to make test assertions exact.
- **The sim's Rehydrate cannot reject a stale sealer — by design.** The
  spec's Rehydrate has no reject clause, and that is exactly why disabling
  E5 exposes the slow waker in TLC and in the sim. Production ADDS the
  belt-and-suspenders reject (a stale takeover seal kills the wake in
  PrepareWake). Don't "fix" the sim to match production or negative
  config #3 goes blind.
- **The fsstore CAS is process-local** (mutex + gen sidecar): sound while
  krilld is the only writer of its own objstore dir, which is the M3
  single-host scope. A second host means GCS (real conditional PUTs) —
  never two hosts sharing one directory. fencetool probes are safe only
  against a quiescent app for the same reason.
- **If the registry database is ever lost, bump `-cell-gen`.** The epoch
  mint lives in registry SQLite; a fresh registry would re-mint low
  counters. A new cell generation makes every new epoch compare higher
  than anything pre-loss (E1's composite ordering).
- **Sync-ack's durability boundary is the guest's fsync.** Apps running
  `synchronous=NORMAL` can have commits that are invisible to the tailer
  until later — the same commits SQLite itself would lose on power loss.
  The contract app uses `synchronous=FULL`; document this for M4 users.
- **Per-request precise reads are the known perf lever.** Sync-ack does an
  ext4-open + journal replay per response; data-disk journals are kept at
  4 MB (`-J size=4`) to bound it. If C-info shows pain, the levers are:
  incremental journal scan, cached FS handle, or holding only when the
  fast view shows pending frames.

Session-level gotchas not recorded elsewhere:

1. **GCP capacity**: n2-standard-16 + Local SSD was exhausted across ALL of
   us-central1; zone-hunt loops must check gcloud's **exit code**, not its
   output (a `| tail` pipeline eats the failure — this bit us).
2. **Local SSD data dies on instance stop**, not just delete. Pull results
   before touching the instance.
3. **`bq` CLI demands interactive init** — query BigQuery via REST with
   `gcloud auth print-access-token` instead.
4. **Editing the HTML docs**: they're self-contained (inline CSS/JS, CSP
   forbids external resources; mermaid renders natively in artifacts). To
   preview locally, Playwright blocks `file://` — serve via
   `python3 -m http.server` and prepend `<meta charset="utf-8">` to a scratch
   copy (the artifact runtime provides charset; a bare local server doesn't).
5. **Sam's context**: samshulman6@gmail.com for GCP; gcloud lives at
   `~/Downloads/google-cloud-sdk/bin/gcloud` and is authed for project
   `ycombinator-503223`.

## How to resume work (protocol for a fresh session)

1. Read `CLAUDE.md` (auto-loaded), then this file.
2. Check "Current state" and "Next action" below — trust them over guesses.
3. If touching the protocol: the tripwire checklist in CLAUDE.md is
   mandatory, spec first.
4. The three M1-gating decisions (name, language, hardware) are RESOLVED —
   see above. Do not re-litigate them; start building.
5. At session end: update the two lines below and CLAUDE.md if the map
   changed.

## Current state / next action

- **Current state:** **M1 + M2 + M3 COMPLETE AND ACCEPTED (all
  2026-07-23).** The data plane exists end to end and passed all four
  pre-registered gates on hardware
  (`m3-gates/RESULTS-2026-07-23-nested.md`): E1–E6 as code across
  `internal/{objstore,sqlitewal,ext4,dataplane,dataplane/sim}`, wired into
  krilld (data disks at `/data`, wake = mint + fenced takeover seal +
  rebuild-if-diverged, freeze = final ship + E5 checkpoint, router
  sync-ack, PITR via `krill restore`, zombie-kill on fence). Whole tree
  green under `go test -race ./...`. No protocol changes were needed: the
  spec and both HTML docs are untouched (tripwire not tripped). The M2
  redeploy-resets-data contract is retired: `/data` survives redeploys.
  MCP server does not yet expose stream/restore (queued). GCP swept to
  zero billing resources (M3 hardware run ≈ $0.20). **2026-07-24:** added
  the learning textbook (`docs/krill-textbook.html`; artifact URL in
  CLAUDE.md — derivative doc, not part of the tripwire) and recorded the
  commercial build-vs-buy posture (decision #8 + the posture table
  above). No code or protocol changes; spec and the three protocol
  artifacts untouched. **2026-07-26:** production hardware ordered
  (decision #3 resolved above: Hetzner EX44-1-LTD, FSN1) and
  `SERVER-SETUP.md` written — provision per that runbook when the
  delivery email arrives; loopback-only posture until M4.
  **2026-07-26 (later): THE BOX IS LIVE — `SERVER-SETUP.md` Phases 0–6
  executed and verified on 46.4.64.187** (`krill-fsn1`, Ubuntu 24.04.4,
  RAID1 `[UU]` across both NVMe, `systemd-detect-virt` = **none** — the
  project's first true metal tier). SSH-only inbound (ufw; 8080/9091
  verified unreachable from outside), keys-only auth, unattended-upgrades
  on, performance governor pinned, FC v1.16.1 + CI kernel
  `vmlinux-6.1.155` at `/srv/fc`, krilld under systemd (`Restart=always`,
  loopback router+admin, data plane + sync-ack on, cell-gen 1) and
  **verified to come back on its own across a reboot**. First metal
  deploy: `guestbook` built in 14.8 s and a full durability round-trip
  passed — write through the router (sync-ack) → freeze → wake-on-request
  → data survived → ACTIVE. **No code changes were needed to run on
  metal**; five deviations were all in the runbook, now fixed in
  `SERVER-SETUP.md` and marked ⚠ (UEFI ESP mandatory — the original
  partition scheme could not have worked; `installimage` not on `$PATH`
  non-interactively and the menu unusable under `xterm-ghostty`;
  `/srv/fc` not created by the FC installer; governor not persistent
  across reboot; `examples/hello` does not exist — it's `guestbook`).
  **Informal, NOT a gate result:** wakes ran ~100–120 ms end to end with
  `last_wake_ms` 49, measured host-side during the initial RAID resync —
  suggests the ~200 ms guest-userspace tax is much smaller on metal, to
  be confirmed by a real A1 run once resync completes.
  **2026-07-26 (later still): PHASE 7 DONE — the box's record now lives
  off-box.** `--objstore gs://krill-fsn1-objstore/krill` (europe-west3,
  uniform access, public-access-prevention, versioning + 30-day
  noncurrent expiry) under a service account scoped to
  `roles/storage.objectAdmin` **on that bucket only**; registry
  snapshots ship to `_control/registry/` every 24 h, keeping 14. Proven,
  not assumed: 5 rows written to `ledger` through the router, then
  `data.ext4` **and** the ship cursor deleted — the next wake rebuilt
  `/data` from GCS alone and returned the identical digest. Survives a
  full reboot (`preflight_ok=true`, key readable unattended at boot).
  Four pivots from the runbook, all now fixed in `SERVER-SETUP.md`:
  (1) **the GCS backend could not authenticate on non-GCP hardware** —
  its chain was `KRILL_GCS_TOKEN` → GCE metadata → `gcloud`, so
  "put a service-account JSON on the box" had nothing to read it;
  service-account auth (RSA-signed JWT → OAuth2 jwt-bearer, stdlib only)
  is new code. (2) **"apps re-seed the new store on next wake" was
  backwards and destructive** — an empty store means an empty stream
  (E4), and the rebuild wipes `/data`; the record must be copied first,
  which is why `objstore.Copy` + `krill objstore-copy` exist.
  (3) the DB is `krill.db`, not `registry.db`. (4) the nightly
  `sqlite3 .backup` shell script became in-daemon `VACUUM INTO` + upload
  (no second copy of the credentials, no `gsutil` on the box).
  **The cost, measured (informal, N=10, host-side, resync complete):** a
  GCS round trip from FSN1 to europe-west3 is ~45 ms, so warm wake went
  ~100–120 ms → **p50 246 ms / p99 280 ms** (`last_wake_ms` 49 → 195)
  and an acked write costs **p50 188 ms / p99 421 ms**. Off-box
  durability therefore costs more than the entire metal-tier gain — the
  right trade (D1 is not kept by a promise on the dying host's disk), but
  it means **the A1 metal run must use `--objstore file://…` to stay
  tier-comparable.** Also found: **`guestbook` is not data-plane-backed**
  (it writes `/var/lib/guestbook.db`, outside `/data`), so its head LSN
  is permanently 0 — the earlier "durability round-trip passed" was the
  rootfs surviving, not the stream. Use `ledger` to verify the data plane.
  **2026-07-26 (last): THE METAL GATES ARE RUN. G4 IS CLOSED AND NO OPEN
  BENCHMARK GATE REMAINS IN THE PROJECT.**
  `wake-bench/RESULTS-2026-07-26-metal.md` and
  `m1-gates/RESULTS-2026-07-26-metal.md`. Headlines, all host-side with the
  resync complete and the governor pinned:
  **G4 PASS(metal)** — N=50, p99 **109 ms** vs the 1 s gate, 0 errors,
  against 4206 ms on nested virt. A 39× move that confirms the tier-1
  diagnosis exactly: 16 nested vCPUs were aggregate-CPU-bound on VM
  exits, not an architectural ceiling. All 50 resumes landed inside
  117 ms of wall clock (~430 wakes/s burst for clones of one snapshot;
  the model needs ~2/s/host). Density 50 resident VMs = **+451 MB**
  (~9 MB each, 57× denser than naive) because the snapshot's memory is
  file-backed and shared.
  **G1 PASS(metal)** 26 ms p99 (was 115 ms), **G5 PASS(metal)** 42× (was
  14.5×); G2/G3 stand at tier 1 where a PASS is conclusive.
  **A1 PASS(metal)** p99 **90 ms** vs the 300 ms gate in its
  pre-registered configuration (`--data-plane=false`), 3.3× faster than
  nested's 298 ms. Ran three ways on the same box, which prices
  everything added since M1: data plane on with a **local** store costs
  **5 ms at p99** (95 ms) — the fencing really is latency-noise — while
  the **production GCS** configuration is p50 258 / **p99 285 ms**.
  ⚠ **That passes A1 by 15 ms.** The 195 ms is not the data plane, it is
  the distance to the object store (~45 ms × 3 round trips on the wake
  path), so the two round-trip cuts logged below stopped being an
  optimization and became the margin on the project's headline gate.
  **The M1 "~200 ms guest-userspace tax" question is answered:** same
  guest, same code, same instrumented boundary, only the tier changed —
  `acquire_ms` 41–49 → 24–33 and `proxy_ms` 211–256 → **43–46**. It was
  overwhelmingly a nested-virt artifact. What is left is a ~20 ms
  question (the bench's simpler guest resumes in 22 ms on this box), which
  demotes it well below M4.
  Two pivots to make the suites runnable on a production box, both fixed
  in commit 9bd13c6 and documented in `SERVER-SETUP.md` ("Running the
  gate suites on this box"): `m1-gates/00-setup.sh` used to `pkill -x
  krilld`, which under `Restart=always` yields two daemons fighting over
  ports, taps and the data dir (it now refuses, and honors `KRILL_DATA`
  so gates run on a scratch dir); and **krilld's taps must be deleted
  first** — they hold `172.16.0.1/30` while the bench wants
  `172.16.0.1/24`, and the `/30` wins the route, so the bench guest is
  unreachable. Deleting them is safe: deterministic host MACs mean a
  re-created tap keeps restored guests' ARP caches valid, verified by
  waking both apps afterwards with no rebuild and no fence.
  **2026-07-26 (final): THE PROJECT HAS A NAME — `krill.run`, and M4's
  DNS prerequisites are closed** (`SERVER-SETUP.md` **Phase 8**).
  Registrar + DNS is Cloudflare, chosen for the M4 wildcard certificate:
  ACME can only issue `*.krill.run` over **DNS-01**, which needs a
  provider API token, and `caddy-dns/cloudflare` is the best-supported
  path. `.run` was picked over `.dev`/`.app` because those are
  HSTS-preloaded — browsers would refuse plain HTTP, killing tunnel-era
  testing before the doorman exists. Live and verified from a public
  resolver: `*.krill.run` + apex → 46.4.64.187 (TTL 300, all records
  **grey-cloud / DNS-only**, so M4's Caddy sees raw clients), plus
  `*.local.krill.run` → 127.0.0.1 for tunnel-era browsing — **which the
  home gateway strips (DNS-rebinding protection), so it needs
  `/etc/resolver/krill.run` on the Mac; the record itself is correct and
  resolves at 1.1.1.1** (diagnosis + fix in Phase 6). Mail is locked down before
  the name ever appears in a share link: null MX (`0 .`),
  `v=spf1 -all`, `p=reject`. `--base-host krill.run` is set on the box.
  **Posture unchanged and re-proven** — SSH-only inbound, router+admin
  on loopback; the name resolves to a closed door. Also staged for M4:
  a Cloudflare token scoped to `Zone:DNS:Edit` **+ `Zone:Zone:Read`**
  (both required — `DNS:Edit` alone cannot resolve the zone ID) on the
  `krill.run` zone only, no TTL (a TTL means silent renewal failure at
  60–90 days) and no client-IP filter (this box's IPv6 /64 would 403 a
  v4-only filter). **Nothing in Go changed:** `--base-host` is cosmetic —
  `router.appName` reads only the first `Host` label and never checks the
  suffix, so routing cannot break and the gate suites keep sending
  `krill.local` correctly. That same gap is an M4 requirement: **the
  doorman must pin the host suffix**, a natural F3 ingredient. The
  daemon default stays `krill.local` (right for a box with no DNS).
- **Next action:** M4 (the doorman: edge auth, share links, the
  three-plane ACL), built per decision #8 — proven components for the
  OAuth/session/JWT plumbing, hand-written ACL and per-app token scoping;
  builder isolation and the egress baseline enter M4 scope once shares
  reach untrusted people. **Its infrastructure prerequisites are now all
  closed** (Phase 7 durability, Phase 8 DNS + ACME token), so the next
  concrete step is the discipline every prior milestone used: **freeze
  the acceptance gates before writing doorman code.** ⚠ Name them
  **F1–F4** ("F" for front door) — the natural next letter, D, collides
  with the durability contract D1–D4, which is load-bearing across the
  spec and both HTML docs (CLAUDE.md tripwire); note the skipped letter
  in the gates file. Candidates: F1 unauthenticated request to a shared
  app → Google login → app serves with correct `X-App-User`; F2 revoke
  takes effect on the next request; F3 the three planes actually
  separate (a use-only user cannot reach the data/edit surfaces —
  **and the router pins the host suffix**, which it does not today);
  F4 the human gate — a non-technical friend opens a share link cold,
  zero setup. Note F1 and F4 are the two that genuinely required a
  domain; F2/F3 are provable over the tunnel. Or the queued side quests
  first: the
  guest-egress netfilter baseline (the metadata-IP drop is one rule —
  cheapest risk-close on the board), MCP stream/restore tools,
  segment group-commit batching + GC past checkpoints (both noted in the
  M3 results findings), and the blog posts (three written in session history;
  the spec-as-test-oracle sim harness is a strong fifth candidate). Push
  commits to origin at session end (`git push`) so CI badges stay live.
- **Promoted from side quest to margin work by the 2026-07-26 A1 run:** cut
  round trips to the object store, which Phase 7 put on the critical
  path at ~45 ms each. The production configuration now passes A1 with
  **15 ms to spare**, so this is no longer an optimization — it is the
  difference between a gate that passes and one that does not survive a
  slower region or one extra round trip. Two independent levers, neither
  taken:
  (a) `Coordinator.PrepareWake` calls `Gateway.CreateStream` (a manifest
  load) and then `SealTakeover` (another load + the CAS) — the first load
  is redundant whenever the stream already exists; (b) caching the
  manifest generation per app would let the seal skip its read entirely,
  since a stale generation is exactly what the CAS already catches. Both
  touch the wake path's fencing sequence, so **the sim harness
  (`internal/dataplane/sim`, three negative configs) is the gate**, and
  (b) needs a hard think about PT-9 before it ships.
