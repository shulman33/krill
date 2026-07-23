package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/registry"
)

// fakeBackend stands in for Firecracker: each "boot" is a real HTTP server
// on 127.0.0.1 so the supervisor's readiness probe runs for real.
type fakeBackend struct {
	mu        sync.Mutex
	addrs     map[string]string // app -> current listener addr
	snapshots map[string]bool   // app -> snapshot files "exist"

	boots     atomic.Int32
	restores  atomic.Int32
	snapCalls atomic.Int32

	failColdBoot bool
	failRestore  bool
	failSnapshot bool
	onColdBoot   func(app registry.App) // ordering-assertion hook
}

type fakeInstance struct {
	srv *http.Server
	ln  net.Listener
}

func (i *fakeInstance) Kill() {
	i.srv.Close()
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{addrs: map[string]string{}, snapshots: map[string]bool{}}
}

func (b *fakeBackend) spawn(app registry.App) (Instance, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", app.Name)
	})}
	go srv.Serve(ln)
	b.mu.Lock()
	b.addrs[app.Name] = ln.Addr().String()
	b.mu.Unlock()
	return &fakeInstance{srv: srv, ln: ln}, nil
}

func (b *fakeBackend) Install(app registry.App, goldenSrc string) error { return nil }
func (b *fakeBackend) Prepare(app registry.App) error                   { return nil }

func (b *fakeBackend) HasSnapshot(app registry.App) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshots[app.Name]
}

func (b *fakeBackend) ColdBoot(ctx context.Context, app registry.App) (Instance, error) {
	if b.onColdBoot != nil {
		b.onColdBoot(app)
	}
	if b.failColdBoot {
		return nil, errors.New("injected cold boot failure")
	}
	b.boots.Add(1)
	return b.spawn(app)
}

func (b *fakeBackend) Restore(ctx context.Context, app registry.App) (Instance, error) {
	if b.failRestore {
		return nil, errors.New("injected restore failure")
	}
	if !b.HasSnapshot(app) {
		return nil, errors.New("restore without snapshot files")
	}
	b.restores.Add(1)
	return b.spawn(app)
}

func (b *fakeBackend) Snapshot(ctx context.Context, app registry.App, inst Instance) error {
	if b.failSnapshot {
		return errors.New("injected snapshot failure")
	}
	b.snapCalls.Add(1)
	b.mu.Lock()
	b.snapshots[app.Name] = true
	b.mu.Unlock()
	return nil
}

func (b *fakeBackend) GuestAddr(app registry.App) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if a, ok := b.addrs[app.Name]; ok {
		return a
	}
	return "127.0.0.1:1" // nothing listens; probe would fail fast
}

func (b *fakeBackend) Purge(app registry.App) error { return nil }

func newSupervisor(t *testing.T, be Backend, cfg Config) (*Supervisor, *registry.Registry) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if cfg.WakeTimeout == 0 {
		cfg.WakeTimeout = 5 * time.Second
	}
	if cfg.FreezeTimeout == 0 {
		cfg.FreezeTimeout = 5 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = time.Hour
	}
	sup := New(reg, be, cfg, slog.Default())
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	return sup, reg
}

