package dataplane

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// shipperFixture: a REAL SQLite database written on the host, tailed by a
// Shipper through DirSource into a mem objstore — the full production data
// path minus the ext4 layer (which has its own kernel-ground-truth tests).
type shipperFixture struct {
	t   *testing.T
	dir string
	db  *sql.DB
	gw  *Gateway
	sh  *Shipper
}

func newShipperFixture(t *testing.T) *shipperFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "app.db")+
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=wal_autocheckpoint(0)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	gw := testGateway()
	if _, err := gw.CreateStream(context.Background(), "app", "s0", nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	f := &shipperFixture{t: t, dir: dir, db: db, gw: gw}
	f.sh = f.newShipper(NewEpoch(1, 1))
	return f
}

func (f *shipperFixture) newShipper(e Epoch) *Shipper {
	return NewShipper(ShipperConfig{
		App: "app", Stream: "s0", Epoch: e, Gateway: f.gw,
		Source:     &DirSource{Dir: f.dir, Base: "app.db"},
		CursorPath: filepath.Join(f.dir, "ship.json"),
	})
}

func (f *shipperFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *shipperFixture) insert(from, n int) {
	f.t.Helper()
	for i := from; i < from+n; i++ {
		f.exec("INSERT INTO kv (k, v) VALUES (?, ?)", i, fmt.Sprintf("v%d", i))
	}
}

// restoreState restores the stream at LSN (0=head) and returns (count, sum)
// of the kv table in the materialized image.
func (f *shipperFixture) restoreState(stream string, lsn uint64) (int, int) {
	f.t.Helper()
	img, _, err := f.gw.Restore(context.Background(), "app", stream, lsn)
	if err != nil {
		f.t.Fatalf("restore: %v", err)
	}
	p := filepath.Join(f.t.TempDir(), "restored.db")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		f.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	var count, sum int
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(k),0) FROM kv").Scan(&count, &sum); err != nil {
		f.t.Fatalf("querying restored image: %v", err)
	}
	return count, sum
}

func TestShipperEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newShipperFixture(t)
	f.exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")
	f.insert(0, 20)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c, s := f.restoreState("s0", 0); c != 20 || s != 190 {
		t.Fatalf("after first sync: count=%d sum=%d", c, s)
	}

	// Incremental: more writes, another sync — restore reflects both.
	f.insert(100, 5)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c, s := f.restoreState("s0", 0); c != 25 || s != 190+510 {
		t.Fatalf("after second sync: count=%d sum=%d", c, s)
	}

	// Freeze: final ship + fenced checkpoint registration.
	m, err := f.sh.FinalCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Checkpoints) != 1 || m.Checkpoints[0].LSN != m.HeadLSN {
		t.Fatalf("checkpoint: %+v (head %d)", m.Checkpoints, m.HeadLSN)
	}
	if err := CheckManifest(m); err != nil {
		t.Fatal(err)
	}
	// Restore from the checkpointed stream still yields the same state.
	if c, s := f.restoreState("s0", 0); c != 25 || s != 700 {
		t.Fatalf("after checkpoint: count=%d sum=%d", c, s)
	}
}

func TestShipperSurvivesGuestCheckpoint(t *testing.T) {
	ctx := context.Background()
	f := newShipperFixture(t)
	f.exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")
	f.insert(0, 10)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// The guest checkpoints its WAL behind our back (RESTART resets salts)
	// and keeps writing. The shipper must re-base, not lose data.
	f.exec("PRAGMA wal_checkpoint(RESTART)")
	f.insert(50, 10)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.gw.Load(ctx, "app", "s0")
	if err != nil {
		t.Fatal(err)
	}
	hasRebase := false
	for _, s := range m.Segments {
		if s.Kind == SegRebase {
			hasRebase = true
		}
	}
	if !hasRebase {
		t.Fatal("no rebase segment after a guest-side WAL reset")
	}
	if c, s := f.restoreState("s0", 0); c != 20 || s != 45+545 {
		t.Fatalf("after rebase: count=%d sum=%d", c, s)
	}
	if err := CheckManifest(m); err != nil {
		t.Fatal(err)
	}
}

