package objstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// FS is a local-directory Store — the single-host M3 backend. Objects are
// files; each object's generation lives in a "<file>.gen" sidecar. All
// mutations happen under one process-wide mutex plus write-temp-then-rename,
// which makes PutCAS a real CAS for every client in this process. krilld is
// the only writer of its own objstore directory, so process-local mutual
// exclusion is the honest scope; a second HOST would talk to a shared store
// (GCS), never to another host's directory.
type FS struct {
	root string
	mu   sync.Mutex
}

func NewFS(root string) *FS { return &FS{root: root} }

func (s *FS) path(key string) string { return filepath.Join(s.root, filepath.FromSlash(key)) }

func (s *FS) Get(_ context.Context, key string) ([]byte, int64, error) {
	if err := validKey(key); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path(key))
	if os.IsNotExist(err) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	gen, err := s.readGen(key)
	if err != nil {
		return nil, 0, err
	}
	return data, gen, nil
}

func (s *FS) Put(_ context.Context, key string, data []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gen, _ := s.readGen(key) // 0 if absent
	return s.write(key, data, gen+1)
}

func (s *FS) PutCAS(_ context.Context, key string, data []byte, expect int64) (int64, error) {
	if err := validKey(key); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.readGen(key)
	if err != nil {
		return 0, err
	}
	if cur != expect {
		return 0, fmt.Errorf("%w: key %s has gen %d, expected %d", ErrCASConflict, key, cur, expect)
	}
	if err := s.write(key, data, cur+1); err != nil {
		return 0, err
	}
	return cur + 1, nil
}

func (s *FS) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".gen") || strings.HasSuffix(p, ".tmp") {
			return nil //nolint:nilerr // a vanished root just lists empty
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func (s *FS) Delete(_ context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range []string{s.path(key), s.path(key) + ".gen"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// write commits data then the new generation, both via rename. Crash
// ordering: a data file with a stale .gen is possible and reads as "the
// write never happened" to the next CAS (it will conflict and re-read),
// which is the safe failure.
func (s *FS) write(key string, data []byte, gen int64) error {
	p := s.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := renameInto(p, data); err != nil {
		return err
	}
	return renameInto(p+".gen", []byte(strconv.FormatInt(gen, 10)))
}

func (s *FS) readGen(key string) (int64, error) {
	b, err := os.ReadFile(s.path(key) + ".gen")
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	gen, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("objstore: corrupt gen sidecar for %s: %w", key, err)
	}
	return gen, nil
}

func renameInto(dst string, data []byte) error {
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
