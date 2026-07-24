package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/dataplane"
	"github.com/shulman33/krill/internal/registry"
)

// fakeDataPlane records the supervisor's hook calls and lets tests script
// their outcomes.
type fakeDataPlane struct {
	mu       sync.Mutex
	calls    []string
	rebuild  bool  // PrepareWake reports the disk was rebuilt
	failPrep error // PrepareWake fails (e.g. fenced at wake)
	kills    map[string]func(error)
}

func (f *fakeDataPlane) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeDataPlane) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDataPlane) PrepareWake(ctx context.Context, app registry.App) (bool, error) {
	f.record("prepare:" + app.Name)
	return f.rebuild, f.failPrep
}

func (f *fakeDataPlane) StartShipping(app registry.App, kill func(error)) error {
	f.record("start:" + app.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kills == nil {
		f.kills = map[string]func(error){}
	}
	f.kills[app.Name] = kill
	return nil
}

func (f *fakeDataPlane) StopShipping(ctx context.Context, app registry.App, final bool) error {
	if final {
		f.record("stop-final:" + app.Name)
	} else {
		f.record("stop:" + app.Name)
	}
	return nil
}

func (f *fakeDataPlane) Sync(ctx context.Context, name string) error {
	f.record("sync:" + name)
	return nil
}

func (f *fakeDataPlane) BranchRestore(ctx context.Context, app registry.App, atLSN uint64, atTime time.Time) (string, uint64, error) {
	f.record("branch:" + app.Name)
	return "s1", atLSN, nil
}

func (f *fakeDataPlane) StreamStatus(ctx context.Context, name, stream string) (*dataplane.Manifest, error) {
	return &dataplane.Manifest{App: name, Stream: stream}, nil
}

func (f *fakeDataPlane) PurgeApp(ctx context.Context, name string) error {
	f.record("purge:" + name)
	return nil
}

func TestDataPlaneWakeFreezeOrdering(t *testing.T) {
	be := newFakeBackend()
	dp := &fakeDataPlane{}
	sup, _ := newSupervisor(t, be, Config{})
	sup.SetDataPlane(dp)
	register(t, sup, "counter")

	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := sup.Freeze("counter"); err != nil {
		t.Fatal(err)
	}
	got := dp.callLog()
	want := []string{"prepare:counter", "start:counter", "stop-final:counter"}
	if len(got) != len(want) {
		t.Fatalf("hook calls: %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hook calls: %v, want %v", got, want)
		}
	}

	// Next wake mints again (restore path).
	_, release, err = sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if be.restores.Load() != 1 {
		t.Fatalf("expected a snapshot restore, got %d restores", be.restores.Load())
	}
}

// TestDataPlaneRebuildDemotesToColdBoot: PrepareWake reporting a rebuilt
// data disk must force a cold boot even when a memory snapshot exists —
// the snapshot no longer matches the disk.
func TestDataPlaneRebuildDemotesToColdBoot(t *testing.T) {
	be := newFakeBackend()
	dp := &fakeDataPlane{}
	sup, reg := newSupervisor(t, be, Config{})
	sup.SetDataPlane(dp)
	register(t, sup, "counter")

	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := sup.Freeze("counter"); err != nil {
		t.Fatal(err)
	}

	dp.rebuild = true
	_, release, err = sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if be.restores.Load() != 0 {
		t.Fatal("restored a snapshot against a rebuilt data disk")
	}
	if be.boots.Load() != 2 {
		t.Fatalf("boots = %d, want 2 (initial + demoted wake)", be.boots.Load())
	}
	meta, _ := reg.Get("counter")
	if meta.SnapshotValid {
		t.Fatal("snapshot still marked valid after a rebuild")
	}
}

// TestDataPlaneFencedWakeFails: a stale epoch at PrepareWake kills the wake
// before any guest instruction runs.
func TestDataPlaneFencedWakeFails(t *testing.T) {
	be := newFakeBackend()
	dp := &fakeDataPlane{failPrep: errors.New("fenced: wake epoch stale")}
	sup, _ := newSupervisor(t, be, Config{})
	sup.SetDataPlane(dp)
	register(t, sup, "counter")

	_, _, err := sup.Acquire(context.Background(), "counter")
	if err == nil {
		t.Fatal("fenced wake served anyway")
	}
	if be.boots.Load() != 0 {
		t.Fatal("guest booted despite a fenced wake")
	}
}

// TestZombieKill: the shipper's OnFenced callback must destroy the
// instance and demote the app; the next wake goes through PrepareWake
// again (fresh epoch).
func TestZombieKill(t *testing.T) {
	be := newFakeBackend()
	dp := &fakeDataPlane{}
	sup, reg := newSupervisor(t, be, Config{})
	sup.SetDataPlane(dp)
	register(t, sup, "counter")

	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()

	dp.mu.Lock()
	kill := dp.kills["counter"]
	dp.mu.Unlock()
	kill(errors.New("fenced by successor"))

	st, err := sup.Status("counter")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateCold {
		t.Fatalf("state after zombie kill = %s, want COLD", st.State)
	}
	meta, _ := reg.Get("counter")
	if meta.SnapshotValid {
		t.Fatal("snapshot valid after zombie kill")
	}
	found := false
	for _, c := range dp.callLog() {
		if c == "stop:counter" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shipper not stopped on zombie kill: %v", dp.callLog())
	}
}

// TestRestoreDataBranches: PITR through the supervisor quiesces, branches,
// and leaves the app COLD with its snapshot invalidated.
func TestRestoreDataBranches(t *testing.T) {
	be := newFakeBackend()
	dp := &fakeDataPlane{}
	sup, reg := newSupervisor(t, be, Config{})
	sup.SetDataPlane(dp)
	register(t, sup, "counter")

	// App is ACTIVE when the restore lands: the claim must kill it.
	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()

	stream, lsn, err := sup.RestoreData(context.Background(), "counter", 42, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if stream != "s1" || lsn != 42 {
		t.Fatalf("restore -> %s@%d, want s1@42", stream, lsn)
	}
	st, _ := sup.Status("counter")
	if st.State != StateCold {
		t.Fatalf("state after restore = %s, want COLD", st.State)
	}
	meta, _ := reg.Get("counter")
	if meta.SnapshotValid {
		t.Fatal("snapshot valid after restore")
	}
}

// TestDeletePurgesObjectStore: deletion reaches into the object store too.
func TestDeletePurgesObjectStore(t *testing.T) {
	be := newFakeBackend()
	dp := &fakeDataPlane{}
	sup, _ := newSupervisor(t, be, Config{})
	sup.SetDataPlane(dp)
	register(t, sup, "counter")

	if err := sup.Delete("counter"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range dp.callLog() {
		if c == "purge:counter" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no object-store purge on delete: %v", dp.callLog())
	}
}
