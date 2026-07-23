package rootfs

import (
	"os"
	"testing"
)

func TestRegisterAndDiskLifecycle(t *testing.T) {
	m := NewManager(t.TempDir())
	src := t.TempDir() + "/golden.ext4"
	if err := os.WriteFile(src, []byte("golden-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Register("app", src); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(m.GoldenPath("app")); string(got) != "golden-bytes" {
		t.Fatalf("golden = %q", got)
	}

	// First EnsureDisk copies golden.
	if err := m.EnsureDisk("app"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(m.DiskPath("app")); string(got) != "golden-bytes" {
		t.Fatalf("disk = %q", got)
	}

	// The guest "writes" to its disk; EnsureDisk must NEVER overwrite it.
	if err := os.WriteFile(m.DiskPath("app"), []byte("app-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureDisk("app"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(m.DiskPath("app")); string(got) != "app-data" {
		t.Fatal("EnsureDisk overwrote live app data — this destroys apps")
	}
}

func TestSnapshotFiles(t *testing.T) {
	m := NewManager(t.TempDir())
	src := t.TempDir() + "/golden.ext4"
	os.WriteFile(src, []byte("x"), 0o644)
	if err := m.Register("app", src); err != nil {
		t.Fatal(err)
	}

	if m.SnapshotExists("app") {
		t.Fatal("no snapshot written yet")
	}
	os.WriteFile(m.VMStatePath("app"), []byte("v"), 0o644)
	if m.SnapshotExists("app") {
		t.Fatal("vmstate alone is not a snapshot")
	}
	os.WriteFile(m.MemPath("app"), []byte("m"), 0o644)
	if !m.SnapshotExists("app") {
		t.Fatal("both files present, should exist")
	}
	if err := m.DropSnapshot("app"); err != nil {
		t.Fatal(err)
	}
	if m.SnapshotExists("app") {
		t.Fatal("snapshot files should be gone")
	}
	if err := m.DropSnapshot("app"); err != nil {
		t.Fatal("dropping a missing snapshot must be a no-op, got:", err)
	}
}

func TestRegisterMissingSource(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.Register("app", "/nonexistent/image.ext4"); err == nil {
		t.Fatal("registering a missing golden should fail")
	}
}

func TestPurge(t *testing.T) {
	m := NewManager(t.TempDir())
	src := t.TempDir() + "/golden.ext4"
	os.WriteFile(src, []byte("x"), 0o644)
	if err := m.Register("app", src); err != nil {
		t.Fatal(err)
	}
	if err := m.Purge("app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.AppDir("app")); !os.IsNotExist(err) {
		t.Fatal("app dir should be gone")
	}
}
