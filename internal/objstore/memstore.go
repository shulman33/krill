package objstore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Mem is the in-memory Store used by tests and the deterministic-simulation
// harness. The sim wraps it (see internal/dataplane/sim) to inject
// partitions; Mem itself is just a correct, mutex-serialized store.
type Mem struct {
	mu   sync.Mutex
	objs map[string]memObj
}

type memObj struct {
	data []byte
	gen  int64
}

func NewMem() *Mem { return &Mem{objs: map[string]memObj{}} }

func (s *Mem) Get(_ context.Context, key string) ([]byte, int64, error) {
	if err := validKey(key); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return append([]byte(nil), o.data...), o.gen, nil
}

func (s *Mem) Put(_ context.Context, key string, data []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = memObj{data: append([]byte(nil), data...), gen: s.objs[key].gen + 1}
	return nil
}

func (s *Mem) PutCAS(_ context.Context, key string, data []byte, expect int64) (int64, error) {
	if err := validKey(key); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.objs[key].gen
	if cur != expect {
		return 0, fmt.Errorf("%w: key %s has gen %d, expected %d", ErrCASConflict, key, cur, expect)
	}
	s.objs[key] = memObj{data: append([]byte(nil), data...), gen: cur + 1}
	return cur + 1, nil
}

func (s *Mem) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Mem) Delete(_ context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, key)
	return nil
}
