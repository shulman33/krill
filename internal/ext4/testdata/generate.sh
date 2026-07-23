#!/bin/sh
# Generates the ext4 test fixtures for internal/ext4 using a REAL Linux
# mkfs.ext4 + kernel, via a privileged docker container (loop mounts).
#
#   ./generate.sh          # writes img-*.ext4.gz + manifest-*.txt here
#
# Three fixtures:
#   img-clean-4k:  4096-byte blocks, cleanly unmounted (checkpointed journal)
#   img-live-4k:   copied WHILE MOUNTED after fsync'd writes — the journal
#                  may hold committed-but-not-checkpointed metadata, which is
#                  exactly the state a host-side tailer reads all day
#   img-clean-1k:  1024-byte blocks (first_data_block=1 layout variant)
#
# Each manifest lists every file as: path size sha256, computed through the
# kernel's own view of the mounted fs — the ground truth the Go reader must
# reproduce, journal replay included.
set -eu
cd "$(dirname "$0")"

docker run -i --privileged --rm -v "$PWD":/out alpine:3.20 sh -eu <<'EOF'
apk add --no-cache e2fsprogs coreutils >/dev/null

build_tree() {
  mnt="$1"
  mkdir -p "$mnt/dir/sub"
  echo "hello, krill" > "$mnt/hello.txt"
  # Interleaved appends fragment the files into many extents, forcing
  # depth>0 extent trees.
  i=1
  while [ "$i" -le 80 ]; do
    seq "$((i * 1000))" "$((i * 1000 + 900))" >> "$mnt/frag-a.bin"
    seq "$((i * 2000))" "$((i * 2000 + 900))" >> "$mnt/frag-b.bin"
    seq "$((i * 3000))" "$((i * 3000 + 900))" >> "$mnt/dir/frag-c.bin"
    i=$((i + 1))
  done
  # A hole at the start and a trailing hole past the last write.
  dd if=/dev/urandom of="$mnt/sparse.bin" bs=4096 count=2 seek=10 conv=notrunc status=none
  truncate -s 100000 "$mnt/sparse.bin"
  # SQLite-shaped names, the actual production payload.
  seq 1 700 > "$mnt/dir/app.db"
  head -c 33 /dev/urandom > "$mnt/dir/app.db-wal"
  sync
}

manifest() {
  (cd "$1" && find . -type f | sort | while read -r f; do
    printf '%s %s %s\n' "$f" "$(stat -c %s "$f")" "$(sha256sum "$f" | cut -d' ' -f1)"
  done)
}

# --- clean + live, 4096-byte blocks ---
dd if=/dev/zero of=/img bs=1M count=16 status=none
mkfs.ext4 -q -F -b 4096 /img
mkdir -p /mnt2 && mount -o loop,commit=1 /img /mnt2
build_tree /mnt2
# Post-sync writes flushed with fsync ONLY: their metadata (inode sizes,
# extents, dirents) is committed to the jbd2 journal but NOT yet written
# back to final locations. Copying the image now captures a state where
# journal replay is load-bearing — a reader that skips it sees stale
# metadata for these files. (sync(2) would checkpoint everything and make
# the live fixture vacuously clean; do not add one before the cp.)
seq 5000 5900 | dd of=/mnt2/dir/app.db oflag=append conv=notrunc,fsync status=none
seq 1 400 | dd of=/mnt2/dir/app.db-wal conv=fsync status=none
echo "written after the last sync" | dd of=/mnt2/dir/late.txt conv=fsync status=none
manifest /mnt2 > /out/manifest-4k.txt
cp /img /out/img-live-4k.ext4       # journal dirty: the point
dumpe2fs -h /img 2>/dev/null | grep -i 'features' || true
umount /mnt2
cp /img /out/img-clean-4k.ext4

# --- clean, 1024-byte blocks ---
dd if=/dev/zero of=/img1 bs=1M count=8 status=none
mkfs.ext4 -q -F -b 1024 /img1
mount -o loop /img1 /mnt2
build_tree /mnt2
manifest /mnt2 > /out/manifest-1k.txt
umount /mnt2
cp /img1 /out/img-clean-1k.ext4

gzip -9 -f /out/img-live-4k.ext4 /out/img-clean-4k.ext4 /out/img-clean-1k.ext4
chown -R "$(stat -c %u:%g /out)" /out 2>/dev/null || true
EOF

ls -la img-*.gz manifest-*.txt
