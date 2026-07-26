package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestStoreContract runs the same CAS-semantics suite over every backend.
func TestStoreContract(t *testing.T) {
	backends := map[string]func(t *testing.T) Store{
		"mem": func(t *testing.T) Store { return NewMem() },
		"fs":  func(t *testing.T) Store { return NewFS(t.TempDir()) },
		"gcs": func(t *testing.T) Store { return newFakeGCS(t) },
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			runStoreContract(t, mk(t))
		})
	}
}

// runStoreContract is the CAS-semantics suite, shared by the fake backends
// and the live-GCS test so a real bucket is held to exactly the same bar.
func runStoreContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()

	// Missing object.
	if _, _, err := s.Get(ctx, "a/b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v, want ErrNotFound", err)
	}

	// Create-if-absent (expect 0).
	gen, err := s.PutCAS(ctx, "a/b", []byte("v1"), 0)
	if err != nil {
		t.Fatalf("PutCAS create: %v", err)
	}
	if gen == 0 {
		t.Fatal("PutCAS returned generation 0 for a live object")
	}
	// Second create-if-absent must conflict.
	if _, err := s.PutCAS(ctx, "a/b", []byte("v1b"), 0); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("PutCAS duplicate create: %v, want ErrCASConflict", err)
	}

	// Read observes the write and its generation.
	data, got, err := s.Get(ctx, "a/b")
	if err != nil || string(data) != "v1" || got != gen {
		t.Fatalf("Get: %q gen %d err %v, want v1 gen %d", data, got, err, gen)
	}

	// CAS with the right generation advances; wrong generation conflicts
	// and leaves the object untouched.
	gen2, err := s.PutCAS(ctx, "a/b", []byte("v2"), gen)
	if err != nil || gen2 <= gen {
		t.Fatalf("PutCAS advance: gen %d err %v", gen2, err)
	}
	if _, err := s.PutCAS(ctx, "a/b", []byte("evil"), gen); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale CAS: %v, want ErrCASConflict", err)
	}
	data, got, _ = s.Get(ctx, "a/b")
	if string(data) != "v2" || got != gen2 {
		t.Fatalf("after stale CAS: %q gen %d, want v2 gen %d (stale write mutated state)", data, got, gen2)
	}

	// Unconditional Put + List ordering.
	for _, k := range []string{"seg/2", "seg/1", "other/x"} {
		if err := s.Put(ctx, k, []byte(k)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	keys, err := s.List(ctx, "seg/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 || keys[0] != "seg/1" || keys[1] != "seg/2" {
		t.Fatalf("List seg/: %v", keys)
	}

	// Delete is idempotent.
	if err := s.Delete(ctx, "seg/1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "seg/1"); err != nil {
		t.Fatalf("Delete twice: %v", err)
	}
	if _, _, err := s.Get(ctx, "seg/1"); !errorsIsNotFound(err) {
		t.Fatalf("Get deleted: %v", err)
	}
}

func errorsIsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func TestInvalidKeys(t *testing.T) {
	s := NewMem()
	for _, k := range []string{"", "/abs", "a/../b"} {
		if err := s.Put(context.Background(), k, nil); err == nil {
			t.Errorf("Put(%q) accepted an invalid key", k)
		}
	}
}

// fakeGCS implements just enough of the GCS XML API for the contract test:
// generation headers, x-goog-if-generation-match, list with prefix.
type fakeGCS struct {
	mu   sync.Mutex
	objs map[string]struct {
		data []byte
		gen  int64
	}
	nextGen int64
	// token is the bearer it demands; "" means "test-token".
	token string
}

func newFakeGCS(t *testing.T) Store {
	g, _ := newFakeGCSStore(t, "")
	g.TokenFunc = func(context.Context) (string, error) { return "test-token", nil }
	return g
}

// newFakeGCSStore wires a GCS client to a fake bucket demanding the given
// bearer token, leaving auth resolution to the real code path.
func newFakeGCSStore(t *testing.T, token string) (*GCS, *fakeGCS) {
	f := &fakeGCS{objs: map[string]struct {
		data []byte
		gen  int64
	}{}, token: token}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	g := NewGCS("bkt", "pre/fix")
	g.Endpoint = srv.URL
	return g, f
}

func (f *fakeGCS) bearer() string {
	if f.token == "" {
		return "test-token"
	}
	return f.token
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.bearer() {
		http.Error(w, "no auth", http.StatusUnauthorized)
		return
	}
	// Bucket list: GET /bkt?prefix=...
	if r.Method == http.MethodGet && strings.Trim(r.URL.Path, "/") == "bkt" {
		prefix := r.URL.Query().Get("prefix")
		var ks []string
		for k := range f.objs {
			if strings.HasPrefix(k, prefix) {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks) // real GCS lists in lexicographic order
		fmt.Fprint(w, `<ListBucketResult>`)
		for _, k := range ks {
			fmt.Fprintf(w, "<Contents><Key>%s</Key></Contents>", k)
		}
		fmt.Fprint(w, `<IsTruncated>false</IsTruncated></ListBucketResult>`)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/bkt/")
	switch r.Method {
	case http.MethodGet:
		o, ok := f.objs[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("x-goog-generation", strconv.FormatInt(o.gen, 10))
		w.Write(o.data)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cur := f.objs[name].gen // 0 if absent
		if m := r.Header.Get("x-goog-if-generation-match"); m != "" {
			want, _ := strconv.ParseInt(m, 10, 64)
			if cur != want {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
		}
		f.nextGen++
		// Real GCS generations are timestamps, not counters — advance by a
		// gap to catch any code assuming +1.
		gen := cur + 1000 + f.nextGen
		f.objs[name] = struct {
			data []byte
			gen  int64
		}{data: body, gen: gen}
		w.Header().Set("x-goog-generation", strconv.FormatInt(gen, 10))
	case http.MethodDelete:
		if _, ok := f.objs[name]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.objs, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}
}
