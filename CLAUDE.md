# CLAUDE.md

## What this project is

**Krill** — a **"small software" cloud** (from YC's RFS): a platform where
agent-written apps deploy in one call, share like a Google Doc, and scale to
zero via Firecracker snapshot/restore. Host-agent binary: `krilld`. The
design docs' "sleeping cloud" framing is narrative only — Krill is the name.

**This is a personal project built for resume value and fun — market demand
is explicitly not a consideration (Sam's decision, 2026-07-23).** Build the
real technology, not wrappers.

**→ Read `ROADMAP.md` before starting any work.** It is the cross-session
continuity document: current state, decision log, milestones M1–M5 with
acceptance gates, open decisions, and the resume protocol. Update its
"Current state / next action" section at the end of any session that changes
them. **M1 and M2 are complete (2026-07-23): `krilld` exists in Go (A1–A4
passed on hardware) and the deploy path — admin deploy endpoint, `krill`
CLI, MCP server — passed B1–B4 on hardware.** Next: M3, the data plane.

## Repository map

| Path | What it is |
|---|---|
| `ROADMAP.md` | **Start here.** Cross-session continuity: state, decisions, milestones, open questions. |
| `FencingProtocol.tla` + `.cfg` | TLA+ spec of the single-writer fencing protocol. **The arbiter of protocol truth.** |
| `docs/small-software-cloud-pressure-test.html` | Source of the "Fencing the Sleeping Cloud" artifact: protocol rules, state machine, pressure tests PT-1–PT-9, interactive unit-economics model. |
| `docs/sleeping-cloud-architecture.html` | Source of the architecture artifact: six diagrams (D1–D6) with fence pills marking where epoch checks are enforced. |
| `docs/sleeping-cloud-explained.html` | Plain-English explainer for non-engineers — no protocol identifiers, but its analogies (deli ticket = epoch, notebook = per-app SQLite, journal = WAL) and headline numbers mirror the technical docs; update it when those change materially. |
| `docs/krill-textbook.html` | "The Sleeping Cloud" — a distributed-systems-textbook read of M1–M4 for Sam's own learning (concepts first, then the implementation, diagrams, self-checks). Derivative of the canonical docs: it cites the same identifiers/numbers but is NOT part of the tripwire; if it drifts from spec/results, fix the textbook. |
| `wake-path-benchmark.md` | 3-day hardware benchmark plan with pass/fail gates G1–G5. |
| `wake-bench/` | Runnable scripts for that plan — two-tier cloud (GCP nested-virt first, EC2 `.metal` spot only on escalation); follow `wake-bench/README.md`. **Tier 1 ran 2026-07-23: G1/G2/G3/G5 pass, G4 escalates at N=50 only — decision: commit.** Results: `wake-bench/RESULTS-2026-07-23-nested.md` + raw data alongside it. |
| `cmd/krilld/` + `internal/` | **The product (M1+M2+M3).** Go host agent: `registry` (SQLite catalog, /30 allocation, M3 epoch mint), `firecracker` (test-pinned API driver, optional data drive), `lifecycle` (state machine + single-flight wakes + janitor + crash reconcile + DEPLOYING claim + M3 DataPlane hooks), `router` (wake-on-request proxy, D2; M3 sync-ack hold), `network`/`rootfs` (taps + deterministic MACs / golden + rootfs disks + M3 data disks that survive redeploys), `builder` (tar context → docker build → `mkfs.ext4 -d` → generated init; mounts `/data`), `guestlog`, `admin` (loopback control API incl. deploy/logs/stream/restore), `host` (composition). `make test`, `make krilld-linux krill-linux fencetool-linux`. |
| `internal/objstore` + `internal/sqlitewal` + `internal/ext4` + `internal/dataplane` | **The M3 data plane: rules E1–E6 as code.** Object store with conditional PUT (fsstore/GCS/mem, E4); SQLite WAL parse/replay at commit boundaries; read-only ext4 **with jbd2 journal replay** (host tails the guest's WAL from outside the VM); epochs/manifest/gateway/shipper/restore/PITR-branching + the lifecycle Coordinator. `internal/dataplane/sim` is the deterministic-simulation harness: **the TLA+ spec run as the test oracle against the real code** — its three negative configs must keep reproducing the TLC counterexamples (PT-1, E6 bug, PT-9). |
| `m3-gates/` | Pre-registered C1–C4 data-plane gates (`GATES.md` frozen before code) + runnable scripts, `fencetool` (stale-epoch prober), `examples/ledger`. **Ran 2026-07-23: all four PASS** — C1–C3 + C-info on nested virt, C4 locally at the full 10k-seed budget — `m3-gates/RESULTS-2026-07-23-nested.md` (wake tax with the data plane on: p50 205 ms). |
| `cmd/krill/` | The CLI: `deploy` (dir → URL, verified), `apps/status/logs/wake/freeze/delete`. Thin admin-API client; flags parse in any position (`parseFlexible`). |
| `mcp/` | TypeScript MCP server (the ROADMAP's language decision): `deploy`/`logs`/`apps`/`delete_app` tools over stdio, thin client of the admin API. `npm install && npm run build` → `dist/index.js`; see `mcp/README.md` for `claude mcp add`. |
| `m1-gates/` | Runnable A1–A4 acceptance gates for krilld + the SQLite gate guest. **Ran 2026-07-23 on nested virt: all four PASS** — `m1-gates/RESULTS-2026-07-23-nested.md` (includes the wake-latency breakdown: ~45 ms daemon, ~200 ms guest-userspace tax). |
| `m2-gates/` | Runnable B1–B4 deploy-path gates + example apps + the B4 stdio MCP driver. **Ran 2026-07-23 on nested virt: all four PASS** — `m2-gates/RESULTS-2026-07-23-nested.md` (incl. the kernel `ip=` autoconfig finding: images need no iproute2). |
| `ci/check-protocol.sh` + `.github/workflows/` | CI: TLC positive + 3 negative configs on every protocol touch; Go vet/race-test/cross-compile on every code touch. |

Published artifact URLs (private; update in place, never mint new URLs):

- Pressure test / economics: https://claude.ai/code/artifact/17633d13-099a-402a-ae03-19f8b6adb7b6
- Architecture: https://claude.ai/code/artifact/611f9b47-da7a-4b88-bf65-654f9339b4ec
- Plain-English explainer: https://claude.ai/code/artifact/62a98e23-a56b-468c-91d4-eb7faf07a604
- Textbook (M1–M4 learning doc): https://claude.ai/code/artifact/3eeb3011-e0a0-43c5-bfbc-2c7e15c1c026

## ⚠️ THE TRIPWIRE — the three protocol artifacts move together. Always.

**`FencingProtocol.tla`, the pressure-test doc, and the architecture doc
cross-reference each other by shared identifiers. A protocol change that
touches one and not the other two leaves the project actively lying to its
readers. Never do it. There are no small exceptions.**

The shared identifiers are load-bearing cross-references — **never renumber
or repurpose them**:

- **E1–E6** — epoch rules (defined in the pressure-test doc §1.3, enforced as
  actions in the spec, drawn as dark fence pills in architecture diagrams D1–D5)
- **D1–D4** — durability contract (pressure-test doc §1.2; referenced in D4's
  write path and the spec's WriteOK action)
- **PT-1–PT-9** — pressure-test scenarios (cards in the doc; reproducible as
  TLC counterexample traces via the spec's constants)
- **I1–I4** — invariants (doc §1.5 ↔ spec invariant definitions)
- **G1–G5** — benchmark gates (`wake-path-benchmark.md` ↔ `wake-bench/` scripts
  ↔ D2's time ruler)

### Checklist for ANY protocol change (all four steps, in this order)

1. **Update `FencingProtocol.tla` first** and re-run TLC. The positive config
   must pass all seven invariants. Then re-run the three negative configs
   (flip `GatewayFencing`, `ReplayOnRestore`, `RegistrationFencing` to FALSE,
   one at a time) — each must still produce a counterexample. A fence that
   can't fail when disabled is a fence the model no longer exercises.
2. **Update `docs/small-software-cloud-pressure-test.html`** — rules E1–E6,
   the state machine, affected PT cards, and the verdict box.
3. **Update `docs/sleeping-cloud-architecture.html`** — fence-pill positions
   and the reading notes under each affected diagram.
4. **Republish both artifacts to their existing URLs** (same file path from
   the publishing conversation; from any other conversation pass `url:` with
   the URLs above).

### Why this rule exists (history, do not delete)

PT-9 ("the slow waker") was found by TLC **after** a human review had
concluded the same configuration was safe — the eight hand-written pressure
tests all missed it, and the spec's own header initially documented the wrong
conclusion. The spec is the only one of the three artifacts that can tell you
when the other two are lying. When prose and spec disagree, **the spec wins,
and the prose gets fixed** — never the reverse without a TLC run proving it.

## Commands

```bash
# Model-check the protocol (get the jar once):
curl -fsSL -o tla2tools.jar https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar
java -cp tla2tools.jar tlc2.TLC -config FencingProtocol.cfg -workers auto FencingProtocol.tla
# Expected: "No error has been found", ~26k distinct states, a few seconds.
# Negative tests: edit the three booleans in FencingProtocol.cfg one at a time.

# Hardware benchmark (two-tier: GCP nested virt, escalate to EC2 .metal spot):
# provision per wake-bench/README.md, copy wake-bench/ over, run numbered scripts.
# Tier rule: latency PASSes on nested are conclusive; FAILs mean escalate — a
# bare FAIL may only be recorded from a metal run.
```

## Conventions

- The HTML docs are self-contained single files (inline CSS/JS, no external
  requests — artifact CSP forbids them). Both are theme-aware via CSS tokens
  under `@media (prefers-color-scheme: dark)` plus `:root[data-theme=...]`
  overrides; edit tokens, not per-component colors.
- Diagram colors are a fixed language (blue = edge/routing, violet = control
  plane, green = durability, orange = untrusted guest, gray = at rest). New
  diagrams must reuse it, and chart palettes were validated for color-vision
  safety — don't swap hues casually.
- Headline economics defaults quoted in prose (1M apps, 0.4% duty, ~$20k/mo,
  ⅓¢ idle app, 17× vs always-resident) must match the calculator defaults in
  the pressure-test doc §3.2 — if you change one, change both, and update the
  same numbers in the plain-English explainer's stats row.
- All benchmark timing happens on the host, never inside a guest (clocks jump
  across resume — the same reason PT-3 forbids guest-side lease timers).
- After any implementation update Current State / next action in ROADMAP.md and also note anything that you had to pivot from the ROADMAP and why.
