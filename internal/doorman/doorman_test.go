package doorman

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/objstore"
)

// The doorman's gates are F1–F3, and this file is their unit-level rehearsal:
// the same sequences the m4-gates scripts run on hardware, driven here
// against a fake identity provider so the ACL, the claim, the revoke and the
// restore can be exercised without a browser or a Google client.
//
// What it deliberately does NOT prove: that Google's real flow works (F1's
// hardware half), or that Caddy wires forward_auth the way we think (F7).

// fakeAuth stands in for Google. AuthCodeURL bounces straight back to the
// callback, so a test "sign-in" is one redirect instead of a consent screen.
type fakeAuth struct {
	ident Identity
	// fail makes Exchange reject, for the cancelled-sign-in path.
	fail bool
}

func (f *fakeAuth) AuthCodeURL(state, nonce string) string {
	q := url.Values{"state": {state}, "code": {"code:" + nonce}}
	return "http://auth.krill.test/_krill/auth/callback?" + q.Encode()
}

func (f *fakeAuth) Exchange(_ context.Context, code, nonce string) (Identity, error) {
	if f.fail {
		return Identity{}, errors.New("declined")
	}
	if code != "code:"+nonce {
		return Identity{}, fmt.Errorf("nonce mismatch: %q vs %q", code, nonce)
	}
	return f.ident, nil
}

type harness struct {
	t      *testing.T
	dir    string
	dbPath string
	store  *Store
	rev    *Revoker
	obj    objstore.Store
	key    *IdentityKey
	auth   *fakeAuth
	srv    *httptest.Server
	ctl    *httptest.Server
	cfg    Config
}

func newHarness(t *testing.T, owners ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, dbPath: filepath.Join(dir, "doorman.db")}
	h.obj = objstore.NewMem()
	h.auth = &fakeAuth{ident: Identity{Subject: "sub-alice", Email: "alice@example.com", Name: "Alice"}}
	h.key = NewTestKey()
	h.cfg = Config{
		BaseHost: "krill.test", AuthHost: "auth.krill.test", Scheme: "http",
		CookieSecure: false, Owners: owners,
		// Unreachable on purpose: a 502 from here proves authorization passed
		// and the request was on its way to krilld, which is the distinction
		// the plane tests care about.
		AdminAddr: "http://127.0.0.1:1",
	}
	h.open()
	t.Cleanup(func() {
		h.srv.Close()
		h.ctl.Close()
		h.store.Close()
	})
	return h
}

// open (re)builds every layer over whatever is on disk — the same startup
// path cmd/krill-doorman runs, so "restart the doorman" in a test is exactly
// what it is in production.
func (h *harness) open() {
	st, err := OpenStore(h.dbPath)
	if err != nil {
		h.t.Fatalf("open store: %v", err)
	}
	h.store = st
	h.rev = NewRevoker(st, h.obj, quietLogger())
	if _, err := h.rev.Sync(context.Background()); err != nil {
		h.t.Fatalf("revocation sync: %v", err)
	}
	apps := StaticApps{"ledger": true, "other": true}
	krilld := NewKrilldApps(h.cfg.AdminAddr)
	if h.srv != nil {
		h.srv.Close()
		h.ctl.Close()
	}
	h.srv = httptest.NewServer(NewServer(h.cfg, st, h.rev, h.key, h.auth, apps, krilld, quietLogger()))
	snap := NewSnapshotter(st, h.obj, h.dbPath, filepath.Join(h.dir, "work"), 0, 3, quietLogger())
	h.ctl = httptest.NewServer(NewControl(h.cfg, st, h.rev, snap, apps, h.key, quietLogger()))
}

// client is a browser: a cookie jar plus a resolver that sends every krill
// hostname to the test server, so the multi-host redirect chain is real.
func (h *harness) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			// Resolved at dial time, not at client construction: a test that
			// restarts the doorman gets a new listener, and the browser is
			// supposed to survive that — which is half of what F2 asserts.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(h.srv.URL, "http://"))
			},
		},
	}
}

// signIn walks a share link all the way through sign-in, exactly as a
// recipient's browser would: claim link → auth host → callback → accept.
func (h *harness) signIn(c *http.Client, app, link string) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	// A browser is what opens a share link, and the doorman answers API
	// clients differently (401 + JSON rather than a redirect into Google).
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatalf("opening share link: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// The last hop lands on the app's own root, which this mux does not serve.
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusOK {
		h.t.Fatalf("share link flow ended at %s (%s)", resp.Status, resp.Request.URL)
	}
}

