// Package lifecycle owns the M1 state machine:
//
//	COLD ──boot──▶ BOOTING ──▶ ACTIVE ──idle──▶ SNAPSHOTTING ──▶ FROZEN
//	FROZEN ──request──▶ WAKING ──▶ ACTIVE            (warm path, the product)
//
// This is the single-host simplification of the protocol docs' machine: no
// leases, no epochs. Single-instance-per-app is enforced by a process-local
// mutex; the fencing machinery arrives in M3, where more than one host makes
// it mean something.
//
// The one safety rule that already exists in M1 (benchmark §3.2): a memory
// snapshot may only ever be restored against the exact disk bytes it was
// paused with. Every transition that lets the disk mutate outside a restore
// marks the snapshot invalid in the registry FIRST.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/shulman33/krill/internal/registry"
)

type State string

const (
	StateCold         State = "COLD"         // registered; wake = cold boot
	StateBooting      State = "BOOTING"      // cold boot in flight
	StateWaking       State = "WAKING"       // snapshot restore in flight
	StateActive       State = "ACTIVE"       // guest running, serving
	StateSnapshotting State = "SNAPSHOTTING" // freeze in flight
	StateFrozen       State = "FROZEN"       // no process, no RAM; snapshot on disk
	StateDeploying    State = "DEPLOYING"    // golden/disk being replaced (M2 deploy path)
)

var (
	ErrNotActive = errors.New("app is not active")
	ErrBusy      = errors.New("app is busy")
)

// Instance is a running microVM, as far as the supervisor cares.
type Instance interface {
	Kill()
}

// Backend performs machine operations for one app. The production backend
// composes Firecracker + taps + disk images; tests substitute a fake so the
// state machine is exercised without KVM.
type Backend interface {
	// Install lays down host state for a new app (dirs, golden image, tap).
	Install(app registry.App, goldenSrc string) error
	// Prepare is the idempotent subset of Install run on every daemon start.
	Prepare(app registry.App) error
	// HasSnapshot reports whether snapshot files exist on disk.
	HasSnapshot(app registry.App) bool
	// ColdBoot launches a fresh VMM and boots the app's disk.
	ColdBoot(ctx context.Context, app registry.App) (Instance, error)
	// Restore launches a fresh VMM and resumes the app's snapshot.
	Restore(ctx context.Context, app registry.App) (Instance, error)
	// Snapshot balloons, pauses and snapshots a running instance. It does
	// not kill the instance — the supervisor does, unconditionally.
	Snapshot(ctx context.Context, app registry.App, inst Instance) error
	// GuestAddr is the host:port the app serves on (probe + proxy target).
	GuestAddr(app registry.App) string
	// Replace swaps in a new golden image and resets the app's disk and
	// snapshot files. Only called with no instance running and the snapshot
	// already invalidated in the registry.
	Replace(app registry.App, goldenSrc string) error
	// SerialLogPath is where the guest's console output lands (the logs API).
	SerialLogPath(app registry.App) string
	// Purge removes all host state for a deleted app.
	Purge(app registry.App) error
}

type Config struct {
	WakeTimeout   time.Duration
	FreezeTimeout time.Duration
	IdleTimeout   time.Duration
	// JanitorInterval defaults to 1s; tests shrink it.
	JanitorInterval time.Duration
}

func (c Config) janitorTick() time.Duration {
	if c.JanitorInterval > 0 {
		return c.JanitorInterval
	}
	return time.Second
}

type Supervisor struct {
	reg *registry.Registry
	be  Backend
	cfg Config
	log *slog.Logger

	mu   sync.Mutex
	apps map[string]*appState
}

type appState struct {
	name string

	mu         sync.Mutex
	state      State
	inst       Instance
	wakeDone   chan struct{} // non-nil while a boot/restore is in flight
	wakeErr    error         // outcome of the last completed wake attempt
	freezeDone chan struct{} // non-nil while a freeze is in flight
	lastUsed   time.Time
	inflight   int
	lastWake   time.Duration // duration of the most recent successful wake
	gone       bool
}

