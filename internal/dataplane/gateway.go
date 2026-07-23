package dataplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/sqlitewal"
)

// ErrFenced: the caller's epoch is stale. Rejection is a signal to the
// pushing host agent that its instance is a zombie: kill it, discard local
// state (E3's rejection clause; PT-1's resolution).
var ErrFenced = errors.New("dataplane: fenced: epoch is stale")

// ErrStreamExists guards double-creation of a stream.
var ErrStreamExists = errors.New("dataplane: stream already exists")

// Gateway is the WAL sink: every durable effect funnels through here, and
// every mutation is a manifest CAS against the object store (E4 — the
// store, not this code, is the arbiter of record; a Gateway instance is a
// cache and can be wrong for free).
//
// The two fencing booleans exist so the deterministic-simulation harness
// can reproduce the TLC counterexample traces (PT-1, the E6 bug, PT-9) by
// disabling one fence at a time, exactly like FencingProtocol.cfg's
// negative configs. Production construction (New) turns both on, always;
// nothing in krilld ever flips them.
type Gateway struct {
	Store objstore.Store
	// GatewayFencing is E3: reject appends and seals below CurEpoch.
	GatewayFencing bool
	// RegistrationFencing is E5: reject checkpoint manifests below CurEpoch.
	RegistrationFencing bool
	// Now is the host clock (injected for the sim's determinism).
	Now func() time.Time
}

// New is the ONLY production constructor: all fences on.
func New(store objstore.Store) *Gateway {
	return &Gateway{Store: store, GatewayFencing: true, RegistrationFencing: true, Now: time.Now}
}

// casRetries bounds manifest CAS races. Conflicts mean another writer moved
// the stream; every retry re-reads and re-applies fencing from scratch.
const casRetries = 8

// CreateStream initializes a stream manifest (generation-0 CAS: exactly one
// creator wins, even across hosts).
func (g *Gateway) CreateStream(ctx context.Context, app, stream string, parent *Branch, headLSN uint64, pageSize int) (*Manifest, error) {
	m := &Manifest{App: app, Stream: stream, Parent: parent, HeadLSN: headLSN, PageSize: pageSize}
	if _, err := g.Store.PutCAS(ctx, manifestKey(app, stream), encodeManifest(m), 0); err != nil {
		if errors.Is(err, objstore.ErrCASConflict) {
			return nil, ErrStreamExists
		}
		return nil, err
	}
	return m, nil
}

// Load fetches the manifest and its CAS generation.
func (g *Gateway) Load(ctx context.Context, app, stream string) (*Manifest, int64, error) {
	b, gen, err := g.Store.Get(ctx, manifestKey(app, stream))
	if err != nil {
		return nil, 0, err
	}
	m, err := decodeManifest(b)
	return m, gen, err
}

// SealTakeover is E6's first half and the load-bearing commitment from
// PT-9: the takeover seal is a fenced gateway WRITE, never elided as
// redundant with the lease. It returns the post-seal manifest and whether
// the caller's epoch was already stale at the store.
//
// Deciding what staleness MEANS belongs to the caller: production treats
// stale as fatal at wake (the belt-and-suspenders reject), while the
// simulation mirrors the spec's Rehydrate — which cannot reject — so that
// disabling RegistrationFencing exposes the slow waker exactly as TLC does.
func (g *Gateway) SealTakeover(ctx context.Context, app, stream string, epoch Epoch) (m *Manifest, stale bool, err error) {
	for try := 0; ; try++ {
		m, gen, err := g.Load(ctx, app, stream)
		if err != nil {
			return nil, false, err
		}
		if !g.GatewayFencing {
			// Unfenced gateway (sim negative config): no seal happens at all,
			// mirroring Rehydrate's curEpoch' = curEpoch when fencing is off.
			return m, false, nil
		}
		if epoch < m.CurEpoch {
			return m, true, nil
		}
		if epoch == m.CurEpoch {
			return m, false, nil // idempotent: we already own the stream
		}
		m.Seals = append(m.Seals, Seal{Epoch: epoch, AtLSN: m.HeadLSN, Time: g.Now().UTC()})
		m.CurEpoch = epoch
		if _, err := g.Store.PutCAS(ctx, manifestKey(app, stream), encodeManifest(m), gen); err == nil {
			return m, false, nil
		} else if !errors.Is(err, objstore.ErrCASConflict) || try >= casRetries {
			return nil, false, err
		}
	}
}

// AppendSegment ships committed WAL frames under epoch (E2/E3): accepted
// iff epoch >= CurEpoch, sealing forward when greater. The gateway assigns
// stream LSNs — frames always append at the manifest head. The segment
// object is written first, the manifest CAS second: a lost race leaves an
// unreferenced object (garbage, harmless — the same rule as unregistered
// snapshots).
func (g *Gateway) AppendSegment(ctx context.Context, app, stream string, epoch Epoch, frames []sqlitewal.Frame, pageSize int) (*Manifest, error) {
	if len(frames) == 0 {
		return nil, errors.New("dataplane: empty segment")
	}
	for try := 0; ; try++ {
		m, gen, err := g.Load(ctx, app, stream)
		if err != nil {
			return nil, err
		}
		if g.GatewayFencing && epoch < m.CurEpoch {
			return nil, fmt.Errorf("%w: append %s < current %s", ErrFenced, epoch, m.CurEpoch)
		}
		if m.PageSize != 0 && m.PageSize != pageSize {
			return nil, fmt.Errorf("dataplane: page size changed %d -> %d (rebase required)", m.PageSize, pageSize)
		}
		from, to := m.HeadLSN, m.HeadLSN+uint64(len(frames))
		key := segmentKey(app, stream, from, to, epoch)
		seg := encodeSegment(segHeader{
			App: app, Stream: stream, Kind: SegFrames, Epoch: epoch,
			FromLSN: from, ToLSN: to, PageSize: pageSize,
		}, frames, nil)
		if err := g.Store.Put(ctx, key, seg); err != nil {
			return nil, err
		}
		if g.GatewayFencing && epoch > m.CurEpoch {
			m.Seals = append(m.Seals, Seal{Epoch: epoch, AtLSN: m.HeadLSN, Time: g.Now().UTC()})
			m.CurEpoch = epoch
		}
		m.PageSize = pageSize
		m.HeadLSN = to
		m.Segments = append(m.Segments, Segment{
			Kind: SegFrames, Epoch: epoch, FromLSN: from, ToLSN: to, Key: key, Time: g.Now().UTC(),
		})
		if _, err := g.Store.PutCAS(ctx, manifestKey(app, stream), encodeManifest(m), gen); err == nil {
			return m, nil
		} else if !errors.Is(err, objstore.ErrCASConflict) || try >= casRetries {
			return nil, err
		}
	}
}