// verify is one forward_auth call, the way Caddy makes it.
func (h *harness) verify(c *http.Client, host, uri string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+host+"/_krill/auth/verify", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Host", host)
	req.Header.Set("X-Forwarded-Uri", uri)
	req.Header.Set("Accept", "text/html")
	noRedirect := *c
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		h.t.Fatalf("verify: %v", err)
	}
	return resp
}

func (h *harness) share(app, plane string) (Share, string) {
	h.t.Helper()
	body, _ := json.Marshal(createShareReq{App: app, Plane: plane, CreatedBy: "sam"})
	resp, err := http.Post(h.ctl.URL+"/v1/shares", "application/json", strings.NewReader(string(body)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("create share: %s: %s", resp.Status, raw)
	}
	var out shareResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out.Share, out.Link
}

func (h *harness) revoke(req revokeReq) (*http.Response, []byte) {
	h.t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(h.ctl.URL+"/v1/revoke", "application/json", strings.NewReader(string(body)))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// quietLogger keeps the doorman's (deliberately chatty) audit lines out of
// test output. The lines themselves are checked by the gate scripts, where
// they are evidence rather than noise.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// F1 — a stranger opens a link, signs in, and the app learns who they are as
// a token it can verify rather than a bare header.
func TestF1_StrangerGetsInWithVerifiableIdentity(t *testing.T) {
	h := newHarness(t)
	_, link := h.share("ledger", "use")

	c := h.client()
	// Before signing in, the door is shut and the app is never consulted.
	if resp := h.verify(c, "ledger.krill.test", "/"); resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated verify = %s, want 302 to sign in", resp.Status)
	}

	h.signIn(c, "ledger", link)
	resp := h.verify(c, "ledger.krill.test", "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify after sign-in = %s, want 200", resp.Status)
	}
	if got := resp.Header.Get("X-App-User"); got != "alice@example.com" {
		t.Fatalf("X-App-User = %q", got)
	}
	if got := resp.Header.Get("X-App-Plane"); got != "use" {
		t.Fatalf("X-App-Plane = %q", got)
	}
	claims, err := VerifyToken(h.key.PublicB64(), resp.Header.Get("X-Krill-Token"), "ledger", time.Now())
	if err != nil {
		t.Fatalf("guest cannot verify the identity token: %v", err)
	}
	if claims.Email != "alice@example.com" || claims.Subject != "sub-alice" {
		t.Fatalf("token claims = %+v", claims)
	}

	// The ACL now names her — F1's "Sam can show them it was them".
	grants, err := h.store.ListGrants("ledger")
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %v, %v", grants, err)
	}
	if grants[0].Email != "alice@example.com" || grants[0].Plane != PlaneUse {
		t.Fatalf("grant = %+v", grants[0])
	}
}

// F3 step 3 — the token names exactly one app.
func TestF3_TokenAudienceIsOneApp(t *testing.T) {
	h := newHarness(t)
	_, link := h.share("ledger", "use")
	c := h.client()
	h.signIn(c, "ledger", link)
	token := h.verify(c, "ledger.krill.test", "/").Header.Get("X-Krill-Token")

	if _, err := VerifyToken(h.key.PublicB64(), token, "other", time.Now()); err == nil {
		t.Fatal("a ledger token verified against another app: the audience is not scoped")
	}
	// And it dies on its own, quickly.
	if _, err := VerifyToken(h.key.PublicB64(), token, "ledger", time.Now().Add(TokenTTL+time.Second)); err == nil {
		t.Fatal("token outlived its TTL")
	}
}

// F3 step 4 — only <app>.<base-host> routes, and nothing else. Every case
// here is one the gates name explicitly.
func TestF3_HostSuffixPinning(t *testing.T) {
	h := newHarness(t)
	_, link := h.share("ledger", "use")
	c := h.client()
	h.signIn(c, "ledger", link)

	for _, host := range []string{
		"ledger.example.invalid",  // right app label, someone else's domain
		"ledger.local.krill.test", // an extra label: not one app label
		"46.4.64.187",             // a bare IP
		"krill.test",              // the base host itself
		"auth.krill.test",         // the auth host is not an app
		"ledger.krill.test.evil",  // suffix as a prefix of a longer name
	} {
		resp := h.verify(c, host, "/")
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Errorf("Host %q verified with %s, want a refusal", host, resp.Status)
		}
	}
	// The real one still works, so the pin is not just "refuse everything".
	if resp := h.verify(c, "ledger.krill.test", "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the legitimate host was refused: %s", resp.Status)
	}
}

