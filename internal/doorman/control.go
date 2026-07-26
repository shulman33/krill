package doorman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Control is the doorman's operator API, and the second endpoint `krill`
// talks to (decision #10b: admin port + 1, 9091 → 9092).
//
//	POST /v1/shares       {app, plane, label?, expires_in?, max_claims?}
//	GET  /v1/shares       ?app=
//	GET  /v1/grants       ?app=            — the ACL, including revoked rows
//	POST /v1/revoke       {kind, app, email|subject, share}
//	GET  /v1/revocations
//	POST /v1/sync         replay the revocation log now
//	POST /v1/snapshot     ship the doorman database now
//	GET  /v1/status       what this doorman is, and whether revokes can land
//
// Loopback only, exactly like krilld's admin API: anything that can reach it
// can grant access to every app on the host.
type Control struct {
	cfg   Config
	store *Store
	rev   *Revoker
	snap  *Snapshotter
	apps  AppSource
	key   *IdentityKey
	log   *slog.Logger
	Now   func() time.Time
}

func NewControl(cfg Config, st *Store, rev *Revoker, snap *Snapshotter, apps AppSource, key *IdentityKey, log *slog.Logger) *Control {
	if log == nil {
		log = slog.Default()
	}
	if cfg.AuthHost == "" {
		cfg.AuthHost = "auth." + cfg.BaseHost
	}
	return &Control{cfg: cfg, store: st, rev: rev, snap: snap, apps: apps,
		key: key, log: log, Now: time.Now}
}

func (c *Control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		w.Write([]byte("ok\n"))
	case path == "/v1/shares" && r.Method == http.MethodPost:
		c.createShare(w, r)
	case path == "/v1/shares" && r.Method == http.MethodGet:
		c.listShares(w, r)
	case path == "/v1/grants" && r.Method == http.MethodGet:
		c.listGrants(w, r)
	case path == "/v1/revoke" && r.Method == http.MethodPost:
		c.revoke(w, r)
	case path == "/v1/revocations" && r.Method == http.MethodGet:
		c.listRevocations(w, r)
	case path == "/v1/sync" && r.Method == http.MethodPost:
		c.sync(w, r)
	case path == "/v1/snapshot" && r.Method == http.MethodPost:
		c.snapshot(w, r)
	case path == "/v1/snapshots" && r.Method == http.MethodGet:
		c.listSnapshots(w, r)
	case path == "/v1/status" && r.Method == http.MethodGet:
		c.status(w, r)
	default:
		http.NotFound(w, r)
	}
}

type createShareReq struct {
	App   string `json:"app"`
	Plane string `json:"plane"`
	Label string `json:"label"`
	// ExpiresIn is a Go duration string ("72h"); empty means never.
	ExpiresIn string `json:"expires_in"`
	MaxClaims int    `json:"max_claims"`
	CreatedBy string `json:"created_by"`
}

type shareResp struct {
	Share Share `json:"share"`
	// Link is returned EXACTLY ONCE. Only its hash is stored, so a lost link
	// is re-issued, never recovered — which is also why a stolen copy of
	// doorman.db is not a set of working share links.
	Link string `json:"link"`
}

func (c *Control) createShare(w http.ResponseWriter, r *http.Request) {
	var req createShareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	plane, err := ParsePlane(req.Plane)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// Refuse to share an app that does not exist: a link to nothing is a
	// support question waiting to happen, and it is the cheapest possible
	// place to catch a typo in an app name.
	exists, err := c.apps.Exists(r.Context(), req.App)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, fmt.Errorf("cannot reach krilld to check %q: %w", req.App, err))
		return
	}
	if !exists {
		fail(w, http.StatusNotFound, fmt.Errorf("no app named %q on this host", req.App))
		return
	}
	var expires time.Time
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("expires_in: %w", err))
			return
		}
		expires = c.Now().Add(d)
	}
	sh, secret, err := c.store.CreateShare(req.App, plane, req.Label, req.CreatedBy, expires, req.MaxClaims, c.Now())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	c.log.Info("share created", "app", sh.App, "share", sh.ID, "plane", sh.Plane,
		"by", req.CreatedBy)
	reply(w, http.StatusCreated, shareResp{Share: sh, Link: c.linkFor(sh.App, secret)})
}

func (c *Control) linkFor(app, secret string) string {
	return c.cfg.Scheme + "://" + app + "." + c.cfg.BaseHost + Reserved + "s/" + secret
}

