// Package sim is the deterministic-simulation harness: FencingProtocol.tla
// run against the REAL data plane. Virtual hosts execute the spec's actions
// (AcquireLease, Rehydrate, Write, BeginSeal, Register, LeaseExpire, Crash,
// Restart) with every externally visible effect going through the real
// Gateway → manifest → object-store code, and the spec's invariants are
// asserted after every single step:
//
//	I1 EpochsMonotoneInWAL, I3 SnapshotsOnHistory  — via CheckManifest,
//	   read off the actual manifest bytes in the store
//	I2 AckedDurable + WriterAtHead, OneInstancePerEpoch, CurEpochBounded
//	   — computed from host state + the actual manifest
//
// Pause is modeled the way the spec models it: a host the scheduler does
// not pick IS paused, so every schedule containing a gap is a pause test —
// including "lease expires and a successor takes over while the holder is
// frozen mid-anything". Crashes, restarts, lease expiry, and object-store
// partitions (fail-closed: the op has no effect) are explicit actions.
//
// Interleaving granularity is the spec's action granularity — the same
// atomicity TLC explores. Sub-action interleavings (mid-CAS races) are
// serialized by the object store's CAS itself and covered by unit tests.
//
// The three fence toggles reproduce FencingProtocol.cfg's negative
// configs: disabling any one of them must let the harness find a violation
// (PT-1, the E6 stale-restore bug, PT-9's slow waker) — a fence that
// cannot fail when disabled is a fence the harness no longer exercises.
package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/shulman33/krill/internal/dataplane"
	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/sqlitewal"
)

// simFrame fabricates one committed 512-byte frame whose page bytes encode
// the writing epoch and intended LSN — synthetic content, real plumbing.
func simFrame(e dataplane.Epoch, lsn uint64) []sqlitewal.Frame {
	page := make([]byte, 512)
	copy(page, fmt.Sprintf("frame:%s:%d", e, lsn))
	return []sqlitewal.Frame{{Pgno: 1, Commit: 1, Data: page}}
}

type hostState string

const (
	stIdle        hostState = "IDLE"
	stRehydrating hostState = "REHYDRATING"
	stActive      hostState = "ACTIVE"
	stSealing     hostState = "SEALING"
	stDead        hostState = "DEAD"
)

type Config struct {
	Hosts     int
	MaxEpoch  uint32 // bound on lease acquisitions (mint counter)
	MaxWrites int    // bound on stream length
	Steps     int    // schedule length

	GatewayFencing      bool // E3
	RegistrationFencing bool // E5
	ReplayOnRestore     bool // E6
}

// Default mirrors the spirit of FencingProtocol.cfg, slightly widened.
func Default() Config {
	return Config{
		Hosts: 3, MaxEpoch: 6, MaxWrites: 8, Steps: 80,
		GatewayFencing: true, RegistrationFencing: true, ReplayOnRestore: true,
	}
}

type Result struct {
	Seed      int64
	Steps     int
	Violation error
	Trace     []string
}

type host struct {
	state   hostState
	epoch   dataplane.Epoch
	lsn     uint64
	pendE   dataplane.Epoch // manifest captured at BeginSeal
	pendLSN uint64
}

type sim struct {
	cfg   Config
	rng   *rand.Rand
	gw    *dataplane.Gateway
	store *faultStore

	mint        uint32 // control plane's epoch counter (E1)
	leaseHolder int    // -1 = None
	hosts       []host
	acked       []uint64
	partitioned []bool
	trace       []string
}

const app, stream = "app", "s0"

// Run executes one seeded schedule and returns the first invariant
// violation, if any, with the action trace that produced it.
func Run(seed int64, cfg Config) Result {
	rng := rand.New(rand.NewSource(seed))
	fs := &faultStore{inner: objstore.NewMem()}
	gw := &dataplane.Gateway{
		Store:               fs,
		GatewayFencing:      cfg.GatewayFencing,
		RegistrationFencing: cfg.RegistrationFencing,
	}
	tick := 0
	gw.Now = func() time.Time { tick++; return time.Unix(int64(tick), 0).UTC() }

	s := &sim{
		cfg: cfg, rng: rng, gw: gw, store: fs,
		leaseHolder: -1,
		hosts:       make([]host, cfg.Hosts),
		partitioned: make([]bool, cfg.Hosts),
	}
	for i := range s.hosts {
		s.hosts[i].state = stIdle
	}
	ctx := context.Background()
	if _, err := gw.CreateStream(ctx, app, stream, nil, 0, 512); err != nil {
		return Result{Seed: seed, Violation: fmt.Errorf("init: %w", err)}
	}
	// The spec's Init: snaps = {(epoch 0, lsn 0)} — the deploy-time empty
	// checkpoint, registered through the real API.
	if _, err := gw.RegisterCheckpoint(ctx, app, stream, 0, 0, []byte("empty")); err != nil {
		return Result{Seed: seed, Violation: fmt.Errorf("init checkpoint: %w", err)}
	}

	for step := 0; step < cfg.Steps; step++ {
		if !s.step(ctx) {
			break // no action enabled (epoch/write bounds exhausted)
		}
		if err := s.checkInvariants(ctx); err != nil {
			return Result{Seed: seed, Steps: step + 1, Violation: err, Trace: s.trace}
		}
	}
	return Result{Seed: seed, Steps: cfg.Steps}
}