func New(reg *registry.Registry, be Backend, cfg Config, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{reg: reg, be: be, cfg: cfg, log: log, apps: map[string]*appState{}}
}

// Reconcile is called once at startup: reload the registry, demote every app
// that was mid-flight when the previous daemon died, and re-create host
// state (taps) that does not survive reboots.
func (s *Supervisor) Reconcile() error {
	apps, err := s.reg.List()
	if err != nil {
		return err
	}
	for _, a := range apps {
		if err := s.be.Prepare(a); err != nil {
			return fmt.Errorf("prepare %s: %w", a.Name, err)
		}
		st := StateCold
		if State(a.State) == StateFrozen && a.SnapshotValid && s.be.HasSnapshot(a) {
			st = StateFrozen
		}
		if st == StateCold {
			// Anything not cleanly FROZEN may have mutated its disk after the
			// snapshot was taken. The snapshot is dead; boot from disk.
			if err := s.reg.SetSnapshotValid(a.Name, false); err != nil {
				return err
			}
		}
		if State(a.State) != st {
			s.log.Warn("reconcile: demoting app", "app", a.Name, "was", a.State, "now", st)
		}
		if err := s.reg.SetState(a.Name, string(st)); err != nil {
			return err
		}
		s.apps[a.Name] = &appState{name: a.Name, state: st, lastUsed: time.Now()}
	}
	return nil
}

func (s *Supervisor) app(name string) (*appState, registry.App, error) {
	meta, err := s.reg.Get(name)
	if err != nil {
		return nil, registry.App{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[name]
	if !ok {
		a = &appState{name: name, state: State(meta.State), lastUsed: time.Now()}
		s.apps[name] = a
	}
	return a, meta, nil
}

// Acquire returns the guest address with the app guaranteed ACTIVE, plus a
// release func the caller must invoke when its request completes. Concurrent
// callers during a wake share one boot (single flight); callers during a
// freeze wait it out and then trigger the wake.
func (s *Supervisor) Acquire(ctx context.Context, name string) (string, func(), error) {
	a, meta, err := s.app(name)
	if err != nil {
		return "", nil, err
	}
	// Bound the whole affair: worst case is waiting out a freeze and then
	// performing a wake.
	ctx, cancel := context.WithTimeout(ctx, s.cfg.FreezeTimeout+s.cfg.WakeTimeout)
	defer cancel()

	for {
		a.mu.Lock()
		if a.gone {
			a.mu.Unlock()
			return "", nil, registry.ErrNotFound
		}
		switch a.state {
		case StateActive:
			a.inflight++
			a.lastUsed = time.Now()
			a.mu.Unlock()
			var once sync.Once
			release := func() {
				once.Do(func() {
					a.mu.Lock()
					a.inflight--
					a.lastUsed = time.Now()
					a.mu.Unlock()
				})
			}
			return s.be.GuestAddr(meta), release, nil

		case StateBooting, StateWaking:
			ch := a.wakeDone
			a.mu.Unlock()
			select {
			case <-ch:
				a.mu.Lock()
				err := a.wakeErr
				a.mu.Unlock()
				if err != nil {
					return "", nil, fmt.Errorf("wake %s: %w", name, err)
				}
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}

		case StateSnapshotting, StateDeploying:
			ch := a.freezeDone
			a.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}

		case StateFrozen, StateCold:
			from := a.state
			target := StateWaking
			if from == StateCold {
				target = StateBooting
			}
			a.state = target
			a.wakeDone = make(chan struct{})
			a.wakeErr = nil
			a.mu.Unlock()
			s.performWake(a, meta, from)
			a.mu.Lock()
			err := a.wakeErr
			a.mu.Unlock()
			if err != nil {
				return "", nil, fmt.Errorf("wake %s: %w", name, err)
			}

		default:
			a.mu.Unlock()
			return "", nil, fmt.Errorf("app %s in unexpected state", name)
		}
	}
}

// performWake runs the boot or restore. It deliberately uses its own context
// rather than the requester's: once a wake starts, other requests may be
// waiting on it, and an abandoned half-restore would leave the disk
// untrustworthy for nothing.
func (s *Supervisor) performWake(a *appState, meta registry.App, from State) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.WakeTimeout)
	defer cancel()

	_ = s.reg.SetState(meta.Name, string(map[State]State{StateFrozen: StateWaking, StateCold: StateBooting}[from]))

	var inst Instance
	var err error
	if from == StateFrozen {
		inst, err = s.be.Restore(ctx, meta)
	} else {
		// Booting mutates the disk. Any leftover snapshot (e.g. from a crash
		// demotion) must be invalidated BEFORE the guest can write.
		if err = s.reg.SetSnapshotValid(meta.Name, false); err == nil {
			inst, err = s.be.ColdBoot(ctx, meta)
		}
	}
	if err == nil {
		err = probeReady(ctx, s.be.GuestAddr(meta))
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		if inst != nil {
			inst.Kill()
		}
		if from == StateFrozen {
			// The restore may have partially resumed the guest; the disk can
			// no longer be trusted to match the snapshot. Next wake cold-boots.
			_ = s.reg.SetSnapshotValid(meta.Name, false)
		}
		a.state = StateCold
		_ = s.reg.SetState(meta.Name, string(StateCold))
		a.wakeErr = err
		s.log.Error("wake failed", "app", meta.Name, "from", from, "err", err)
	} else {
		a.inst = inst
		a.state = StateActive
		a.lastUsed = time.Now()
		a.lastWake = time.Since(start)
		_ = s.reg.SetState(meta.Name, string(StateActive))
		s.log.Info("wake", "app", meta.Name, "from", from, "ms", time.Since(start).Milliseconds())
	}
	close(a.wakeDone)
	a.wakeDone = nil
}