func register(t *testing.T, sup *Supervisor, name string) registry.App {
	t.Helper()
	meta, err := sup.Register(registry.App{
		Name: name, VCPUs: 1, MemMiB: 512, GuestPort: 8000,
	}, "unused-golden-path")
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestColdBootThenFreezeThenRestore(t *testing.T) {
	be := newFakeBackend()
	sup, reg := newSupervisor(t, be, Config{})
	register(t, sup, "counter")

	// First touch: cold boot.
	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if be.boots.Load() != 1 || be.restores.Load() != 0 {
		t.Fatalf("boots=%d restores=%d, want 1/0", be.boots.Load(), be.restores.Load())
	}
	if st, _ := sup.Status("counter"); st.State != StateActive {
		t.Fatalf("state = %s, want ACTIVE", st.State)
	}

	// Freeze: snapshot taken, registry marked valid.
	if err := sup.Freeze("counter"); err != nil {
		t.Fatal(err)
	}
	if st, _ := sup.Status("counter"); st.State != StateFrozen {
		t.Fatalf("state = %s, want FROZEN", st.State)
	}
	if m, _ := reg.Get("counter"); !m.SnapshotValid || m.State != "FROZEN" {
		t.Fatalf("registry after freeze: %+v", m)
	}

	// Second touch: warm restore, not a boot.
	_, release, err = sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if be.boots.Load() != 1 || be.restores.Load() != 1 {
		t.Fatalf("boots=%d restores=%d, want 1/1", be.boots.Load(), be.restores.Load())
	}
}

func TestSingleFlightWake(t *testing.T) {
	be := newFakeBackend()
	sup, _ := newSupervisor(t, be, Config{})
	register(t, sup, "counter")

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := sup.Acquire(context.Background(), "counter")
			if err != nil {
				errs <- err
				return
			}
			release()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if be.boots.Load() != 1 {
		t.Fatalf("%d concurrent requests caused %d boots, want exactly 1", n, be.boots.Load())
	}
}

func TestFreezeRefusesInflight(t *testing.T) {
	be := newFakeBackend()
	sup, _ := newSupervisor(t, be, Config{})
	register(t, sup, "counter")

	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Freeze("counter"); !errors.Is(err, ErrBusy) {
		t.Fatalf("freeze with inflight request: want ErrBusy, got %v", err)
	}
	release()
	if err := sup.Freeze("counter"); err != nil {
		t.Fatalf("freeze after release: %v", err)
	}
}

func TestJanitorFreezesIdleApp(t *testing.T) {
	be := newFakeBackend()
	sup, _ := newSupervisor(t, be, Config{
		IdleTimeout:     50 * time.Millisecond,
		JanitorInterval: 10 * time.Millisecond,
	})
	register(t, sup, "counter")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.RunJanitor(ctx)

	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ := sup.Status("counter")
		if st.State == StateFrozen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor never froze the idle app (state %s)", st.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if be.snapCalls.Load() != 1 {
		t.Fatalf("snapshot calls = %d, want 1", be.snapCalls.Load())
	}

	// A frozen app must wake again on demand.
	_, release, err = sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if be.restores.Load() != 1 {
		t.Fatalf("restores = %d, want 1", be.restores.Load())
	}
}

func TestWakeFailureLeavesAppCold(t *testing.T) {
	be := newFakeBackend()
	be.failColdBoot = true
	sup, reg := newSupervisor(t, be, Config{WakeTimeout: time.Second})
	register(t, sup, "counter")

	_, _, err := sup.Acquire(context.Background(), "counter")
	if err == nil {
		t.Fatal("acquire should fail when boot fails")
	}
	if st, _ := sup.Status("counter"); st.State != StateCold {
		t.Fatalf("state = %s, want COLD after failed wake", st.State)
	}
	if m, _ := reg.Get("counter"); m.State != "COLD" {
		t.Fatalf("registry state = %s, want COLD", m.State)
	}

	// Recovery: fix the backend, next request boots fine.
	be.failColdBoot = false
	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestFailedRestoreInvalidatesSnapshotAndFallsBackCold(t *testing.T) {
	be := newFakeBackend()
	sup, reg := newSupervisor(t, be, Config{WakeTimeout: time.Second})
	register(t, sup, "counter")

	// Get to FROZEN.
	_, release, _ := sup.Acquire(context.Background(), "counter")
	release()
	if err := sup.Freeze("counter"); err != nil {
		t.Fatal(err)
	}

	be.failRestore = true
	if _, _, err := sup.Acquire(context.Background(), "counter"); err == nil {
		t.Fatal("acquire should fail when restore fails")
	}
	// The failed restore may have partially resumed the guest: the snapshot
	// must never be paired with this disk again.
	if m, _ := reg.Get("counter"); m.SnapshotValid {
		t.Fatal("snapshot still marked valid after failed restore")
	}

	// Next wake must cold-boot, not retry the snapshot.
	be.failRestore = false
	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if be.boots.Load() != 2 || be.restores.Load() != 0 {
		t.Fatalf("boots=%d restores=%d, want 2/0 (restore path must be dead)", be.boots.Load(), be.restores.Load())
	}
}

func TestColdBootInvalidatesSnapshotBeforeDiskCanMutate(t *testing.T) {
	be := newFakeBackend()
	sup, reg := newSupervisor(t, be, Config{})
	register(t, sup, "counter")

	// Plant a stale-but-valid-looking snapshot state, as a crash demotion
	// would: snapshot files exist, but the app must cold boot.
	be.mu.Lock()
	be.snapshots["counter"] = true
	be.mu.Unlock()
	if err := reg.SetSnapshotValid("counter", true); err != nil {
		t.Fatal(err)
	}
	// App state in the supervisor is COLD (fresh registration), so Acquire
	// cold-boots. THE invariant: by the time the guest could write its disk,
	// the registry bit must already be false.
	be.onColdBoot = func(app registry.App) {
		if m, _ := reg.Get(app.Name); m.SnapshotValid {
			t.Error("snapshot still valid at ColdBoot time — invalidation must precede disk mutation")
		}
	}
	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAcquireDuringFreezeWaitsThenRewakes(t *testing.T) {
	be := newFakeBackend()
	sup, _ := newSupervisor(t, be, Config{})
	register(t, sup, "counter")

	_, release, _ := sup.Acquire(context.Background(), "counter")
	release()

	freezeDone := make(chan error, 1)
	go func() { freezeDone <- sup.Freeze("counter") }()

	// Wait until the freeze has claimed the app, then race a request at it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, _ := sup.Status("counter")
		if st.State == StateSnapshotting || st.State == StateFrozen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("freeze never started")
		}
	}
	_, release, err := sup.Acquire(context.Background(), "counter")
	if err != nil {
		t.Fatalf("acquire during/after freeze: %v", err)
	}
	release()
	if err := <-freezeDone; err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if st, _ := sup.Status("counter"); st.State != StateActive {
		t.Fatalf("state = %s, want ACTIVE (request after freeze re-woke it)", st.State)
	}
}

