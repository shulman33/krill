#!/bin/sh
# PID 1 inside a Krill builder microVM.
#
# Contract with internal/buildvm on the host:
#   /dev/vdb  the submitted build context, read-only
#   /dev/vdc  an empty ext4 filesystem; whatever lands here becomes the app's
#             golden image, so this script populates it and nothing has to be
#             copied back out through the host's ext4 parser
#   console   everything printed here is the build log; the LAST line matching
#             KRILL-BUILD-RESULT: is the structured verdict
#   cmdline   krill_app=<name> krill_out_mb=<n> krill_ip=... krill_gw=...
#
# The VM powers itself off when finished. If it does not, the host kills it on
# a timeout measured by the HOST's clock — nothing in here is trusted to bound
# anything, which is the same reason PT-3 forbids guest-side lease timers.
set -u

STAGE="startup"

emit() { # $1 = true|false, $2 = extra JSON fields (leading comma) or empty
  printf 'KRILL-BUILD-RESULT: {"ok":%s,"stage":"%s"%s}\n' "$1" "$STAGE" "${2:-}"
}

off() {
  sync
  poweroff -f 2>/dev/null || reboot -f 2>/dev/null
  # If the kernel has neither, let the host's timeout do it.
  while true; do sleep 60; done
}

fail() { # keep the message on one line: the host parses exactly one
  msg=$(printf '%s' "$1" | tr '\n\r\t' '   ' | sed 's/\\/\\\\/g; s/"/\\"/g' | cut -c1-1500)
  emit false ",\"error\":\"$msg\""
  off
}

mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs dev /dev 2>/dev/null
mount -t tmpfs tmpfs /tmp 2>/dev/null
mount -t tmpfs tmpfs /run 2>/dev/null
# BuildKit's workers want cgroup v2 for the containers they run.
mkdir -p /sys/fs/cgroup && mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null

APP=$(sed -n 's/.*krill_app=\([^ ]*\).*/\1/p' /proc/cmdline)
IP=$(sed -n 's/.*krill_ip=\([^ ]*\).*/\1/p' /proc/cmdline)
GW=$(sed -n 's/.*krill_gw=\([^ ]*\).*/\1/p' /proc/cmdline)
[ -n "$APP" ] || APP=app

# eth0 is usually already up via kernel ip= autoconfig; this is the fallback.
if command -v ip >/dev/null 2>&1 && [ -n "$IP" ]; then
  ip addr add "$IP" dev eth0 2>/dev/null
  ip link set eth0 up 2>/dev/null
  ip route add default via "$GW" 2>/dev/null
fi
# What is actually reachable is decided by the host's nftables baseline (F6):
# a container registry and a resolver. Naming resolvers here does not grant
# anything.
printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf

echo "krill-build: app=$APP starting"

STAGE="mounting disks"
mkdir -p /ctx /out
mount -o ro /dev/vdb /ctx || fail "cannot mount the build context"
mount /dev/vdc /out       || fail "cannot mount the output disk"
[ -f /ctx/Dockerfile ] || fail "the build context has no Dockerfile"

STAGE="starting buildkitd"
buildkitd --oci-worker-snapshotter=native --root /tmp/buildkit >/tmp/buildkitd.log 2>&1 &
BUILDKITD=$!
i=0
until buildctl debug workers >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 150 ] && fail "buildkitd did not become ready: $(tail -c 800 /tmp/buildkitd.log)"
  kill -0 "$BUILDKITD" 2>/dev/null || fail "buildkitd exited: $(tail -c 800 /tmp/buildkitd.log)"
  sleep 0.2
done

# THIS is the line the whole milestone is about: someone else's Dockerfile
# executing inside a microVM with two disks and a registry-only network,
# instead of on the host as root against a root daemon.
#
# Two outputs from one cached build: `local` gives the flattened filesystem
# (BuildKit resolves whiteouts, so nothing here has to), and `oci` gives the
# image config — the ENTRYPOINT/CMD/ENV/EXPOSE an app needs to actually run.
STAGE="docker build"
buildctl build \
  --frontend dockerfile.v0 \
  --local context=/ctx \
  --local dockerfile=/ctx \
  --output type=local,dest=/rootfs \
  >/tmp/build.log 2>&1 || fail "build failed: $(tail -c 2000 /tmp/build.log)"
cat /tmp/build.log

STAGE="image config"
buildctl build \
  --frontend dockerfile.v0 \
  --local context=/ctx \
  --local dockerfile=/ctx \
  --output type=oci,dest=/tmp/img.tar \
  >/tmp/oci.log 2>&1 || fail "exporting the image config failed: $(tail -c 1200 /tmp/oci.log)"