func TestShipperRestartResumes(t *testing.T) {
	ctx := context.Background()
	f := newShipperFixture(t)
	f.exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")
	f.insert(0, 10)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	headBefore := f.sh.ShippedLSN()

	// New shipper (daemon restart / next wake, next epoch), same cursor
	// file: it must NOT re-ship what the stream already has.
	sh2 := f.newShipper(NewEpoch(1, 2))
	f.insert(100, 3)
	if err := sh2.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.gw.Load(ctx, "app", "s0")
	if err != nil {
		t.Fatal(err)
	}
	if m.HeadLSN <= headBefore {
		t.Fatal("second epoch shipped nothing")
	}
	if c, s := f.restoreState("s0", 0); c != 13 || s != 45+303 {
		t.Fatalf("after restart: count=%d sum=%d", c, s)
	}
	// The takeover left the epoch trail: segments first from e1, then e2.
	if m.Segments[0].Epoch != NewEpoch(1, 1) || m.Segments[len(m.Segments)-1].Epoch != NewEpoch(1, 2) {
		t.Fatalf("epoch trail: %+v", m.Segments)
	}
	if err := CheckManifest(m); err != nil {
		t.Fatal(err)
	}
}

func TestShipperFencedIsFatal(t *testing.T) {
	ctx := context.Background()
	f := newShipperFixture(t)
	f.exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")
	f.insert(0, 5)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	// A successor epoch seals the stream; the old shipper's next ship must
	// fail fenced, permanently, and fire OnFenced.
	fencedFired := make(chan error, 1)
	f.sh.cfg.OnFenced = func(err error) { fencedFired <- err }
	if _, _, err := f.gw.SealTakeover(ctx, "app", "s0", NewEpoch(1, 9)); err != nil {
		t.Fatal(err)
	}
	f.insert(50, 5)
	if err := f.sh.Sync(ctx); err == nil {
		t.Fatal("zombie shipper shipped under a sealed successor epoch")
	}
	if err := f.sh.Err(); err == nil {
		t.Fatal("fencing was not fatal")
	}
	<-fencedFired
	// Sync keeps failing without touching the gateway.
	if err := f.sh.Sync(ctx); err == nil {
		t.Fatal("fenced shipper accepted another Sync")
	}
}

func TestPITRBranching(t *testing.T) {
	ctx := context.Background()
	f := newShipperFixture(t)
	f.exec("CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")

	// Phase A.
	f.insert(0, 10)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	lsnA := f.sh.ShippedLSN()

	// Phase B.
	f.insert(100, 10)
	if err := f.sh.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	parentBefore, _, err := f.gw.Store.Get(ctx, manifestKey("app", "s0"))
	if err != nil {
		t.Fatal(err)
	}

	// Branch at the phase-A point: the branch serves A only.
	if _, err := f.gw.CreateBranch(ctx, "app", "s0", "s1", lsnA); err != nil {
		t.Fatal(err)
	}
	if c, s := f.restoreState("s1", 0); c != 10 || s != 45 {
		t.Fatalf("branch state: count=%d sum=%d, want phase A only", c, s)
	}

	// D4: the parent's history is untouched (only the informational branch
	// list may differ), and its head still restores phase A+B.
	parentAfter, _, err := f.gw.Store.Get(ctx, manifestKey("app", "s0"))
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := decodeManifest(parentBefore)
	pa, _ := decodeManifest(parentAfter)
	pa.Branches = nil
	pb.Branches = nil
	if string(encodeManifest(pa)) != string(encodeManifest(pb)) {
		t.Fatal("branching mutated parent history")
	}
	if c, s := f.restoreState("s0", 0); c != 20 || s != 45+1045 {
		t.Fatalf("parent after branch: count=%d sum=%d", c, s)
	}

	// The branch takes its own writes: ship a rebase-free append under a
	// new epoch via a shipper bound to s1... (its local db continues from
	// phase A+B on disk, so instead verify restore at the parent's OLD head
	// still works — nothing was truncated).
	if c, s := f.restoreState("s0", lsnA); c != 10 || s != 45 {
		t.Fatalf("parent at lsnA: count=%d sum=%d", c, s)
	}
}