// AppendRebase ships a complete database image as one logical stream step —
// the recovery path for lost frame granularity (guest checkpointed its WAL
// between polls). Fencing is identical to AppendSegment.
func (g *Gateway) AppendRebase(ctx context.Context, app, stream string, epoch Epoch, image []byte, pageSize int) (*Manifest, error) {
	for try := 0; ; try++ {
		m, gen, err := g.Load(ctx, app, stream)
		if err != nil {
			return nil, err
		}
		if g.GatewayFencing && epoch < m.CurEpoch {
			return nil, fmt.Errorf("%w: rebase %s < current %s", ErrFenced, epoch, m.CurEpoch)
		}
		from, to := m.HeadLSN, m.HeadLSN+1
		key := segmentKey(app, stream, from, to, epoch) + ".rebase"
		seg := encodeSegment(segHeader{
			App: app, Stream: stream, Kind: SegRebase, Epoch: epoch,
			FromLSN: from, ToLSN: to, PageSize: pageSize,
		}, nil, image)
		if err := g.Store.Put(ctx, key, seg); err != nil {
			return nil, err
		}
		if g.GatewayFencing && epoch > m.CurEpoch {
			m.Seals = append(m.Seals, Seal{Epoch: epoch, AtLSN: m.HeadLSN, Time: g.Now().UTC()})
			m.CurEpoch = epoch
		}
		m.PageSize = pageSize
		m.HeadLSN = to
		m.Segments = append(m.Segments, Segment{
			Kind: SegRebase, Epoch: epoch, FromLSN: from, ToLSN: to, Key: key, Time: g.Now().UTC(),
		})
		if _, err := g.Store.PutCAS(ctx, manifestKey(app, stream), encodeManifest(m), gen); err == nil {
			return m, nil
		} else if !errors.Is(err, objstore.ErrCASConflict) || try >= casRetries {
			return nil, err
		}
	}
}

// RegisterCheckpoint registers a full-database image at an exact stream LSN
// (E5). Registration is fenced at the CAS: an epoch below CurEpoch AT THE
// MOMENT OF THE CAS is rejected — the slow waker's forged lineage dies
// here, and on its path this is the ONLY fence (PT-9). The blob is written
// before the CAS; if registration is rejected the blob is garbage by rule.
func (g *Gateway) RegisterCheckpoint(ctx context.Context, app, stream string, epoch Epoch, lsn uint64, image []byte) (*Manifest, error) {
	key := checkpointKey(app, stream, lsn, epoch)
	if err := g.Store.Put(ctx, key, image); err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		m, gen, err := g.Load(ctx, app, stream)
		if err != nil {
			return nil, err
		}
		if g.RegistrationFencing && epoch < m.CurEpoch {
			return nil, fmt.Errorf("%w: checkpoint registration %s < current %s", ErrFenced, epoch, m.CurEpoch)
		}
		if lsn > m.HeadLSN {
			return nil, fmt.Errorf("dataplane: checkpoint LSN %d beyond head %d", lsn, m.HeadLSN)
		}
		m.Checkpoints = append(m.Checkpoints, Checkpoint{Epoch: epoch, LSN: lsn, Key: key, Time: g.Now().UTC()})
		if _, err := g.Store.PutCAS(ctx, manifestKey(app, stream), encodeManifest(m), gen); err == nil {
			return m, nil
		} else if !errors.Is(err, objstore.ErrCASConflict) || try >= casRetries {
			return nil, err
		}
	}
}

// FetchSegment retrieves and decodes one segment object, verifying it
// against the manifest entry that referenced it.
func (g *Gateway) FetchSegment(ctx context.Context, s Segment) ([]sqlitewal.Frame, []byte, error) {
	b, _, err := g.Store.Get(ctx, s.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("dataplane: fetching %s: %w", s.Key, err)
	}
	h, frames, image, err := decodeSegment(b)
	if err != nil {
		return nil, nil, err
	}
	if h.Kind != s.Kind || h.FromLSN != s.FromLSN || h.ToLSN != s.ToLSN || h.Epoch != s.Epoch {
		return nil, nil, fmt.Errorf("dataplane: segment %s does not match its manifest entry", s.Key)
	}
	return frames, image, nil
}

// FetchCheckpoint retrieves a registered checkpoint image.
func (g *Gateway) FetchCheckpoint(ctx context.Context, c Checkpoint) ([]byte, error) {
	b, _, err := g.Store.Get(ctx, c.Key)
	if err != nil {
		return nil, fmt.Errorf("dataplane: fetching checkpoint %s: %w", c.Key, err)
	}
	return b, nil
}