// F3 steps 2 and 5 — a use-only holder cannot reach the data or edit planes,
// and the refusal happens at the doorman.
func TestF3_PlanesSeparate(t *testing.T) {
	h := newHarness(t)
	_, useLink := h.share("ledger", "use")
	c := h.client()
	h.signIn(c, "ledger", useLink)

	for _, path := range []string{"/_krill/data/stream", "/_krill/data/db"} {
		resp, err := c.Get("http://ledger.krill.test" + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("use-only holder reached %s with %s, want 403", path, resp.Status)
		}
	}
	resp, err := c.Post("http://ledger.krill.test/_krill/edit/deploy", "application/gzip", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("use-only holder reached the edit plane with %s, want 403", resp.Status)
	}

	// An app she was never shared on. Unauthenticated there (sessions are
	// per-app), so the first answer is a redirect to sign in; after she does,
	// the answer is 403 — and at no point does the request reach the app.
	if got := h.verify(c, "other.krill.test", "/"); got.StatusCode != http.StatusFound {
		t.Errorf("cross-app first touch = %s, want a redirect to sign in", got.Status)
	}
	ssoInto(t, c, "other") // she already holds a master session, so this is silent
	if got := h.verify(c, "other.krill.test", "/"); got.StatusCode != http.StatusForbidden {
		t.Errorf("cross-app access after sign-in = %s, want 403", got.Status)
	}

	// A data holder may read data and still may not deploy; an edit holder may
	// do both. This is the whole of "edit is a superset only where intended".
	dataSh, dataLink := h.share("ledger", "data")
	_ = dataSh
	h.auth.ident = Identity{Subject: "sub-dana", Email: "dana@example.com"}
	dc := h.client()
	h.signIn(dc, "ledger", dataLink)
	if resp := mustGet(t, dc, "http://ledger.krill.test/_krill/data/stream"); resp != http.StatusBadGateway {
		t.Errorf("data holder reading the stream = %d, want 502 (authorized, krilld unreachable in this test)", resp)
	}
	if resp := mustPost(t, dc, "http://ledger.krill.test/_krill/edit/deploy"); resp != http.StatusForbidden {
		t.Errorf("data holder deploying = %d, want 403", resp)
	}
	_, editLink := h.share("ledger", "edit")
	h.auth.ident = Identity{Subject: "sub-eve", Email: "eve@example.com"}
	ec := h.client()
	h.signIn(ec, "ledger", editLink)
	if resp := mustGet(t, ec, "http://ledger.krill.test/_krill/data/stream"); resp != http.StatusBadGateway {
		t.Errorf("edit holder reading the stream = %d, want 502 (edit ⊃ data)", resp)
	}
	if resp := mustPost(t, ec, "http://ledger.krill.test/_krill/edit/deploy"); resp != http.StatusBadGateway {
		t.Errorf("edit holder deploying = %d, want 502 (authorized, krilld unreachable in this test)", resp)
	}
}

// F2 — revoke lands on the very next request, for both variants, and no
// restart, freeze/wake or TTL is involved.
func TestF2_RevokeTakesEffectOnTheNextRequest(t *testing.T) {
	h := newHarness(t)
	sh, link := h.share("ledger", "use")

	alice := h.client()
	h.signIn(alice, "ledger", link)
	h.auth.ident = Identity{Subject: "sub-bob", Email: "bob@example.com"}
	bob := h.client()
	h.signIn(bob, "ledger", link)

	for name, c := range map[string]*http.Client{"alice": alice, "bob": bob} {
		if resp := h.verify(c, "ledger.krill.test", "/"); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s should be in before the revoke: %s", name, resp.Status)
		}
	}

	// (a) revoke the claimed identity.
	if resp, raw := h.revoke(revokeReq{Kind: RevokeIdentity, App: "ledger",
		Email: "alice@example.com", By: "sam"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke identity: %s: %s", resp.Status, raw)
	}
	if resp := h.verify(alice, "ledger.krill.test", "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked identity served with %s on the very next request", resp.Status)
	}
	if resp := h.verify(bob, "ledger.krill.test", "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoking alice also cut off bob: %s", resp.Status)
	}

	// Re-opening the link she still has must NOT re-admit her: a revoke that
	// one click undoes is not a revoke.
	h.auth.ident = Identity{Subject: "sub-alice", Email: "alice@example.com"}
	h.signIn(alice, "ledger", link)
	if resp := h.verify(alice, "ledger.krill.test", "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a revoked identity re-claimed the link she was sent: %s", resp.Status)
	}

	// (b) revoke the link itself, with bob holding a live session from it.
	if resp, raw := h.revoke(revokeReq{Kind: RevokeShare, Share: sh.ID, By: "sam"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke share: %s: %s", resp.Status, raw)
	}
	if resp := h.verify(bob, "ledger.krill.test", "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("session claimed from a revoked link served with %s", resp.Status)
	}
	// And the link cannot be claimed by anyone, ever again.
	h.auth.ident = Identity{Subject: "sub-carl", Email: "carl@example.com"}
	carl := h.client()
	resp, err := carl.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a revoked link was still openable: %s", resp.Status)
	}
}

