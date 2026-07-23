package ext4

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// loadImage gunzips a fixture into memory and wraps it as an io.ReaderAt.
func loadImage(t *testing.T, name string) *bytes.Reader {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(img)
}

type manifestEntry struct {
	path   string
	size   int64
	sha256 string
}

// loadManifest reads the ground-truth listing captured through the mounted
// kernel view at fixture-generation time.
func loadManifest(t *testing.T, name string) []manifestEntry {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out []manifestEntry
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e manifestEntry
		if _, err := fmt.Sscanf(line, "%s %d %s", &e.path, &e.size, &e.sha256); err != nil {
			t.Fatalf("manifest line %q: %v", line, err)
		}
		e.path = strings.TrimPrefix(e.path, ".")
		out = append(out, e)
	}
	if len(out) < 5 {
		t.Fatalf("suspiciously small manifest: %d entries", len(out))
	}
	return out
}

// TestReadMatchesKernel: every file in every fixture must come back
// byte-identical to what the kernel served from the mounted filesystem —
// including the img-live fixture, whose freshest metadata exists only as
// uncheckpointed jbd2 transactions (dumpe2fs shows needs_recovery).
func TestReadMatchesKernel(t *testing.T) {
	cases := []struct{ img, manifest string }{
		{"img-clean-4k.ext4.gz", "manifest-4k.txt"},
		{"img-live-4k.ext4.gz", "manifest-4k.txt"},
		{"img-clean-1k.ext4.gz", "manifest-1k.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.img, func(t *testing.T) {
			fs, err := Open(loadImage(t, tc.img), Options{})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range loadManifest(t, tc.manifest) {
				data, err := fs.ReadFile(want.path)
				if err != nil {
					t.Errorf("ReadFile(%s): %v", want.path, err)
					continue
				}
				if int64(len(data)) != want.size {
					t.Errorf("%s: size %d, want %d", want.path, len(data), want.size)
					continue
				}
				if got := hex.EncodeToString(sha256sum(data)); got != want.sha256 {
					t.Errorf("%s: content hash mismatch", want.path)
				}
				if sz, err := fs.FileSize(want.path); err != nil || sz != want.size {
					t.Errorf("FileSize(%s) = %d, %v; want %d", want.path, sz, err, want.size)
				}
			}
		})
	}
}

func TestMissingFiles(t *testing.T) {
	fs, err := Open(loadImage(t, "img-clean-4k.ext4.gz"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/nope", "/dir/nope", "/hello.txt/child", "/dir/sub/deep/deeper"} {
		if _, err := fs.ReadFile(p); !errors.Is(err, ErrNotExist) {
			t.Errorf("ReadFile(%s): %v, want ErrNotExist", p, err)
		}
	}
	// Directories are not regular files.
	if _, err := fs.ReadFile("/dir"); !errors.Is(err, ErrNotExist) {
		t.Errorf("ReadFile(/dir): %v, want ErrNotExist", err)
	}
}

// TestLiveImageNeedsReplay guards the fixture itself: the live image must
// contain files whose metadata exists ONLY in the jbd2 journal, so that
// TestReadMatchesKernel/img-live is actually exercising replay. If a
// regenerated fixture ever goes vacuous (a stray sync before the copy),
// this fails instead of silently weakening the suite.
func TestLiveImageNeedsReplay(t *testing.T) {
	fs, err := Open(loadImage(t, "img-live-4k.ext4.gz"), Options{SkipJournal: true})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := 0
	for _, want := range loadManifest(t, "manifest-4k.txt") {
		data, err := fs.ReadFile(want.path)
		if err != nil || int64(len(data)) != want.size ||
			hex.EncodeToString(sha256sum(data)) != want.sha256 {
			mismatch++
		}
	}
	if mismatch == 0 {
		t.Fatal("live fixture is vacuous: nothing differs without journal replay")
	}
}

// TestSkipJournal: the fast-poll path must at least open and read cleanly
// checkpointed files (payload validity is the WAL layer's job).
func TestSkipJournal(t *testing.T) {
	fs, err := Open(loadImage(t, "img-live-4k.ext4.gz"), Options{SkipJournal: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile("/hello.txt"); err != nil {
		t.Fatalf("ReadFile without journal replay: %v", err)
	}
}

func TestNotExt4(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 4096)), Options{}); !errors.Is(err, ErrNotExt4) {
		t.Fatalf("Open(zeros): %v, want ErrNotExt4", err)
	}
}

func sha256sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
