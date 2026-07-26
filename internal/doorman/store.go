// Package doorman is Krill's front door: the edge process that decides who
// may talk to an app, before krilld's router is allowed to wake it.
//
// Two constraints shaped everything here, and neither was a preference
// (ROADMAP decision #9):
//
//  1. F3 forbids an unauthorized request from waking an app — "a fence that
//     bills is still a fence that failed" — and krilld's router wakes on
//     request. So authorization must complete strictly UPSTREAM of krilld.
//     Caddy terminates TLS and forward_auths here; only a 200 from this
//     package lets a request reach the router at all.
//  2. krilld runs as root (taps, mkfs.ext4, docker). The internet-facing
//     OAuth surface cannot live inside it without contradicting the thesis of
//     F5 and F6. The doorman is a separate, unprivileged process with its own
//     database.
//
// The share model, frozen before any of this was written: THE LINK IS THE
// CAPABILITY. Holding an unguessable link is the grant; the Google sign-in
// that follows establishes who is using it — for X-App-User and for revoke —
// not whether they may.
//
// # Why the ACL cannot live in krilld's registry
//
// The registry is the E1 epoch mint, and its documented recovery is "roll
// back to a ≤24 h snapshot and bump --cell-gen." That is correct for a mint,
// because the composite epoch fences everything older. Applied to auth state
// the same rollback silently UN-REVOKES shares. Auth state and the mint want
// opposite things from a restore and therefore cannot share a restore path
// (F2's corollary). Hence: a separate database, and revocations that are
// tombstones replayed from the object store rather than rows a snapshot can
// take away.
package doorman

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrNoShare covers "no such link", "revoked link" and "expired link"
	// deliberately: a share token is a secret, and distinguishing those cases
	// tells a probe which of its guesses was once real.
	ErrNoShare = errors.New("no such share link")
	ErrNoGrant = errors.New("not authorized for this app")
)

// Plane is one of the three permissions a share can grant.
//
// The planes are ordered, and the order is the whole of "edit is a superset
// only where intended" (F3 step 5): edit ⊃ data ⊃ use. Editing an app implies
// being able to see its data and use it; the reverse never holds. Every
// authorization decision in this package funnels through Allows so the
// relationship exists in exactly one place.
type Plane string

const (
	PlaneUse  Plane = "use"  // send requests to the app
	PlaneData Plane = "data" // read or export the app's /data
	PlaneEdit Plane = "edit" // replace the app's code
)

func (p Plane) rank() int {
	switch p {
	case PlaneUse:
		return 1
	case PlaneData:
		return 2
	case PlaneEdit:
		return 3
	}
	return 0
}

// Valid reports whether p is one of the three planes.
func (p Plane) Valid() bool { return p.rank() > 0 }

// Allows reports whether a holder of plane p may exercise plane want.
func (p Plane) Allows(want Plane) bool {
	return p.rank() > 0 && want.rank() > 0 && p.rank() >= want.rank()
}