// F2 step 3 — the amendment. Destroy every byte of local auth state, restore
// from the object store alone, and the revoked user is still out.
func TestF2_RevocationSurvivesTotalLocalStateLoss(t *testing.T) {
	h := newHarness(t)
	_, link := h.share("ledger", "use")
	alice := h.client()
	h.signIn(alice, "ledger", link)
	if resp := h.verify(alice, "ledger.krill.test", "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: %s", resp.Status)
	}

	// A snapshot taken BEFORE the revoke is the dangerous one: restoring it is
	// exactly the "recovery undoes a revoke" failure the gate was tightened to
	// forbid.
	snap := NewSnapshotter(h.store, h.obj, h.dbPath, filepath.Join(h.dir, "work"), 0, 3, quietLogger())
	if _, err := snap.RunOnce(context.Background()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if resp, raw := h.revoke(revokeReq{Kind: RevokeIdentity, App: "ledger",
		Email: "alice@example.com", By: "sam"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %s: %s", resp.Status, raw)
	}
	if resp := h.verify(alice, "ledger.krill.test", "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoke did not take: %s", resp.Status)
	}

	// Total local-state loss: stop, delete the database (and its WAL), restore
	// from the object store, start again.
	h.store.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(h.dbPath + suffix)
	}
	info, err := RestoreLatest(context.Background(), h.obj, h.dbPath)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if info.Revocations != 0 {
		t.Fatalf("the restored snapshot already contained the revocation (%d) — "+
			"this test would then prove nothing", info.Revocations)
	}
	h.open() // replays the revocation log, as the daemon does at every start

	if resp := h.verify(alice, "ledger.krill.test", "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a restore resurrected a revoked grant: %s — F2 FAIL", resp.Status)
	}
	// The restore did bring the rest of the ACL back, so this is a real
	// restore rather than an empty database trivially denying everything.
	if shares, err := h.store.ListShares("ledger"); err != nil || len(shares) != 1 {
		t.Fatalf("restored shares = %v, %v; the restore did not actually restore", shares, err)
	}
}

// A revoke that cannot be made durable must fail loudly rather than succeed
// locally — otherwise the operator believes in a revoke that a restore undoes.
func TestF2_RevokeWithoutAnObjectStoreIsRefused(t *testing.T) {
	h := newHarness(t)
	h.rev = NewRevoker(h.store, nil, quietLogger())
	_, err := h.rev.Revoke(context.Background(), Revocation{Kind: RevokeApp, App: "ledger"})
	if !errors.Is(err, ErrNoRecord) {
		t.Fatalf("revoke without a record store returned %v, want ErrNoRecord", err)
	}
}