mkdir -p /tmp/oci
tar -xf /tmp/img.tar -C /tmp/oci || fail "the OCI export is not a tar"
blob() { printf '/tmp/oci/blobs/%s' "$(printf '%s' "$1" | tr ':' '/')"; }

DESC=$(jq -r '.manifests[0].digest' /tmp/oci/index.json 2>/dev/null)
[ -n "$DESC" ] && [ "$DESC" != "null" ] || fail "no manifest in the OCI export"
MEDIA=$(jq -r '.mediaType // ""' "$(blob "$DESC")" 2>/dev/null)
case "$MEDIA" in
  *image.index*|*manifest.list*)
    # A multi-platform result: descend one level to this platform's manifest.
    DESC=$(jq -r '.manifests[0].digest' "$(blob "$DESC")")
    ;;
esac
CFG=$(jq -r '.config.digest' "$(blob "$DESC")" 2>/dev/null)
[ -n "$CFG" ] && [ "$CFG" != "null" ] || fail "no image config in the OCI export"
CONFIG=$(jq -c '.config // {}' "$(blob "$CFG")")

PORT=$(printf '%s' "$CONFIG" | jq -r '(.ExposedPorts // {}) | keys[]?' \
        | grep -E '^[0-9]+(/tcp)?$' | sed 's|/tcp$||' | sort -n | head -1)
[ -n "${PORT:-}" ] || PORT=0

STAGE="init"
[ -e /rootfs/bin/sh ] || [ -e /rootfs/usr/bin/sh ] \
  || fail "the built image has no /bin/sh; the generated init needs a shell (use debian-slim, alpine, ...)"

# The generated init, byte-for-byte equivalent in behavior to
# internal/builder.InitScript: kernel filesystems, the network contract, the
# /data mount, the doorman's identity key, then the image's own command.
{
  printf '#!/bin/sh\n'
  printf '# Generated inside a Krill builder microVM for app %s. PID 1.\n' "$APP"
  cat <<'INIT'
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs dev /dev 2>/dev/null
IP="$(sed -n 's/.*krill_ip=\([^ ]*\).*/\1/p' /proc/cmdline)"
GW="$(sed -n 's/.*krill_gw=\([^ ]*\).*/\1/p' /proc/cmdline)"
if command -v ip >/dev/null 2>&1 && [ -n "$IP" ]; then
  ip addr add "$IP" dev eth0 2>/dev/null
  ip link set eth0 up 2>/dev/null
  ip route add default via "$GW" 2>/dev/null
fi
if [ -b /dev/vdb ]; then
  mkdir -p /data
  mount /dev/vdb /data 2>/dev/null || echo "krill-init: mounting /data failed" >&2
fi
KRILL_IDENTITY_PUBKEY="$(sed -n 's/.*krill_idkey=\([^ ]*\).*/\1/p' /proc/cmdline)"
export KRILL_IDENTITY_PUBKEY
export HOME="${HOME:-/root}"
INIT
  printf '%s' "$CONFIG" | jq -r '(.Env // [])[]' | while IFS= read -r e; do
    k=${e%%=*}
    v=${e#*=}
    case "$k" in
      [A-Za-z_]*) printf 'export %s=%s\n' "$k" "$(printf '%s' "$v" | sed "s/'/'\\\\''/g; s/^/'/; s/\$/'/")" ;;
    esac
  done
  wd=$(printf '%s' "$CONFIG" | jq -r '.WorkingDir // "/"')
  [ -n "$wd" ] || wd=/
  printf 'cd %s\n' "$(printf '%s' "$wd" | sed "s/'/'\\\\''/g; s/^/'/; s/\$/'/")"
  argv=$(printf '%s' "$CONFIG" | jq -r '((.Entrypoint // []) + (.Cmd // [])) | @sh')
  [ -n "$argv" ] || fail "the image has no ENTRYPOINT or CMD — add a CMD to the Dockerfile"
  printf 'exec %s\n' "$argv"
} > /rootfs/krill-init.sh
chmod 0755 /rootfs/krill-init.sh

STAGE="populating the output disk"
# The output disk IS the golden image. tar rather than cp so ownership, modes
# and symlinks survive exactly as the image had them (--numeric-owner for the
# same reason internal/builder uses it: container uids must land as-is).
(cd /rootfs && tar -cf - --numeric-owner .) | (cd /out && tar -xf - --numeric-owner) \
  || fail "copying the built filesystem to the output disk failed"

sync
umount /out 2>/dev/null || fail "the output disk did not unmount cleanly"
STAGE="done"
emit true ",\"guest_port\":${PORT}"
off