// Freeze snapshots an ACTIVE, idle app and kills its VMM. Returns ErrBusy if
// requests are in flight, ErrNotActive if there is nothing to freeze.
func (s *Supervisor) Freeze(name string) error {
	a, meta, err := s.app(name)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.state != StateActive {
		a.mu.Unlock()
		return ErrNotActive
	}
	if a.inflight > 0 {
		n := a.inflight
		a.mu.Unlock()
		s.log.Info("freeze rejected: requests in flight", "app", name, "inflight", n)
		return ErrBusy
	}
	inst := a.inst
	a.state = StateSnapshotting
	a.freezeDone = make(chan struct{})
	a.mu.Unlock()

	start := time.Now()
	// Ordering matters: the snapshot files on disk are about to be replaced,
	// so the pairing bit goes false before Firecracker writes a byte.
	_ = s.reg.SetSnapshotValid(name, false)
	_ = s.reg.SetState(name, string(StateSnapshotting))

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.FreezeTimeout)
	err = s.be.Snapshot(ctx, meta, inst)
	cancel()
	inst.Kill() // unconditionally: freeing the RAM is the point

	a.mu.Lock()
	a.inst = nil
	if err != nil {
		// Snapshot failed but the disk is intact: the app is COLD, not broken.
		a.state = StateCold
		_ = s.reg.SetState(name, string(StateCold))
		s.log.Error("freeze failed; app demoted to COLD", "app", name, "err", err)
	} else {
		a.state = StateFrozen
		_ = s.reg.SetSnapshotValid(name, true)
		_ = s.reg.SetState(name, string(StateFrozen))
		s.log.Info("freeze", "app", name, "ms", time.Since(start).Milliseconds())
	}
	close(a.freezeDone)
	a.freezeDone = nil
	a.mu.Unlock()
	return err
}

// RunJanitor demotes idle ACTIVE apps to FROZEN until ctx is done. This loop
// is what makes the platform scale-to-zero unattended.
func (s *Supervisor) RunJanitor(ctx context.Context) {
	t := time.NewTicker(s.cfg.janitorTick())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, a := range s.snapshotApps() {
			a.mu.Lock()
			idle := a.state == StateActive && a.inflight == 0 &&
				time.Since(a.lastUsed) >= s.cfg.IdleTimeout
			a.mu.Unlock()
			if idle {
				// Freeze re-checks state under lock; a request that sneaks in
				// wins via ErrBusy and the app stays up.
				name := a.name
				go func() {
					if err := s.Freeze(name); err != nil &&
						!errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotActive) {
						s.log.Error("janitor freeze", "app", name, "err", err)
					}
				}()
			}
		}
	}
}