type action struct {
	name string
	h    int
	run  func(ctx context.Context)
}

// step gathers every enabled action (spec's Next relation) and runs one.
func (s *sim) step(ctx context.Context) bool {
	m, _, err := s.gw.Load(ctx, app, stream)
	if err != nil {
		return false // cannot happen with the mem store outside a partition
	}
	var acts []action

	if s.leaseHolder >= 0 {
		acts = append(acts, action{name: "LeaseExpire", h: -1, run: func(context.Context) {
			s.leaseHolder = -1
		}})
	}
	for i := range s.hosts {
		i := i
		h := &s.hosts[i]
		switch h.state {
		case stIdle:
			if s.leaseHolder < 0 && s.mint < s.cfg.MaxEpoch {
				acts = append(acts, action{name: "AcquireLease", h: i, run: func(context.Context) {
					s.mint++
					s.leaseHolder = i
					h.epoch = dataplane.NewEpoch(1, s.mint)
					h.state = stRehydrating
				}})
			}
		case stRehydrating:
			acts = append(acts, action{name: "Rehydrate", h: i, run: s.rehydrate(i)})
		case stActive:
			if int(m.HeadLSN) < s.cfg.MaxWrites {
				acts = append(acts, action{name: "Write", h: i, run: s.write(i)})
			}
			acts = append(acts, action{name: "BeginSeal", h: i, run: func(context.Context) {
				h.pendE, h.pendLSN = h.epoch, h.lsn
				h.state = stSealing
			}})
		case stSealing:
			acts = append(acts, action{name: "Register", h: i, run: s.register(i)})
		}
		if h.state == stRehydrating || h.state == stActive || h.state == stSealing {
			acts = append(acts, action{name: "Crash", h: i, run: func(context.Context) {
				h.state = stDead
			}})
		}
		if h.state == stDead {
			acts = append(acts, action{name: "Restart", h: i, run: func(context.Context) {
				*h = host{state: stIdle}
			}})
		}
		// Partition toggles are always available: they model the store
		// becoming unreachable from this host (PT-1's setup).
		acts = append(acts, action{name: "TogglePartition", h: i, run: func(context.Context) {
			s.partitioned[i] = !s.partitioned[i]
		}})
	}
	if len(acts) == 0 {
		return false
	}
	a := acts[s.rng.Intn(len(acts))]
	s.store.failing = a.h >= 0 && s.partitioned[a.h]
	a.run(ctx)
	s.store.failing = false
	who := "cp"
	if a.h >= 0 {
		who = fmt.Sprintf("h%d", a.h)
	}
	s.trace = append(s.trace, fmt.Sprintf("%s: %s", who, a.name))
	return true
}

// rehydrate is E6: seal the stream at the new epoch (a fenced gateway
// write), pick a registered checkpoint, and — replay toggle permitting —
// apply the WAL delta to head before serving. The spec's Rehydrate cannot
// reject a stale sealer (production adds that as belt-and-suspenders; the
// harness must not, or disabling E5 could never expose the slow waker).
func (s *sim) rehydrate(i int) func(ctx context.Context) {
	return func(ctx context.Context) {
		h := &s.hosts[i]
		m, _, err := s.gw.SealTakeover(ctx, app, stream, h.epoch)
		if err != nil {
			return // store unreachable: the action had no effect (retry later)
		}
		ck := m.Checkpoints[s.rng.Intn(len(m.Checkpoints))]
		if s.cfg.ReplayOnRestore {
			h.lsn = m.HeadLSN
		} else {
			h.lsn = ck.LSN
		}
		h.state = stActive
	}
}

