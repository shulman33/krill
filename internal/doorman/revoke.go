package doorman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/objstore"
)

// RevocationPrefix is the append-only tombstone log. It lives under
// _control/ for the same reason registry backups do: app deletion purges
// "apps/<name>/...", and a revocation must survive the deletion of the app it
// refers to.
const RevocationPrefix = "_control/doorman/revocations/"

// revTimeLayout keys revocations so lexicographic order is chronological
// order — the object store's List is then the whole index.
const revTimeLayout = "20060102T150405.000000000Z"

// Revoker is F2 as a mechanism.
//
// The rule, stated once: A REVOKE IS NOT ACKNOWLEDGED UNTIL IT IS DURABLE AT
// THE OBJECT STORE. That is D1's rule — the one an app's acked write obeys —
// applied to auth. The consequence is the property the gate actually tests:
// the object store, not this host's disk, is the record of what has been
// taken away, so restoring the doorman's database from any point in the past
// cannot resurrect access. Local rows are a cache of a log we do not own.
//
// The log is never pruned. A tombstone is a few hundred bytes and outliving
// everything is its entire job.
type Revoker struct {
	store *Store
	obj   objstore.Store
	log   *slog.Logger
	// Now is overridable in tests.
	Now func() time.Time
}

func NewRevoker(st *Store, obj objstore.Store, log *slog.Logger) *Revoker {
	if log == nil {
		log = slog.Default()
	}
	return &Revoker{store: st, obj: obj, log: log, Now: time.Now}
}

// ErrNoRecord is returned when a revoke is attempted with no object store
// behind it. Failing the call is deliberate: a revoke that only exists on the
// box being revoked from is the exact failure F2 was amended to forbid.
var ErrNoRecord = errors.New("revocation has nowhere durable to land: the doorman has no object store")

// Revoke captures what the tombstone kills, writes it to the object store,
// and only then applies it locally. The order is the point.
func (rv *Revoker) Revoke(ctx context.Context, r Revocation) (Revocation, error) {
	if rv.obj == nil {
		return Revocation{}, ErrNoRecord
	}
	if r.At.IsZero() {
		r.At = rv.Now().UTC()
	}
	if r.ID == "" {
		r.ID = r.At.UTC().Format(revTimeLayout) + "-" + shortID()
	}
	// Capture before writing: the tombstone must name the grants as they exist
	// now, so that replaying it against a database restored from any other
	// moment kills the same rows.
	grants, err := rv.capture(r)
	if err != nil {
		return Revocation{}, err
	}
	r.Grants = grants
	body, err := json.Marshal(r)
	if err != nil {
		return Revocation{}, err
	}
	// Put, not PutCAS: each tombstone has a unique key and is immutable, the
	// same shape the data plane uses for WAL segments. There is nothing to
	// compare-and-swap because nothing is ever overwritten.
	if err := rv.obj.Put(ctx, RevocationPrefix+r.ID+".json", body); err != nil {
		return Revocation{}, fmt.Errorf("revoke not durable, so not applied: %w", err)
	}
	if err := rv.apply(r); err != nil {
		// The revoke IS in force — it is in the log, and the next Sync or
		// restart applies it — but say so precisely rather than reporting a
		// clean success the local database does not yet reflect.
		return r, fmt.Errorf("revoke is durable at the object store but the local apply failed "+
			"(it takes effect on the next sync or restart): %w", err)
	}
	return r, nil
}

// capture lists the grant IDs a tombstone kills. It reads the grants table,
// which is exactly why the answer is recorded in the log rather than
// recomputed at replay time: the table is restorable state, the log is not.
func (rv *Revoker) capture(r Revocation) ([]string, error) {
	var q string
	var args []any
	switch r.Kind {
	case RevokeIdentity:
		q, args = `SELECT id FROM grants WHERE app = ? AND subject = ?`, []any{r.App, r.Subject}
	case RevokeShare:
		q, args = `SELECT id FROM grants WHERE share_id = ?`, []any{r.Share}
	case RevokeApp:
		q, args = `SELECT id FROM grants WHERE app = ?`, []any{r.App}
	default:
		return nil, nil // a session tombstone kills no grants
	}
	rows, err := rv.store.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// apply writes one tombstone into the local tables. Idempotent by primary
// key, which is what makes replay safe to run as often as we like.
func (rv *Revoker) apply(r Revocation) error {
	names, err := json.Marshal(r.Grants)
	if err != nil {
		return err
	}
	if _, err := rv.store.db.Exec(`
		INSERT INTO revocations (id, kind, app, subject, share, session, grants, at, by, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		r.ID, r.Kind, r.App, r.Subject, r.Share, r.Session, string(names),
		ts(r.At), r.By, r.Reason); err != nil {
		return err
	}
	for _, g := range r.Grants {
		if _, err := rv.store.db.Exec(`
			INSERT INTO revoked_grants (grant_id, rev_id, at) VALUES (?, ?, ?)
			ON CONFLICT(grant_id, rev_id) DO NOTHING`, g, r.ID, ts(r.At)); err != nil {
			return err
		}
	}
	return nil
}

// Sync replays the log over the local database and reports how many
// tombstones were new.
//
// This runs at every start — including the start that follows "delete the
// doorman's database", which is F2 step 3 — and periodically thereafter. It
// is the mechanism by which a restore can only ever move revocation forward.
func (rv *Revoker) Sync(ctx context.Context) (int, error) {
	if rv.obj == nil {
		return 0, nil
	}
	keys, err := rv.obj.List(ctx, RevocationPrefix)
	if err != nil {
		return 0, fmt.Errorf("listing the revocation log: %w", err)
	}
	have, err := rv.knownIDs()
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, k := range keys {
		if !strings.HasSuffix(k, ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(k, RevocationPrefix), ".json")
		if _, ok := have[id]; ok {
			continue
		}
		body, _, err := rv.obj.Get(ctx, k)
		if err != nil {
			return applied, fmt.Errorf("reading revocation %s: %w", k, err)
		}
		var r Revocation
		if err := json.Unmarshal(body, &r); err != nil {
			return applied, fmt.Errorf("parsing revocation %s: %w", k, err)
		}
		if r.ID == "" {
			r.ID = id
		}
		if err := rv.apply(r); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

func (rv *Revoker) knownIDs() (map[string]struct{}, error) {
	rows, err := rv.store.db.Query(`SELECT id FROM revocations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// List returns the local tombstones, newest first. The IDs sort
// chronologically, so this is also the audit timeline.
func (rv *Revoker) List(limit int) ([]Revocation, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := rv.store.db.Query(`
		SELECT id, kind, app, subject, share, session, grants, at, by, reason
		  FROM revocations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revocation{}
	for rows.Next() {
		var r Revocation
		var at, grants string
		if err := rows.Scan(&r.ID, &r.Kind, &r.App, &r.Subject, &r.Share,
			&r.Session, &grants, &at, &r.By, &r.Reason); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(grants), &r.Grants)
		r.At = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunSync keeps the local view converged with the log. A single doorman is
// the only writer, so this is belt-and-suspenders today; it is also what
// makes a second doorman (M5) a configuration change rather than a redesign.
func (rv *Revoker) RunSync(ctx context.Context, every time.Duration) {
	if rv.obj == nil || every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := rv.Sync(ctx)
			if err != nil {
				rv.log.Error("revocation log sync failed", "err", err)
			} else if n > 0 {
				rv.log.Warn("revocations applied from the log that this host had not seen",
					"count", n)
			}
		}
	}
}
