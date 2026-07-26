# M4 acceptance gates — pre-registered

*Written 2026-07-26, BEFORE any M4 code exists. Same discipline as the
benchmark and M1/M2/M3: the pass/fail criteria are frozen here first, so the
implementation can't quietly bend the test to fit what got built.*

M4 is **the doorman**: the first milestone where software Sam did not write
is driven by people Sam did not vet. Three theses, one milestone (scope
decided 2026-07-26, see "Scope decisions" below):

1. **Edge auth** — Google OAuth in front of every app, a signed per-app
   identity the guest can trust, share links, revoke, and a three-plane ACL
   (use / data / edit).
2. **An isolated builder** — `docker build` of an untrusted context moves off
   the host and into a throwaway Krill microVM. The platform isolates its own
   builder with the primitive it sells.
3. **First exposure** — TLS on a real name, and the router stops binding
   loopback. This is the last commit of the milestone, never the first.

## ⚠ Why these are F-gates, and which letters are skipped

The natural next letter after M3's C-series is **D — and D is taken.** `D1–D4`
is the durability contract, cross-referenced by `FencingProtocol.tla`, the
pressure-test doc §1.2, and the architecture doc's D4 write path. `E1–E6`
(epoch rules), `G1–G5` (benchmark gates) and `I1–I4` (invariants) are
likewise load-bearing across the three tripwire artifacts (CLAUDE.md).

**F is for front door.** D and E are skipped deliberately; do not "fix" the
sequence. Any future milestone picks from H, J, K, … and re-reads this note
first.

## Terminology

- **Plane** — one of three permissions a share can grant. **use** = send
  requests to the app. **data** = read or export the app's `/data` contents.
  **edit** = replace the app's code (`krill deploy`). Note "data plane" here
  is a *permission*, not M3's WAL-shipping machinery; where ambiguity is
  possible this file says "data **share** plane".
- **Share link** — an unguessable capability URL (decision below). Holding it
  is the grant; the Google sign-in that follows establishes *who* is using it,
  for `X-App-User` and revoke, not *whether* they may.
- **Claim** — the first successful sign-in against a share link, which binds
  that Google identity to the link on the app's ACL.
