// Package regbackup ships the registry database off-box, on a schedule, into
// the object store.
//
// Why this is not optional: the registry is the epoch mint (E1). RAID1
// survives a dead disk; it does not survive a dead server, a fat-fingered
// rm, or a billing problem. Everything else on the box is reconstructible —
// rootfs images rebuild from a deploy, data disks rebuild from the stream —
// but the mint's counters exist in exactly one place, and losing them costs
// the app catalog too.
//
// The protocol consequence of a *restore* is the reason each backup carries a
// sidecar manifest: a restored mint will re-issue epoch counters it has
// already handed out, which violates E1's monotonicity. The composite epoch
// (cell generation, counter) is what saves this — bumping --cell-gen fences
// every pre-loss epoch at once — so the sidecar records the cell generation
// the backup was taken under and the minimum safe one to restart with.
package regbackup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/registry"
)

// Prefix namespaces backups away from app data ("apps/<name>/..."), so app
// deletion's prefix purge can never reach them.
const Prefix = "_control/registry/"

// tsLayout keys backups so lexicographic order is chronological order — the
// object store's List is then all the index we need.
const tsLayout = "20060102T150405Z"

// Source is the slice of the registry a backup needs. *registry.Registry
// implements it.
type Source interface {
	BackupTo(path string) error
	Stats() (registry.Stats, error)
}

// Info is the sidecar manifest, and the admin API's view of one backup.
type Info struct {
	Key     string    `json:"key"`
	TakenAt time.Time `json:"taken_at"`
	// Bytes is the compressed object; DBBytes the SQLite file it came from.
	Bytes   int64  `json:"bytes"`
	DBBytes int64  `json:"db_bytes"`
	SHA256  string `json:"sha256"`
	Host    string `json:"host"`
	// CellGen is the generation krilld was running under when this snapshot
	// was taken; RestoreCellGen is the minimum --cell-gen that is safe to
	// restart with after restoring it (E1).
	CellGen        uint32 `json:"cell_gen"`
	RestoreCellGen uint32 `json:"restore_cell_gen"`
	Apps           int    `json:"apps"`
	MaxEpoch       uint64 `json:"max_epoch"`
}

type Config struct {
	Store  objstore.Store
	Source Source
	// WorkDir holds the temporary snapshot while it is compressed.
	WorkDir string
	// CellGen is krilld's --cell-gen, recorded in each Info.
	CellGen uint32
	// Interval is the target age of the newest backup (0 disables the
	// scheduler entirely). Keep is how many to retain.
	Interval time.Duration
	Keep     int
	Log      *slog.Logger
	// Now is overridable in tests.
	Now func() time.Time
}

type Manager struct {
	cfg Config
}

func New(cfg Config) *Manager {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Keep <= 0 {
		cfg.Keep = 14
	}
	return &Manager{cfg: cfg}
}

// RunOnce takes a snapshot, ships it with its sidecar, and prunes.
func (m *Manager) RunOnce(ctx context.Context) (Info, error) {
	var info Info
	if m.cfg.Store == nil || m.cfg.Source == nil {
		return info, errors.New("registry backup: not configured")
	}
	if err := os.MkdirAll(m.cfg.WorkDir, 0o700); err != nil {
		return info, err
	}
	tmp := filepath.Join(m.cfg.WorkDir, "registry-snapshot.db")
	// A previous run killed mid-flight leaves this behind; VACUUM INTO would
	// then refuse to write.
	_ = os.Remove(tmp)
	if err := m.cfg.Source.BackupTo(tmp); err != nil {
		return info, err
	}
	defer os.Remove(tmp)

	raw, err := os.ReadFile(tmp)
	if err != nil {
		return info, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return info, err
	}
	if err := zw.Close(); err != nil {
		return info, err
	}
	sum := sha256.Sum256(buf.Bytes())

	st, err := m.cfg.Source.Stats()
	if err != nil {
		return info, err
	}
	host, _ := os.Hostname()
	now := m.cfg.Now().UTC()
	ts := now.Format(tsLayout)
	info = Info{
		Key:            Prefix + ts + ".db.gz",
		TakenAt:        now,
		Bytes:          int64(buf.Len()),
		DBBytes:        int64(len(raw)),
		SHA256:         hex.EncodeToString(sum[:]),
		Host:           host,
		CellGen:        m.cfg.CellGen,
		RestoreCellGen: m.cfg.CellGen + 1,
		Apps:           st.Apps,
		MaxEpoch:       st.MaxEpoch,
	}
	if err := m.cfg.Store.Put(ctx, info.Key, buf.Bytes()); err != nil {
		return info, fmt.Errorf("registry backup: uploading %s: %w", info.Key, err)
	}
	side, _ := json.MarshalIndent(info, "", "  ")
	if err := m.cfg.Store.Put(ctx, sidecarOf(info.Key), side); err != nil {
		return info, fmt.Errorf("registry backup: uploading sidecar for %s: %w", info.Key, err)
	}
	if err := m.prune(ctx); err != nil {
		// The backup itself landed; a failed prune is a cost problem, not a
		// durability one.
		m.cfg.Log.Warn("registry backup: pruning old backups", "err", err)
	}
	return info, nil
}

