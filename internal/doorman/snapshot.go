package doorman

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/objstore"
)

// SnapshotPrefix holds compressed copies of the doorman's database.
//
// Note the division of labor, which is C1's shape and M3's restore path
// wearing different clothes: THE SNAPSHOT IS THE CHECKPOINT AND THE
// REVOCATION LOG IS THE DELTA. Restoring means "take the newest checkpoint,
// then replay every tombstone over it" — so a snapshot may be stale by up to
// an interval without ever costing a revocation. That asymmetry is deliberate
// and is the whole reason revocations do not ride in here: losing a share
// link means re-sharing, losing a revocation means F2 FAILs.
const SnapshotPrefix = "_control/doorman/db/"

const snapTimeLayout = "20060102T150405Z"

// SnapshotInfo is the sidecar written beside each snapshot.
type SnapshotInfo struct {
	Key     string    `json:"key"`
	TakenAt time.Time `json:"taken_at"`
	Bytes   int64     `json:"bytes"`
	DBBytes int64     `json:"db_bytes"`
	SHA256  string    `json:"sha256"`
	Host    string    `json:"host"`
	Shares  int       `json:"shares"`
	Grants  int       `json:"grants"`
	// Revocations is the count at snapshot time, recorded so a restore can be
	// sanity-checked: after replay the live count must be >= this number.
	Revocations int `json:"revocations"`
}

// Snapshotter ships the doorman's database off-box on a schedule.
type Snapshotter struct {
	store   *Store
	obj     objstore.Store
	dbPath  string
	workDir string
	// Interval is the target AGE of the newest snapshot (0 = off). Age-driven
	// like regbackup, so a crash-looping doorman under Restart=always cannot
	// spray snapshots.
	interval time.Duration
	keep     int
	log      *slog.Logger
	Now      func() time.Time
}

func NewSnapshotter(st *Store, obj objstore.Store, dbPath, workDir string, interval time.Duration, keep int, log *slog.Logger) *Snapshotter {
	if log == nil {
		log = slog.Default()
	}
	if keep <= 0 {
		keep = 14
	}
	return &Snapshotter{store: st, obj: obj, dbPath: dbPath, workDir: workDir,
		interval: interval, keep: keep, log: log, Now: time.Now}
}

// backupTo writes a consistent copy via VACUUM INTO — a single transaction
// against the live database, so it captures committed WAL content with no
// quiesce. Copying the file with cp would race the -wal and can tear the ACL.
func (s *Store) backupTo(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("doorman snapshot: %s already exists (VACUUM INTO refuses to overwrite)", path)
	}
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

