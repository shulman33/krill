# Krill

[![protocol: model-checked](https://github.com/shulman33/krill/actions/workflows/protocol.yml/badge.svg)](https://github.com/shulman33/krill/actions/workflows/protocol.yml)
[![krilld](https://github.com/shulman33/krill/actions/workflows/krilld.yml/badge.svg)](https://github.com/shulman33/krill/actions/workflows/krilld.yml)
[![mcp](https://github.com/shulman33/krill/actions/workflows/mcp.yml/badge.svg)](https://github.com/shulman33/krill/actions/workflows/mcp.yml)

A cloud for **small software**: apps written by agents for five users, that
deploy in one call, share like a doc, and cost nothing while nobody is using
them. Apps run in Firecracker microVMs that are **snapshotted to disk when
idle and restored on the next HTTP request** — fast enough that the caller
never notices anyone was asleep.

The ocean runs on uncountable tiny organisms.

## Measured, not argued

Wake-path numbers from the pre-registered benchmark
([plan](wake-path-benchmark.md), [results](wake-bench/RESULTS-2026-07-23-nested.md)),
run on GCP nested virtualization — strictly slower than the metal this would
ship on:

| Number | Value | Gate |
|---|---|---|
| Warm resume → first HTTP 200, p99 | **115 ms** | ≤ 300 ms ✓ |
| Cold-cache resume → HTTP 200, p99 | **432 ms** | ≤ 1.5 s ✓ |
| Stored footprint per idle app | **69 MB** | ≤ 250 MB ✓ |
| Resume vs cold boot | **14.5×** faster | ≥ 2× ✓ |

Total benchmark cost: ~$2.50 of spot compute.

## The part that is actually hard

Scale-to-zero's failure mode isn't latency — it's **correctness**. A VM that
was paused mid-write and restored twice, or restored from a stale snapshot
while a newer instance runs elsewhere, will destroy user data in ways no
retry loop fixes. Krill's single-writer fencing protocol is specified in TLA+
([`FencingProtocol.tla`](FencingProtocol.tla)) and **model-checked in CI on
every push**: 7 invariants over ~26k states, plus three *negative* configs
proving each fence still catches the failure it exists for when disabled.

The model checker found a protocol bug (PT-9, the "slow waker" — a
lineage-forging race on the snapshot-registration path) that a careful human
review had explicitly signed off as safe. The spec is the arbiter of truth
here; the prose gets fixed to match it, never the reverse.

Design docs: [pressure test & unit economics](docs/small-software-cloud-pressure-test.html) ·
[architecture](docs/sleeping-cloud-architecture.html) ·
[plain-English explainer](docs/sleeping-cloud-explained.html)

## krilld — the host agent (M1, done)

One Go daemon per KVM host: registers apps, boots microVMs, freezes them
when idle, wakes them on request.

```
curl -s -X POST 127.0.0.1:9091/v1/apps \
  -d '{"name":"counter","golden":"/srv/krill/images/counter.ext4"}'

curl http://counter.yourhost:8080/        # frozen? it wakes. ~100 ms later: 200
```

- **Wake-on-request router** — requests to FROZEN apps are held while the
  snapshot restores, then proxied; concurrent requests share one wake.
- **Lifecycle** — `COLD → BOOTING → ACTIVE ⇄ (SNAPSHOTTING → FROZEN → WAKING)`
  with an idle janitor; crash recovery demotes mid-flight apps and kills
  their snapshots (a snapshot may only ever meet the exact disk it was
  paused with).
- **Firecracker driver** — the API call sequence proven by the benchmark,
  ported verbatim and pinned by tests.
- **Networking** — per-app /30 subnets, deterministic MACs, pinned ARP
  entries (each of those words is a debugging story; see the
  [benchmark gotchas](wake-path-benchmark.md)).

Acceptance gates A1–A4 are pre-registered in [`ROADMAP.md`](ROADMAP.md) and
runnable from [`m1-gates/`](m1-gates/). All four passed on hardware
([results](m1-gates/RESULTS-2026-07-23-nested.md)).

## The deploy path (M2, done)

A directory with a plain Dockerfile — no init scripts, no VM anything —
becomes a running, scale-to-zero app in one command:

```
$ krill deploy ./guestbook
✓ deployed guestbook (created, 1024 MB image, build 22.1s)
  url:  http://guestbook.krill.local:8080/
  ready: yes (first wake 1946 ms, state ACTIVE)
```

Under the hood: tar.gz upload → `docker build` → `docker export` →
`mkfs.ext4 -d` (no loop mounts) → injected init that reads the network
contract off the kernel command line → registered app → **verification
wake**. The deploy response says `ready: true`, or hands back the guest's
actual traceback, structured — a broken deploy explains itself in the same
payload:

```
NameError: name 'FastAPI' is not defined. Did you mean: 'fastapi'?
```

The same loop is exposed as an [MCP server](mcp/) (`deploy`, `logs`,
`apps`, `delete_app`), so an agent deploys and iterates on a real app with
one tool call. Redeploys keep the app's IP, invalidate stale snapshots
before any byte moves, and (until the M3 data plane) reset app data —
asserted by the gates, not just documented.

Gates B1–B4 are pre-registered in [`m2-gates/`](m2-gates/); all four
passed on hardware ([results](m2-gates/RESULTS-2026-07-23-nested.md)),
including a 22 s cold-cache deploy-to-verified-ready.

## Roadmap

| Milestone | What | Status |
|---|---|---|
| M1 | `krilld`: registry, VMM driver, lifecycle, wake router | **done** — A1–A4 pass on hardware |
| M2 | deploy path: MCP server + CLI, dir → rootfs → URL | **done** — B1–B4 pass on hardware |
| M3 | data plane: host-side WAL shipping, epoch fencing as code, PITR — the TLA+ spec becomes the test oracle | next |
| M4 | sharing: OAuth edge, per-app identity headers, share links | |
| M5 | second host, live lease service, lazy restore, dedupe | |

*Personal project, built for depth: own orchestration, own fencing protocol,
own data plane — no wrappers.*
