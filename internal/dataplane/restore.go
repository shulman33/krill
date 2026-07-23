package dataplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/sqlitewal"
)

// Restore materializes the database image at target LSN (0 = stream head),
// resolving history across PITR parent links: latest usable checkpoint or
// rebase at-or-below target, then WAL-delta replay to target (E6's replay
// half). The returned LSN is the target actually reached; content is the
// last commit boundary at or below it (uncommitted frame tails never
// materialize, enforced by sqlitewal.Replay).
func (g *Gateway) Restore(ctx context.Context, app, stream string, target uint64) ([]byte, uint64, error) {
	path, err := g.lineage(ctx, app, stream)
	if err != nil {
		return nil, 0, err
	}
	leaf := path[len(path)-1]
	if target == 0 || target > leaf.HeadLSN {
		target = leaf.HeadLSN
	}

	// Effective history: each manifest contributes its span up to where its
	// child branched (the leaf contributes up to target).
	type span struct {
		m     *Manifest
		upper uint64
	}
	spans := make([]span, len(path))
	upper := target
	for i := len(path) - 1; i >= 0; i-- {
		spans[i] = span{m: path[i], upper: upper}
		if path[i].Parent != nil {
			if path[i].Parent.LSN < upper {
				upper = path[i].Parent.LSN
			}
		}
	}

	// Best starting point: the highest-LSN checkpoint on the effective path.
	var base []byte
	var baseLSN uint64
	var bestKey string
	for _, sp := range spans {
		for _, c := range sp.m.Checkpoints {
			if c.LSN <= sp.upper && c.LSN >= baseLSN && c.Key != "" {
				baseLSN, bestKey = c.LSN, c.Key
			}
		}
	}
	if bestKey != "" {
		b, _, err := g.Store.Get(ctx, bestKey)
		if err != nil {
			return nil, 0, fmt.Errorf("dataplane: restore base: %w", err)
		}
		base = b
	}

	// Replay segments (baseLSN, target], oldest manifest first. A rebase
	// replaces the image outright.
	img := base
	for _, sp := range spans {
		lower := uint64(0)
		if sp.m.Parent != nil {
			lower = sp.m.Parent.LSN
		}
		for _, s := range sp.m.Segments {
			if s.ToLSN <= baseLSN || s.ToLSN <= lower || s.FromLSN >= sp.upper {
				continue
			}
			frames, image, err := g.FetchSegment(ctx, s)
			if err != nil {
				return nil, 0, err
			}
			if s.Kind == SegRebase {
				img = image
				continue
			}
			// Cut a straddling segment at the span bound / base.
			if s.FromLSN < baseLSN {
				frames = frames[baseLSN-s.FromLSN:]
			}
			if s.ToLSN > sp.upper {
				frames = frames[:sp.upper-max64(s.FromLSN, baseLSN)]
			}
			ps := sp.m.PageSize
			if ps == 0 {
				ps = leaf.PageSize
			}
			img, err = sqlitewal.Replay(img, ps, frames)
			if err != nil {
				return nil, 0, fmt.Errorf("dataplane: replaying %s: %w", s.Key, err)
			}
		}
	}
	return img, target, nil
}

// lineage returns the manifest chain root-first: [oldest ancestor, ..., stream].
func (g *Gateway) lineage(ctx context.Context, app, stream string) ([]*Manifest, error) {
	var rev []*Manifest
	for depth := 0; ; depth++ {
		if depth > 32 {
			return nil, errors.New("dataplane: branch lineage deeper than 32")
		}
		m, _, err := g.Load(ctx, app, stream)
		if err != nil {
			return nil, err
		}
		rev = append(rev, m)
		if m.Parent == nil {
			break
		}
		stream = m.Parent.Stream
	}
	out := make([]*Manifest, len(rev))
	for i, m := range rev {
		out[len(rev)-1-i] = m
	}
	return out, nil
}

// ResolveTime maps a wall-clock instant to the newest LSN shipped at or
// before it (host clocks only — segment times are stamped by the shipper's
// host, never the guest).
func (g *Gateway) ResolveTime(ctx context.Context, app, stream string, at time.Time) (uint64, error) {
	path, err := g.lineage(ctx, app, stream)
	if err != nil {
		return 0, err
	}
	var lsn uint64
	upper := path[len(path)-1].HeadLSN
	for i := len(path) - 1; i >= 0; i-- {
		m := path[i]
		for _, s := range m.Segments {
			if !s.Time.After(at) && s.ToLSN <= upper && s.ToLSN > lsn {
				lsn = s.ToLSN
			}
		}
		if m.Parent != nil && m.Parent.LSN < upper {
			upper = m.Parent.LSN
		}
	}
	return lsn, nil
}

// CreateBranch forks a new stream at atLSN of fromStream — PITR as
// branching, never truncation (D4/PT-8): the parent manifest's history is
// untouched; the child references it as a prefix. Returns the child
// manifest.
func (g *Gateway) CreateBranch(ctx context.Context, app, fromStream, newStream string, atLSN uint64) (*Manifest, error) {
	parent, _, err := g.Load(ctx, app, fromStream)
	if err != nil {
		return nil, err
	}
	if atLSN > parent.HeadLSN {
		return nil, fmt.Errorf("dataplane: branch point %d beyond head %d", atLSN, parent.HeadLSN)
	}
	child, err := g.CreateStream(ctx, app, newStream,
		&Branch{Stream: fromStream, LSN: atLSN}, atLSN, parent.PageSize)
	if err != nil {
		return nil, err
	}
	// Record the branch on the parent (informational; CAS-retried, and a
	// lost race after child creation leaves only a missing back-pointer).
	for try := 0; ; try++ {
		p, gen, err := g.Load(ctx, app, fromStream)
		if err != nil {
			return child, err
		}
		p.Branches = append(p.Branches, Branch{Stream: newStream, LSN: atLSN})
		if _, err := g.Store.PutCAS(ctx, manifestKey(app, fromStream), encodeManifest(p), gen); err == nil {
			return child, nil
		} else if !errors.Is(err, objstore.ErrCASConflict) || try >= casRetries {
			return child, err
		}
	}
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
