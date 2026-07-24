package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shulman33/krill/internal/registry"
)

// ControlPlane is the E1 mint: a strictly monotone per-app epoch counter in
// a linearizable store. *registry.Registry implements it.
type ControlPlane interface {
	MintEpoch(app string) (uint32, error)
	SetStreamID(app, stream string) error
}

// Disks is the slice of the rootfs manager the data plane needs.
type Disks interface {
	DataDiskPath(name string) string
	ShipCursorPath(name string) string
	BuildDataDisk(name string, files map[string][]byte, sizeMB int) error
	HasDataDisk(name string) bool
}

// Coordinator wires the data plane into krilld's lifecycle: it implements
// the supervisor's DataPlane hooks. One per daemon.
//
// The wake path (E6, in order): mint a fresh epoch → fenced takeover seal
// (stale = the wake DIES — the PT-9 belt-and-suspenders) → reconcile the
// local data disk against the stream head (rebuild = checkpoint + WAL-delta
// replay when they diverge) → only then may the instance serve.
type Coordinator struct {
	GW      *Gateway
	CP      ControlPlane
	Disks   Disks
	CellGen uint32
	// DBPath is the SQLite database's path inside the data disk. The guest
	// contract: the disk is mounted at /data, so "/app.db" here means the
	// app writes /data/app.db.
	DBPath     string
	DataDiskMB int
	Log        *slog.Logger

	mu       sync.Mutex
	epochs   map[string]Epoch
	shippers map[string]*Shipper
}

func NewCoordinator(gw *Gateway, cp ControlPlane, disks Disks, cellGen uint32, dataDiskMB int, log *slog.Logger) *Coordinator {
	if log == nil {
		log = slog.Default()
	}
	return &Coordinator{
		GW: gw, CP: cp, Disks: disks, CellGen: cellGen,
		DBPath: "/app.db", DataDiskMB: dataDiskMB, Log: log,
		epochs: map[string]Epoch{}, shippers: map[string]*Shipper{},
	}
}

// PrepareWake runs BEFORE the VMM starts. Returns rebuilt=true if the data
// disk was reconstructed from the object store — the FC snapshot must not
// be restored against it (the supervisor demotes to a cold boot).
func (c *Coordinator) PrepareWake(ctx context.Context, app registry.App) (bool, error) {
	name, stream := app.Name, streamOf(app)
	if _, err := c.GW.CreateStream(ctx, name, stream, nil, 0, 0); err != nil && !errors.Is(err, ErrStreamExists) {
		return false, fmt.Errorf("creating stream: %w", err)
	}
	counter, err := c.CP.MintEpoch(name)
	if err != nil {
		return false, err
	}
	epoch := NewEpoch(c.CellGen, counter)
	m, stale, err := c.GW.SealTakeover(ctx, name, stream, epoch)
	if err != nil {
		return false, fmt.Errorf("takeover seal: %w", err)
	}
	if stale {
		// Someone (necessarily another cell generation or a manual epoch
		// bump — one host mints monotonically) owns a newer epoch: this
		// wake is a zombie before it began.
		return false, fmt.Errorf("%w: wake epoch %s vs stream epoch %s", ErrFenced, epoch, m.CurEpoch)
	}

	cur := readCursorFileAt(c.Disks.ShipCursorPath(name))
	hadDisk := c.Disks.HasDataDisk(name)
	inSync := hadDisk && cur.Stream == stream && cur.ShippedLSN == m.HeadLSN
	rebuilt := false
	if !inSync {
		if err := c.rebuildDisk(ctx, name, stream, 0); err != nil {
			return false, err
		}
		rebuilt = true
		c.Log.Warn("data disk rebuilt from object store",
			"app", name, "stream", stream, "head", m.HeadLSN,
			"had_disk", hadDisk, "cursor_lsn", cur.ShippedLSN, "cursor_stream", cur.Stream)
	}
	c.mu.Lock()
	c.epochs[name] = epoch
	c.mu.Unlock()
	return rebuilt, nil
}

// rebuildDisk materializes the stream at target (0 = head) into a fresh
// data disk and aligns the cursor with it.
func (c *Coordinator) rebuildDisk(ctx context.Context, name, stream string, target uint64) error {
	img, lsn, err := c.GW.Restore(ctx, name, stream, target)
	if err != nil {
		return fmt.Errorf("restore from object store: %w", err)
	}
	files := map[string][]byte{}
	if len(img) > 0 {
		files[strings.TrimPrefix(c.DBPath, "/")] = img
	}
	if err := c.Disks.BuildDataDisk(name, files, c.DataDiskMB); err != nil {
		return err
	}
	return writeCursorFileAt(c.Disks.ShipCursorPath(name), cursorFile{Stream: stream, ShippedLSN: lsn})
}

