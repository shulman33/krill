package objstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestGCSLive holds a REAL bucket to the same contract the fakes pass. It is
// the check to run when credentials are first installed on a host — a fake
// that implements x-goog-if-generation-match correctly proves nothing about
// whether this box can authenticate, and whether the key's IAM role actually
// permits list and delete as well as read and write.
//
//	KRILL_GCS_TEST_BUCKET=krill-fsn1-objstore \
//	KRILL_GCS_CREDENTIALS=/etc/krill/gcs.json \
//	go test -run TestGCSLive -v ./internal/objstore/
//
// It writes under _test/<unix nanos>/ and deletes what it wrote.
func TestGCSLive(t *testing.T) {
	bucket := os.Getenv("KRILL_GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("set KRILL_GCS_TEST_BUCKET to run the live GCS contract test")
	}
	prefix := fmt.Sprintf("_test/%d", time.Now().UnixNano())
	s := NewGCS(bucket, prefix)
	ctx := context.Background()

	t.Cleanup(func() {
		keys, err := s.List(ctx, "")
		if err != nil {
			t.Logf("cleanup list failed: %v", err)
			return
		}
		for _, k := range keys {
			if err := s.Delete(ctx, k); err != nil {
				t.Logf("cleanup delete %s: %v", k, err)
			}
		}
	})

	// Check first: it is the narrowest possible statement of "this store can
	// be the arbiter of record", and its failure message is the diagnostic.
	if err := Check(ctx, s); err != nil {
		t.Fatalf("Check against gs://%s/%s: %v", bucket, prefix, err)
	}
	t.Logf("authenticated via %s", s.AuthVia())
	runStoreContract(t, s)
}