// List returns known backups, newest first, reading each sidecar. Backups
// whose sidecar is missing still appear, with only what the key encodes.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	keys, err := m.snapshotKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(keys))
	for i := len(keys) - 1; i >= 0; i-- {
		k := keys[i]
		info := Info{Key: k, TakenAt: timeOf(k)}
		if b, _, err := m.cfg.Store.Get(ctx, sidecarOf(k)); err == nil {
			_ = json.Unmarshal(b, &info)
			info.Key = k
		}
		out = append(out, info)
	}
	return out, nil
}

// Newest returns when the most recent backup was taken (zero time if none).
func (m *Manager) Newest(ctx context.Context) (time.Time, error) {
	keys, err := m.snapshotKeys(ctx)
	if err != nil || len(keys) == 0 {
		return time.Time{}, err
	}
	return timeOf(keys[len(keys)-1]), nil
}

// Due reports whether the newest backup is older than Interval. This — not a
// timer since process start — is what makes the schedule survive restarts.
func (m *Manager) Due(ctx context.Context) (bool, error) {
	if m.cfg.Interval <= 0 {
		return false, nil
	}
	newest, err := m.Newest(ctx)
	if err != nil {
		return false, err
	}
	return m.cfg.Now().Sub(newest) >= m.cfg.Interval, nil
}

// Run is the scheduler: it keeps the newest backup younger than Interval.
//
// It is deliberately age-driven rather than "back up at startup, then every
// Interval": krilld runs under Restart=always, and a daemon crash-looping
// every few seconds must not upload a backup per restart.
func (m *Manager) Run(ctx context.Context) {
	if m.cfg.Interval <= 0 {
		return
	}
	// Check often enough that a long-lived daemon notices the interval
	// elapsing, cheaply — a List of a 30-object prefix.
	tick := m.cfg.Interval
	if tick > time.Hour {
		tick = time.Hour
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		due, err := m.Due(ctx)
		switch {
		case err != nil:
			m.cfg.Log.Error("registry backup: cannot reach the backup store", "err", err)
		case !due:
			m.cfg.Log.Debug("registry backup: recent enough")
		default:
			info, err := m.RunOnce(ctx)
			if err != nil {
				m.cfg.Log.Error("registry backup FAILED", "err", err)
			} else {
				m.cfg.Log.Info("registry backed up", "key", info.Key,
					"bytes", info.Bytes, "apps", info.Apps,
					"max_epoch", info.MaxEpoch, "cell_gen", info.CellGen)
			}
		}
		timer.Reset(tick)
	}
}

func (m *Manager) prune(ctx context.Context) error {
	keys, err := m.snapshotKeys(ctx)
	if err != nil {
		return err
	}
	if len(keys) <= m.cfg.Keep {
		return nil
	}
	for _, k := range keys[:len(keys)-m.cfg.Keep] {
		if err := m.cfg.Store.Delete(ctx, k); err != nil {
			return err
		}
		if err := m.cfg.Store.Delete(ctx, sidecarOf(k)); err != nil {
			return err
		}
		m.cfg.Log.Info("registry backup pruned", "key", k)
	}
	return nil
}

// snapshotKeys returns the .db.gz keys in chronological (= lexicographic)
// order.
func (m *Manager) snapshotKeys(ctx context.Context) ([]string, error) {
	keys, err := m.cfg.Store.List(ctx, Prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		if strings.HasSuffix(k, ".db.gz") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func sidecarOf(key string) string { return strings.TrimSuffix(key, ".db.gz") + ".json" }

func timeOf(key string) time.Time {
	base := strings.TrimSuffix(strings.TrimPrefix(key, Prefix), ".db.gz")
	t, err := time.Parse(tsLayout, base)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
