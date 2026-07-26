package registry

import (
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func mkApp(name string) App {
	return App{Name: name, VCPUs: 1, MemMiB: 512, GuestPort: 8000, State: "COLD"}
}

func TestCreateGetRoundTrip(t *testing.T) {
	r := open(t)
	created, err := r.Create(mkApp("counter"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("counter")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "counter" || got.VCPUs != 1 || got.MemMiB != 512 ||
		got.GuestPort != 8000 || got.State != "COLD" || got.SnapshotValid {
		t.Fatalf("round trip mangled the app: %+v", got)
	}
	if got.SubnetIdx != created.SubnetIdx {
		t.Fatalf("subnet mismatch: %d vs %d", got.SubnetIdx, created.SubnetIdx)
	}
}

func TestSubnetAllocationSmallestFree(t *testing.T) {
	r := open(t)
	for i, name := range []string{"a", "b", "c"} {
		app, err := r.Create(mkApp(name))
		if err != nil {
			t.Fatal(err)
		}
		if app.SubnetIdx != i {
			t.Fatalf("app %s got subnet %d, want %d", name, app.SubnetIdx, i)
		}
	}
	// Freeing the middle slot must make it the next allocation.
	if err := r.Delete("b"); err != nil {
		t.Fatal(err)
	}
	app, err := r.Create(mkApp("d"))
	if err != nil {
		t.Fatal(err)
	}
	if app.SubnetIdx != 1 {
		t.Fatalf("app d got subnet %d, want the freed 1", app.SubnetIdx)
	}
}

func TestInvalidInputsRejected(t *testing.T) {
	r := open(t)
	bad := []App{
		mkApp("Has-Caps"),
		mkApp("-leading-dash"),
		mkApp(""),
		mkApp("way-too-long-name-for-a-dns-label-really"),
		{Name: "ok", VCPUs: 0, MemMiB: 512, GuestPort: 8000},
		{Name: "ok", VCPUs: 1, MemMiB: 16, GuestPort: 8000},
		{Name: "ok", VCPUs: 1, MemMiB: 512, GuestPort: 0},
	}
	for _, a := range bad {
		if _, err := r.Create(a); err == nil {
			t.Errorf("Create(%+v) should have failed", a)
		}
	}
	if _, err := r.Create(mkApp("counter")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(mkApp("counter")); err == nil {
		t.Error("duplicate name should have failed")
	}
}

func TestStateAndSnapshotUpdates(t *testing.T) {
	r := open(t)
	if _, err := r.Create(mkApp("counter")); err != nil {
		t.Fatal(err)
	}
	if err := r.SetState("counter", "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSnapshotValid("counter", true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("counter")
	if got.State != "ACTIVE" || !got.SnapshotValid {
		t.Fatalf("updates lost: %+v", got)
	}
	if err := r.SetState("ghost", "ACTIVE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "krill.db")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(mkApp("survivor")); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSnapshotValid("survivor", true); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got, err := r2.Get("survivor")
	if err != nil {
		t.Fatal(err)
	}
	if !got.SnapshotValid {
		t.Fatal("snapshot validity did not survive reopen")
	}
}

func TestDelete(t *testing.T) {
	r := open(t)
	if _, err := r.Create(mkApp("doomed")); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := r.Delete("doomed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

// TestBackupTo: the snapshot must be a valid, complete registry — including
// rows that are still only in the WAL (synchronous=FULL commits them, but
// they have not been checkpointed into the main db file).
func TestBackupTo(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(filepath.Join(dir, "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, n := range []string{"guestbook", "ledger"} {
		if _, err := r.Create(App{Name: n, VCPUs: 1, MemMiB: 128, GuestPort: 8080, State: "COLD"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.MintEpoch("guestbook"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MintEpoch("guestbook"); err != nil {
		t.Fatal(err)
	}
	st, err := r.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Apps != 2 || st.MaxEpoch != 2 {
		t.Fatalf("Stats() = %+v, want 2 apps / max epoch 2", st)
	}

	dst := filepath.Join(dir, "backup.db")
	if err := r.BackupTo(dst); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if err := r.BackupTo(dst); err == nil {
		t.Error("BackupTo overwrote an existing file")
	}

	// The restored copy must be openable and carry every fact, epochs included.
	r2, err := Open(dst)
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer r2.Close()
	apps, err := r2.List()
	if err != nil || len(apps) != 2 {
		t.Fatalf("restored List() = %d apps, %v", len(apps), err)
	}
	// A restored mint re-issues epoch 3 — which is exactly why E1 demands a
	// --cell-gen bump after a restore.
	if c, err := r2.MintEpoch("guestbook"); err != nil || c != 3 {
		t.Fatalf("restored MintEpoch = %d, %v; want the counter to survive", c, err)
	}
}