// ParsePlane is the one accepted spelling of each plane.
func ParsePlane(s string) (Plane, error) {
	p := Plane(strings.ToLower(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("unknown plane %q (want use, data or edit)", s)
	}
	return p, nil
}

// Session lifetimes (ROADMAP decision #10c). They are deliberately far apart
// from the identity token's ~5 minutes: F2 changed what a session lifetime is
// FOR. With revocation instant, durable and restore-proof, lifetime is a
// hygiene control rather than a security control — short sessions are what
// you use to compensate for weak revocation, and that compensation is no
// longer needed.
const (
	SessionSliding  = 30 * 24 * time.Hour
	SessionAbsolute = 90 * 24 * time.Hour
)

// flowTTL bounds the one-time secrets that carry a browser between hosts
// during sign-in (OAuth state, the cross-host handoff code). Minutes, not
// hours: these exist only for the length of a redirect chain.
const flowTTL = 10 * time.Minute

// Share is an unguessable capability URL for one app and one plane.
type Share struct {
	ID        string    `json:"id"`  // public, printable: "sh_3f9c1a4b"
	App       string    `json:"app"`
	Plane     Plane     `json:"plane"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	// ExpiresAt zero = never. MaxClaims 0 = unlimited distinct identities.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	MaxClaims int       `json:"max_claims,omitempty"`
	// Claims is how many identities have claimed this link (read-only).
	Claims int `json:"claims"`
	// Revoked is derived from the tombstone log, never from a column a
	// restore could roll back.
	Revoked   bool      `json:"revoked"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// Grant is one identity's claim on one app: the ACL row F1 says must exist
// after a stranger signs in.
type Grant struct {
	ID string `json:"id"`
	App string `json:"app"`
	// Subject is Google's stable "sub". Email can change; sub cannot, so the
	// ACL is keyed on sub and displays email.
	Subject   string    `json:"subject"`
	Email     string    `json:"email"`
	Name      string    `json:"name,omitempty"`
	Plane     Plane     `json:"plane"`
	ShareID   string    `json:"share_id,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	Revoked   bool      `json:"revoked"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// Session is the browser's opaque handle. Two kinds:
//
//	sso — the master session, host-only on the auth host. Identity only.
//	app — one per app host, so a guest that reads its own Cookie header
//	      learns nothing about any other app. Guests are untrusted code;
//	      a domain-wide session cookie forwarded into one would be a
//	      platform-wide session-theft primitive.
type Session struct {
	ID        string // hex sha256 of the cookie value; the value itself is never stored
	Kind      string // "sso" | "app"
	App       string
	Subject   string
	Email     string
	Name      string
	CreatedAt time.Time
	LastSeen  time.Time
}

// Revocation is a tombstone. It is written to the object store BEFORE it is
// acknowledged (F2: D1's rule applied to auth) and replayed from there at
// every start, which is what makes "a revoke a recovery can undo is not a
// revoke" enforceable rather than aspirational.
//
// A tombstone names the grant IDs it kills, and that detail decides three
// behaviors at once:
//
//   - Restoring an older database cannot resurrect access, because grant IDs
//     are stable and the tombstone still names them.
//   - A revoked person cannot re-admit themselves by re-opening the link they
//     were sent: re-claiming updates the SAME grant row (the app/subject/share
//     triple is unique), so it comes back still named by the tombstone.
//   - An operator CAN deliberately re-admit someone, by issuing a fresh link.
//     That produces a new grant with a new ID, which no tombstone names. This
//     is not "undoing a revoke" — it is a new, explicit grant, and it leaves
//     the revocation in the log where the audit trail wants it.
type Revocation struct {
	// ID is both the primary key and the object-store key suffix. It sorts
	// chronologically so the log reads as a timeline.
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // share | identity | app | session
	App     string    `json:"app,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Share   string    `json:"share,omitempty"`
	Session string    `json:"session,omitempty"`
	// Grants are the grant IDs this tombstone kills, captured at revoke time.
	Grants  []string  `json:"grants,omitempty"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

const (
	RevokeShare    = "share"
	RevokeIdentity = "identity"
	RevokeApp      = "app"
	RevokeSession  = "session"
)

// Store is the doorman's durable state. One SQLite file, owned by the
// unprivileged doorman user and shared with nothing.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS shares (
  id         TEXT PRIMARY KEY,
  app        TEXT NOT NULL,
  plane      TEXT NOT NULL,
  secret_id  TEXT NOT NULL UNIQUE,   -- hex sha256 of the link token
  label      TEXT NOT NULL DEFAULT '',
  created    TEXT NOT NULL,
  created_by TEXT NOT NULL DEFAULT '',
  expires    TEXT NOT NULL DEFAULT '',
  max_claims INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_shares_app ON shares(app);

CREATE TABLE IF NOT EXISTS grants (
  id       TEXT PRIMARY KEY,
  app      TEXT NOT NULL,
  subject  TEXT NOT NULL,
  email    TEXT NOT NULL,
  name     TEXT NOT NULL DEFAULT '',
  plane    TEXT NOT NULL,
  share_id TEXT NOT NULL DEFAULT '',
  claimed  TEXT NOT NULL,
  seen     TEXT NOT NULL DEFAULT '',
  UNIQUE(app, subject, share_id)
);
CREATE INDEX IF NOT EXISTS idx_grants_lookup ON grants(app, subject);

CREATE TABLE IF NOT EXISTS sessions (
  id      TEXT PRIMARY KEY,
  kind    TEXT NOT NULL,
  app     TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL,
  email   TEXT NOT NULL,
  name    TEXT NOT NULL DEFAULT '',
  created TEXT NOT NULL,
  seen    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS flows (
  id        TEXT PRIMARY KEY,
  kind      TEXT NOT NULL,
  app       TEXT NOT NULL DEFAULT '',
  nonce     TEXT NOT NULL DEFAULT '',
  return_to TEXT NOT NULL DEFAULT '',
  claim     TEXT NOT NULL DEFAULT '',
  subject   TEXT NOT NULL DEFAULT '',
  email     TEXT NOT NULL DEFAULT '',
  name      TEXT NOT NULL DEFAULT '',
  created   TEXT NOT NULL,
  expires   TEXT NOT NULL
);

-- The tombstones. Deliberately not a column on shares/grants: a column is
-- state a restore can roll back, and F2 FAILs on exactly that.
CREATE TABLE IF NOT EXISTS revocations (
  id      TEXT PRIMARY KEY,
  kind    TEXT NOT NULL,
  app     TEXT NOT NULL DEFAULT '',
  subject TEXT NOT NULL DEFAULT '',
  share   TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '',
  grants  TEXT NOT NULL DEFAULT '',   -- JSON array of grant ids killed
  at      TEXT NOT NULL,
  by      TEXT NOT NULL DEFAULT '',
  reason  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_rev_share ON revocations(share);
CREATE INDEX IF NOT EXISTS idx_rev_ident ON revocations(app, subject);
CREATE INDEX IF NOT EXISTS idx_rev_session ON revocations(session);

-- Derived from revocations.grants by replay, so it is rebuilt from the log
-- rather than restored from a snapshot. Joining against it is what makes a
-- dead grant stay dead.
CREATE TABLE IF NOT EXISTS revoked_grants (
  grant_id TEXT NOT NULL,
  rev_id   TEXT NOT NULL,
  at       TEXT NOT NULL,
  PRIMARY KEY (grant_id, rev_id)
);
`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// One connection: this is a host-local control plane serving one box, and
	// it sidesteps SQLITE_BUSY entirely (same call the registry makes).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("doorman schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- shares

// CreateShare mints a link. The secret is returned exactly once and never
// stored — only its hash is, so a copy of doorman.db does not hand an
// attacker every live share link.
func (s *Store) CreateShare(app string, plane Plane, label, by string, expires time.Time, maxClaims int, now time.Time) (Share, string, error) {
	if !plane.Valid() {
		return Share{}, "", fmt.Errorf("invalid plane %q", plane)
	}
	secret, err := randomToken()
	if err != nil {
		return Share{}, "", err
	}
	sh := Share{
		ID:        "sh_" + shortID(),
		App:       app,
		Plane:     plane,
		Label:     label,
		CreatedAt: now.UTC(),
		CreatedBy: by,
		ExpiresAt: expires.UTC(),
		MaxClaims: maxClaims,
	}
	if expires.IsZero() {
		sh.ExpiresAt = time.Time{}
	}
	_, err = s.db.Exec(`
		INSERT INTO shares (id, app, plane, secret_id, label, created, created_by, expires, max_claims)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sh.ID, sh.App, string(sh.Plane), hashToken(secret), sh.Label,
		ts(sh.CreatedAt), sh.CreatedBy, ts(sh.ExpiresAt), sh.MaxClaims)
	if err != nil {
		return Share{}, "", fmt.Errorf("creating share for %s: %w", app, err)
	}
	return sh, secret, nil
}

// ShareByToken resolves a link secret, enforcing revocation, expiry and the
// claim cap. Every failure returns ErrNoShare — see the comment there.
func (s *Store) ShareByToken(token string, now time.Time) (Share, error) {
	row := s.db.QueryRow(shareCols+` WHERE secret_id = ?`, hashToken(token))
	sh, err := s.scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, ErrNoShare
	}
	if err != nil {
		return Share{}, err
	}
	if sh.Revoked {
		return Share{}, ErrNoShare
	}
	if !sh.ExpiresAt.IsZero() && now.After(sh.ExpiresAt) {
		return Share{}, ErrNoShare
	}
	if sh.MaxClaims > 0 && sh.Claims >= sh.MaxClaims {
		return Share{}, ErrNoShare
	}
	return sh, nil
}

func (s *Store) ShareByID(id string) (Share, error) {
	row := s.db.QueryRow(shareCols+` WHERE id = ?`, id)
	sh, err := s.scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, ErrNoShare
	}
	return sh, err
}

// ListShares returns an app's links (all apps when app == ""), newest first.
func (s *Store) ListShares(app string) ([]Share, error) {
	q := shareCols
	var args []any
	if app != "" {
		q += ` WHERE app = ?`
		args = append(args, app)
	}
	q += ` ORDER BY created DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Share{}
	for rows.Next() {
		sh, err := s.scanShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// shareCols computes revoked-ness and the claim count in the query, so no
// caller can read a share without also reading its tombstone.
const shareCols = `
	SELECT s.id, s.app, s.plane, s.label, s.created, s.created_by, s.expires, s.max_claims,
	       (SELECT COUNT(*) FROM grants g WHERE g.share_id = s.id),
	       (SELECT r.at FROM revocations r
	         WHERE (r.kind = 'share' AND r.share = s.id)
	            OR (r.kind = 'app' AND r.app = s.app)
	         ORDER BY r.at LIMIT 1)
	  FROM shares s`

func (s *Store) scanShare(sc scanner) (Share, error) {
	var sh Share
	var plane, created, expires string
	var revoked sql.NullString
	err := sc.Scan(&sh.ID, &sh.App, &plane, &sh.Label, &created, &sh.CreatedBy,
		&expires, &sh.MaxClaims, &sh.Claims, &revoked)
	if err != nil {
		return Share{}, err
	}
	sh.Plane = Plane(plane)
	sh.CreatedAt = parseTime(created)
	sh.ExpiresAt = parseTime(expires)
	if revoked.Valid && revoked.String != "" {
		sh.Revoked = true
		sh.RevokedAt = parseTime(revoked.String)
	}
	return sh, nil
}

// ---------------------------------------------------------------- grants

// Claim binds an identity to a share — the moment F1 calls "claim". It is
// idempotent: re-opening a link you already hold does not create a second
// grant, and re-claiming after the operator raised the plane upgrades it.
func (s *Store) Claim(sh Share, subject, email, name string, now time.Time) (Grant, error) {
	g := Grant{
		ID:        "gr_" + shortID(),
		App:       sh.App,
		Subject:   subject,
		Email:     email,
		Name:      name,
		Plane:     sh.Plane,
		ShareID:   sh.ID,
		ClaimedAt: now.UTC(),
		LastSeen:  now.UTC(),
	}
	_, err := s.db.Exec(`
		INSERT INTO grants (id, app, subject, email, name, plane, share_id, claimed, seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app, subject, share_id) DO UPDATE SET
		  email = excluded.email,
		  name  = excluded.name,
		  plane = excluded.plane,
		  seen  = excluded.seen`,
		g.ID, g.App, g.Subject, g.Email, g.Name, string(g.Plane), g.ShareID,
		ts(g.ClaimedAt), ts(g.LastSeen))
	if err != nil {
		return Grant{}, fmt.Errorf("claiming share %s: %w", sh.ID, err)
	}
	// Re-read: on conflict the durable row keeps its original id and claim time.
	return s.grantFor(sh.App, subject, sh.ID)
}

// GrantDirect records an identity-based grant with no link behind it. Not
// required by any gate; it exists because `krill share --user` is the obvious
// thing to reach for, and the frozen scope decision permits it as long as it
// cannot weaken F2 — which it cannot, since revocation is keyed on identity
// and app, not on how the grant arrived.
func (s *Store) GrantDirect(app string, plane Plane, subject, email, name string, now time.Time) (Grant, error) {
	g := Grant{
		ID: "gr_" + shortID(), App: app, Subject: subject, Email: email,
		Name: name, Plane: plane, ClaimedAt: now.UTC(), LastSeen: now.UTC(),
	}
	_, err := s.db.Exec(`
		INSERT INTO grants (id, app, subject, email, name, plane, share_id, claimed, seen)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(app, subject, share_id) DO UPDATE SET
		  email = excluded.email, name = excluded.name, plane = excluded.plane`,
		g.ID, g.App, g.Subject, g.Email, g.Name, string(g.Plane), ts(g.ClaimedAt), ts(g.LastSeen))
	if err != nil {
		return Grant{}, err
	}
	return s.grantFor(app, subject, "")
}

func (s *Store) grantFor(app, subject, shareID string) (Grant, error) {
	row := s.db.QueryRow(grantCols+` WHERE g.app = ? AND g.subject = ? AND g.share_id = ?`,
		app, subject, shareID)
	return scanGrant(row)
}

// Best returns the strongest live grant an identity holds on an app.
//
// This is THE authorization query and every clause is load-bearing. A grant
// is live only while: no tombstone names it, the link it came from has not
// been revoked, and the app itself has not been revoked. Because tombstones
// are replayed from the object store at every start, no restore of this
// database can make a revoked grant live again — which is F2 step 3 reduced
// to three NOT EXISTS clauses.
func (s *Store) Best(app, subject string) (Grant, error) {
	row := s.db.QueryRow(grantCols+`
		WHERE g.app = ? AND g.subject = ?`+liveGrant+`
		ORDER BY CASE g.plane WHEN 'edit' THEN 3 WHEN 'data' THEN 2 ELSE 1 END DESC
		LIMIT 1`, app, subject)
	g, err := scanGrant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, ErrNoGrant
	}
	return g, err
}

const liveGrant = `
	  AND NOT EXISTS (SELECT 1 FROM revoked_grants rg WHERE rg.grant_id = g.id)
	  AND NOT EXISTS (SELECT 1 FROM revocations r
	                   WHERE r.kind = 'share' AND g.share_id <> '' AND r.share = g.share_id)
	  AND NOT EXISTS (SELECT 1 FROM revocations r
	                   WHERE r.kind = 'app' AND r.app = g.app)`

// ListGrants is the ACL as the operator sees it: every claim on an app,
// including revoked ones, because "show them it was them" (F1) and "prove the
// revoke stuck" (F2) are the same screen.
func (s *Store) ListGrants(app string) ([]Grant, error) {
	q := grantCols
	var args []any
	if app != "" {
		q += ` WHERE g.app = ?`
		args = append(args, app)
	}
	q += ` ORDER BY g.claimed DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

const grantCols = `
	SELECT g.id, g.app, g.subject, g.email, g.name, g.plane, g.share_id, g.claimed, g.seen,
	       COALESCE(
	         (SELECT rg.at FROM revoked_grants rg WHERE rg.grant_id = g.id ORDER BY rg.at LIMIT 1),
	         (SELECT r.at FROM revocations r
	           WHERE (r.kind = 'share' AND g.share_id <> '' AND r.share = g.share_id)
	              OR (r.kind = 'app' AND r.app = g.app)
	           ORDER BY r.at LIMIT 1))
	  FROM grants g`

func scanGrant(sc scanner) (Grant, error) {
	var g Grant
	var plane, claimed, seen string
	var revoked sql.NullString
	err := sc.Scan(&g.ID, &g.App, &g.Subject, &g.Email, &g.Name, &plane,
		&g.ShareID, &claimed, &seen, &revoked)
	if err != nil {
		return Grant{}, err
	}
	g.Plane = Plane(plane)
	g.ClaimedAt = parseTime(claimed)
	g.LastSeen = parseTime(seen)
	if revoked.Valid && revoked.String != "" {
		g.Revoked = true
		g.RevokedAt = parseTime(revoked.String)
	}
	return g, nil
}

// TouchGrant records that an identity actually used an app. Best-effort: a
// failure here must never turn a working request into an error.
func (s *Store) TouchGrant(id string, now time.Time) {
	_, _ = s.db.Exec(`UPDATE grants SET seen = ? WHERE id = ?`, ts(now.UTC()), id)
}

// PruneApp removes an app's shares and grants. Called on a definitive
// "app does not exist" from krilld — never on a transport error, because a
// krilld that is merely restarting must not cost anyone their access.
func (s *Store) PruneApp(app string) error {
	if _, err := s.db.Exec(`DELETE FROM shares WHERE app = ?`, app); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM grants WHERE app = ?`, app); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE app = ?`, app)
	return err
}

// ---------------------------------------------------------------- sessions

// NewSession returns the cookie value (256 bits of randomness) and stores
// only its hash. There is no signed, stateless variant of this on purpose:
// F2 forbids a refusal that waits for a token to expire, and a stateless
// cookie is precisely a credential the server cannot take back.
func (s *Store) NewSession(kind, app, subject, email, name string, now time.Time) (string, error) {
	value, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`
		INSERT INTO sessions (id, kind, app, subject, email, name, created, seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(value), kind, app, subject, email, name, ts(now.UTC()), ts(now.UTC()))
	if err != nil {
		return "", err
	}
	return value, nil
}

// Session resolves a cookie value, enforcing both lifetimes and the session
// tombstone. Expired or revoked sessions are deleted as they are found.
func (s *Store) Session(value, kind, app string, now time.Time) (Session, bool) {
	id := hashToken(value)
	var sess Session
	var created, seen string
	err := s.db.QueryRow(`
		SELECT id, kind, app, subject, email, name, created, seen
		  FROM sessions WHERE id = ? AND kind = ? AND app = ?`, id, kind, app).
		Scan(&sess.ID, &sess.Kind, &sess.App, &sess.Subject, &sess.Email, &sess.Name, &created, &seen)
	if err != nil {
		return Session{}, false
	}
	sess.CreatedAt, sess.LastSeen = parseTime(created), parseTime(seen)
	if now.Sub(sess.LastSeen) > SessionSliding || now.Sub(sess.CreatedAt) > SessionAbsolute {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
		return Session{}, false
	}
	var revoked int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM revocations WHERE kind = 'session' AND session = ?`, id).Scan(&revoked)
	if revoked > 0 {
		return Session{}, false
	}
	// Slide the window, but not on every request: one write per session per
	// minute is enough to keep a daily user logged in forever, and it keeps
	// the hot path read-only.
	if now.Sub(sess.LastSeen) > time.Minute {
		_, _ = s.db.Exec(`UPDATE sessions SET seen = ? WHERE id = ?`, ts(now.UTC()), id)
	}
	return sess, true
}

// DropSession forgets one session (sign-out). Revoking an identity does not
// need this — Best already refuses — but a user who signs out should not
// leave a usable handle behind.
func (s *Store) DropSession(value, kind, app string) {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE id = ? AND kind = ? AND app = ?`,
		hashToken(value), kind, app)
}

// ---------------------------------------------------------------- flows

// Flow is a one-time secret carrying a browser across a redirect: the OAuth
// state/nonce on the way to Google, and the cross-host handoff code that
// turns the master session into a per-app session.
type Flow struct {
	Kind     string
	App      string
	Nonce    string
	ReturnTo string
	Claim    string // share id being claimed, if this flow came from a link
	Subject  string
	Email    string
	Name     string
}

func (s *Store) NewFlow(f Flow, now time.Time) (string, error) {
	value, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`
		INSERT INTO flows (id, kind, app, nonce, return_to, claim, subject, email, name, created, expires)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(value), f.Kind, f.App, f.Nonce, f.ReturnTo, f.Claim,
		f.Subject, f.Email, f.Name, ts(now.UTC()), ts(now.UTC().Add(flowTTL)))
	if err != nil {
		return "", err
	}
	return value, nil
}