func (c *Control) listShares(w http.ResponseWriter, r *http.Request) {
	list, err := c.store.ListShares(r.URL.Query().Get("app"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusOK, list)
}

func (c *Control) listGrants(w http.ResponseWriter, r *http.Request) {
	list, err := c.store.ListGrants(r.URL.Query().Get("app"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusOK, list)
}

type revokeReq struct {
	Kind    string `json:"kind"` // share | identity | app | session
	App     string `json:"app"`
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Share   string `json:"share"`
	Session string `json:"session"`
	By      string `json:"by"`
	Reason  string `json:"reason"`
}

// revoke is F2's operator half. It returns only after the tombstone is
// durable at the object store — a 200 here is a promise that no restore can
// take back, and a 503 means nothing was revoked at all.
func (c *Control) revoke(w http.ResponseWriter, r *http.Request) {
	var req revokeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rv := Revocation{Kind: req.Kind, App: req.App, Subject: req.Subject,
		Share: req.Share, Session: req.Session, By: req.By, Reason: req.Reason}
	switch req.Kind {
	case RevokeShare:
		sh, err := c.store.ShareByID(req.Share)
		if err != nil {
			fail(w, http.StatusNotFound, fmt.Errorf("no share %q", req.Share))
			return
		}
		rv.App = sh.App
	case RevokeIdentity:
		if rv.Subject == "" {
			subject, err := c.subjectFor(req.App, req.Email)
			if err != nil {
				fail(w, http.StatusNotFound, err)
				return
			}
			rv.Subject = subject
		}
		if rv.App == "" {
			fail(w, http.StatusBadRequest, errors.New("identity revoke needs an app"))
			return
		}
	case RevokeApp:
		if rv.App == "" {
			fail(w, http.StatusBadRequest, errors.New("app revoke needs an app"))
			return
		}
	case RevokeSession:
		if rv.Session == "" {
			fail(w, http.StatusBadRequest, errors.New("session revoke needs a session id"))
			return
		}
	default:
		fail(w, http.StatusBadRequest, fmt.Errorf("unknown revoke kind %q", req.Kind))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	done, err := c.rev.Revoke(ctx, rv)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, ErrNoRecord) {
			code = http.StatusServiceUnavailable
		}
		c.log.Error("REVOKE FAILED — nothing was revoked", "kind", req.Kind,
			"app", rv.App, "err", err)
		fail(w, code, err)
		return
	}
	c.log.Warn("revoked", "id", done.ID, "kind", done.Kind, "app", done.App,
		"subject", done.Subject, "share", done.Share, "grants", len(done.Grants))
	reply(w, http.StatusOK, done)
}

// subjectFor maps an email to the stable Google subject the ACL is keyed on.
func (c *Control) subjectFor(app, email string) (string, error) {
	if email == "" {
		return "", errors.New("need --user <email> or a subject")
	}
	grants, err := c.store.ListGrants(app)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(email)
	for _, g := range grants {
		if strings.ToLower(g.Email) == want {
			return g.Subject, nil
		}
	}
	return "", fmt.Errorf("%q has no grant on %q (see `krill shares %s`)", email, app, app)
}

func (c *Control) listRevocations(w http.ResponseWriter, r *http.Request) {
	list, err := c.rev.List(200)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusOK, list)
}

func (c *Control) sync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	n, err := c.rev.Sync(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"applied": n})
}

func (c *Control) snapshot(w http.ResponseWriter, r *http.Request) {
	if c.snap == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("no object store: snapshots are off"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	info, err := c.snap.RunOnce(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	reply(w, http.StatusOK, info)
}

func (c *Control) listSnapshots(w http.ResponseWriter, r *http.Request) {
	if c.snap == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("no object store: snapshots are off"))
		return
	}
	list, err := c.snap.List(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	reply(w, http.StatusOK, list)
}

// status answers the question an operator actually has before sharing
// anything with a person: can this box take access away again?
func (c *Control) status(w http.ResponseWriter, r *http.Request) {
	shares, grants, revs := c.store.counts()
	out := map[string]any{
		"base_host":      c.cfg.BaseHost,
		"auth_host":      c.cfg.AuthHost,
		"scheme":         c.cfg.Scheme,
		"owners":         c.cfg.Owners,
		"identity_key":   c.key.KeyID,
		"identity_pub":   c.key.PublicB64(),
		"shares":         shares,
		"grants":         grants,
		"revocations":    revs,
		"revoke_durable": c.rev.obj != nil,
	}
	if c.rev.obj != nil {
		if d, ok := c.rev.obj.(interface{ Describe() string }); ok {
			out["objstore"] = d.Describe()
		}
	}
	reply(w, http.StatusOK, out)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	reply(w, code, map[string]string{"error": err.Error()})
}
