package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/shulman33/krill/internal/sqlitewal"
)

// Shipper tails one app's SQLite WAL from outside the VM and ships
// committed frames to the gateway under its epoch (E2). One shipper exists
// per ACTIVE instance, created at wake with a freshly minted epoch and
// stopped at freeze. Its whole life is: scan, stamp, ship, repeat — and if
// the gateway ever says ErrFenced, this instance is a zombie and the
// OnFenced callback tells the supervisor to kill it.
type Shipper struct {
	cfg ShipperConfig

	mu       sync.Mutex // serializes step(); the loop and Sync both take it
	scanCur  sqlitewal.Cursor
	shipped  cursorFile // durable progress (what's persisted)
	pending  []sqlitewal.Frame
	pendAge  time.Time
	pageSize int
	fatal    error

	stop chan struct{}
	done chan struct{}
}

type ShipperConfig struct {
	App, Stream string
	Epoch       Epoch
	Gateway     *Gateway
	Source      DataSource
	// CursorPath persists shipping progress locally (a cache — the manifest
	// is the record; a lost cursor file only costs a rebase).
	CursorPath string
	// OnFenced fires once if the gateway fences this shipper (the instance
	// must die; PT-1's resolution). May be nil.
	OnFenced func(error)

	PollInterval time.Duration // default 250ms
	MaxSegBytes  int           // ship when pending exceeds this (default 1MiB)
	MaxSegAge    time.Duration // ...or is older than this (default 1s)
	Log          *slog.Logger
}

// cursorFile is the persisted progress: which WAL generation we were
// consuming, how many of its frames are already in the stream, and the
// stream LSN they reached.
type cursorFile struct {
	Stream     string `json:"stream"`
	ShippedLSN uint64 `json:"shipped_lsn"`
	HasGen     bool   `json:"has_gen"`
	Salt1      uint32 `json:"salt1"`
	Salt2      uint32 `json:"salt2"`
	WALFrames  int    `json:"wal_frames"` // frames of this generation shipped
}

func NewShipper(cfg ShipperConfig) *Shipper {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}
	if cfg.MaxSegBytes <= 0 {
		cfg.MaxSegBytes = 1 << 20
	}
	if cfg.MaxSegAge <= 0 {
		cfg.MaxSegAge = time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Shipper{cfg: cfg, stop: make(chan struct{}), done: make(chan struct{})}
	s.loadCursor()
	return s
}

func (s *Shipper) loadCursor() {
	b, err := os.ReadFile(s.cfg.CursorPath)
	if err != nil {
		return
	}
	var c cursorFile
	if json.Unmarshal(b, &c) == nil && c.Stream == s.cfg.Stream {
		s.shipped = c
	}
}

func (s *Shipper) saveCursor() {
	s.shipped.Stream = s.cfg.Stream
	b, _ := json.Marshal(s.shipped)
	tmp := s.cfg.CursorPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, s.cfg.CursorPath)
	}
}

// Run polls until Stop; call in a goroutine.
func (s *Shipper) Run() {
	defer close(s.done)
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			if s.fatal == nil {
				if err := s.step(context.Background(), false, false); err != nil {
					s.cfg.Log.Error("shipper", "app", s.cfg.App, "err", err)
				}
			}
			s.mu.Unlock()
		}
	}
}

// Stop halts the loop (idempotent) and waits for it to exit.
func (s *Shipper) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

// Err reports a fatal condition (fencing): the instance must not serve.
func (s *Shipper) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// ShippedLSN is the durable stream position.
func (s *Shipper) ShippedLSN() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shipped.ShippedLSN
}

// Sync ships everything committed-and-visible right now, using the precise
// (fs-journal-replayed) view. This is the D1 hold: a response is released
// to the client only after Sync returns nil.
func (s *Shipper) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return s.fatal
	}
	return s.step(ctx, true, true)
}

// step is one scan-and-maybe-ship pass. Callers hold s.mu.
func (s *Shipper) step(ctx context.Context, precise, flush bool) error {
	wal, err := s.cfg.Source.WAL(precise)
	if err != nil {
		return fmt.Errorf("reading wal: %w", err)
	}
	res, err := sqlitewal.Scan(wal, s.scanCur)
	switch {
	case errors.Is(err, sqlitewal.ErrSaltChange):
		return s.rebase(ctx)
	case err != nil:
		return fmt.Errorf("scanning wal: %w", err)
	}

	// First contact with a WAL generation: reconcile with persisted progress.
	if !s.scanCur.Init && res.Cursor.Init {
		if s.shipped.HasGen &&
			(s.shipped.Salt1 != res.Cursor.Salt1 || s.shipped.Salt2 != res.Cursor.Salt2) {
			// The WAL was reset while we were away; unshipped frames from the
			// old generation are unknowable. Re-base from the full image.
			s.scanCur = sqlitewal.Cursor{}
			return s.rebase(ctx)
		}
		if !s.shipped.HasGen {
			s.shipped.HasGen = true
			s.shipped.Salt1, s.shipped.Salt2 = res.Cursor.Salt1, res.Cursor.Salt2
			s.shipped.WALFrames = 0
		}
		// Drop frames this generation already shipped (daemon restart path).
		if drop := s.shipped.WALFrames; drop > 0 {
			if drop > len(res.Frames) {
				// Persisted progress claims more frames than the WAL holds:
				// only a reset explains it (missed salt reuse is impossible,
				// salts are random). Re-base.
				s.scanCur = sqlitewal.Cursor{}
				return s.rebase(ctx)
			}
			res.Frames = res.Frames[drop:]
		}
	}
	s.scanCur = res.Cursor
	if s.pageSize == 0 && res.Header.PageSize != 0 {
		s.pageSize = res.Header.PageSize
	}
	if len(res.Frames) > 0 {
		if len(s.pending) == 0 {
			s.pendAge = time.Now()
		}
		s.pending = append(s.pending, res.Frames...)
	}

	cut := sqlitewal.LastCommit(s.pending)
	if cut < 0 {
		return nil
	}
	due := flush ||
		(cut+1)*s.pageSize >= s.cfg.MaxSegBytes ||
		time.Since(s.pendAge) >= s.cfg.MaxSegAge
	if !due {
		return nil
	}
	return s.ship(ctx, cut+1)
}

