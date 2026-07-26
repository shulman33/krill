package regbackup

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/registry"
)

func newFixture(t *testing.T, keep int, interval time.Duration) (*Manager, objstore.Store, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Create(registry.App{
		Name: "guestbook", VCPUs: 1, MemMiB: 128, GuestPort: 8080, State: "COLD",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		if _, err := reg.MintEpoch("guestbook"); err != nil {
			t.Fatal(err)
		}
	}
	store := objstore.NewMem()
	now := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	m := New(Config{
		Store: store, Source: reg, WorkDir: filepath.Join(dir, "backup"),
		CellGen: 3, Interval: interval, Keep: keep,
		Now: func() time.Time { return now },
	})
	return m, store, &now
}

func TestRunOnceShipsARestorableSnapshot(t *testing.T) {
	ctx := context.Background()
	m, store, _ := newFixture(t, 14, 24*time.Hour)

	info, err := m.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if info.Key != "_control/registry/20260726T180000Z.db.gz" {
		t.Errorf("key = %s", info.Key)
	}
	if info.Apps != 1 || info.MaxEpoch != 7 {
		t.Errorf("info = %+v, want 1 app / max epoch 7", info)
	}
	// E1: restoring this snapshot must be accompanied by a cell-gen bump, and
	// the backup itself has to say so — nobody remembers at 3am.
	if info.CellGen != 3 || info.RestoreCellGen != 4 {
		t.Errorf("cell_gen = %d, restore_cell_gen = %d; want 3 and 4", info.CellGen, info.RestoreCellGen)
	}
	if info.Bytes == 0 || info.DBBytes == 0 || len(info.SHA256) != 64 {
		t.Errorf("info = %+v, want sizes and a sha256", info)
	}

	// The shipped object must be a real, openable registry — this is the
	// whole promise. Decompress, open, and read the epoch counter back.
	gz, _, err := store.Get(ctx, info.Key)
	if err != nil {
		t.Fatalf("Get %s: %v", info.Key, err)
	}
	zr, err := gzip.NewReader(strings.NewReader(string(gz)))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restored, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reg2, err := registry.Open(restored)
	if err != nil {
		t.Fatalf("opening the restored backup: %v", err)
	}
	defer reg2.Close()
	st, err := reg2.Stats()
	if err != nil || st.Apps != 1 || st.MaxEpoch != 7 {
		t.Fatalf("restored stats = %+v (%v), want 1 app / max epoch 7", st, err)
	}

	// The sidecar is readable on its own, and List surfaces it.
	if _, _, err := store.Get(ctx, "_control/registry/20260726T180000Z.json"); err != nil {
		t.Errorf("sidecar missing: %v", err)
	}
	list, err := m.List(ctx)
	if err != nil || len(list) != 1 || list[0].MaxEpoch != 7 {
		t.Fatalf("List = %+v (%v)", list, err)
	}
}

func TestRetentionKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	m, store, now := newFixture(t, 3, 24*time.Hour)
	var keys []string
	for i := 0; i < 5; i++ {
		info, err := m.RunOnce(ctx)
		if err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
		keys = append(keys, info.Key)
		*now = now.Add(24 * time.Hour)
	}
	all, err := store.List(ctx, Prefix)
	if err != nil {
		t.Fatal(err)
	}
	// 3 snapshots + 3 sidecars.
	if len(all) != 6 {
		t.Fatalf("store holds %d objects (%v), want 6", len(all), all)
	}
	for _, gone := range keys[:2] {
		if _, _, err := store.Get(ctx, gone); err == nil {
			t.Errorf("%s should have been pruned", gone)
		}
		if _, _, err := store.Get(ctx, sidecarOf(gone)); err == nil {
			t.Errorf("%s sidecar should have been pruned", gone)
		}
	}
	list, err := m.List(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("List = %d entries (%v), want 3", len(list), err)
	}
	// Newest first.
	if list[0].Key != keys[4] {
		t.Errorf("List[0] = %s, want the newest %s", list[0].Key, keys[4])
	}
}

// Due is the crash-loop guard: the schedule is driven by the age of the
// newest backup, not by how long this process has been up.
func TestDueIsDrivenByBackupAge(t *testing.T) {
	ctx := context.Background()
	m, _, now := newFixture(t, 14, 24*time.Hour)

	if due, err := m.Due(ctx); err != nil || !due {
		t.Fatalf("Due with no backups = %v (%v), want true", due, err)
	}
	if _, err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// A daemon restarting five times an hour must not ship five backups.
	if due, err := m.Due(ctx); err != nil || due {
		t.Fatalf("Due right after a backup = %v (%v), want false", due, err)
	}
	*now = now.Add(23 * time.Hour)
	if due, _ := m.Due(ctx); due {
		t.Error("Due at 23h = true, want false")
	}
	*now = now.Add(2 * time.Hour)
	if due, _ := m.Due(ctx); !due {
		t.Error("Due at 25h = false, want true")
	}
}

func TestIntervalZeroDisables(t *testing.T) {
	m, _, _ := newFixture(t, 14, 0)
	if due, err := m.Due(context.Background()); err != nil || due {
		t.Fatalf("Due with Interval=0 = %v (%v), want false", due, err)
	}
	// Run must return immediately rather than block forever.
	done := make(chan struct{})
	go func() { m.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with Interval=0")
	}
}
