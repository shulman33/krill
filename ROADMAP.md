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
| Wake-path benchmark (`wake-path-benchmark.md` + `wake-bench/`) | **RAN 2026-07-23 on GCP nested virt (~$2.50 total).** G1 ✓ (warm resume p99 115 ms), G2 ✓ (cold 432 ms), G3 ✓ (69 MB stored/app), G5 ✓ (14.5× vs cold boot), G4 escalate-at-N=50-only (linear to N=25; throughput already 6× requirement). **Decision: COMMIT to the microVM architecture.** Full data: `wake-bench/RESULTS-2026-07-23-nested.md` + `wake-bench/results-nested-2026-07-23/`. |
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
   was capacity-starved, us-east1-b worked). Permanent always-on hardware
   (mini-PC vs Hetzner) is deliberately deferred until M4 makes it matter.

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

### M5 — dessert tray (pick by joy, any order)

Second host + real lease service (PT-1 live), UFFD lazy restore from network
storage, content-addressed snapshot dedupe, `i3.metal` hour to close G4,
VMGenID reseeding, web dashboard, custom domains.

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
  zero billing resources (M3 hardware run ≈ $0.20).
- **Next action:** M4 (the doorman: edge auth, share links, the
  three-plane ACL) — or the queued side quests first: MCP stream/restore
  tools, segment group-commit batching + GC past checkpoints (both noted
  in the M3 results findings), the ~200 ms guest-userspace wake-tax
  investigation, and the blog posts (three written in session history;
  the spec-as-test-oracle sim harness is a strong fifth candidate). Push
  commits to origin at session end (`git push`) so CI badges stay live.