- **Doorman** — the edge process terminating TLS, running the OAuth flow, and
  minting the per-app identity token. Assembled from proven components
  (decision #8); only the ACL, share links, and per-app scoping are ours.
- **Builder VM** — the throwaway Krill microVM a deploy's `docker build` runs
  inside. Distinct from an app VM in exactly one way that matters: it is the
  only guest permitted outbound to a container registry.

## Scope decisions (frozen 2026-07-26, with Sam)

- **Share model: the link is the capability.** A recipient opens a link, signs
  in with Google, and is recorded on the ACL at claim time. Sam does not need
  to know a recipient's address in advance — that is what makes F4 winnable.
  Identity-based-only shares (`--user friend@gmail.com`) are not required by
  any gate below; if they ship anyway, they must not weaken F2.
- **All three planes ship, including remote `edit`.** This is why the builder
  gates (F5) and the egress baseline (F6) are *inside* M4 rather than
  dessert: per the ROADMAP posture map, edit shares mean other people's
  agents push Dockerfiles, and today that executes their instructions as root
  on the host, outside any microVM. The rejected alternative was shipping
  use+data first and splitting the builder into an M4.5.
- **Consequence, stated plainly:** M4 is now a ~3–4 week milestone carrying
  two hard things at once. If it needs to be cut, cut it at the *plane*
  boundary — ship F1–F4 + F7 with `edit` ungranteable, and F5/F6 become
  M4.5's gate set. Do not cut by weakening a gate.

## Ordering rule (this milestone's PT-3)

F1, F2, F3, F5 and F6 are provable **before anything is exposed** — through
the SSH tunnel, against the doorman on loopback. F4 and F7 are the exposure
gates and are the only two that require the public listener. Run them in that
order, last. **The doorman must pass its own gates before the door opens**;
a green F4 obtained by exposing the router early is a failed milestone, not
an early one.

---

## F1 — Identity at the edge: a stranger gets in, and the app knows who they are

The core doorman gate. E-series fencing is untouched by any of this; the
doorman sits in front of the router's wake hold, never inside it.

1. `krill share ledger --plane use` → a share link with an unguessable token.
2. From a browser profile with **no** session for this deployment, open the
   link.
3. Complete Google sign-in as an identity that has never touched this app.
4. Observe the app's own view of the request.

**PASS:** the request completes, the app serves normally, and the guest
receives `X-App-User` equal to the signed-in Google identity — arriving as a
token the app can verify, not a bare header. The app's ACL now lists that
identity as claimed against that link. A wake, if the app was FROZEN, still
happens inside the router's hold: the user sees a slow first byte, never a
502 or a login loop.
**FAIL:** the app serves without authentication; or `X-App-User` is absent,
wrong, or unverifiable; or the flow requires the recipient to create anything
(an account, a password, an invite acceptance) beyond signing in with Google.

## F2 — Revoke takes effect on the next request

The property that makes sharing safe to do casually. Sessions are a cache;
revocation must not wait for one to expire.

1. With the F1 session live and working, revoke — both variants:
   a. revoke the **claimed identity** (`krill unshare ledger --user …`);
   b. revoke the **link** itself, with a second identity holding a live
      session claimed from that same link.
2. From the already-authenticated browser, issue the very next request.

**PASS:** both next requests are refused (403), with no krilld restart, no
app freeze/wake in between, and no waiting out a TTL. A revoked link cannot
be re-claimed by anyone. Revocation is durable: it survives a krilld restart
and an app freeze/wake cycle.
**FAIL:** the revoked session serves even once; or refusal depends on a cookie
or JWT expiring; or the revocation is lost across a restart.

## F3 — The three planes actually separate, and the door has one keyhole

Where the hand-written product code lives, so where the adversarial pressure
belongs. Every check below is run **as a real request from a use-only user**,
not asserted from a unit test.

1. Share `ledger` with identity U granting **use only**.
2. As U, attempt in turn: the app itself (must work); the data-share surface
   (read/export `/data`); any edit surface (`krill deploy`, the admin API);
   another app U was never shared on.
3. **Token scoping:** capture the identity token the doorman minted for U on
   `ledger` and replay it against a second app. The audience is one app.
4. **Host-suffix pinning:** send requests through the public listener with
   `Host: ledger.example.invalid`, `Host: ledger.local.krill.run`, a bare IP,
   and `Host: <valid-app>.<attacker-domain>`.
5. Repeat 2–3 for a **data**-plane user attempting edit, and an **edit**-plane
   user — confirming edit is a superset only where intended.

**PASS:** every unauthorized attempt is refused, and refused *at the doorman* —
no request for an unauthorized plane reaches a guest at all. The replayed
token is rejected for audience mismatch. Only hostnames under the configured
base host route; everything else is refused before any wake is triggered.
**FAIL:** any cross-plane access; any token usable against a second app; any
unauthorized request that wakes an app (a fence that bills is still a fence
that failed); or any accepted `Host` outside the base domain.

*Why step 4 is a gate and not a nicety: as of 2026-07-26 `router.appName`
reads only the first DNS label and never validates the suffix, so any Host
with a valid app label routes. The doorman is where that closes.*

## F4 — The human gate: a non-technical friend, cold, zero setup

The milestone's reason to exist, and the only gate that cannot be scripted.
Run it on a real person who has not seen the project.

1. Deploy an app worth using (not `guestbook` — see the note in
   `SERVER-SETUP.md`; `ledger` or better, something with an actual purpose).
2. Send the share link over a normal channel — iMessage, Slack — with **no
   instructions**.
3. Watch them use it on a phone, on a network that is not Sam's, without
   answering questions.

**PASS:** they reach a working app without asking for help, without
installing anything, and without a security warning of any kind. The app's
first wake is invisible to them or reads as ordinary slowness. Sam can then
show them, in the ACL, that it was *them* who used it.
**FAIL:** any question that has to be answered before they succeed; any
browser TLS/HSTS warning; a cold-wake so slow they think it broke; a login
loop; or a mobile-browser OAuth flow that dead-ends.

*Record verbatim what they said, especially confusion. This is the only
usability signal the project will get for free.*

## F5 — The builder is a guest, and a hostile build stays inside it

Today-risk #1, closed on purpose. `docker build` of an untrusted context must
execute inside a throwaway Krill microVM, never on the host.

1. Add a deliberately hostile build context to `m4-gates/examples/hostile/`
   (the analogue of M2's `broken`) whose Dockerfile, **at build time**,
   attempts to: read `/etc/krill/gcs.json` and the host's registry DB; reach
   the admin API on `127.0.0.1:9091`; reach the object store with any
   ambient credential; write outside its own context; read another app's
   `data.ext4`; enumerate host processes; and persist something that outlives
   the build.
2. Deploy it via an **edit**-plane share, as a non-Sam identity, **through the
   doorman** — over the tunnel is fine and correct here, because the threat is
   the build context, not the packet's origin. What must not happen is
   reaching `docker build` by calling the admin API directly; that path is
   Sam's and proves nothing.
3. Inspect the host afterwards.

**PASS:** every attempt fails from inside the build. The host's credentials,
admin API, registry, object store and other apps' data are unreachable and
unmodified; nothing persists past the builder VM's destruction; the builder
VM is destroyed whether the build succeeded, failed, or hung (and a hung
build is killed by a bounded timeout). `docker build` never runs on the host
for any deploy that arrives over the network. A normal deploy still works and
still lands within a build budget recorded alongside the M2/metal numbers.
**FAIL:** any host artifact reachable or modified from a build; any builder VM
surviving its deploy; any network-arriving deploy whose build ran on the
host; or a resource-exhaustion build (disk-filling, fork-bombing) that
degrades the box for other apps.

## F6 — Egress: apps stay silent, builders reach exactly one thing

Today-risk #2. Guests currently have **no** outbound at all (no NAT is
configured) — the baseline must land in the same change that first grants
any, and the builder VM is precisely what first needs it.

1. From an **app** guest, attempt outbound: arbitrary HTTP/HTTPS, DNS,
   SMTP on 25/465/587, the host's own loopback and management addresses,
   other apps' subnets, and the link-local/metadata range (N/A on Hetzner —
   assert it anyway; the rule must travel if the platform ever runs on a
   cloud host).
