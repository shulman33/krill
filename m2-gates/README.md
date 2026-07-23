# M2 acceptance gates — the deploy path

Pre-registered **before** implementation (same discipline as the wake-path
benchmark and the M1 gates: decide what "done" means first, then build).

M2 is: **directory → Docker build → ext4 rootfs → registered app → URL**,
driven by a CLI (`krill deploy`) and an MCP server, with a structured `logs`
feedback loop so an agent can self-heal. The ROADMAP gate sentence: *"Claude
Code deploys and iterates on a real app with one tool call."*

## The four gates

All gates run on the standard dev box (GCP nested virt per
`wake-bench/README.md`, Firecracker + Docker installed, krilld running).
All timing is host-side wall clock. PASS/FAIL is one-sided the usual way:
a PASS on nested virt is conclusive; a latency FAIL escalates to metal
before being recorded as a FAIL.

### B1 — one-command deploy of a real app

`krill deploy examples/guestbook --name guestbook` — a directory containing
only an app (`app.py` + `Dockerfile`; **no init script, no network setup, no
krill-specific anything**) — must, with zero other manual steps:

1. print the app's URL,
2. report the app **ready** (deploy verifies by waking the app once), and
3. a `curl` through the wake-on-request router returns 200 with the app's
   actual response body.

Budget: **≤ 240 s** end-to-end on a cold Docker cache (includes base-image
pull), **≤ 90 s** on a warm cache. The builder — not the app author — injects
the init that satisfies the M1 network contract (`krill_ip=`/`krill_gw=`
kernel args).

### B2 — iterate: redeploy replaces the app, snapshots never serve stale code

After B1, with the app FROZEN (it has a valid snapshot of v1):

1. modify the source (bump a visible version marker to `v2`),
2. run the same deploy command again — budget **≤ 90 s** warm,
3. the **first** request through the router after redeploy serves `v2` —
   the v1 snapshot must never answer (redeploy invalidates it),
4. freeze → wake again still serves `v2` (the new snapshot pairs with the
   new disk),
5. guestbook rows written before the redeploy are **gone** — M2 explicitly
   resets app data on redeploy (the disk is rebuilt from the new golden;
   durable app data is M3's job). This is asserted, not just documented.

### B3 — the self-heal loop: a broken app explains itself

1. Deploy `examples/broken` (a Python app with an import-time bug) to a new
   name. The build succeeds; the guest crashes at boot.
2. The deploy response itself must say **ready: false** and include the
   Python traceback (structured, machine-readable — not "ssh in and look at
   a file"). `krill logs <name>` must return the same traceback with the
   offending file and line, via the admin API's structured log endpoint.
3. Deploy the fixed source to the same name → **ready: true**, router
   returns 200.

The point: the agent's next prompt after a failed deploy contains everything
needed to fix the app, from the same tool it already called.

### B4 — one MCP tool call, end to end

A scripted stdio JSON-RPC client (standing in for Claude Code) speaks MCP to
`mcp/dist/index.js`:

1. `tools/call deploy {directory: examples/guestbook, name: <fresh>}` — the
   **single tool result** contains the URL and ready status,
2. `curl` of that URL through the router returns 200,
3. `tools/call logs {name}` returns the log tail.

This is the ROADMAP gate sentence minus the model itself: one tool call in,
running app + URL out.

## Non-gates (recorded as informational only)

- Deploy of an image **without iproute2** (does the kernel `ip=` autoconfig
  fallback carry the network contract?). Whatever the answer, it's a
  documented capability note, not a gate.
- Wake latency is NOT re-gated here — A1 already owns that number. B-gates
  only assert 200s, not p99s.

## Running

```
./00-setup.sh     # build guest prerequisites, install krilld + krill, start daemon
./b1-deploy.sh
./b2-iterate.sh
./b3-selfheal.sh
./b4-mcp.sh
```

Results go to `results/` and get written up in `RESULTS-<date>-<tier>.md`.
