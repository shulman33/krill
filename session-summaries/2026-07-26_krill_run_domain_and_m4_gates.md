# krill.run + M4 gates and doorman architecture — Session Summary (2026-07-26)

## TL;DR

Two things happened. First, step 3 of `next-steps.md`: bought `krill.run`,
pointed wildcard DNS at the production box, flipped `--base-host`, and proved
from outside that a resolving name still reaches a closed door. Second, and
larger: froze M4's acceptance gates (**F1–F7**, `m4-gates/GATES.md`) before
any doorman code exists, then designed the doorman itself well enough to
record it as decision #9.

**No product code was written.** Four commits, all docs and decisions, all
pushed to `origin/main` (`802fdda`, `8877e86`, `387b1ba`, `6a0527e`). M1–M3
remain accepted and untouched; the TLA+ spec and the three tripwire artifacts
were not touched, because nothing here changes an epoch rule.

M4 got **bigger** during this session, deliberately: Sam chose to ship all
three share planes including remote `edit`, which drags builder isolation and
the egress baseline inside the milestone unconditionally. The estimate moved
from ~1–2 weeks to ~3–4. That is recorded as a pivot in `ROADMAP.md`, along
with a pre-committed cut line if it runs long.

## Why we did this

`next-steps.md` argued that domain and DNS should be bought *before* M4
rather than during it, because DNS propagation, TLD choice, and DNS-provider
choice all gate the doorman and each is the kind of thing that stalls a
milestone when discovered mid-build. Steps 1–2 (metal A-suite, G4 closed)
were already done in a prior session; this session did steps 3 and 4.

Success criteria were: a name resolving at the box with the posture
provably unchanged, and M4's pass/fail criteria frozen before implementation
so the results mean something — the same discipline that makes M1–M3's
"all gates PASS" trustworthy.

## Architectural decisions that matter

### 1. `krill.run`, registered and DNS-hosted at Cloudflare

`.run` over `.dev`/`.app`/`.page` because those are **HSTS-preloaded** —
browsers refuse plain HTTP unconditionally, which would have killed
tunnel-era `http://…:8080` testing before the doorman exists. Verified
against the real preload list via `hstspreload.org/api/v2/status`; `.run`,
`.sh`, `.io`, `.cloud`, `.host`, `.works`, `.computer` all clear.

Cloudflare because M4 needs a `*.krill.run` certificate, ACME can only issue
wildcards over **DNS-01**, that requires a provider API token, and
`caddy-dns/cloudflare` is the best-supported path. Buying elsewhere would
have meant migrating DNS mid-milestone.

Wildcard at the apex (`ledger.krill.run`, not `ledger.apps.krill.run`)
because share links are the product surface and explicit records beat a
wildcard anyway, so `www`/dashboard/mail are unaffected later.

### 2. The share link **is** the capability

A recipient opens an unguessable link, signs in with Google, and is bound to
the app's ACL at claim time. Sam never needs to know their address in
advance — which is exactly what makes F4 winnable.

Rejected: pre-named recipients only (`--user friend@gmail.com`), which is a
tighter default but requires knowing a Google address before sharing;
and both-modes-per-share, which is the eventual destination but roughly
doubles the acceptance surface for a milestone whose point is proving the
doorman works at all.

### 3. All three planes ship in M4, including remote `edit`

