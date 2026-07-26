#!/usr/bin/env bash
# Build the builder VM's golden image. Run once per host, as root, on the box.
#
#   sudo m4-gates/builder-image/build.sh /srv/fc/builder.ext4
#
# This is the one build that still happens on the host with docker, and that
# is correct: this image is operator-controlled input, exactly like the guest
# kernel. Everything a *user* submits builds inside the VM this produces.
set -euo pipefail

OUT="${1:-/srv/fc/builder.ext4}"
SIZE_MB="${SIZE_MB:-3072}"
TAG="krill-builder"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

command -v docker >/dev/null || { echo "need docker on the host to build this image once" >&2; exit 1; }
command -v mkfs.ext4 >/dev/null || { echo "need e2fsprogs" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { echo "run as root: mkfs.ext4 -d must preserve ownership" >&2; exit 1; }

echo "==> docker build $TAG"
docker build -t "$TAG" "$HERE"

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
CID="$(docker create "$TAG")"
echo "==> exporting the filesystem"
docker export "$CID" | tar -x --numeric-owner -C "$STAGE"
docker rm "$CID" >/dev/null

# krill-build.sh is init; the Dockerfile already placed it, but copy again so
# an edit to the script does not need a docker cache bust.
install -m 0755 "$HERE/krill-build.sh" "$STAGE/krill-build.sh"

echo "==> mkfs.ext4 -> $OUT (${SIZE_MB} MB)"
mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F -d "$STAGE" "$OUT"

echo "==> done"
ls -lh "$OUT"
cat <<EOF

Point krilld at it:

  --build-vm-image  $OUT
  --build-vm-kernel <a kernel with cgroup v2, namespaces and overlayfs>

⚠ The kernel matters. See README.md in this directory: the Firecracker CI
  kernel is built for tiny app guests and is not guaranteed to carry what a
  container build needs. If buildkitd fails to start inside the VM, that is
  the first thing to check — the failure arrives on the build log with the
  buildkitd tail attached.
EOF