// FreezeAll snapshots every ACTIVE app; used for graceful shutdown so acked
// writes sitting in guest RAM reach a snapshot instead of dying with the
// daemon.
func (s *Supervisor) FreezeAll() {
	var wg sync.WaitGroup
	for _, a := range s.snapshotApps() {
		a.mu.Lock()
		active := a.state == StateActive
		a.mu.Unlock()
		if active {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				if err := s.Freeze(name); err != nil {
					s.log.Error("shutdown freeze", "app", name, "err", err)
				}
			}(a.name)
		}
	}
	wg.Wait()
}

func (s *Supervisor) snapshotApps() []*appState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*appState, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, a)
	}
	return out
}

// Register creates the app end-to-end: registry row, host dirs + golden
// image, tap. New apps start COLD.
func (s *Supervisor) Register(spec registry.App, goldenSrc string) (registry.App, error) {
	spec.State = string(StateCold)
	meta, err := s.reg.Create(spec)
	if err != nil {
		return registry.App{}, err
	}
	if err := s.be.Install(meta, goldenSrc); err != nil {
		_ = s.reg.Delete(meta.Name) // roll back the row; host state is purged below
		_ = s.be.Purge(meta)
		return registry.App{}, err
	}
	s.mu.Lock()
	// Only-if-absent: a racing Acquire may have lazily created the entry
	// from the just-committed registry row; two entries would mean two VMs.
	if _, ok := s.apps[meta.Name]; !ok {
		s.apps[meta.Name] = &appState{name: meta.Name, state: StateCold, lastUsed: time.Now()}
	}
	s.mu.Unlock()
	s.log.Info("registered", "app", meta.Name, "subnet", meta.SubnetIdx)
	return meta, nil
}

// Deploy is the M2 path: install a freshly built golden image under a name.
// A new name registers; an existing name is redeployed in place — same
// subnet, same IP, new code, reset disk. Returns the app and whether it was
// created (vs replaced).
//
// Redeploy resets the app's disk: whatever the old code wrote is gone. That
// is the documented M2 contract — durable app data is the data plane's job
// (M3), not the deploy path's.
func (s *Supervisor) Deploy(ctx context.Context, spec registry.App, goldenSrc string) (registry.App, bool, error) {
	if _, err := s.reg.Get(spec.Name); errors.Is(err, registry.ErrNotFound) {
		meta, err := s.Register(spec, goldenSrc)
		return meta, true, err
	} else if err != nil {
		return registry.App{}, false, err
	}
	meta, err := s.redeploy(ctx, spec, goldenSrc)
	return meta, false, err
}

// redeploy claims the app with the DEPLOYING guard state (requests arriving
// meanwhile wait, then cold-boot the new code), kills any running instance,
// and replaces golden + disk. In-flight requests lose to a deploy: this is
// an explicit operator action, and small-software requests are small.
func (s *Supervisor) redeploy(ctx context.Context, spec registry.App, goldenSrc string) (registry.App, error) {
	a, _, err := s.app(spec.Name)
	if err != nil {
		return registry.App{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.FreezeTimeout+s.cfg.WakeTimeout)
	defer cancel()

	for {
		a.mu.Lock()
		if a.gone {
			a.mu.Unlock()
			return registry.App{}, registry.ErrNotFound
		}
		var ch chan struct{}
		switch a.state {
		case StateBooting, StateWaking:
			ch = a.wakeDone
		case StateSnapshotting, StateDeploying:
			ch = a.freezeDone
		}
		if ch == nil {
			break // quiescent (COLD/FROZEN) or ACTIVE — claimable, still locked
		}
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return registry.App{}, fmt.Errorf("deploy %s: app busy: %w", spec.Name, ctx.Err())
		}
	}
	if a.inst != nil {
		a.inst.Kill()
		a.inst = nil
	}
	a.state = StateDeploying
	a.freezeDone = make(chan struct{})
	a.mu.Unlock()

	finish := func() {
		a.mu.Lock()
		a.state = StateCold
		close(a.freezeDone)
		a.freezeDone = nil
		a.mu.Unlock()
		_ = s.reg.SetState(spec.Name, string(StateCold))
	}

	// Ordering, same rule as everywhere: the pairing bit goes false before
	// anything can touch golden, disk, or the snapshot files.
	_ = s.reg.SetState(spec.Name, string(StateDeploying))
	if err := s.reg.SetSnapshotValid(spec.Name, false); err != nil {
		finish()
		return registry.App{}, err
	}
	if err := s.reg.UpdateSpec(spec); err != nil {
		finish()
		return registry.App{}, err
	}
	meta, err := s.reg.Get(spec.Name)
	if err != nil {
		finish()
		return registry.App{}, err
	}
	if err := s.be.Replace(meta, goldenSrc); err != nil {
		// Golden replacement is atomic (old or new, never half): the app is
		// still bootable, just possibly on the old code. COLD either way.
		finish()
		return registry.App{}, fmt.Errorf("replace %s: %w", spec.Name, err)
	}
	finish()
	s.log.Info("redeployed", "app", spec.Name)
	return meta, nil
}