This is the decision that resized the milestone. Because other people's
agents can then push Dockerfiles, builder isolation (today-risk #1) and the
egress baseline (today-risk #2) became **unconditional** M4 scope rather than
the conditional "once shares reach untrusted people" the ROADMAP previously
carried. The builder gets dogfooded: untrusted `docker build` moves into a
throwaway Krill microVM.

Rejected: shipping use+data first and splitting the builder into an M4.5.

**Pre-committed cut line, recorded in both the gates file and the ROADMAP:**
if M4 runs long, cut at the *plane* boundary — ship F1–F4 + F7 with `edit`
ungranteable and let F5/F6 become M4.5 — **never** by weakening a gate.

### 4. The doorman's shape (ROADMAP decision #9)

**Caddy terminates TLS and `forward_auth`s to a small unprivileged
`krill-doorman` process that owns its own SQLite; krilld's router stays on
loopback, untouched.**

Two constraints forced this, neither a preference:

- **F3 forbids an unauthorized request from waking an app** ("a fence that
  bills is still a fence that failed") and krilld's router wakes on request.
  So authorization must complete strictly upstream of krilld.
- **krilld runs as root** (creates taps, runs `mkfs.ext4`, drives Docker).
  The internet-facing OAuth surface cannot live inside it without
  contradicting the thesis of F5 and F6.

**Consequence worth remembering: the router never un-loopbacks.** Caddy binds
443 and proxies to `127.0.0.1:8080`. The "un-loopback the router" step that
every earlier plan treated as the last and most dangerous commit simply never
happens — a risk deleted rather than sequenced. F7 is graded accordingly.

**The wake hold stays in krilld's router.** `Acquire` is single-flight,
`ModifyResponse` carries M3 sync-ack, `retryTransport` races the guest's
accept loop, and all of it was gated by C1–C4. An earlier framing in this
same session — "Caddy with the wake hold as middleware" — was **wrong** and is
recorded in the ROADMAP as rejected so it doesn't get re-proposed.

Rejected: **oauth2-proxy** (its model is a static allowlist with no hook for
"on successful callback, bind this identity to the capability token in the
original URL," which *is* the frozen share model — so it degenerates to
authenticate-everyone plus our own authz service, paying a process and a hop
for cookie handling alone); **Pomerium** (heavier, same gap); **auth state in
krilld's registry**, whether by shared file or via the admin API (see #5).

### 5. F2 amended — a revoke a recovery can undo is not a revoke

Designing where auth state lives surfaced that **the registry and the ACL
want opposite things from a restore.** The registry's documented recovery is
"roll back to a ≤24 h snapshot, bump `--cell-gen` to `restore_cell_gen`" —
correct for the mint, because fencing is what makes rolling it back safe. The
same rollback applied to auth state silently **un-revokes shares**.

So F2 gained step 3 (revocation survives total local-state loss, C1's shape
applied to auth) and the rule that a revoke is not acked until durable at the
object store — D1's rule, applied to auth. Its corollary is now explicit in
the gates file: auth state and the registry cannot share a restore path.

This also established the **amendment rule**, now written into
`m4-gates/GATES.md` with a tracking table:

- **Tightening** a frozen gate is design work discovering a criterion was too
  weak. Allowed, recorded with its reason.
- **Loosening** is the implementation asking the test to move. Requires a
  reason that does not reference what has already been built, and is far more
  suspect after code exists than before.

This amendment: tightening, no M4 code existed.

### 6. Three pre-code decisions (ROADMAP decision #10)

- **F4's demo app is a shared watchlist** at `watchlist.krill.run` (a
  subdomain — routing is by first DNS label, never a path). Items carry
  "added by whoever, when," which puts `X-App-User` on screen where a
  non-technical person can confirm it is right. Must write `/data/app.db`.
  Should be written by an agent through the MCP server, since "agent-written
  apps deploy in one call" is the pitch. Rejected: a grocery list (higher
  repeat use, but wants a housemate while F4 wants a friend on someone else's
  network) and a split-the-bill tracker (does arithmetic people will check,
  so the usability test would surface rounding bugs instead of doorman bugs).
- **`krill share` ships in the existing `cmd/krill` binary**, second endpoint
  at admin-port + 1 (9091 → 9092). `objstore-copy` already set the precedent
  of not being a pure single-endpoint client. `krill apps` becomes the first
  command fanning out to both services.
- **Two lifetimes, deliberately far apart.** Session (browser ↔ doorman):
  opaque 256-bit id → server row, 30-day sliding, 90-day absolute cap.
  Identity token (doorman → guest): signed, `aud` = exactly one app, ~5
  minutes, minted per request. F2 changed what session lifetime is *for* —
  with revocation instant, durable and restore-proof, lifetime is hygiene,
  not security. The guest token stays short for the opposite reason: the
  guest is the untrusted party, so a leak must die in minutes.

## Where things live

| Path | What changed |
|---|---|
| `m4-gates/GATES.md` | **New.** F1–F7, scope decisions, ordering rule, amendment rule + table |
| `ROADMAP.md` | Decisions #9 and #10; M4 section resized with the pivot; current-state entry for Phase 8 |
| `SERVER-SETUP.md` | New **Phase 8** (the name); Phase 5 `--base-host`; Phase 6 browser-access rewrite; header now Phases 0–8 |
| `README.md:86` | Sample deploy output prints `guestbook.krill.run:8080` |
| `session-summaries/` | **New directory** — this file is the first entry |

On the production box (not in git):

- `/etc/systemd/system/krilld.service` — `--base-host krill.run`
- `/etc/systemd/system/krilld.service.bak-2026-07-26` — pre-change backup

Relevant existing code, unchanged but load-bearing for M4:

- `internal/router/router.go:136` — `appName`, reads only the first Host label
- `internal/admin/admin.go:413` — `appURL`, the only consumer of `BaseHost`
- `internal/config/config.go:97` — daemon default stays `krill.local`
- `internal/registry/registry.go:115,132` — `apps` and `epochs` tables; `epochs` is the E1 mint
- `internal/regbackup/regbackup.go:41` — `_control/registry/` prefix, the pattern the doorman's backup should copy

## Current state: what's built and verified

**DNS (verified from `@1.1.1.1`, not the local resolver):**

| Record | Value |
|---|---|
| `*.krill.run`, `krill.run` | A → `46.4.64.187`, TTL 300, DNS-only (grey cloud) |
| `*.local.krill.run` | A → `127.0.0.1` |
| `krill.run` MX | `0 .` (null MX, RFC 7505) |
| `krill.run` TXT | `v=spf1 -all` |
| `_dmarc.krill.run` TXT | `v=DMARC1; p=reject; adkim=s; aspf=s` |

**Box:** `--base-host krill.run` live. Restart was clean — graceful
"shutting down: freezing active apps" on stop, `preflight_ok=true cell_gen=1
sync_ack=true` on start. Both apps FROZEN with valid snapshots afterward.

**Posture, re-proven from the Mac after DNS went live:** 80, 443, 8080, 9091
all closed; 22 open; `curl -m 8 http://ledger.krill.run/` → `curl: (28)
Connection timed out`. The name resolves to a closed door.

**Routing:** `Host: ledger.krill.run` and `Host: ledger.krill.local` returned
the *identical* digest — the suffix-is-ignored behavior demonstrated, not
assumed.

**Redeploy preserved the data plane:** before and after `krill deploy
m3-gates/examples/ledger`, `count 15, sum 160, digest aa7e396b…`, `head_lsn`
50 unchanged. `cur_epoch` advanced `4294967303 → 4294967304`, which is E1
minting on wake — the fence working, not drift.

**Cloudflare API token created and stored** (not on the box yet): scoped
`Zone:DNS:Edit` + `Zone:Zone:Read` on the `krill.run` zone only, no TTL, no
client-IP filter. At M4 it lands as `/etc/krill/cloudflare.env`, mode `0600`,
via `EnvironmentFile=`.

## How to reproduce the checks

```bash
# DNS, from a resolver that isn't yours
dig +short NS krill.run @1.1.1.1
dig +short anything.krill.run @1.1.1.1        # -> 46.4.64.187
dig +short TXT _dmarc.krill.run @1.1.1.1

# Posture, from the Mac (NOT from the box — the outside view is the point)
for p in 80 443 8080 9091 22; do nc -vz -G 5 -w 5 46.4.64.187 $p; done
curl -m 8 http://ledger.krill.run/            # must time out

# Routing + data, through the tunnel
ssh -f -N -o ExitOnForwardFailure=yes -o ControlMaster=yes -o ControlPath=~/.ssh/cm-krill krill
export KRILL_ADMIN=http://127.0.0.1:9091
go build -o bin/krill ./cmd/krill             # Makefile only cross-builds for linux
curl -s -H "Host: ledger.krill.run" http://127.0.0.1:8080/
./bin/krill stream ledger | grep -E 'head_lsn|cur_epoch'
ssh -O exit -o ControlPath=~/.ssh/cm-krill krill

# Cloudflare token — the SECOND call is the one that proves Zone:Read
curl -s -H "Authorization: Bearer $CF_TOKEN" https://api.cloudflare.com/client/v4/user/tokens/verify
curl -s -H "Authorization: Bearer $CF_TOKEN" "https://api.cloudflare.com/client/v4/zones?name=krill.run"
```

## Gotchas and footguns

1. **`*.local.krill.run` does not resolve on Sam's home network.** The
   gateway (`2600:4040:a4c9:c900::1`) returns `NOERROR` with an *empty answer
   section* for any public name pointing into loopback — DNS-rebinding
   protection. Not specific to this zone: `localtest.me` fails identically
   while `ledger.krill.run` resolves fine through the same resolver. Fix is
   `/etc/resolver/krill.run` containing `nameserver 1.1.1.1`. **This was not
   applied** — see "What's left."
2. **`dig` bypasses `/etc/resolver` by design.** After applying that fix,
   verifying with `dig` will keep showing the failure. Verify with
   `dscacheutil -q host -a name …` or plain `curl`.
3. **Cloudflare's "Edit zone DNS" token template is insufficient on its
   own.** `caddy-dns/cloudflare` needs `Zone:Zone:Read` **as well as**
   `Zone:DNS:Edit` — `DNS:Edit` alone cannot resolve the zone ID, and the
   failure reads like a generic auth error while you are mid-ACME.
4. **Never set a TTL on that token.** It expires into a silent certificate
   renewal failure 60–90 days out.
5. **Do not add a client-IP filter to that token yet.** The box has an IPv6
   /64; a v4-only filter 403s any request that egresses over v6, and the
   error looks like a permissions problem. Add it after ACME works, listing
   both addresses.
6. **`--base-host` is cosmetic.** `router.appName` reads only the first DNS
   label and never validates the suffix, so changing it cannot break routing.
   The gate suites still send `Host: <app>.krill.local` and **that is
   correct** — do not "fix" them. The daemon default also stays
   `krill.local`, which is right for a box with no DNS.
7. **That same gap is a real hole**: today any Host with a valid app label
   routes. It is now F3's suffix-pinning requirement.
8. **The live systemd unit differs from the runbook snippet** — it carries
   Phase 7's `--objstore gs://…`, `KRILL_GCS_CREDENTIALS`, and the registry
   backup flags. Read the actual file before `sed`-ing it; a backup was left
   at `krilld.service.bak-2026-07-26`.
9. **ssh `ControlPath` must be short** (`~/.ssh/cm-krill`) or ssh fails with
   `unix_listener: path … too long for Unix domain socket`.
10. **`.dev`, `.app`, `.page` are HSTS-preloaded.** Confirmed against
    `hstspreload.org/api/v2/status`. Do not register a project domain on one
    of those while any plain-HTTP testing phase remains.
11. **Two self-inconsistencies were caught in the gates draft** and are worth
    knowing as a class: F5 originally required the hostile deploy to arrive
    "over the public path," contradicting the ordering rule (the threat is
    the build context, not the packet's origin); and F7 said to "re-run"
    Phase 8's exposure check, which asserts the *opposite* for 80/443 after
    exposure. Pre-registered gates need an internal-consistency read too.
12. **`guestbook` is not data-plane-backed** (writes `/var/lib/guestbook.db`,
    outside `/data`). Pre-existing finding, but it is why the demo app
    decision explicitly requires `/data/app.db`.
13. **Cloudflare's dashboard nags are wrong here.** "Proxying is required for
    most security features" — grey cloud is deliberate. "Visitors cannot
    reach krill.run" — that is the posture, not a misconfiguration.

## What's left to ship

**Must-have, in order:**

1. **Caddy + wildcard cert against ACME *staging*.** Deliberately first: it
   is the only piece with an external dependency that can fail in ways you
   do not control, and Let's Encrypt rate limits are per-registered-domain
   per-week. Serve nothing but a 404 on 443.
2. **Doorman skeleton** — OIDC round trip, opaque session, no ACL. F1's spine.
3. **ACL, share links, claim, revoke** — F2 and F3.
4. **Builder in a throwaway microVM** + `m4-gates/examples/hostile/` — F5.
5. **Egress baseline** (nftables per tap; app guests silent, builder VMs reach
   a registry only) — F6.
6. **The watchlist app**, agent-written via MCP — without it F4 has nothing
   to run against.
7. **Exposure** — production cert, 80/443 open, F7 last.

**Nice-to-have:**

- `/etc/resolver/krill.run` on the Mac (gotcha 1).
- `m4-gates/` runnable scripts and a `RESULTS-TEMPLATE.md`, matching the
  m1/m2/m3 layout. Only `GATES.md` exists so far.
- The promoted A1 margin work (cut object-store round trips) — production
  passes A1 by only **15 ms**. Not blocking M4, but it touches the wake
  path's fencing sequence and wants its own session with the sim harness as
  the gate.

## Branch state

`main`, four commits, all pushed to `origin/main`
(`github.com/shulman33/krill`). Working tree clean except two pre-existing
untracked paths that were deliberately left alone: `next-steps.md` and
`design-prototypes/`.

## Known unknowns

1. **Google OAuth verification status could threaten F4.** F4's PASS clause
   says the friend must succeed "without a security warning of any kind." An
   OAuth client in *Testing* status limits you to explicitly-added test users;
   an unverified client requesting sensitive scopes shows a warning
   interstitial. The intended mitigation is to request **only**
   `openid`/`email`/`profile`, which should avoid verification entirely —
   **but this was not verified this session.** Check it before building the
   flow, not the week F4 runs.
2. **How `krill-doorman` backs itself up.** Decision #9 says its own SQLite
   with synchronous ship-on-revoke, modelled on `internal/regbackup`. Whether
   that reuses `regbackup` directly or is a parallel implementation is
   undecided.
3. **How app deletion cascades to ACL rows.** Lazy prune on lookup-miss was
   designed (fails closed — unknown app 404s at the doorman before any wake)
   but not built.
4. **Whether Caddy's `forward_auth` covers the whole flow** or a custom Caddy
   module is needed for the OAuth callback routes specifically.
5. **What the doorman does while krilld is restarting.** Auth would fail
   during that window. Probably fine — krilld restarts freeze apps anyway, so
   requests would fail regardless — but the behavior is unspecified.