func (s *Store) counts() (shares, grants, revocations int) {
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM shares`).Scan(&shares)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM grants`).Scan(&grants)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM revocations`).Scan(&revocations)
	return
}

// Empty reports whether this database has never held any auth state — the
// signal that a restore is wanted rather than a first run being clobbered.
func (s *Store) Empty() bool {
	sh, gr, rv := s.counts()
	return sh == 0 && gr == 0 && rv == 0
}

func (sn *Snapshotter) RunOnce(ctx context.Context) (SnapshotInfo, error) {
	var info SnapshotInfo
	if sn.obj == nil {
		return info, errors.New("doorman snapshot: no object store configured")
	}
	if err := os.MkdirAll(sn.workDir, 0o700); err != nil {
		return info, err
	}
	tmp := filepath.Join(sn.workDir, "doorman-snapshot.db")
	_ = os.Remove(tmp) // a run killed mid-flight leaves this behind
	if err := sn.store.backupTo(tmp); err != nil {
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
	host, _ := os.Hostname()
	now := sn.Now().UTC()
	shares, grants, revs := sn.store.counts()
	info = SnapshotInfo{
		Key: SnapshotPrefix + now.Format(snapTimeLayout) + ".db.gz", TakenAt: now,
		Bytes: int64(buf.Len()), DBBytes: int64(len(raw)),
		SHA256: hex.EncodeToString(sum[:]), Host: host,
		Shares: shares, Grants: grants, Revocations: revs,
	}
	if err := sn.obj.Put(ctx, info.Key, buf.Bytes()); err != nil {
		return info, fmt.Errorf("doorman snapshot: uploading %s: %w", info.Key, err)
	}
	side, _ := json.MarshalIndent(info, "", "  ")
	if err := sn.obj.Put(ctx, sidecarOf(info.Key), side); err != nil {
		return info, fmt.Errorf("doorman snapshot: uploading sidecar: %w", err)
	}
	if err := sn.prune(ctx); err != nil {
		sn.log.Warn("doorman snapshot: pruning", "err", err)
	}
	return info, nil
}

// RestoreLatest writes the newest snapshot to dbPath. The caller must not
// have the database open — this is a cold path, run before OpenStore.
//
// It intentionally does NOT restore revocations to their snapshot state:
// Revoker.Sync replays the full log on top afterwards, which is what makes
// the restored doorman strictly more revoked than the snapshot, never less.
func RestoreLatest(ctx context.Context, obj objstore.Store, dbPath string) (SnapshotInfo, error) {
	var info SnapshotInfo
	keys, err := snapshotKeys(ctx, obj)
	if err != nil {
		return info, err
	}
	if len(keys) == 0 {
		return info, errors.New("no doorman snapshot in the object store")
	}
	key := keys[len(keys)-1]
	body, _, err := obj.Get(ctx, key)
	if err != nil {
		return info, fmt.Errorf("reading %s: %w", key, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return info, fmt.Errorf("%s is not gzip: %w", key, err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return info, err
	}
	// Remove the sidecar WAL/SHM of any previous database at this path;
	// leaving them beside a replaced main file is how you get a corrupt open.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
	if err := os.WriteFile(dbPath, raw, 0o600); err != nil {
		return info, err
	}
	info.Key, info.TakenAt, info.DBBytes = key, snapTimeOf(key), int64(len(raw))
	if side, _, err := obj.Get(ctx, sidecarOf(key)); err == nil {
		_ = json.Unmarshal(side, &info)
		info.Key = key
	}
	return info, nil
}

func (sn *Snapshotter) List(ctx context.Context) ([]SnapshotInfo, error) {
	keys, err := snapshotKeys(ctx, sn.obj)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, 0, len(keys))
	for i := len(keys) - 1; i >= 0; i-- {
		info := SnapshotInfo{Key: keys[i], TakenAt: snapTimeOf(keys[i])}
		if side, _, err := sn.obj.Get(ctx, sidecarOf(keys[i])); err == nil {
			_ = json.Unmarshal(side, &info)
			info.Key = keys[i]
		}
		out = append(out, info)
	}
	return out, nil
}

// Due reports whether the newest snapshot is older than the interval.
func (sn *Snapshotter) Due(ctx context.Context) (bool, error) {
	if sn.interval <= 0 || sn.obj == nil {
		return false, nil
	}
	keys, err := snapshotKeys(ctx, sn.obj)
	if err != nil {
		return false, err
	}
	var newest time.Time
	if len(keys) > 0 {
		newest = snapTimeOf(keys[len(keys)-1])
	}
	return sn.Now().Sub(newest) >= sn.interval, nil
}

func (sn *Snapshotter) Run(ctx context.Context) {
	if sn.interval <= 0 || sn.obj == nil {
		return
	}
	tick := sn.interval
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
		due, err := sn.Due(ctx)
		switch {
		case err != nil:
			sn.log.Error("doorman snapshot: cannot reach the object store", "err", err)
		case due:
			info, err := sn.RunOnce(ctx)
			if err != nil {
				sn.log.Error("doorman snapshot FAILED", "err", err)
			} else {
				sn.log.Info("doorman database snapshotted", "key", info.Key,
					"shares", info.Shares, "grants", info.Grants, "revocations", info.Revocations)
			}
		}
		timer.Reset(tick)
	}
}

func (sn *Snapshotter) prune(ctx context.Context) error {
	keys, err := snapshotKeys(ctx, sn.obj)
	if err != nil || len(keys) <= sn.keep {
		return err
	}
	for _, k := range keys[:len(keys)-sn.keep] {
		if err := sn.obj.Delete(ctx, k); err != nil {
			return err
		}
		if err := sn.obj.Delete(ctx, sidecarOf(k)); err != nil {
			return err
		}
	}
	return nil
}

func snapshotKeys(ctx context.Context, obj objstore.Store) ([]string, error) {
	if obj == nil {
		return nil, errors.New("no object store configured")
	}
	keys, err := obj.List(ctx, SnapshotPrefix)
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

func snapTimeOf(key string) time.Time {
	base := strings.TrimSuffix(strings.TrimPrefix(key, SnapshotPrefix), ".db.gz")
	t, err := time.Parse(snapTimeLayout, base)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
