# M4 built — the doorman, the isolated builder, the egress baseline (2026-07-26)

## TL;DR

M4's code exists end to end: `krill-doorman` (edge auth, three-plane ACL, share
links, restore-proof revocation), `internal/egress` (F6's nftables baseline),
`internal/buildvm` (F5's throwaway build VM), the CLI and MCP sharing verbs,
runnable F1–F7 gate scripts, `SERVER-SETUP.md` Phase 9, and `deploy/Caddyfile`.
Five commits, all pushed to `origin/main` (`654effd`, `d282448`, `433b311`,
`51d3ede`, `5fb1af2`).

**No F-gate has been run. Nothing touched the production box.** That distinction
is the whole point of pre-registering gates, so it is stated first and it is
stated in `ROADMAP.md` the same way: M4 is *built*, not *done*.

What was verified locally: `go vet ./...` and `go test -race ./...` green, all
four binaries cross-compile for linux/amd64. `internal/doorman`'s tests rehearse
F1–F3 against a fake OIDC provider, including the full revoke → snapshot →
destroy-database → restore → still-403 sequence F2 was tightened to require.
`internal/admin` asserts from both sides that a network-arriving deploy never
reaches host docker. The watchlist app's pure-Python ed25519 verifier was
cross-checked against Go's — a real minted token verifies, and wrong audience,
wrong key, expired and tampered all fail.

The TLA+ spec and the three tripwire artifacts are **untouched**: M4 changes no
epoch rule, exactly as `m4-gates/GATES.md` predicted.

## Why we did this

`m4-gates/GATES.md` froze F1–F7 in the previous session, before any M4 code
existed. ROADMAP decisions #9 and #10 fixed the doorman's shape (Caddy +
`forward_auth` to a small unprivileged process, krilld's router untouched on
loopback) and three pre-code choices (the watchlist demo app, `krill share` in
the existing binary, two deliberately-far-apart lifetimes).

This session was the implementation. The success criterion was not "it works" —
nothing here has been run against Google, a certificate, or a real friend — but
"every gate has something to run against, and the reasoning behind each fork is
recorded rather than re-derivable."

## Architectural decisions that matter

These are new this session; they are also recorded as **ROADMAP decision #11**
so a fresh session finds them without reading this file.

### 1. Per-app session cookies over a master session, not one domain-wide cookie

A request proxied to a guest carries its `Cookie` header, and guests run
agent-written code the platform explicitly does not trust. A `.krill.run`
session cookie would therefore be a **platform-wide session-theft primitive
handed to every app**.

So: the master session is host-only on `auth.krill.run` and never leaves it;
each app host gets its own opaque session, minted through a single-use handoff
code. Blast radius of a hostile guest reading its own `Cookie` header is one app
and one user's own access. Caddy also strips the app cookie on the way to the
guest.

Cost: one extra redirect on the first visit to each app (`sso` → `accept`).
Rejected: one domain-wide cookie (simpler, one fewer hop, unacceptable under
this project's own threat model).

### 2. The identity public key rides the kernel command line, not JWKS

F6 leaves app guests with **no outbound network at all**. A JWKS fetch would
mean cutting a hole through the baseline for every app — the baseline's first
exception, on day one, for the security feature.

An ed25519 public key is 32 bytes → 44 base64 characters, which fits comfortably
in the kernel cmdline where the network contract already travels
(`krill_idkey=`). The generated init exports it as `KRILL_IDENTITY_PUBKEY`.
Rotation is a wake, not a redeploy. This is also *why* the token is ed25519 and
not RSA.

Also published at `/_krill/jwks.json` for tooling; guests do not need it.

### 3. A revocation tombstone names the grant IDs it kills

One mechanism, three behaviors that all had to hold simultaneously:

- Restoring an older database cannot resurrect access — grant IDs are stable and
  the tombstone still names them.
- A revoked person cannot re-admit themselves by re-opening the link they were
  sent — re-claiming updates the *same* grant row (`UNIQUE(app, subject,
  share_id)` + `ON CONFLICT`), so it comes back still named.
- An operator *can* deliberately re-admit someone with a fresh link — new share
  → new grant ID → no tombstone names it. That is a new grant, not an undone
  revoke, and the revocation stays in the log where the audit trail wants it.

The first draft used blanket `(app, subject)` tombstones, which got behavior 1
and 2 right and made 3 impossible. Worth knowing: the fix was to make the
tombstone *specific* rather than *general*.

### 4. There is no host-build fallback

When no builder VM is configured, a deploy arriving through the doorman is
**refused** (503), not built on the host. The fallback is the vulnerability, so
it does not exist. `--build-isolation` is `off | untrusted | all`; `untrusted`
is the default. The `X-Krill-Deploy-Untrusted` header can only ever *raise*
isolation — forging it on the loopback admin API costs the forger the host path
and gains nothing.

### 5. `--egress-build-allow` — a tension F6 does not resolve on its own

F6 says builder VMs reach "the registry and nothing else", and almost every real
Dockerfile also runs `apt-get` or `pip install`. `m3-gates/examples/ledger`
needs only its base image; `m2-gates/examples/guestbook` needs both apt and pip.

So the builder allowlist is two lists feeding one nft set: container registries
(default: Docker Hub's three names) and package sources the operator explicitly
chose (default: **empty**). A named allowlist is still not the "general internet
access" F6 FAILs on, but it is a bigger surface than the registry alone, so it
should read as a decision in the unit file rather than be discovered.

### 6. The output disk *is* the golden image

`internal/buildvm` gives the builder VM two disks: the context read-only on
`/dev/vdb`, and an **empty formatted ext4** on `/dev/vdc` that the build
populates. That disk becomes the app's golden image directly, so nothing large
is read back through `internal/ext4`'s pure-Go parser. Result and log arrive on
the serial console — the channel that already made M1's guest tracebacks
debuggable.

Rejected: writing a tar/image into the output disk and extracting it host-side
(a gigabyte through the ext4 reader, for no benefit).

## Where things live

| Path | What |
|---|---|
| `internal/doorman/store.go` | Schema, three-plane ACL (`Plane.Allows` is the *only* place edit ⊃ data ⊃ use is expressed), shares, grants, sessions, flows, tombstones. `Best()` at `store.go:~430` is THE authorization query. |
| `internal/doorman/revoke.go` | The tombstone log. `Revoke()` captures grant IDs → `Put` to the object store → *then* applies locally. `Sync()` replays at every start. |
| `internal/doorman/snapshot.go` | The doorman DB checkpoint + `RestoreLatest`. Snapshot = checkpoint, log = delta: M3's restore path in different clothes. |
| `internal/doorman/token.go` | ed25519 minting, `VerifyToken`, `TokenTTL = 5m`. |
| `internal/doorman/oidc.go` | Google via `x/oauth2` + `coreos/go-oidc` — the first non-SQLite deps this repo has taken (decision #8: buy the plumbing). |
| `internal/doorman/server.go` | `verify` (the forward_auth target), the three-hop sign-in, `claimLink`, the data/edit planes, `appFromHost` (F3 step 4), `deployParams` (clamping). |
| `internal/doorman/control.go` | The operator API on 9092: shares, grants, revoke, sync, snapshot, status. |
| `cmd/krill-doorman/main.go` | Wiring + `lazyGoogle` (OIDC discovery deferred to first use, so a Google outage cannot crash-loop the process that owns revocation). |
| `cmd/krill/share.go` | `krill share/shares/unshare/doorman`; `doormanAddr` derives 9092 from 9091. |
| `internal/egress/egress.go` | The whole F6 ruleset as one rendered string, applied by one `nft -f -`. |
| `internal/buildvm/buildvm.go` | F5's orchestrator. |
| `m4-gates/builder-image/` | The builder VM's rootfs (`buildkitd --oci-worker-snapshotter=native`) and `krill-build.sh`, its PID 1. |
| `m4-gates/examples/watchlist/` | F4's app + `krill_identity.py` (stdlib-only ed25519). |
| `m4-gates/examples/hostile/` | F5's adversary. |
| `deploy/Caddyfile` | The annotated front door. |

Touched elsewhere: `internal/admin/admin.go` (isolation policy, `/v1/apps/{name}/data`),
`internal/host/backend.go` (`krill_idkey=`), `internal/builder/builder.go`
(init exports the key; `NewResult`), `internal/router/router.go`
(`RouteSuffixes`, opt-in), `internal/dataplane/coordinator.go` (`ExportDB`),
`internal/network/network.go` (`DeriveBuilder`), `internal/firecracker/machine.go`
(`Extra []Drive`), `internal/config/config.go` (the new flags).

## How to run / reproduce

```bash
# Everything that can be checked without hardware:
go vet ./... && go test -race ./...
make krilld-linux krill-linux doorman-linux fencetool-linux

# The doorman's F1-F3 rehearsal specifically:
go test -race -run 'TestF1|TestF2|TestF3' ./internal/doorman/ -v

# The Go↔Python ed25519 cross-check (this is how the verifier was validated):
KRILL_EMIT_TOKEN=/tmp/tok.txt go test ./internal/doorman/ -run TestMintForPythonVerifier -count=1
cd m4-gates/examples/watchlist && python3 -c "
import sys; sys.path.insert(0,'.')
import krill_identity as ki
pub, tok, other = open('/tmp/tok.txt').read().split()
print(ki.verify(tok, 'watchlist', pub))"
```

The gate suite itself: `m4-gates/README.md`. The tunnel now needs **9090 and
9092** as well as 8080/9091.

## Gotchas and footguns

1. **The doorman's tests looked broken because Go's `http.Client` sends no
   `Accept` header.** `startSignIn` answers API clients with 401 + JSON and
   browsers with a 302 into Google. A test that does not set
   `Accept: text/html` gets the JSON path and looks like a failure. Real
   browsers always send it.
2. **`httptest.NewServer` gets a new port on restart.** The F2 restore test
   restarts the whole doorman; a client whose `DialContext` captured the old
   address at construction time fails with connection-refused. Resolve the
   address *at dial time* (`doorman_test.go:120`).
3. **Sessions are per-app, so "cross-app access" first returns 302, not 403.**
   The user is unauthenticated *for that app*. The meaningful assertion is: sign
   in there (silently, via the master session), *then* get 403. The first draft
   of `TestF3_PlanesSeparate` asserted 403 immediately and was wrong about the
   design, not about the code.
4. **nftables rule comments must be the LAST element of a rule.**
   `counter comment "x" drop` does not parse; `counter drop comment "x"` does.
   Cost one rewrite of the generator.
5. **`AppTapPrefix = "krill"` matches builder taps too** (`krillb0` starts with
   `krill`). That is deliberate — the shared prohibitions apply to both classes —
   but it means **rule order is the policy**: prohibitions first, then the
   builder accepts, then the builder catch-all drop, then apps. `egress_test.go`
   asserts the ordering, not just the presence, because a prohibition after an
   accept is a dead line.
6. **A `go build ./cmd/krill` (no `-o`) drops a `krill` binary in the repo root**
   and `.gitignore` only covers `/krilld`. It nearly got committed.
7. **`internal/` packages cannot be imported from a scratch `main.go` outside the
   module.** To get a real token out of Go for the Python cross-check, the
   emitter had to be a test in the package (`mint_export_test.go`, skipped unless
   `KRILL_EMIT_TOKEN` is set).
8. **The `Bash` tool's working directory persists across calls.** A `cd` into
   `m4-gates/examples/watchlist` made three subsequent `ls m3-gates/` calls fail
   confusingly.
9. **F2 step 3 can pass vacuously**, and did not have a guard until the review
   pass. Restoring a snapshot that already contains the revocation proves nothing
   about the log — this is M3's fixture-vacuity trap in new clothes.
   `f2-revoke.sh` now snapshots *before* revoking and refuses to continue if the
   log holds no revocation that snapshot lacks, or if the hourly timer took a
   newer one in between.
10. **The edit plane forwarded the caller's whole query string** to krilld's
    deploy endpoint, handing a remote deployer `size_mb`. `deployParams`
    (`server.go`) now allowlists and clamps; unknown parameters do not travel.
    Found on review, not by a test — worth remembering that the review pass
    caught a real F5-relevant hole.
11. **`--base-host` is still cosmetic for krilld's router.** `RouteSuffixes` is
    opt-in and empty by default, because the gate suites correctly send
    `krill.local`. Production sets `--route-suffixes krill.run,krill.local`. The
    doorman pins unconditionally; that is where F3 step 4 is graded.
12. **`m4-gates/results/` holds a real browser session cookie** during a run (F1
    saves one so F2/F3 can reuse it). It is gitignored; discard it afterwards.
13. **`guestbook` is still not data-plane-backed** (writes `/var/lib`). The
    watchlist deliberately writes `/data/app.db` with `synchronous=FULL`.

## What's left to ship

**Must-have, in order** (this is also `ROADMAP.md`'s next-action list):

1. `SERVER-SETUP.md` Phase 9 steps 1–4: the Google OAuth client, the
   `krill-doorman` user/unit, krilld's new flags, the builder image.
2. `m4-gates/00-setup.sh`.
3. **F1, F2, F3, F5, F6 through the tunnel, with 80/443 still closed.**
4. Phase 9 steps 5–6: Caddy against **ACME staging**, then the doorman wired in.
5. Phase 9 step 7: production certificate → **F4** (a real friend) → **F7**
   (from the Mac).
6. `m4-gates/RESULTS-2026-XX-XX-metal.md`, including the F-info wake-latency
   delta the doorman adds.

**Nice-to-have**: `/etc/resolver/krill.run` on the Mac; MCP `stream`/`restore`
tools; the A1 margin work (object-store round trips).

## Branch state

`main`, five commits, all pushed to `origin/main`
(`github.com/shulman33/krill`). Working tree clean except `next-steps.md` and
`design-prototypes/`, both pre-existing and deliberately left alone.

## Known unknowns

1. **The builder kernel.** A container build needs cgroup v2 and namespaces; the
   Firecracker CI kernel is built for minimal app guests and it is untested here.
   `--oci-worker-snapshotter=native` removes the overlayfs requirement
   specifically so there is one fewer thing to get right. If `buildkitd` will not
   start, the failure arrives in the deploy response with its log tail attached.
   Escalation path: `m4-gates/builder-image/README.md`.
2. **Google's OAuth verification status.** Still unverified, and still the one
   thing that could fail F4 on a warning interstitial. The mitigation (request
   only `openid`/`email`/`profile`) is written into Phase 9 step 1 as a ⚠.
   **Confirm before building the client, not the week F4 runs.**
3. **Whether the in-VM `krill-build.sh` produces an init identical in behavior to
   `internal/builder.InitScript`.** Both read the OCI config for
   ENV/WorkingDir/Entrypoint/Cmd/EXPOSE; only the Go one has ever run. An app
   should not be able to tell which builder produced it, and nothing yet proves
   that.
4. **Whether `--egress-build-allow` empty is survivable in practice.** The
   watchlist builds base-image-only on purpose; most real Dockerfiles will not.
   The first `apt-get` failure inside a builder VM is expected, not a bug.
5. **What the doorman does while krilld restarts** is now specified (fail closed:
   503, positive lookups cached 30s so a short restart is invisible) but not
   observed under a real restart.

## One thing worth re-reading before touching this code

`ROADMAP.md` decision #9's second constraint: krilld is root, so the
internet-facing OAuth surface cannot live inside it. And F3's: an unauthorized
request must not wake an app. Together they are why authorization completes
strictly upstream of krilld and why a 200 from `doorman.verify` is the only path
inward. If a future change is ever tempted to put an auth check inside krilld's
router, both constraints have to be re-argued first.
