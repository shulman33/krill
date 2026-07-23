package dataplane

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/sqlitewal"
)

func testGateway() *Gateway {
	g := New(objstore.NewMem())
	// Deterministic clock, monotone.
	base := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	n := 0
	g.Now = func() time.Time { n++; return base.Add(time.Duration(n) * time.Second) }
	return g
}

func frame(pgno, commit uint32, fill byte, ps int) sqlitewal.Frame {
	d := make([]byte, ps)
	for i := range d {
		d[i] = fill
	}
	return sqlitewal.Frame{Pgno: pgno, Commit: commit, Data: d}
}

func TestGatewayFencing(t *testing.T) {
	ctx := context.Background()
	g := testGateway()
	const ps = 512
	e1, e2, e3 := NewEpoch(1, 1), NewEpoch(1, 2), NewEpoch(1, 3)

	if _, err := g.CreateStream(ctx, "app", "s0", nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CreateStream(ctx, "app", "s0", nil, 0, 0); !errors.Is(err, ErrStreamExists) {
		t.Fatalf("double create: %v", err)
	}

	// e2 appends first: seals forward from 0, and the seal is recorded.
	m, err := g.AppendSegment(ctx, "app", "s0", e2, []sqlitewal.Frame{frame(1, 1, 0xAA, ps)}, ps)
	if err != nil {
		t.Fatal(err)
	}
	if m.CurEpoch != e2 || m.HeadLSN != 1 || len(m.Seals) != 1 {
		t.Fatalf("after e2 append: cur=%s head=%d seals=%d", m.CurEpoch, m.HeadLSN, len(m.Seals))
	}

	// Snapshot the manifest bytes; every fenced attempt must leave them
	// untouched (the C2 gate condition).
	before, _, err := g.Store.Get(ctx, manifestKey("app", "s0"))
	if err != nil {
		t.Fatal(err)
	}

	// E3: stale append rejected.
	if _, err := g.AppendSegment(ctx, "app", "s0", e1, []sqlitewal.Frame{frame(1, 1, 0xBB, ps)}, ps); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale append: %v, want ErrFenced", err)
	}
	// E5: stale checkpoint registration rejected.
	if _, err := g.RegisterCheckpoint(ctx, "app", "s0", e1, 1, []byte("img")); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale registration: %v, want ErrFenced", err)
	}
	after, _, err := g.Store.Get(ctx, manifestKey("app", "s0"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a fenced attempt mutated the manifest")
	}

	// SealTakeover: e3 seals; repeat is idempotent; e2 now reports stale.
	m, stale, err := g.SealTakeover(ctx, "app", "s0", e3)
	if err != nil || stale || m.CurEpoch != e3 {
		t.Fatalf("seal e3: cur=%v stale=%v err=%v", m.CurEpoch, stale, err)
	}
	if len(m.Seals) != 2 || m.Seals[1].AtLSN != 1 {
		t.Fatalf("takeover seal record: %+v", m.Seals)
	}
	if _, stale, err = g.SealTakeover(ctx, "app", "s0", e3); err != nil || stale {
		t.Fatalf("idempotent seal: stale=%v err=%v", stale, err)
	}
	if _, stale, err = g.SealTakeover(ctx, "app", "s0", e2); err != nil || !stale {
		t.Fatalf("stale seal must report stale=true (err=%v)", err)
	}

	// Valid registration at the current epoch.
	m, err = g.RegisterCheckpoint(ctx, "app", "s0", e3, 1, []byte("img"))
	if err != nil || len(m.Checkpoints) != 1 {
		t.Fatalf("registration at current epoch: %v", err)
	}
	if err := CheckManifest(m); err != nil {
		t.Fatalf("CheckManifest: %v", err)
	}
}

func TestGatewaySegmentRoundtrip(t *testing.T) {
	ctx := context.Background()
	g := testGateway()
	const ps = 512
	e := NewEpoch(1, 1)
	if _, err := g.CreateStream(ctx, "app", "s0", nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	fr := []sqlitewal.Frame{frame(1, 0, 1, ps), frame(2, 2, 2, ps)}
	m, err := g.AppendSegment(ctx, "app", "s0", e, fr, ps)
	if err != nil {
		t.Fatal(err)
	}
	got, img, err := g.FetchSegment(ctx, m.Segments[0])
	if err != nil || img != nil || len(got) != 2 {
		t.Fatalf("fetch: %d frames, img=%v, err=%v", len(got), img != nil, err)
	}
	if got[0].Pgno != 1 || got[1].Commit != 2 || !bytes.Equal(got[1].Data, fr[1].Data) {
		t.Fatal("segment frames did not round-trip")
	}

	rb, err := g.AppendRebase(ctx, "app", "s0", e, []byte("full-image"), ps)
	if err != nil || rb.HeadLSN != 3 {
		t.Fatalf("rebase: head=%d err=%v", rb.HeadLSN, err)
	}
	_, img, err = g.FetchSegment(ctx, rb.Segments[1])
	if err != nil || string(img) != "full-image" {
		t.Fatalf("rebase fetch: %q err=%v", img, err)
	}
	if err := CheckManifest(rb); err != nil {
		t.Fatal(err)
	}
}

func TestCheckManifestCatchesForgery(t *testing.T) {
	// A hand-forged slow-waker manifest: checkpoint claiming epoch-1
	// lineage over a prefix containing epoch-2 frames.
	m := &Manifest{
		App: "a", Stream: "s0", CurEpoch: NewEpoch(1, 2), HeadLSN: 2,
		Segments: []Segment{
			{Kind: SegFrames, Epoch: NewEpoch(1, 1), FromLSN: 0, ToLSN: 1, Key: "k1"},
			{Kind: SegFrames, Epoch: NewEpoch(1, 2), FromLSN: 1, ToLSN: 2, Key: "k2"},
		},
		Checkpoints: []Checkpoint{{Epoch: NewEpoch(1, 1), LSN: 2, Key: "c"}},
	}
	if err := CheckManifest(m); err == nil {
		t.Fatal("forged lineage passed CheckManifest")
	}

	// Epoch regression in the segment chain (I1).
	m2 := &Manifest{
		App: "a", Stream: "s0", CurEpoch: NewEpoch(1, 2), HeadLSN: 2,
		Segments: []Segment{
			{Kind: SegFrames, Epoch: NewEpoch(1, 2), FromLSN: 0, ToLSN: 1, Key: "k1"},
			{Kind: SegFrames, Epoch: NewEpoch(1, 1), FromLSN: 1, ToLSN: 2, Key: "k2"},
		},
	}
	if err := CheckManifest(m2); err == nil {
		t.Fatal("epoch regression passed CheckManifest")
	}
}