func TestReconcileDemotesMidFlightStates(t *testing.T) {
	be := newFakeBackend()
	sup, reg := newSupervisor(t, be, Config{})
	register(t, sup, "crashed-active")
	register(t, sup, "clean-frozen")

	// Simulate the previous daemon dying at awkward moments.
	reg.SetState("crashed-active", "ACTIVE")
	reg.SetSnapshotValid("crashed-active", true) // stale pairing from before the crash
	be.mu.Lock()
	be.snapshots["crashed-active"] = true
	be.snapshots["clean-frozen"] = true
	be.mu.Unlock()
	reg.SetState("clean-frozen", "FROZEN")
	reg.SetSnapshotValid("clean-frozen", true)

	sup2 := New(reg, be, Config{WakeTimeout: 5 * time.Second, FreezeTimeout: 5 * time.Second, IdleTimeout: time.Hour}, slog.Default())
	if err := sup2.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// The crashed-while-ACTIVE app mutated its disk after its snapshot:
	// demoted to COLD, snapshot dead.
	m, _ := reg.Get("crashed-active")
	if m.State != "COLD" || m.SnapshotValid {
		t.Fatalf("crashed app after reconcile: state=%s valid=%v, want COLD/false", m.State, m.SnapshotValid)
	}
	// The cleanly frozen app keeps its snapshot.
	m, _ = reg.Get("clean-frozen")
	if m.State != "FROZEN" || !m.SnapshotValid {
		t.Fatalf("frozen app after reconcile: state=%s valid=%v, want FROZEN/true", m.State, m.SnapshotValid)
	}

	// And behavior matches: frozen restores, crashed cold-boots.
	_, r1, err := sup2.Acquire(context.Background(), "clean-frozen")
	if err != nil {
		t.Fatal(err)
	}
	r1()
	_, r2, err := sup2.Acquire(context.Background(), "crashed-active")
	if err != nil {
		t.Fatal(err)
	}
	r2()
	if be.restores.Load() != 1 || be.boots.Load() != 1 {
		t.Fatalf("restores=%d boots=%d, want 1/1", be.restores.Load(), be.boots.Load())
	}
}

func TestDeleteLifecycle(t *testing.T) {
	be := newFakeBackend()
	sup, _ := newSupervisor(t, be, Config{})
	register(t, sup, "doomed")

	_, release, _ := sup.Acquire(context.Background(), "doomed")
	if err := sup.Delete("doomed"); !errors.Is(err, ErrBusy) {
		t.Fatalf("delete with inflight: want ErrBusy, got %v", err)
	}
	release()
	if err := sup.Delete("doomed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sup.Acquire(context.Background(), "doomed"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("acquire after delete: want ErrNotFound, got %v", err)
	}
}

func TestFreezeAllOnShutdown(t *testing.T) {
	be := newFakeBackend()
	sup, _ := newSupervisor(t, be, Config{})
	for _, n := range []string{"a", "b", "c"} {
		register(t, sup, n)
		_, release, err := sup.Acquire(context.Background(), n)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	sup.FreezeAll()
	for _, n := range []string{"a", "b", "c"} {
		if st, _ := sup.Status(n); st.State != StateFrozen {
			t.Fatalf("app %s = %s after FreezeAll, want FROZEN", n, st.State)
		}
	}
	if be.snapCalls.Load() != 3 {
		t.Fatalf("snapshot calls = %d, want 3", be.snapCalls.Load())
	}
}
