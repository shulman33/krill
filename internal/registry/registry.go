// Package registry is krilld's durable app catalog: one SQLite database
// holding every registered app, its resources, and its quiescent state.
// (Yes, the platform's own metadata lives in SQLite. On-brand.)
//
// Only quiescent facts are durable. Transient lifecycle states (WAKING,
// BOOTING, ACTIVE, SNAPSHOTTING) are persisted so a crashed daemon can tell
// which apps were mid-flight — Reconcile demotes those to COLD and
// invalidates their snapshots, because a disk that mutated after the
// snapshot was taken must never be paired with that snapshot again.
package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("app not found")

// nameRE: apps are routed by DNS label (first label of the Host header), so
// names must be valid DNS labels.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func ValidName(name string) bool { return nameRE.MatchString(name) }

type App struct {
	ID        int64
	Name      string
	VCPUs     int
	MemMiB    int
	GuestPort int
	// SubnetIdx is the app's slot in 172.16.<idx>.0/30 — it determines tap
	// name, host/guest IPs and both deterministic MACs.
	SubnetIdx int
	// KernelPath and BootArgs override the daemon defaults when non-empty.
	KernelPath string
	BootArgs   string
	// State is the last persisted lifecycle state (see package lifecycle).
	State string
	// SnapshotValid is true only while the snapshot on disk is exactly
	// paired with the app's active disk image. Anything that can mutate the
	// disk outside a restore (cold boot, crash while running) must clear it
	// BEFORE the mutation becomes possible.
	SnapshotValid bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Registry struct {
	db *sql.DB
}

func Open(path string) (*Registry, error) {
	// modernc.org/sqlite; single connection is plenty for a host-local
	// control plane and sidesteps SQLITE_BUSY entirely.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("registry schema: %w", err)
	}
	return &Registry{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS apps (
  id             INTEGER PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,
  vcpus          INTEGER NOT NULL,
  mem_mib        INTEGER NOT NULL,
  guest_port     INTEGER NOT NULL,
  subnet_idx     INTEGER NOT NULL UNIQUE,
  kernel_path    TEXT NOT NULL DEFAULT '',
  boot_args      TEXT NOT NULL DEFAULT '',
  state          TEXT NOT NULL,
  snapshot_valid INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);`

func (r *Registry) Close() error { return r.db.Close() }

// Create inserts a new app, allocating the smallest free subnet index.
// Subnet indexes are capped at 255: third octet of 172.16.<idx>.0/30.
func (r *Registry) Create(a App) (App, error) {
	if !ValidName(a.Name) {
		return App{}, fmt.Errorf("invalid app name %q (need DNS label: %s)", a.Name, nameRE)
	}
	if a.VCPUs < 1 || a.MemMiB < 32 || a.GuestPort < 1 || a.GuestPort > 65535 {
		return App{}, fmt.Errorf("invalid resources for %q: need vcpus>=1, mem_mib>=32, guest_port in 1..65535", a.Name)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return App{}, err
	}
	defer tx.Rollback()

	var idx int
	// Smallest non-negative integer not currently allocated.
	err = tx.QueryRow(`
		SELECT COALESCE(MIN(t.idx + 1), 0) FROM (
		  SELECT -1 AS idx UNION SELECT subnet_idx FROM apps
		) t WHERE t.idx + 1 NOT IN (SELECT subnet_idx FROM apps)`).Scan(&idx)
	if err != nil {
		return App{}, err
	}
	if idx > 255 {
		return App{}, errors.New("no free subnets: 256 apps per host is the M1 ceiling")
	}

	now := time.Now().UTC()
	res, err := tx.Exec(`
		INSERT INTO apps (name, vcpus, mem_mib, guest_port, subnet_idx,
		                  kernel_path, boot_args, state, snapshot_valid,
		                  created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		a.Name, a.VCPUs, a.MemMiB, a.GuestPort, idx,
		a.KernelPath, a.BootArgs, a.State,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return App{}, fmt.Errorf("insert app %q: %w", a.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return App{}, err
	}
	a.ID, _ = res.LastInsertId()
	a.SubnetIdx = idx
	a.SnapshotValid = false
	a.CreatedAt, a.UpdatedAt = now, now
	return a, nil
}

func (r *Registry) Get(name string) (App, error) {
	row := r.db.QueryRow(selectCols+` WHERE name = ?`, name)
	return scanApp(row)
}

func (r *Registry) List() ([]App, error) {
	rows, err := r.db.Query(selectCols + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetState persists a lifecycle transition. Snapshot validity is a separate,
// deliberate call: forgetting it should fail safe (snapshot stays invalid).
func (r *Registry) SetState(name, state string) error {
	return r.update(name, `state = ?`, state)
}

// SetSnapshotValid flips the pairing bit between the app's snapshot files
// and its active disk.
func (r *Registry) SetSnapshotValid(name string, valid bool) error {
	v := 0
	if valid {
		v = 1
	}
	return r.update(name, `snapshot_valid = ?`, v)
}

func (r *Registry) Delete(name string) error {
	res, err := r.db.Exec(`DELETE FROM apps WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Registry) update(name, setClause string, val any) error {
	res, err := r.db.Exec(
		`UPDATE apps SET `+setClause+`, updated_at = ? WHERE name = ?`,
		val, time.Now().UTC().Format(time.RFC3339Nano), name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

const selectCols = `
	SELECT id, name, vcpus, mem_mib, guest_port, subnet_idx, kernel_path,
	       boot_args, state, snapshot_valid, created_at, updated_at FROM apps`

type scanner interface{ Scan(...any) error }

func scanApp(s scanner) (App, error) {
	var a App
	var snap int
	var created, updated string
	err := s.Scan(&a.ID, &a.Name, &a.VCPUs, &a.MemMiB, &a.GuestPort,
		&a.SubnetIdx, &a.KernelPath, &a.BootArgs, &a.State, &snap,
		&created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.SnapshotValid = snap == 1
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	a.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return a, nil
}