2. From a **builder** VM, attempt the same set, plus the container registry
   it legitimately needs.
3. Drive a per-app rate limit past its threshold.

**PASS:** app guests reach nothing outbound by default; a guest granted
egress still cannot reach 25/465/587, the host's management surfaces, other
apps' subnets, or link-local. Builder VMs reach the registry and nothing
else. Rate limits engage and are observable. Apps remain reachable *inbound*
through the router throughout — the baseline must not break the product.
**FAIL:** any app-to-app path; any SMTP path; any guest reaching the admin API
or the host's credentials; a builder VM with general internet access; or a
rate limit that can be exceeded.

*Hetzner blocks outbound 25/465 provider-side, so 587 and HTTP-API abuse are
ours to own. One host means one IP: abuse gets the box null-routed and that
is a total outage.*

## F7 — Exposure: the door opens, and only the door

The last commit of the milestone. Everything below was true before this gate
ran, and must remain true after.

1. Obtain a wildcard certificate for `*.krill.run` via ACME **DNS-01**, using
   the pre-staged Cloudflare token (`Zone:DNS:Edit` + `Zone:Zone:Read`,
   `SERVER-SETUP.md` Phase 8).
2. Open 80/443 and un-loopback the router behind the doorman.
3. Re-run Phase 8's port scan **from off-box** — with the expectation
   inverted for exactly two ports: 80 and 443 must now answer, while 8080
   and 9091 must still refuse. Phase 8's own "all four closed" assertion is
   superseded here and nowhere else.
4. Force a renewal (ACME staging first) and confirm it is unattended.

**PASS:** `https://<app>.krill.run/` serves through the doorman with a valid
certificate and an A-grade TLS configuration; 80 redirects to 443; **the
admin API (9091) and the router's own port are unreachable from the
internet**; every F1–F3 result still holds against the public listener; and
renewal completes with no human in the loop. `ufw` and the listener bindings
agree with each other — the firewall is not the only thing keeping the admin
API private.
**FAIL:** the admin API reachable from outside; any app reachable without the
doorman; a certificate that requires manual renewal; or any F1–F3 gate that
passed on loopback and fails in the open.

## Where they run

All of it on the production box (`krill-fsn1`, 46.4.64.187) — M4 is the first
milestone with prerequisites the bench VMs cannot supply: a real domain, a
real certificate, and a real Google OAuth client. Follow **"Running the gate
suites on this box"** in `SERVER-SETUP.md`: stop the unit rather than
`pkill`, use a scratch `KRILL_DATA`, and delete krilld's taps before anything
that wants `172.16.0.1/24`.

⚠ **F4 must run against production state, not a scratch dir** — a friend
opening a link to a throwaway app on a scratch data dir proves nothing about
the box that will still be serving it next week.

Results land in `m4-gates/RESULTS-2026-XX-XX-metal.md` per the template
convention, with the same tier honesty as every prior suite.

## Not gated here (deliberately)

- **Wake latency under the doorman.** The OAuth round trip is not on the wake
  path, but the doorman *is* a new hop in front of the router. Record the
  delta as informational (an **F-info**, in the C-info mold) rather than
  gating it — A1 is already carrying only 15 ms of margin in the production
  configuration, and a latency gate here would conflate two problems.
- **SSO federation, custom domains, a dashboard.** M5.
- **The three protocol artifacts.** M4 changes no epoch rule, so the tripwire
  should stay untripped. If the doorman ever needs to touch an E-rule, stop
  and follow the CLAUDE.md checklist — spec first, TLC positive plus all
  three negative configs, then both HTML docs, then republish.
