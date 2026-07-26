package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Check verifies that a store can actually be the arbiter of record (E4),
// not merely that it is reachable. It exercises the full conditional-PUT
// cycle — create-or-advance, read-back, and a stale CAS that MUST be
// rejected — under a throwaway key, then deletes it.
//
// Why this and not a ping: a store that accepts writes but ignores
// x-goog-if-generation-match would pass a ping and lose the fence. Cheap to
// run at startup; the alternative is discovering it mid-wake.
func Check(ctx context.Context, s Store) error {
	const key = "_control/preflight/probe"
	_, gen, err := s.Get(ctx, key)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("objstore preflight: read: %w", err)
		}
		gen = 0 // expect-0 = "must not exist", the create case
	}
	want := []byte(time.Now().UTC().Format(time.RFC3339Nano))
	newGen, err := s.PutCAS(ctx, key, want, gen)
	if err != nil {
		return fmt.Errorf("objstore preflight: conditional write: %w", err)
	}
	got, gotGen, err := s.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("objstore preflight: read-back: %w", err)
	}
	if !bytes.Equal(got, want) || gotGen != newGen {
		return fmt.Errorf("objstore preflight: read-back mismatch (gen %d want %d): "+
			"this store is not read-after-write consistent and cannot hold the manifest", gotGen, newGen)
	}
	// The half that matters: the generation we just superseded must no longer
	// be able to write. A store that allows this cannot fence a zombie.
	if _, err := s.PutCAS(ctx, key, []byte("stale"), gen); !errors.Is(err, ErrCASConflict) {
		return fmt.Errorf("objstore preflight: a stale conditional write was NOT rejected (err=%v): "+
			"this store does not implement compare-and-swap and must not be used as the object store", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		return fmt.Errorf("objstore preflight: delete: %w", err)
	}
	return nil
}

// CopyOpts configures Copy.
type CopyOpts struct {
	// Prefix limits the copy ("" = the whole store).
	Prefix string
	// Force permits overwriting destination objects whose bytes differ.
	// Without it a differing object aborts the copy: the usual cause is a
	// copy pointed at a store that is already somebody's arbiter of record.
	Force bool
	// OnObject, if set, is called per key with the action taken:
	// "copy", "skip" (identical bytes already there) or "overwrite".
	OnObject func(key string, size int, action string)
}

// CopyReport counts what Copy did.
type CopyReport struct {
	Copied  int   `json:"copied"`
	Skipped int   `json:"skipped"`
	Bytes   int64 `json:"bytes"`
}

// Copy mirrors every object under a prefix from src to dst. It exists for one
// operationally sharp reason: the object store is the arbiter of record (E4),
// so repointing krilld's --objstore at an empty bucket does not migrate
// anything — it declares every app's history to be empty, and the next wake
// faithfully rebuilds each data disk to match. The record must move before
// the pointer does.
//
// Re-running is safe and cheap: identical objects are skipped, so an
// interrupted copy resumes. Manifests are written last, so an interrupted
// copy never leaves a manifest referencing segments that have not landed.
//
// Copy assumes the source is quiescent (krilld stopped, or every app frozen).
// It does not attempt to be consistent against a moving stream.
func Copy(ctx context.Context, dst, src Store, o CopyOpts) (CopyReport, error) {
	var rep CopyReport
	keys, err := src.List(ctx, o.Prefix)
	if err != nil {
		return rep, fmt.Errorf("objstore copy: listing source: %w", err)
	}
	manifestLast(keys)
	for _, k := range keys {
		data, _, err := src.Get(ctx, k)
		if errors.Is(err, ErrNotFound) {
			continue // raced with a GC; nothing to move
		}
		if err != nil {
			return rep, fmt.Errorf("objstore copy: reading %s: %w", k, err)
		}
		action := "copy"
		if cur, _, err := dst.Get(ctx, k); err == nil {
			if bytes.Equal(cur, data) {
				rep.Skipped++
				if o.OnObject != nil {
					o.OnObject(k, len(data), "skip")
				}
				continue
			}
			if !o.Force {
				return rep, fmt.Errorf("objstore copy: %s already exists at the destination with "+
					"different contents (%d bytes there, %d here); refusing to overwrite without force — "+
					"is the destination already a live object store?", k, len(cur), len(data))
			}
			action = "overwrite"
		}
		if err := dst.Put(ctx, k, data); err != nil {
			return rep, fmt.Errorf("objstore copy: writing %s: %w", k, err)
		}
		rep.Copied++
		rep.Bytes += int64(len(data))
		if o.OnObject != nil {
			o.OnObject(k, len(data), action)
		}
	}
	return rep, nil
}

// manifestLast reorders keys so manifests are written after everything they
// point at, preserving List's lexicographic order within each group.
func manifestLast(keys []string) {
	sort.SliceStable(keys, func(i, j int) bool {
		return !isManifest(keys[i]) && isManifest(keys[j])
	})
}

func isManifest(k string) bool { return strings.HasSuffix(k, "/manifest.json") }
