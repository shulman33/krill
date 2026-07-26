# M4 gates — running them

The criteria are frozen in [`GATES.md`](GATES.md), written before any M4 code
existed. This file is only how to run them.

## Where, and in what order

All of it on the production box (`krill-fsn1`, 46.4.64.187) — M4 is the first
milestone with prerequisites a bench VM cannot supply: a real domain, a real
certificate, a real Google OAuth client. **F4 runs against production state,
not a scratch data dir**: a friend opening a link to a throwaway app proves
nothing about the box that will still be serving it next week.

**The ordering rule is this milestone's PT-3.** F1, F2, F3, F5 and F6 are all
provable through the SSH tunnel, against the doorman on loopback, before
anything is exposed. F4 and F7 are the only two that need the public listener,
and they run last. A green F4 obtained by exposing the router early is a
failed milestone, not an early one.

```bash
# On the box (or through a tunnel forwarding 8080, 9091 AND 9092):
cd m4-gates
./00-setup.sh          # preflight: refuses if a result would be meaningless
./f1-identity.sh       # the one gate with a human in it
./f2-revoke.sh         # needs root: it stops the doorman and deletes its DB
./f3-planes.sh
./f5-builder.sh        # needs root for the host-inspection half
./f6-egress.sh         # needs root to read nftables

# Then, and only then:
#   F4 — a real person, a real phone, a network that is not Sam's
BOX_IP=46.4.64.187 ./f7-exposure.sh     # ⚠ RUN FROM THE MAC, not the box
```

Results land in `results/` (gitignored) and get written up per
[`RESULTS-TEMPLATE.md`](RESULTS-TEMPLATE.md).

## The tunnel needs one more port than before

M1–M3 needed 8080 and 9091. The sharing verbs talk to the doorman on **9092**,
and the forward_auth surface is **9090**:

```bash
ssh -f -N -o ExitOnForwardFailure=yes \
    -L 8080:127.0.0.1:8080 -L 9090:127.0.0.1:9090 \
    -L 9091:127.0.0.1:9091 -L 9092:127.0.0.1:9092 \
    -o ControlMaster=yes -o ControlPath=~/.ssh/cm-krill krill
```

(`ControlPath` must be short or ssh fails with `unix_listener: path too long`.)

## What is manual, and why

- **F1's sign-in.** "Complete Google sign-in as an identity that has never
  touched this app" cannot be scripted without becoming a different test. The
  script does the halves a script can do — mint the link, watch the ACL until
  the claim lands, then assert everything downstream — and asks for one paste
  of the session cookie, which F2 and F3 reuse.
- **F2 step (b)** needs a *second* browser identity holding a live session
  claimed from the same link. A run without it is an incomplete F2, and the
  results template has a line saying so.
- **F4 entirely.** It is the milestone's reason to exist and the only gate
  that cannot be scripted. Record verbatim what they said, especially the
  confusion — it is the only usability signal this project gets for free.
- **F5's hung-build timeout** (it costs a full `--build-timeout`) and **F7's
  renewal check** (it takes a forced ACME round trip).

## The examples

| Path | What it is for |
|---|---|
| `examples/watchlist/` | F4's app. Items carry "added by whoever, when", so `X-App-User` is on screen where a non-technical person can confirm it. Writes `/data/app.db`, and **verifies** the identity token rather than trusting the header. |
| `examples/hostile/` | F5's adversary: a context whose build-time `RUN` lines probe for host credentials, the admin API, the registry database, other apps' data, metadata IPs, SMTP and persistence — then serves what it found, alongside the same probes re-run from inside the running guest for F6. It **must build successfully**: a context that fails to build proves nothing, because the probes never ran. |
| `builder-image/` | The builder microVM's rootfs and its `krill-build.sh` init. Built once per host; see its README for the open kernel question. |

## Gotchas

1. **`results/` holds a real session cookie** during a run. It is gitignored;
   discard it when the run is over.
2. **Nothing here restarts krilld.** It runs under `Restart=always`; a `pkill`
   yields two daemons fighting over ports, taps and the data dir (the M1
   lesson, fixed in commit 9bd13c6).
3. **F2 step 3 deletes the doorman's database.** That is the gate, and it is
   safe *because* the revocation log lives in the object store — but check
   `00-setup.sh` reported `revoke_durable: true` first, or you are deleting
   state with nothing behind it.
4. **The gate suites still send `Host: <app>.krill.local` to krilld's router**
   and that remains correct: the router has never validated the suffix, and
   `--base-host` is cosmetic. The *doorman* pins the suffix, which is what
   F3 step 4 tests.
5. **`krill share` talks to the doorman, `krill deploy` talks to krilld.** One
   binary, two services (decision #10b). If a sharing verb hangs, check that
   the tunnel forwards 9092.
