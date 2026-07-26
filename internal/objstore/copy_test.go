package objstore

import (
	"context"
	"strings"
	"testing"
)

func seedStream(t *testing.T, s Store) []string {
	t.Helper()
	ctx := context.Background()
	keys := []string{
		"apps/guestbook/s0/seg/0000000000000000-0000000000004096-g1.c1",
		"apps/guestbook/s0/seg/0000000000004096-0000000000008192-g1.c2",
		"apps/guestbook/s0/ckpt/0000000000008192-g1.c2.db",
		"apps/guestbook/s0/manifest.json",
		"_control/registry/20260726T170000Z.db.gz",
	}
	for _, k := range keys {
		if err := s.Put(ctx, k, []byte("payload:"+k)); err != nil {
			t.Fatalf("seeding %s: %v", k, err)
		}
	}
	return keys
}

// TestCopyMovesTheRecord: the whole point of Copy — repointing --objstore at
// an empty bucket would otherwise declare every stream empty (E4).
func TestCopyMovesTheRecord(t *testing.T) {
	ctx := context.Background()
	src, dst := NewMem(), NewMem()
	want := seedStream(t, src)

	var order []string
	rep, err := Copy(ctx, dst, src, CopyOpts{OnObject: func(k string, _ int, _ string) {
		order = append(order, k)
	}})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if rep.Copied != len(want) || rep.Skipped != 0 {
		t.Fatalf("report = %+v, want %d copied", rep, len(want))
	}
	for _, k := range want {
		got, _, err := dst.Get(ctx, k)
		if err != nil || string(got) != "payload:"+k {
			t.Errorf("dst missing %s: %q %v", k, got, err)
		}
	}
	// Manifests land last: an interrupted copy must never leave a manifest
	// pointing at segments that have not arrived.
	if last := order[len(order)-1]; !strings.HasSuffix(last, "/manifest.json") {
		t.Errorf("last object copied = %s, want the manifest", last)
	}
}

func TestCopyIsResumable(t *testing.T) {
	ctx := context.Background()
	src, dst := NewMem(), NewMem()
	want := seedStream(t, src)
	if _, err := Copy(ctx, dst, src, CopyOpts{}); err != nil {
		t.Fatal(err)
	}
	rep, err := Copy(ctx, dst, src, CopyOpts{})
	if err != nil {
		t.Fatalf("second Copy: %v", err)
	}
	if rep.Copied != 0 || rep.Skipped != len(want) {
		t.Fatalf("re-run report = %+v, want everything skipped", rep)
	}
}

// A copy aimed at a store that already holds a different record must stop,
// not silently rewrite somebody's history.
func TestCopyRefusesToClobber(t *testing.T) {
	ctx := context.Background()
	src, dst := NewMem(), NewMem()
	seedStream(t, src)
	if err := dst.Put(ctx, "apps/guestbook/s0/manifest.json", []byte("a different history")); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(ctx, dst, src, CopyOpts{}); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	rep, err := Copy(ctx, dst, src, CopyOpts{Force: true})
	if err != nil {
		t.Fatalf("forced Copy: %v", err)
	}
	if rep.Copied == 0 {
		t.Fatal("forced Copy copied nothing")
	}
	got, _, _ := dst.Get(ctx, "apps/guestbook/s0/manifest.json")
	if !strings.HasPrefix(string(got), "payload:") {
		t.Errorf("manifest not overwritten under Force: %q", got)
	}
}

func TestCopyPrefix(t *testing.T) {
	ctx := context.Background()
	src, dst := NewMem(), NewMem()
	seedStream(t, src)
	rep, err := Copy(ctx, dst, src, CopyOpts{Prefix: "_control/"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 1 {
		t.Fatalf("copied %d objects under _control/, want 1", rep.Copied)
	}
	if _, _, err := dst.Get(ctx, "apps/guestbook/s0/manifest.json"); err == nil {
		t.Error("prefix copy leaked an object outside the prefix")
	}
}

func TestCheckPassesRealBackends(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]Store{
		"mem": NewMem(),
		"fs":  NewFS(t.TempDir()),
		"gcs": newFakeGCS(t),
	} {
		if err := Check(ctx, s); err != nil {
			t.Errorf("Check(%s): %v", name, err)
		}
	}
}

// noCAS is the failure Check exists to catch: a store that accepts writes and
// ignores the generation precondition. It would pass any liveness ping and
// then quietly fail to fence a zombie writer.
type noCAS struct{ Store }

func (n noCAS) PutCAS(ctx context.Context, key string, data []byte, _ int64) (int64, error) {
	if err := n.Store.Put(ctx, key, data); err != nil {
		return 0, err
	}
	_, gen, err := n.Store.Get(ctx, key)
	return gen, err
}

func TestCheckRejectsAStoreWithoutCAS(t *testing.T) {
	err := Check(context.Background(), noCAS{NewMem()})
	if err == nil || !strings.Contains(err.Error(), "does not implement compare-and-swap") {
		t.Fatalf("err = %v, want Check to reject a store with no CAS", err)
	}
}
