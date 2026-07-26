# M4 gate results — <DATE>, <TIER>

*Copy to `RESULTS-YYYY-MM-DD-metal.md` and fill in. Same tier honesty as every
prior suite: a PASS records what was actually observed, and a step that was
skipped says so rather than being folded into a green line.*

**Gates as frozen:** `m4-gates/GATES.md` (F1–F7, written before any M4 code;
F2 amended once — a tightening — before any M4 code existed).

## Configuration

| | |
|---|---|
| Host | krill-fsn1, 46.4.64.187 (Hetzner EX44, bare metal) |
| krilld | `<version / commit>` |
| krill-doorman | `<version / commit>` |
| Caddy | `<version>` + `caddy-dns/cloudflare` |
| Base host | `krill.run`, auth host `auth.krill.run` |
| Object store | `<gs://... >` |
| Builder VM | image `<path>`, kernel `<path>`, isolation `<untrusted|all>` |
| Egress | `--app-egress=<>`, registries `<>`, build-allow `<>` |
| Apps under test | `watchlist`, `ledger`, `hostile` |

## Summary

| Gate | Verdict | Note |
|---|---|---|
| F1 identity at the edge | | |
| F2 revoke, incl. total local-state loss | | |
| F3 three planes + audience + host pinning | | |
| F4 the human gate | | |
| F5 hostile build stays in the builder VM | | |
| F6 egress baseline | | |
| F7 exposure | | |
| F-info wake latency under the doorman | (informational) | |

---

## F1 — Identity at the edge

- Share link minted: `sh_<id>`, plane `use`
- Signed in as: `<email>` (an identity that had never touched this app)
- ACL after the claim: `results/f1-acl.json`
- `X-App-User`: `<value>` — matches the ACL claimant: yes/no
- `X-Krill-Token` claims: `results/f1-token-claims.json`
- The guest verified the token itself (`/whoami` → `verified: true`): yes/no
- Wake behavior the recipient saw: `<invisible / slow first byte / …>`

## F2 — Revoke

| Step | Result |
|---|---|
| (a) identity revoked → next request | |
| (a′) re-opening the link they still hold | |
| (b) link revoked with a 2nd live session | **ran / SKIPPED** |
| (b) revoked link re-openable by anyone | |
| (3) survives total local-state loss | |

- Revocations in the log before the loss: `<n>`; after the restore: `<n>`
- Shares recovered by the restore: `<n>` (0 would mean an empty database, not
  a restore — the gate would prove nothing)
- Revocation log: `results/f2-revocations.json`

⚠ If step (b) was skipped, F2 is **incomplete**, not passing.

## F3 — Planes, audience, host pinning

Host-suffix pinning:

| Host | Status |
|---|---|
| `<app>.example.invalid` | |
| `<app>.local.krill.run` | |
| bare IP | |
| `krill.run` | |
| `auth.krill.run` | |
| `<app>.krill.run.attacker.example` | |
| `<app>.krill.run` (legitimate) | |

Planes:

| Holder | app | data | edit |
|---|---|---|---|
| use | | | |
| data | | | |
| edit | | | |

- Token audience: `<aud>`; replay against `<other app>`: `<status>`
- **Did any refused request wake an app?** `<other app>` state before/after:
  `<…>` — an unauthorized request that woke an app is an F3 FAIL even though
  it was refused.

## F4 — The human gate

- Who: `<a non-technical friend who has not seen the project>`
- Device / network: `<phone model, not Sam's network>`
- Channel the link was sent over: `<iMessage / Slack>`, with no instructions
- Did they reach a working app without asking for help? yes/no
- Did they install anything? yes/no
- Any security warning of any kind? yes/no
- Was the first wake invisible, or did it read as ordinary slowness?
- Could Sam then show them, in the ACL, that it was them?

**Verbatim quotes, especially confusion — this is the only usability signal
the project gets for free:**

> …

## F5 — The builder is a guest

- Deploy path: through the doorman on an edit-plane link (not the admin API)
- HTTP `<code>` in `<n>`s; `isolated`: `<true|false>`
- Build-time probe report: `results/f5-probe-report.txt`

| Probe | Reached? |
|---|---|
| `/etc/krill/gcs.json` | |
| registry database | |
| admin API `127.0.0.1:9091` | |
| object store / metadata credentials | |
| writes outside the context | |
| another app's `data.ext4` | |
| host processes | |
| persistence past the VM | |

- Builder taps before/after: `<…>` / `<…>`
- Builder scratch dirs surviving: `<n>`
- Host credential digests unchanged: yes/no
- Hung-build timeout tested: **ran / SKIPPED** (`<n>`s to kill)
- Normal deploy still works: `<n>`s — compare with M2/metal `<n>`s

## F6 — Egress

Loaded ruleset: `results/f6-ruleset.txt`. App-guest probes:
`results/f6-app-probes.json`.

| From an app guest | Result |
|---|---|
| admin API (loopback / gateway) | |
| sshd on the host | |
| another app's guest | |
| link-local `169.254.169.254` | |
| SMTP 25 / 465 / 587 / 2525 | |
| arbitrary HTTPS | |
| DNS | |

| From a builder VM | Result |
|---|---|
| the container registry | |
| everything else | |

- Rate limit: **driven past its threshold / only asserted present**
  - counter before → after: `<…>`
- Apps remained reachable inbound throughout: yes/no

## F7 — Exposure

| Port (from off-box) | State | Expected |
|---|---|---|
| 80 | | open |
| 443 | | open |
| 8080 | | closed |
| 9090 | | closed |
| 9091 | | closed |
| 9092 | | closed |
| 22 | | open |

- Certificate: `*.krill.run`, issuer `<…>`, expires `<…>`, staging: no
- 80 → 443 redirect: `<Location>`
- Listener bindings agree with ufw (`ss -ltnp` shows 127.0.0.1): yes/no
- F1–F3 re-run against the public listener: `<…>`
- Renewal forced and completed unattended: yes/no

## F-info — wake latency under the doorman

Not gated (GATES.md, "Not gated here"). The OAuth round trip is not on the
wake path, but the doorman is a new hop in front of the router.

| Path | p50 | p99 |
|---|---|---|
| router direct (`Host:` header, no doorman) | | |
| through the doorman, session cached | | |
| through Caddy + doorman, public | | |

A1's production configuration was p50 258 / p99 285 ms with a 300 ms gate.
Record whether the doorman moved that, and by how much.

## Pivots from the plan

*What differed from GATES.md, ROADMAP.md or SERVER-SETUP.md, and why. Fix the
runbook in the same session.*

## Findings

*Anything a future session must not rediscover.*
