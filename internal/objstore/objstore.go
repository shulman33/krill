// Package objstore is the data plane's durable record: a small object-store
// interface whose one non-negotiable feature is a conditional PUT.
//
// E4 (protocol doc §1.3): "The stream manifest lives in object storage, and
// every mutation is a conditional PUT (compare-and-swap on manifest
// generation). The object store — not the gateway — is the arbiter of
// record." Everything in the data plane that must be linearizable funnels
// through PutCAS; everything else (WAL segments, checkpoint blobs) is
// immutable and written once under a unique key.
//
// Backends: fsstore (a local directory — the single-host M3 default),
// gcsstore (GCS via XML API conditional PUTs), memstore (tests and the
// deterministic-simulation harness).
package objstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotFound = errors.New("object not found")
	// ErrCASConflict: the object's generation did not match the caller's
	// expectation. The caller must re-read and re-decide — never blind-retry.
	ErrCASConflict = errors.New("generation mismatch (CAS conflict)")
)

// Store is the minimal object-store surface the data plane needs. Keys are
// slash-separated paths ("apps/counter/streams/s0/manifest.json").
type Store interface {
	// Get returns the object's bytes and its current generation.
	Get(ctx context.Context, key string) ([]byte, int64, error)
	// Put writes unconditionally. Reserved for immutable objects under
	// never-reused keys (segments, checkpoints); manifests go through PutCAS.
	Put(ctx context.Context, key string, data []byte) error
	// PutCAS writes iff the object's current generation equals expect
	// (expect 0 = the object must not exist yet) and returns the new
	// generation. On mismatch it returns ErrCASConflict and writes nothing.
	PutCAS(ctx context.Context, key string, data []byte, expect int64) (int64, error)
	// List returns keys with the given prefix, lexicographically sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes an object; deleting a missing key is not an error
	// (GC must be idempotent).
	Delete(ctx context.Context, key string) error
}

// Open builds a Store from a URL-ish spec:
//
//	file:///abs/path   local directory (fsstore)
//	gs://bucket/prefix Google Cloud Storage
//	mem:               in-memory (tests)
func Open(spec string) (Store, error) {
	switch {
	case strings.HasPrefix(spec, "file://"):
		return NewFS(strings.TrimPrefix(spec, "file://")), nil
	case strings.HasPrefix(spec, "gs://"):
		rest := strings.TrimPrefix(spec, "gs://")
		bucket, prefix, _ := strings.Cut(rest, "/")
		if bucket == "" {
			return nil, fmt.Errorf("objstore: empty bucket in %q", spec)
		}
		return NewGCS(bucket, prefix), nil
	case spec == "mem:":
		return NewMem(), nil
	default:
		return nil, fmt.Errorf("objstore: unrecognized spec %q (want file:///path, gs://bucket, or mem:)", spec)
	}
}

func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return fmt.Errorf("objstore: invalid key %q", key)
	}
	return nil
}