// SerialLogPath exposes where an app's console output lives (the logs API).
func (s *Supervisor) SerialLogPath(name string) (string, error) {
	meta, err := s.reg.Get(name)
	if err != nil {
		return "", err
	}
	return s.be.SerialLogPath(meta), nil
}

// Delete removes an app entirely. A running instance is killed, not frozen —
// deletion is not a graceful path.
func (s *Supervisor) Delete(name string) error {
	a, meta, err := s.app(name)
	if err != nil {
		return err
	}
	a.mu.Lock()
	switch a.state {
	case StateBooting, StateWaking, StateSnapshotting, StateDeploying:
		a.mu.Unlock()
		return ErrBusy
	case StateActive:
		if a.inflight > 0 {
			a.mu.Unlock()
			return ErrBusy
		}
		a.inst.Kill()
		a.inst = nil
	}
	a.gone = true
	a.mu.Unlock()

	if err := s.reg.Delete(name); err != nil {
		return err
	}
	if err := s.be.Purge(meta); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.apps, name)
	s.mu.Unlock()
	s.log.Info("deleted", "app", name)
	return nil
}

// Status is the admin API's view of one app.
type Status struct {
	Name          string    `json:"name"`
	State         State     `json:"state"`
	SnapshotValid bool      `json:"snapshot_valid"`
	SubnetIdx     int       `json:"subnet_idx"`
	GuestAddr     string    `json:"guest_addr"`
	VCPUs         int       `json:"vcpus"`
	MemMiB        int       `json:"mem_mib"`
	Inflight      int       `json:"inflight"`
	LastUsed      time.Time `json:"last_used"`
	LastWakeMs    int64     `json:"last_wake_ms"`
}

func (s *Supervisor) Status(name string) (Status, error) {
	a, meta, err := s.app(name)
	if err != nil {
		return Status{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return Status{
		Name:          meta.Name,
		State:         a.state,
		SnapshotValid: meta.SnapshotValid,
		SubnetIdx:     meta.SubnetIdx,
		GuestAddr:     s.be.GuestAddr(meta),
		VCPUs:         meta.VCPUs,
		MemMiB:        meta.MemMiB,
		Inflight:      a.inflight,
		LastUsed:      a.lastUsed,
		LastWakeMs:    a.lastWake.Milliseconds(),
	}, nil
}

func (s *Supervisor) StatusAll() ([]Status, error) {
	metas, err := s.reg.List()
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(metas))
	for _, m := range metas {
		st, err := s.Status(m.Name)
		if err != nil {
			continue // deleted between List and Status; fine
		}
		out = append(out, st)
	}
	return out, nil
}

// probeReady polls a TCP connect until the guest accepts. For a restored
// snapshot the listening socket is already in the resumed memory image, so
// the first dial lands; cold boots poll until the app is up. All timing is
// host-side — guest clocks jump across resume.
func probeReady(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 200 * time.Millisecond}
	for {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest %s never became reachable: %w", addr, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