// ConsumeFlow is single-use by construction: the DELETE is the check, so a
// replayed state parameter finds nothing.
func (s *Store) ConsumeFlow(value, kind string, now time.Time) (Flow, bool) {
	var f Flow
	var expires string
	err := s.db.QueryRow(`
		DELETE FROM flows WHERE id = ? AND kind = ?
		RETURNING kind, app, nonce, return_to, claim, subject, email, name, expires`,
		hashToken(value), kind).
		Scan(&f.Kind, &f.App, &f.Nonce, &f.ReturnTo, &f.Claim, &f.Subject, &f.Email, &f.Name, &expires)
	if err != nil || now.After(parseTime(expires)) {
		return Flow{}, false
	}
	return f, true
}

// SweepExpired drops flows and sessions that can no longer authorize
// anything. Revocations are never swept: they are the record.
func (s *Store) SweepExpired(now time.Time) {
	_, _ = s.db.Exec(`DELETE FROM flows WHERE expires < ?`, ts(now.UTC()))
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE seen < ? OR created < ?`,
		ts(now.UTC().Add(-SessionSliding)), ts(now.UTC().Add(-SessionAbsolute)))
}

// ---------------------------------------------------------------- helpers

type scanner interface{ Scan(...any) error }

// randomToken is 256 bits, base64url, no padding: unguessable is the entire
// security of a share link, so this is the one place the project refuses to
// economize.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func hashToken(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func shortID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition this process can serve through.
		panic("doorman: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