// ship pushes the first n pending frames as one segment. Callers hold s.mu.
func (s *Shipper) ship(ctx context.Context, n int) error {
	m, err := s.cfg.Gateway.AppendSegment(ctx, s.cfg.App, s.cfg.Stream, s.cfg.Epoch, s.pending[:n], s.pageSize)
	if err != nil {
		if errors.Is(err, ErrFenced) {
			s.fail(err)
		}
		return err
	}
	s.pending = append([]sqlitewal.Frame(nil), s.pending[n:]...)
	s.pendAge = time.Now()
	s.shipped.ShippedLSN = m.HeadLSN
	s.shipped.WALFrames += n
	s.saveCursor()
	return nil
}

// rebase ships the complete current database image as one stream step —
// frame granularity was lost (the guest checkpointed its WAL between
// polls, or local progress is unknowable). Durability is preserved; PITR
// granularity coarsens across the jump.
//
// The WAL snapshot is captured ONCE: image content and cursor state both
// derive from it, so a commit racing in after the capture is neither in
// the image nor marked shipped — the next poll picks it up.
func (s *Shipper) rebase(ctx context.Context) error {
	db, res, err := s.preciseView()
	if err != nil {
		return err
	}
	ps := s.effectivePageSize(db, res)
	if ps == 0 {
		ps = 4096 // no db at all: ship an empty rebase with a sane default
	}
	img, err := sqlitewal.Replay(db, ps, res.Frames)
	if err != nil {
		return err
	}
	m, err := s.cfg.Gateway.AppendRebase(ctx, s.cfg.App, s.cfg.Stream, s.cfg.Epoch, img, ps)
	if err != nil {
		if errors.Is(err, ErrFenced) {
			s.fail(err)
		}
		return err
	}
	// The image subsumes exactly the frames of the captured scan.
	s.pending = nil
	s.scanCur = res.Cursor
	s.shipped = cursorFile{
		Stream:     s.cfg.Stream,
		ShippedLSN: m.HeadLSN,
		HasGen:     res.Cursor.Init,
		Salt1:      res.Cursor.Salt1,
		Salt2:      res.Cursor.Salt2,
		WALFrames:  len(res.Frames),
	}
	s.saveCursor()
	s.cfg.Log.Warn("shipper re-based stream (frame granularity lost)",
		"app", s.cfg.App, "lsn", m.HeadLSN)
	return nil
}

// preciseView reads db + wal through the journal-replayed view and scans
// the wal from scratch.
func (s *Shipper) preciseView() ([]byte, sqlitewal.ScanResult, error) {
	db, err := s.cfg.Source.DB(true)
	if err != nil {
		return nil, sqlitewal.ScanResult{}, err
	}
	wal, err := s.cfg.Source.WAL(true)
	if err != nil {
		return nil, sqlitewal.ScanResult{}, err
	}
	res, err := sqlitewal.Scan(wal, sqlitewal.Cursor{})
	if err != nil {
		return nil, sqlitewal.ScanResult{}, err
	}
	return db, res, nil
}

func (s *Shipper) effectivePageSize(db []byte, res sqlitewal.ScanResult) int {
	ps := res.Header.PageSize
	if ps == 0 {
		if info, err := sqlitewal.ParseDBHeader(db); err == nil {
			ps = info.PageSize
		}
	}
	if ps != 0 && s.pageSize == 0 {
		s.pageSize = ps
	}
	return ps
}

// materialize builds the current committed database image from the precise
// local view: main file + committed WAL frames replayed — byte-for-byte
// what SQLite's own checkpoint would produce. Returns nil if the app has
// never created its database.
func (s *Shipper) materialize(ctx context.Context) ([]byte, error) {
	_ = ctx
	db, res, err := s.preciseView()
	if err != nil {
		return nil, err
	}
	ps := s.effectivePageSize(db, res)
	if ps == 0 {
		return nil, nil
	}
	return sqlitewal.Replay(db, ps, res.Frames)
}

// FinalCheckpoint runs at freeze, after the guest is paused: ship whatever
// remains (precise view), then register a checkpoint of the full image at
// the stream head (E5). Returns the manifest after registration.
func (s *Shipper) FinalCheckpoint(ctx context.Context) (*Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatal != nil {
		return nil, s.fatal
	}
	if err := s.step(ctx, true, true); err != nil {
		return nil, err
	}
	img, err := s.materialize(ctx)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, nil // app never created its database: nothing to register
	}
	m, err := s.cfg.Gateway.RegisterCheckpoint(ctx, s.cfg.App, s.cfg.Stream, s.cfg.Epoch, s.shipped.ShippedLSN, img)
	if err != nil {
		if errors.Is(err, ErrFenced) {
			s.fail(err)
		}
		return nil, err
	}
	return m, nil
}

// fail records the fatal fencing error and fires OnFenced once.
func (s *Shipper) fail(err error) {
	if s.fatal != nil {
		return
	}
	s.fatal = err
	s.cfg.Log.Error("shipper fenced: instance is a zombie", "app", s.cfg.App, "epoch", s.cfg.Epoch, "err", err)
	if s.cfg.OnFenced != nil {
		go s.cfg.OnFenced(err)
	}
}
