# The builder microVM (F5)

`docker build` of a context somebody else submitted must not run on the host.
It executes their instructions, as root, outside any microVM, against a root
daemon — today-risk #1 in the ROADMAP's posture map. M4 moves it inside the
primitive this platform already sells.

```
context dir ──mkfs.ext4──> ctx.ext4 (ro) ─┐
                                          ├─> throwaway microVM ─> golden.ext4
empty formatted golden.ext4 ──────────────┘        buildkitd runs here
```

Orchestration is `internal/buildvm`; this directory is the guest half.

## Build it once per host

```bash
sudo m4-gates/builder-image/build.sh /srv/fc/builder.ext4
```

Then in krilld's unit:

```
--build-vm-image  /srv/fc/builder.ext4
--build-vm-kernel /srv/fc/vmlinux-builder
--build-isolation untrusted        # or `all` to isolate Sam's deploys too
```

With no `--build-vm-image`, krilld **refuses** deploys that arrive through the
doorman rather than building them on the host. That refusal is deliberate: a
fallback to the host path is the exact vulnerability this closes, so there
isn't one.

## ⚠ The kernel is the open hardware question

The Firecracker CI kernel (`vmlinux-6.1.155`, what app guests boot) is built
for minimal app guests. A container build needs more of Linux than that:

- **cgroup v2** — BuildKit's workers put each `RUN` in a cgroup.
- **user + mount + pid namespaces** — runc's whole model.
- **`CONFIG_OVERLAY_FS`** — *not* needed here, because the build script runs
  `buildkitd --oci-worker-snapshotter=native`. Native copies instead of
  layering: slower, and it works on kernels that have no overlayfs. That
  choice exists precisely so the kernel question has one fewer answer to get
  right.

If `buildkitd` does not come up inside the VM, the failure arrives in the
deploy response with the last 800 bytes of `buildkitd.log` attached — that is
the first place to look, and it will name the missing capability.

Getting a suitable kernel, in increasing order of effort:

1. Try the CI kernel first. It may be enough; nobody has measured.
2. Use the distro kernel, **uncompressed** — Firecracker boots `vmlinux`, not
   `vmlinuz`. `/usr/src/linux-headers-*/vmlinux` is not it either; extract
   with `extract-vmlinux` from the kernel source tree.
3. Build one from the Firecracker kernel config plus `CONFIG_CGROUPS`,
   `CONFIG_USER_NS`, `CONFIG_OVERLAY_FS`, and the `CONFIG_NAMESPACES` family.

Whatever it ends up being, it is operator input like the app kernel and the
version-lock rule applies to it too: builder VMs take no snapshots, so a
kernel change costs nothing except a re-test.

## What the guest half guarantees

`krill-build.sh` is PID 1 in the builder VM. Its contract with the host:

| Where | What |
|---|---|
| `/dev/vdb` | the submitted context, **read-only** |
| `/dev/vdc` | an empty ext4 filesystem that becomes the app's golden image |
| console | the build log; the last `KRILL-BUILD-RESULT:` line is the verdict |
| cmdline | `krill_app=`, `krill_out_mb=`, `krill_ip=`, `krill_gw=` |

The VM powers itself off when done. If it does not — a hung build, a fork
bomb, a Dockerfile that sleeps forever — the host kills it on a timeout
measured by **the host's** clock. Nothing inside is trusted to bound anything,
for the same reason PT-3 forbids guest-side lease timers.

It also generates the app's `/krill-init.sh` from the image's own OCI config
(ENV, WorkingDir, Entrypoint, Cmd, the first EXPOSEd port), which is the same
job `internal/builder.InitScript` does on the host path. The two must stay
behavior-identical: an app should not be able to tell which builder produced
it.

## What the isolation rests on

- The VM sees **two disks and nothing else**. No host filesystem, no
  `/etc/krill/gcs.json`, no registry database, no other app's `data.ext4`.
- Its tap is `krillb*`, which the F6 nftables baseline treats as the one guest
  class with any egress at all: a container registry and a resolver. It cannot
  reach `127.0.0.1:9091` because it cannot reach the host at all.
- It is destroyed on success, on failure, on timeout and on a hang. The
  teardown is a `defer`, and the tap goes with it — a permitted egress path
  attached to nothing is not left lying around.

`m4-gates/examples/hostile/` is the context that tries all of the above.