// StartShipping begins tailing once the instance serves. kill is invoked
// (once) if the gateway ever fences this epoch: the instance is a zombie
// and must die (PT-1's resolution).
func (c *Coordinator) StartShipping(app registry.App, kill func(error)) error {
	name := app.Name
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.shippers[name]; ok {
		return fmt.Errorf("shipper already running for %s", name)
	}
	epoch, ok := c.epochs[name]
	if !ok {
		return fmt.Errorf("no epoch for %s: PrepareWake did not run", name)
	}
	sh := NewShipper(ShipperConfig{
		App: name, Stream: streamOf(app), Epoch: epoch, Gateway: c.GW,
		Source:     &Ext4Source{ImagePath: c.Disks.DataDiskPath(name), DBPath: c.DBPath},
		CursorPath: c.Disks.ShipCursorPath(name),
		OnFenced:   kill,
		Log:        c.Log,
	})
	c.shippers[name] = sh
	go sh.Run()
	return nil
}

// StopShipping halts the tail loop. With finalCheckpoint (the freeze path,
// guest already paused) it ships the remaining delta and registers a
// checkpoint at the head (E5).
func (c *Coordinator) StopShipping(ctx context.Context, app registry.App, finalCheckpoint bool) error {
	c.mu.Lock()
	sh := c.shippers[app.Name]
	delete(c.shippers, app.Name)
	c.mu.Unlock()
	if sh == nil {
		return nil
	}
	sh.Stop()
	if !finalCheckpoint {
		return nil
	}
	if _, err := sh.FinalCheckpoint(ctx); err != nil {
		return fmt.Errorf("final checkpoint: %w", err)
	}
	return nil
}

// Sync is the D1 hold: returns once everything committed-and-visible on
// the data disk is durable at the gateway. The router calls this before
// releasing a response.
func (c *Coordinator) Sync(ctx context.Context, name string) error {
	c.mu.Lock()
	sh := c.shippers[name]
	c.mu.Unlock()
	if sh == nil {
		return nil // not actively serving: nothing can be in flight
	}
	return sh.Sync(ctx)
}

// BranchRestore is PITR (D4): fork a new stream at the requested point,
// rebuild the data disk to it, and repoint the app. The parent stream is
// never modified. The supervisor guarantees the app is quiesced.
func (c *Coordinator) BranchRestore(ctx context.Context, app registry.App, atLSN uint64, atTime time.Time) (string, uint64, error) {
	name, from := app.Name, streamOf(app)
	if !atTime.IsZero() {
		lsn, err := c.GW.ResolveTime(ctx, name, from, atTime)
		if err != nil {
			return "", 0, err
		}
		atLSN = lsn
	}
	var newStream string
	for i := 1; ; i++ {
		if i > 1000 {
			return "", 0, errors.New("dataplane: more than 1000 branches")
		}
		newStream = fmt.Sprintf("s%d", i)
		_, err := c.GW.CreateBranch(ctx, name, from, newStream, atLSN)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrStreamExists) {
			return "", 0, err
		}
	}
	if err := c.rebuildDisk(ctx, name, newStream, 0); err != nil {
		return "", 0, err
	}
	if err := c.CP.SetStreamID(name, newStream); err != nil {
		return "", 0, err
	}
	c.Log.Info("PITR branch", "app", name, "from", from, "at_lsn", atLSN, "stream", newStream)
	return newStream, atLSN, nil
}

// StreamStatus returns the app's current manifest (the admin API's view).
func (c *Coordinator) StreamStatus(ctx context.Context, name, stream string) (*Manifest, error) {
	m, _, err := c.GW.Load(ctx, name, stream)
	return m, err
}

// PurgeApp removes every object under the app's prefix. Called on app
// deletion — deletion is explicit and total, and it is what makes epoch
// counters resettable (a re-created app must find a clean slate).
func (c *Coordinator) PurgeApp(ctx context.Context, name string) error {
	keys, err := c.GW.Store.List(ctx, "apps/"+name+"/")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := c.GW.Store.Delete(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

func streamOf(app registry.App) string {
	if app.StreamID == "" {
		return "s0"
	}
	return app.StreamID
}

func readCursorFileAt(path string) cursorFile {
	var c cursorFile
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func writeCursorFileAt(path string, c cursorFile) error {
	b, _ := json.Marshal(c)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