// Owners hold edit everywhere without a share. This is how Sam reaches his
// own apps through his own front door.
func TestOwnerHoldsEveryPlane(t *testing.T) {
	h := newHarness(t, "sam@example.com")
	h.auth.ident = Identity{Subject: "sub-sam", Email: "sam@example.com"}
	c := h.client()
	// No share link at all: sign in at the auth host, then land on the app.
	resp, err := c.Get("http://auth.krill.test/_krill/auth/sso?" +
		url.Values{"app": {"ledger"}, "rd": {"http://ledger.krill.test/"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := h.verify(c, "ledger.krill.test", "/")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("owner verify = %s", got.Status)
	}
	if got.Header.Get("X-App-Plane") != "edit" {
		t.Fatalf("owner plane = %q, want edit", got.Header.Get("X-App-Plane"))
	}
}

// The bearer form of a share link is what lets an agent deploy without a
// browser — F5's delivery path. It must obey exactly the same plane rules.
func TestBearerShareTokenObeysPlanes(t *testing.T) {
	h := newHarness(t)
	_, useLink := h.share("ledger", "use")
	_, editLink := h.share("ledger", "edit")
	useTok := tokenOf(t, useLink)
	editTok := tokenOf(t, editLink)

	if code := bearerPost(t, h, "http://ledger.krill.test/_krill/edit/deploy", useTok); code != http.StatusForbidden {
		t.Errorf("use-plane bearer deployed: %d", code)
	}
	if code := bearerPost(t, h, "http://ledger.krill.test/_krill/edit/deploy", editTok); code != http.StatusBadGateway {
		t.Errorf("edit-plane bearer = %d, want 502 (authorized, krilld unreachable here)", code)
	}
	// A token for one app is useless against another.
	if code := bearerPost(t, h, "http://other.krill.test/_krill/edit/deploy", editTok); code != http.StatusForbidden {
		t.Errorf("edit bearer worked cross-app: %d", code)
	}
}

// Sessions are opaque handles to rows, never signed blobs: the property F2
// depends on. A forged or unknown value must simply not resolve.
func TestSessionsAreOpaqueServerSide(t *testing.T) {
	h := newHarness(t)
	if _, ok := h.store.Session(strings.Repeat("a", 43), "app", "ledger", time.Now()); ok {
		t.Fatal("an invented session value resolved")
	}
	v, err := h.store.NewSession("app", "ledger", "sub-x", "x@example.com", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.Session(v, "app", "other", time.Now()); ok {
		t.Fatal("an app session resolved against a different app")
	}
	if _, ok := h.store.Session(v, "sso", "", time.Now()); ok {
		t.Fatal("an app session resolved as a master session")
	}
	// Both lifetimes are enforced, and the absolute cap wins over sliding use.
	old := time.Now().Add(-SessionAbsolute - time.Hour)
	v2, _ := h.store.NewSession("app", "ledger", "sub-y", "y@example.com", "", old)
	if _, ok := h.store.Session(v2, "app", "ledger", time.Now()); ok {
		t.Fatal("a session past its absolute cap still resolved")
	}
}

func TestPlaneOrdering(t *testing.T) {
	cases := []struct {
		holds, wants Plane
		allow        bool
	}{
		{PlaneUse, PlaneUse, true}, {PlaneUse, PlaneData, false}, {PlaneUse, PlaneEdit, false},
		{PlaneData, PlaneUse, true}, {PlaneData, PlaneData, true}, {PlaneData, PlaneEdit, false},
		{PlaneEdit, PlaneUse, true}, {PlaneEdit, PlaneData, true}, {PlaneEdit, PlaneEdit, true},
		{Plane("admin"), PlaneUse, false},
	}
	for _, c := range cases {
		if got := c.holds.Allows(c.wants); got != c.allow {
			t.Errorf("%s.Allows(%s) = %v, want %v", c.holds, c.wants, got, c.allow)
		}
	}
}

func TestShareLinkSecretIsNotStored(t *testing.T) {
	h := newHarness(t)
	_, link := h.share("ledger", "use")
	secret := tokenOf(t, link)
	raw, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the share secret is stored in the database: a stolen copy would be a set of working links")
	}
	if _, err := base64.RawURLEncoding.DecodeString(secret); err != nil {
		t.Fatalf("share secret is not base64url: %v", err)
	}
	if len(secret) < 43 {
		t.Fatalf("share secret is %d chars, want 256 bits of entropy", len(secret))
	}
}

// --------------------------------------------------------------- helpers

// ssoInto completes sign-in for one app using whatever master session the
// browser already holds — the silent second-app hop that makes several apps
// feel like one product.
func ssoInto(t *testing.T, c *http.Client, app string) {
	t.Helper()
	u := "http://auth.krill.test/_krill/auth/sso?" + url.Values{
		"app": {app}, "rd": {"http://" + app + ".krill.test/"},
	}.Encode()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept", "text/html")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func tokenOf(t *testing.T, link string) string {
	t.Helper()
	i := strings.LastIndex(link, "/_krill/s/")
	if i < 0 {
		t.Fatalf("not a share link: %s", link)
	}
	return link[i+len("/_krill/s/"):]
}

func bearerPost(t *testing.T, h *harness, target, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+token)
	c := h.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func mustGet(t *testing.T, c *http.Client, target string) int {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func mustPost(t *testing.T, c *http.Client, target string) int {
	t.Helper()
	resp, err := c.Post(target, "application/gzip", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
