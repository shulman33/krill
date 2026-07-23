// Package rootfs manages per-app disk images.
//
// Layout under <dataDir>/apps/<name>/:
//
//	golden.ext4  — immutable master image, written once at registration
//	disk.ext4    — the app's live disk: created from golden on first boot,
//	               then persistent for the app's lifetime (its data lives here
//	               until M3 gives us a real data plane)
//	snap/        — vmstate + mem written by Firecracker at freeze
//	serial.log   — VMM/serial output for debugging
//
// THE rule (wake-path-benchmark.md §3.2): a memory snapshot embeds
// filesystem state that must match the disk byte-for-byte. disk.ext4 may
// only be touched by (a) the guest via virtio while running, or (b) a copy
// from golden — and (b) must invalidate any snapshot first. The lifecycle
// package enforces the invalidation ordering; this package only moves bytes.
package rootfs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type Manager struct {
	dataDir string
}

func NewManager(dataDir string) *Manager { return &Manager{dataDir: dataDir} }

func (m *Manager) AppDir(name string) string {
	return filepath.Join(m.dataDir, "apps", name)
}
func (m *Manager) GoldenPath(name string) string { return filepath.Join(m.AppDir(name), "golden.ext4") }
func (m *Manager) DiskPath(name string) string   { return filepath.Join(m.AppDir(name), "disk.ext4") }
func (m *Manager) SnapDir(name string) string    { return filepath.Join(m.AppDir(name), "snap") }
func (m *Manager) VMStatePath(name string) string {
	return filepath.Join(m.SnapDir(name), "vmstate")
}
func (m *Manager) MemPath(name string) string    { return filepath.Join(m.SnapDir(name), "mem") }
func (m *Manager) SockPath(name string) string   { return filepath.Join(m.AppDir(name), "fc.sock") }
func (m *Manager) SerialLog(name string) string  { return filepath.Join(m.AppDir(name), "serial.log") }

// EnsureDirs creates the app's directory tree; idempotent, run on every
// daemon start (taps and dirs are cheap; golden images are not re-copied).
func (m *Manager) EnsureDirs(name string) error {
	return os.MkdirAll(m.SnapDir(name), 0o755)
}

// Register creates the app directory and installs the golden image from
// srcPath. The golden is a private copy: the caller's file is theirs to
// delete.
func (m *Manager) Register(name, srcPath string) error {
	if err := m.EnsureDirs(name); err != nil {
		return err
	}
	return copySparse(srcPath, m.GoldenPath(name))
}

// EnsureDisk guarantees disk.ext4 exists, copying from golden only if the
// app has never booted. It never overwrites an existing disk — that would
// destroy app data.
func (m *Manager) EnsureDisk(name string) error {
	if _, err := os.Stat(m.DiskPath(name)); err == nil {
		return nil
	}
	return copySparse(m.GoldenPath(name), m.DiskPath(name))
}

// DropSnapshot removes snapshot files. Callers must have already marked the
// snapshot invalid in the registry — files disappear after the fact, never
// before.
func (m *Manager) DropSnapshot(name string) error {
	for _, p := range []string{m.VMStatePath(name), m.MemPath(name)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// SnapshotExists reports whether both snapshot files are present.
func (m *Manager) SnapshotExists(name string) bool {
	for _, p := range []string{m.VMStatePath(name), m.MemPath(name)} {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// PunchHoles releases the zero pages the balloon reclaimed from the memory
// snapshot (lib.sh ran `fallocate -d`). Best-effort: a fully materialized
// mem file is correct, just bigger.
func (m *Manager) PunchHoles(name string) {
	if runtime.GOOS == "linux" {
		_ = exec.Command("fallocate", "-d", m.MemPath(name)).Run()
	}
}

// Purge removes everything the app owns on disk.
func (m *Manager) Purge(name string) error {
	return os.RemoveAll(m.AppDir(name))
}

// copySparse copies src to dst preserving holes where the platform allows.
// On Linux, GNU cp's --sparse=always both preserves existing holes and
// re-sparsifies zero blocks — exactly what lib.sh:fresh_rootfs did.
func copySparse(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source image: %w", err)
	}
	if runtime.GOOS == "linux" {
		out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput()
		if err != nil {
			return fmt.Errorf("cp --sparse %s -> %s: %w: %s", src, dst, err, out)
		}
		return nil
	}
	// Non-Linux (dev machines, tests): plain copy.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