// write is WriteOK/WriteFenced: one frame through the real gateway. The
// gateway's E3 decides acceptance; rejection kills the instance (PT-1).
func (s *sim) write(i int) func(ctx context.Context) {
	return func(ctx context.Context) {
		h := &s.hosts[i]
		frame := simFrame(h.epoch, h.lsn+1)
		m, err := s.gw.AppendSegment(ctx, app, stream, h.epoch, frame, 512)
		switch {
		case err == nil:
			h.lsn = m.HeadLSN
			s.acked = append(s.acked, m.HeadLSN) // D1: ack at acceptance, never before
		case errors.Is(err, dataplane.ErrFenced):
			h.state = stDead // E3 rejection: the instance is a zombie
		default:
			// partition or transient store failure: no effect
		}
	}
}

// register is RegisterOK/RegisterFenced: E5 at the real gateway.
func (s *sim) register(i int) func(ctx context.Context) {
	return func(ctx context.Context) {
		h := &s.hosts[i]
		img := []byte(fmt.Sprintf("ckpt-%s-%d", h.pendE, h.pendLSN))
		_, err := s.gw.RegisterCheckpoint(ctx, app, stream, h.pendE, h.pendLSN, img)
		switch {
		case err == nil:
			if s.leaseHolder == i {
				s.leaseHolder = -1
			}
			*h = host{state: stIdle}
		case errors.Is(err, dataplane.ErrFenced):
			h.state = stDead
		default:
		}
	}
}

// checkInvariants reads the REAL manifest back from the store and asserts
// everything the spec asserts.
func (s *sim) checkInvariants(ctx context.Context) error {
	m, _, err := s.gw.Load(ctx, app, stream)
	if err != nil {
		return fmt.Errorf("manifest unreadable: %w", err)
	}
	// I1 + I3 + segment-chain integrity + E4 sanity, off the manifest.
	if err := dataplane.CheckManifest(m); err != nil {
		return err
	}
	// I2 part 1 — AckedDurable: every acknowledged LSN is durable history.
	for _, l := range s.acked {
		if l > m.HeadLSN {
			return fmt.Errorf("AckedDurable violated: acked LSN %d beyond head %d", l, m.HeadLSN)
		}
	}
	// I2 part 2 / I4 — WriterAtHead: a current-epoch ACTIVE instance saw
	// every acknowledged write. This is the invariant that dies when E6's
	// replay is skipped.
	for i, h := range s.hosts {
		if h.state == stActive && h.epoch >= m.CurEpoch && h.lsn != m.HeadLSN {
			return fmt.Errorf("WriterAtHead violated: h%d epoch %s at lsn %d, head %d", i, h.epoch, h.lsn, m.HeadLSN)
		}
	}
	// E1 sanity — OneInstancePerEpoch.
	live := 0
	for _, h := range s.hosts {
		if (h.state == stRehydrating || h.state == stActive || h.state == stSealing) &&
			h.epoch == dataplane.NewEpoch(1, s.mint) && h.epoch > 0 {
			live++
		}
	}
	if live > 1 {
		return fmt.Errorf("OneInstancePerEpoch violated: %d live instances at epoch %d", live, s.mint)
	}
	// E4 sanity — CurEpochBounded.
	if m.CurEpoch > dataplane.NewEpoch(1, s.mint) {
		return fmt.Errorf("CurEpochBounded violated: manifest %s ahead of mint %d", m.CurEpoch, s.mint)
	}
	return nil
}

// faultStore fails every operation while failing is set — a partition
// between the acting host and the object store. Fail-closed: the inner
// store is never touched, so a partitioned op has no effect at all (the
// spec's atomicity; partial effects would need a finer-grained spec).
type faultStore struct {
	inner   objstore.Store
	failing bool
}

var errPartitioned = errors.New("sim: partitioned from object store")

func (f *faultStore) Get(ctx context.Context, key string) ([]byte, int64, error) {
	if f.failing {
		return nil, 0, errPartitioned
	}
	return f.inner.Get(ctx, key)
}
func (f *faultStore) Put(ctx context.Context, key string, data []byte) error {
	if f.failing {
		return errPartitioned
	}
	return f.inner.Put(ctx, key, data)
}
func (f *faultStore) PutCAS(ctx context.Context, key string, data []byte, expect int64) (int64, error) {
	if f.failing {
		return 0, errPartitioned
	}
	return f.inner.PutCAS(ctx, key, data, expect)
}
func (f *faultStore) List(ctx context.Context, prefix string) ([]string, error) {
	if f.failing {
		return nil, errPartitioned
	}
	return f.inner.List(ctx, prefix)
}
func (f *faultStore) Delete(ctx context.Context, key string) error {
	if f.failing {
		return errPartitioned
	}
	return f.inner.Delete(ctx, key)
}
