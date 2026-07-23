# M2 gate results — 2026-07-23, nested (tier 1)

Machine: GCP n2-standard-16, us-east1-c, nested virt, Local SSD (NVMe) at
/srv. Firecracker v1.16.1, kernel firecracker-ci/v1.14 vmlinux-6.1.155
(same pair as M1/A-gates). Docker 29.1.3. Ubuntu 24.04. Node 20.20.2.
krilld: this commit. Total instance time ≈ 35 min (~$0.60).

| Gate | Verdict | Headline number |
|---|---|---|
| B1 one-command deploy | **PASS** | cold-cache deploy → ready: **22 s** (budget 240); warm: **4 s** (budget 90) |
| B2 iterate | **PASS** | warm redeploy → ready: **6 s**; stale snapshot never served; data reset asserted |
| B3 self-heal | **PASS** | traceback structured in the deploy response *and* the logs tool |
| B4 MCP one tool call | **PASS** | one `tools/call deploy` → URL + ready; app answered through the router |

## B1 — one-command deploy

`krill deploy examples/guestbook --name guestbook` on a box that had never
seen the base image: 22 s from command to verified-ready (docker build 20.1 s
of it — base image pull + pip install dominate; the microVM part of the
pipeline is ~2 s). The app dir is a plain Dockerfile + app.py — no init
script, no network setup, nothing krill-specific. Warm-cache same command:
4 s. Router answered with the correct body (`{"app":"guestbook","version":"v1"}`).

## B2 — iterate

Seeded a guestbook row, froze (valid v1 snapshot), bumped the version
marker, redeployed: 6 s to verified-ready. First post-redeploy response was
v2 — the v1 snapshot (which contained the seeded row in RAM) never answered.
Post-redeploy freeze → wake stayed v2. The seeded row was gone, as asserted:
**redeploy resets app data in M2** (durable data is M3's job).

## B3 — self-heal

`examples/broken` (import-time `NameError`) built fine, deployed, and the
deploy response itself came back `ready: false` with:

```
NameError: name 'FastAPI' is not defined. Did you mean: 'fastapi'?
```

as a structured `python_traceback` error (file and line preserved in
detail), plus the `kernel_panic` "Attempted to kill init" entry with the
hint that the app's process exited. `krill logs brokenapp` returned the
same through the logs endpoint. Deploying the fixed source to the same name
returned `ready: true` and the router served it. The agent loop never needs
ssh.

## B4 — MCP

Scripted stdio JSON-RPC client (`mcp-drive.mjs`) → `mcp/dist/index.js`:
initialize, `tools/call deploy {directory, name}`. The single tool result
contained the URL and "Ready: yes"; the router served the app; `tools/call
logs` returned the tail. This is the ROADMAP gate sentence minus the model.

## Informational (non-gate)

- **No-iproute2 images work.** A bare `python:3.12-slim` app (no `ip`
  binary anywhere) deployed `ready: true`: the Firecracker CI kernel has
  `CONFIG_IP_PNP`, so the `ip=<guest>::<gw>:255.255.255.252::eth0:off` boot
  arg krilld appends configures eth0 before userspace. The generated init's
  `ip`-if-present path is a fallback, not a requirement. **Arbitrary
  Dockerfiles deploy with zero special packages.** The builder still warns
  (kernels without IP_PNP would need the in-guest path).
- First wake of a freshly deployed app (cold boot incl. uvicorn start):
  ~1.9 s. Warm wake from snapshot stayed in A1 territory (last observed
  45 ms daemon-side). B-gates don't gate latency; A1 owns it.

## Surprises / gotchas for the ROADMAP

- Go's `flag` package stops parsing at the first positional arg, so
  `krill deploy dir --json` silently ignored `--json` — first B1 run failed
  on jq, not on the platform. CLI now parses flags flexibly
  (`parseFlexible`); unit-tested.
- `mkfs.ext4 -d` (populate from directory, no loop mount) worked exactly as
  intended on Ubuntu 24.04 e2fsprogs — the whole build pipeline runs
  without a single `mount`.
