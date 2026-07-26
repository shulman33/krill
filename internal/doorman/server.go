package doorman

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/registry"
)

// Reserved is the URL prefix the doorman owns on every host it fronts.
// Apps cannot serve paths under it; the Caddyfile routes the whole prefix
// here before anything reaches krilld's router.
const Reserved = "/_krill/"

// Config is the doorman's view of the deployment.
type Config struct {
	// BaseHost is the domain apps hang off. Requests whose Host is not
	// exactly "<app>.<BaseHost>" are refused before any wake — F3 step 4,
	// which exists because krilld's router reads only the first DNS label
	// and never validates the suffix.
	BaseHost string
	// AuthHost is the single host Google redirects back to. It must be a real
	// name under BaseHost (the wildcard certificate covers it) and it is the
	// only host that ever holds the master session cookie.
	AuthHost string
	// Scheme is how the outside world reaches the doorman. https in
	// production; http exists so F1–F3 can be proved through the SSH tunnel
	// BEFORE the door opens, which is this milestone's ordering rule.
	Scheme string
	// CookieSecure sets the Secure attribute and the __Host- prefix. Off only
	// for tunnel-era testing over plain http.
	CookieSecure bool
	// Owners hold the edit plane on every app: the operator's own accounts.
	// Everyone else must be granted a share.
	Owners []string
	// RouterAddr is krilld's loopback router. The doorman never proxies app
	// traffic itself — Caddy does that after a 200 — but the data and edit
	// planes reach krilld's admin API through AdminAddr.
	AdminAddr string
}

func (c Config) issuer() string { return c.Scheme + "://" + c.AuthHost }

// Server is the doorman's HTTP surface: one mux serving every host Caddy
// fronts, distinguishing them by Host header.
type Server struct {
	cfg   Config
	store *Store
	rev   *Revoker
	key   *IdentityKey
	auth  Authenticator
	apps  AppSource
	log   *slog.Logger
	// admin proxies the data and edit planes to krilld.
	krilld *KrilldApps
	owners map[string]bool
	Now    func() time.Time
}

func NewServer(cfg Config, st *Store, rev *Revoker, key *IdentityKey, auth Authenticator, apps AppSource, krilld *KrilldApps, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if cfg.AuthHost == "" {
		cfg.AuthHost = "auth." + cfg.BaseHost
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	owners := map[string]bool{}
	for _, o := range cfg.Owners {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			owners[o] = true
		}
	}
	return &Server{cfg: cfg, store: st, rev: rev, key: key, auth: auth,
		apps: apps, krilld: krilld, log: log, owners: owners, Now: time.Now}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/_krill/auth/verify":
		s.verify(w, r)
	case path == "/_krill/auth/sso":
		s.sso(w, r)
	case path == "/_krill/auth/callback":
		s.callback(w, r)
	case path == "/_krill/auth/accept":
		s.accept(w, r)
	case path == "/_krill/auth/logout":
		s.logout(w, r)
	case path == "/_krill/jwks.json":
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.Write(s.key.JWKS())
	case path == "/_krill/whoami":
		s.whoami(w, r)
	case strings.HasPrefix(path, "/_krill/s/"):
		s.claimLink(w, r, strings.TrimPrefix(path, "/_krill/s/"))
	case strings.HasPrefix(path, "/_krill/data/"):
		s.dataPlane(w, r, strings.TrimPrefix(path, "/_krill/data/"))
	case strings.HasPrefix(path, "/_krill/edit/"):
		s.editPlane(w, r, strings.TrimPrefix(path, "/_krill/edit/"))
	case path == "/healthz":
		w.Write([]byte("ok\n"))
	default:
		http.NotFound(w, r)
	}
}

// ------------------------------------------------------------- the verifier

// verify is what Caddy's forward_auth calls on every single request, and the
// only thing standing between the internet and a wake.
//
// A 200 here is the sole way a request reaches krilld's router. Anything else
// — a redirect to sign in, a 403, a 404 for an app that does not exist — ends
// the request WITHOUT the router ever hearing about it, which is F3's "a
// fence that bills is still a fence that failed" enforced structurally rather
// than by care.
func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	// Only the local proxy may ask this question: every X-Forwarded-* header
	// below is attacker-controlled if anyone else can reach this endpoint.
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	host := firstValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	app, ok := s.appFromHost(host)
	if !ok {
		// F3 step 4. Refused here, before any app lookup, so a Host naming a
		// real app under someone else's domain is as dead as a nonsense one.
		s.deny(w, r, http.StatusForbidden, "Unknown address",
			fmt.Sprintf("%s is not a Krill app address.", html.EscapeString(host)))
		return
	}
	exists, err := s.apps.Exists(r.Context(), app)
	switch {
	case errors.Is(err, ErrKrilldDown):
		// Fail closed and say so. krilld being down means the app could not
		// serve anyway; what must never happen is guessing "yes" and letting
		// an unauthorized request through, or guessing "no" and pruning an ACL.
		s.deny(w, r, http.StatusServiceUnavailable, "Temporarily unavailable",
			"Krill is restarting. Try again in a moment.")
		return
	case err != nil:
		s.log.Error("app lookup failed", "app", app, "err", err)
		s.deny(w, r, http.StatusBadGateway, "Error", "Could not check this app.")
		return
	case !exists:
		// Lazy prune: a definitive 404 from krilld is the signal that an app
		// was deleted, and its ACL rows should go with it. Fails closed —
		// the request is refused first, the cleanup is a side effect.
		if err := s.store.PruneApp(app); err != nil {
			s.log.Error("pruning ACL for deleted app", "app", app, "err", err)
		}
		s.deny(w, r, http.StatusNotFound, "No such app",
			fmt.Sprintf("There is no app named %q here.", html.EscapeString(app)))
		return
	}

	sess, ok := s.appSession(r, app)
	if !ok {
		s.startSignIn(w, r, app, host, "")
		return
	}
	g, err := s.authorize(app, sess.Subject, sess.Email, sess.Name, PlaneUse)
	if err != nil {
		s.deny(w, r, http.StatusForbidden, "Not shared with you",
			fmt.Sprintf("%s is signed in, but does not have access to this app.",
				html.EscapeString(sess.Email)))
		return
	}
	now := s.Now()
	token, err := s.key.Mint(s.cfg.issuer(), app, g, now)
	if err != nil {
		s.log.Error("minting identity token", "app", app, "err", err)
		s.deny(w, r, http.StatusInternalServerError, "Error", "Could not mint an identity token.")
		return
	}
	s.store.TouchGrant(g.ID, now)
	// The guest gets the email for display and the token for proof. Nothing
	// here is a bearer credential for anything except this one app, for the
	// next five minutes.
	w.Header().Set("X-App-User", g.Email)
	w.Header().Set("X-App-User-Id", g.Subject)
	if g.Name != "" {
		w.Header().Set("X-App-User-Name", g.Name)
	}
	w.Header().Set("X-App-Plane", string(g.Plane))
	w.Header().Set("X-Krill-Token", token)
	w.WriteHeader(http.StatusOK)
}

// authorize is the single decision point: owners first, then the ACL.
func (s *Server) authorize(app, subject, email, name string, want Plane) (Grant, error) {
	if email != "" && s.owners[strings.ToLower(email)] {
		return Grant{
			ID: "owner", App: app, Subject: subject, Email: email, Name: name,
			Plane: PlaneEdit, ClaimedAt: s.Now(),
		}, nil
	}
	g, err := s.store.Best(app, subject)
	if err != nil {
		return Grant{}, err
	}
	if !g.Plane.Allows(want) {
		return Grant{}, fmt.Errorf("%w: holds %s, needs %s", ErrNoGrant, g.Plane, want)
	}
	return g, nil
}

// appFromHost enforces the host suffix and the app-name grammar together.
// "<app>.<BaseHost>" and nothing else: not a bare IP, not a deeper subdomain,
// not the auth host, not an app label under another domain.
func (s *Server) appFromHost(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil {
		return "", false
	}
	if host == s.cfg.AuthHost {
		return "", false
	}
	suffix := "." + strings.ToLower(s.cfg.BaseHost)
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	// Exactly one label. "ledger.local.krill.run" must not resolve to
	// "ledger" — the gates name that case explicitly.
	if label == "" || strings.Contains(label, ".") || !registry.ValidName(label) {
		return "", false
	}
	return label, true
}

// ------------------------------------------------------------- sign-in flow
//
// Three hops, and the shape is deliberate. The master session lives ONLY on
// the auth host; every app host gets its own session cookie. Guests run
// agent-written code the platform does not trust, and a request proxied to a
// guest carries its Cookie header — so a domain-wide session cookie would be
// a platform-wide session-theft primitive handed to every app. Per-app
// cookies bound the blast radius of a hostile guest to the access its own
// user already had.
//
//	app host      /_krill/auth/verify   no session  -> 302 auth host
//	auth host     /_krill/auth/sso      master session? -> mint handoff code
//	                                    else -> Google, then back here
//	app host      /_krill/auth/accept   consume code -> app session -> back

// startSignIn sends an unauthenticated browser toward the auth host,
// remembering where it was going and (if it arrived via a link) what it is
// claiming.
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request, app, host, claimShareID string) {
	uri := firstValue(r.Header.Get("X-Forwarded-Uri"))
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	returnTo := s.cfg.Scheme + "://" + host + uri
	if !wantsHTML(r) {
		// An API client should get a machine answer, not a redirect into a
		// Google consent screen it cannot render.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "not signed in", "sign_in": s.ssoURL(app, returnTo, claimShareID),
		})
		return
	}
	http.Redirect(w, r, s.ssoURL(app, returnTo, claimShareID), http.StatusFound)
}

func (s *Server) ssoURL(app, returnTo, claimShareID string) string {
	q := url.Values{"app": {app}, "rd": {returnTo}}
	if claimShareID != "" {
		q.Set("claim", claimShareID)
	}
	return s.cfg.Scheme + "://" + s.cfg.AuthHost + "/_krill/auth/sso?" + q.Encode()
}

// sso runs on the auth host. If the browser already has a master session it
// mints a one-time handoff code; otherwise it starts the Google round trip
// and resumes here afterwards.
func (s *Server) sso(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	app, returnTo, claim := q.Get("app"), q.Get("rd"), q.Get("claim")
	if !registry.ValidName(app) || !s.ownReturnTo(returnTo, app) {
		http.Error(w, "bad sign-in request", http.StatusBadRequest)
		return
	}
	if sess, ok := s.ssoSession(r); ok {
		s.handOff(w, r, app, returnTo, claim, Identity{
			Subject: sess.Subject, Email: sess.Email, Name: sess.Name,
		})
		return
	}
	nonce, err := randomToken()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state, err := s.store.NewFlow(Flow{
		Kind: "oauth", App: app, Nonce: nonce, ReturnTo: returnTo, Claim: claim,
	}, s.Now())
	if err != nil {
		s.log.Error("starting oauth flow", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.auth.AuthCodeURL(state, nonce), http.StatusFound)
}

// callback is the single redirect URI registered with Google.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.deny(w, r, http.StatusForbidden, "Sign-in cancelled",
			"Google reported: "+html.EscapeString(e))
		return
	}
	flow, ok := s.store.ConsumeFlow(q.Get("state"), "oauth", s.Now())
	if !ok {
		// Expired, replayed, or forged. All three deserve the same answer,
		// and the same remedy: start again from the link.
		s.deny(w, r, http.StatusBadRequest, "Sign-in expired",
			"That sign-in took too long or was already used. Open the link again.")
		return
	}
	ident, err := s.auth.Exchange(r.Context(), q.Get("code"), flow.Nonce)
	if err != nil {
		s.log.Warn("oauth exchange failed", "err", err)
		s.deny(w, r, http.StatusForbidden, "Sign-in failed", "Google did not confirm that sign-in.")
		return
	}
	// The master session: identity only, host-only on the auth host, never
	// sent to an app and therefore never visible to a guest.
	value, err := s.store.NewSession("sso", "", ident.Subject, ident.Email, ident.Name, s.Now())
	if err != nil {
		s.log.Error("creating sso session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, s.cookieName("sso"), value, SessionAbsolute)
	s.handOff(w, r, flow.App, flow.ReturnTo, flow.Claim, ident)
}

// handOff mints the one-time code that carries an established identity from
// the auth host to an app host. Short-lived, single-use, and bound to one
// app: it is a credential in a URL, so it must be worth as little as possible.
func (s *Server) handOff(w http.ResponseWriter, r *http.Request, app, returnTo, claim string, ident Identity) {
	code, err := s.store.NewFlow(Flow{
		Kind: "handoff", App: app, ReturnTo: returnTo, Claim: claim,
		Subject: ident.Subject, Email: ident.Email, Name: ident.Name,
	}, s.Now())
	if err != nil {
		s.log.Error("minting handoff", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u := s.cfg.Scheme + "://" + app + "." + s.cfg.BaseHost + "/_krill/auth/accept?" +
		url.Values{"code": {code}}.Encode()
	http.Redirect(w, r, u, http.StatusFound)
}

// accept runs on the app host: it turns a handoff code into an app session,
// performing the claim if this browser arrived from a share link.
func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appFromHost(r.Host)
	if !ok {
		http.Error(w, "bad host", http.StatusForbidden)
		return
	}
	flow, ok := s.store.ConsumeFlow(r.URL.Query().Get("code"), "handoff", s.Now())
	if !ok || flow.App != app {
		s.deny(w, r, http.StatusBadRequest, "Sign-in expired",
			"That sign-in link was already used. Open the original link again.")
		return
	}
	now := s.Now()
	if flow.Claim != "" {
		// Re-read the share rather than trusting the flow: the operator may
		// have revoked the link during the seconds this browser spent at
		// Google, and the last word belongs to the current state of the ACL.
		sh, err := s.store.ShareByID(flow.Claim)
		switch {
		case err != nil || sh.App != app:
			s.log.Warn("claim referenced an unknown share", "app", app, "share", flow.Claim)
		case sh.Revoked:
			s.log.Info("claim refused: link revoked mid-sign-in", "app", app, "share", sh.ID)
		default:
			if _, err := s.store.Claim(sh, flow.Subject, flow.Email, flow.Name, now); err != nil {
				s.log.Error("claiming share", "share", sh.ID, "err", err)
			} else {
				s.log.Info("share claimed", "app", app, "share", sh.ID,
					"email", flow.Email, "plane", sh.Plane)
			}
		}
	}
	value, err := s.store.NewSession("app", app, flow.Subject, flow.Email, flow.Name, now)
	if err != nil {
		s.log.Error("creating app session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, s.cookieName("app"), value, SessionAbsolute)
	rd := flow.ReturnTo
	if !s.ownReturnTo(rd, app) {
		rd = s.cfg.Scheme + "://" + app + "." + s.cfg.BaseHost + "/"
	}
	http.Redirect(w, r, rd, http.StatusFound)
}

// claimLink is the share URL a recipient actually opens. It never reveals
// whether an unknown token was ever real.
func (s *Server) claimLink(w http.ResponseWriter, r *http.Request, token string) {
	app, ok := s.appFromHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sh, err := s.store.ShareByToken(token, s.Now())
	if err != nil || sh.App != app {
		s.deny(w, r, http.StatusNotFound, "Link not valid",
			"This link has been revoked, has expired, or never existed.")
		return
	}
	// Already signed in on this app host: claim now, no round trip.
	if sess, ok := s.appSession(r, app); ok {
		if _, err := s.store.Claim(sh, sess.Subject, sess.Email, sess.Name, s.Now()); err != nil {
			s.log.Error("claiming share", "share", sh.ID, "err", err)
		}
		http.Redirect(w, r, s.cfg.Scheme+"://"+r.Host+"/", http.StatusFound)
		return
	}
	// The share id travels through the flow, not the URL: the raw token is
	// never written anywhere the browser or a log can keep it.
	s.startSignIn(w, r, app, r.Host, sh.ID)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if app, ok := s.appFromHost(r.Host); ok {
		if c, err := r.Cookie(s.cookieName("app")); err == nil {
			s.store.DropSession(c.Value, "app", app)
		}
		s.clearCookie(w, s.cookieName("app"))
		http.Redirect(w, r, s.cfg.Scheme+"://"+r.Host+"/", http.StatusFound)
		return
	}
	if c, err := r.Cookie(s.cookieName("sso")); err == nil {
		s.store.DropSession(c.Value, "sso", "")
	}
	s.clearCookie(w, s.cookieName("sso"))
	s.page(w, http.StatusOK, "Signed out", "You are signed out of Krill on this browser.")
}

// whoami is the "show them it was them" endpoint: F1's ACL check from the
// recipient's own seat, and the handle the gate scripts read.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"authenticated": false}
	code := http.StatusOK
	if app, ok := s.appFromHost(r.Host); ok {
		out["app"] = app
		if sess, ok := s.appSession(r, app); ok {
			out["authenticated"] = true
			out["email"], out["subject"], out["name"] = sess.Email, sess.Subject, sess.Name
			if g, err := s.authorize(app, sess.Subject, sess.Email, sess.Name, PlaneUse); err == nil {
				out["plane"], out["grant"], out["share"] = g.Plane, g.ID, g.ShareID
				out["authorized"] = true
			} else {
				out["authorized"] = false
				code = http.StatusForbidden
			}
		} else {
			code = http.StatusUnauthorized
		}
	} else if r.Host == s.cfg.AuthHost || strings.HasPrefix(r.Host, s.cfg.AuthHost+":") {
		if sess, ok := s.ssoSession(r); ok {
			out["authenticated"] = true
			out["email"], out["subject"] = sess.Email, sess.Subject
		} else {
			code = http.StatusUnauthorized
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(out)
}

// ------------------------------------------------------- data & edit planes

// planeCaller resolves who is asking on a programmatic surface. Two ways in:
// a browser session, or a bearer share token.
//
// The bearer form is not a shortcut — it is the frozen share model taken
// literally. THE LINK IS THE CAPABILITY, so presenting the link itself is a
// legitimate way to exercise it from a script or an agent, which is the only
// way F5's "deploy through the doorman as a non-Sam identity" can happen
// without a browser in the loop. The acting identity is the sole claimant if
// there is exactly one, and the share id otherwise, so the audit trail never
// silently attributes a deploy to a person who did not do it.
func (s *Server) planeCaller(r *http.Request, app string, want Plane) (Grant, error) {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		sh, err := s.store.ShareByToken(token, s.Now())
		if err != nil || sh.App != app {
			return Grant{}, ErrNoShare
		}
		if !sh.Plane.Allows(want) {
			return Grant{}, fmt.Errorf("%w: link holds %s, needs %s", ErrNoGrant, sh.Plane, want)
		}
		g := Grant{ID: sh.ID, App: app, Plane: sh.Plane, ShareID: sh.ID,
			Email: "share:" + sh.ID, Subject: "share:" + sh.ID}
		if claims, err := s.store.ListGrants(app); err == nil {
			var live []Grant
			for _, c := range claims {
				if c.ShareID == sh.ID && !c.Revoked {
					live = append(live, c)
				}
			}
			if len(live) == 1 {
				g.Email, g.Subject, g.Name = live[0].Email, live[0].Subject, live[0].Name
			}
		}
		return g, nil
	}
	sess, ok := s.appSession(r, app)
	if !ok {
		return Grant{}, ErrNoGrant
	}
	return s.authorize(app, sess.Subject, sess.Email, sess.Name, want)
}

// dataPlane is the "data" share: a read-only window onto the app's durable
// state, served from krilld's admin API. A use-only holder gets 403 here, and
// that refusal is the point of F3 step 2.
func (s *Server) dataPlane(w http.ResponseWriter, r *http.Request, rest string) {
	app, ok := s.appFromHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	g, err := s.planeCaller(r, app, PlaneData)
	if err != nil {
		s.denyPlane(w, r, app, PlaneData, err)
		return
	}
	s.log.Info("data plane access", "app", app, "by", g.Email, "path", rest)
	switch rest {
	case "stream":
		s.relayGET(w, r, "/v1/apps/"+app+"/stream")
	case "status":
		s.relayGET(w, r, "/v1/apps/"+app)
	case "logs":
		s.relayGET(w, r, "/v1/apps/"+app+"/logs?tail="+url.QueryEscape(firstOr(r.URL.Query().Get("tail"), "100")))
	case "db":
		// The app's SQLite file, read out of its data disk by the host. This
		// is what "export /data" means concretely.
		s.relayGET(w, r, "/v1/apps/"+app+"/data")
	default:
		http.NotFound(w, r)
	}
}

// editPlane is the "edit" share: replace the app's code. Everything hostile
// about M4 arrives here, which is why F5 requires the build itself to happen
// inside a throwaway microVM.
func (s *Server) editPlane(w http.ResponseWriter, r *http.Request, rest string) {
	app, ok := s.appFromHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	g, err := s.planeCaller(r, app, PlaneEdit)
	if err != nil {
		s.denyPlane(w, r, app, PlaneEdit, err)
		return
	}
	if rest != "deploy" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	s.log.Warn("deploy through the doorman", "app", app, "by", g.Email, "share", g.ShareID)
	target, err := url.Parse(s.cfg.AdminAddr)
	if err != nil {
		http.Error(w, "misconfigured admin address", http.StatusInternalServerError)
		return
	}
	q, err := deployParams(r.URL.Query())
	if err != nil {
		s.deny(w, r, http.StatusBadRequest, "Bad deploy request", html.EscapeString(err.Error()))
		return
	}
	// Never let a caller aim the deploy at a different app: the path is
	// rewritten from the Host-derived name, not from anything they sent.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = "/v1/apps/" + app + "/deploy"
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = target.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			// krilld records who asked, so a hostile build has a name on it.
			pr.Out.Header.Set("X-Krill-Deploy-By", g.Email)
			pr.Out.Header.Set("X-Krill-Deploy-Untrusted", "1")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log.Error("deploy relay", "app", app, "err", err)
			http.Error(w, "krill: deploy could not reach the builder: "+err.Error(),
				http.StatusBadGateway)
		},
	}
	s.krilld.Forget(app)
	proxy.ServeHTTP(w, r)
}

// deployParams filters and clamps what an edit-plane holder may ask the
// builder for.
//
// Passing the caller's query string through would hand them krilld's resource
// knobs: size_mb of a few hundred thousand fills the disk for every other app
// on the box, and F5 FAILs on exactly that ("a resource-exhaustion build ...
// that degrades the box for other apps"). The operator's own path through the
// admin API keeps the unclamped versions; this is the one that arrives from
// the network.
func deployParams(in url.Values) (url.Values, error) {
	out := url.Values{}
	limits := map[string][2]int{
		"vcpus":      {1, 4},
		"mem_mib":    {32, 4096},
		"guest_port": {1, 65535},
		"size_mb":    {64, 8192},
	}
	for key, bounds := range limits {
		raw := in.Get(key)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be a number", key)
		}
		if n < bounds[0] || n > bounds[1] {
			return nil, fmt.Errorf("%s must be between %d and %d", key, bounds[0], bounds[1])
		}
		out.Set(key, raw)
	}
	// The verification wake is what turns "it built" into "it runs", and it is
	// the only feedback a remote deployer gets. Let it be turned off, nothing
	// else.
	if in.Get("verify") == "false" {
		out.Set("verify", "false")
	}
	return out, nil
}

func (s *Server) relayGET(w http.ResponseWriter, r *http.Request, path string) {
	body, code, err := s.krilld.krilldGet(r.Context(), path)
	if err != nil {
		http.Error(w, "krill: "+err.Error(), http.StatusBadGateway)
		return
	}
	ct := "application/json"
	if strings.HasSuffix(path, "/data") {
		ct = "application/octet-stream"
		w.Header().Set("Content-Disposition", `attachment; filename="app.db"`)
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(code)
	w.Write(body)
}

func (s *Server) denyPlane(w http.ResponseWriter, r *http.Request, app string, want Plane, err error) {
	if errors.Is(err, ErrNoGrant) || errors.Is(err, ErrNoShare) {
		s.deny(w, r, http.StatusForbidden, "Not allowed",
			fmt.Sprintf("This needs the %q share on %s.", want, html.EscapeString(app)))
		return
	}
	s.deny(w, r, http.StatusForbidden, "Not allowed", html.EscapeString(err.Error()))
}

// ------------------------------------------------------------- sessions etc.

func (s *Server) appSession(r *http.Request, app string) (Session, bool) {
	c, err := r.Cookie(s.cookieName("app"))
	if err != nil {
		return Session{}, false
	}
	return s.store.Session(c.Value, "app", app, s.Now())
}

func (s *Server) ssoSession(r *http.Request) (Session, bool) {
	c, err := r.Cookie(s.cookieName("sso"))
	if err != nil {
		return Session{}, false
	}
	return s.store.Session(c.Value, "sso", "", s.Now())
}

// cookieName carries the __Host- prefix when the deployment is https. That
// prefix is a browser-enforced promise — host-only, Path=/, Secure — which is
// exactly the scoping this design depends on.
func (s *Server) cookieName(kind string) string {
	if s.cfg.CookieSecure {
		return "__Host-krill_" + kind
	}
	return "krill_" + kind
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		// No Domain attribute, ever: that is what makes the cookie host-only,
		// so an app host cannot see the auth host's session or another app's.
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		// Lax, not Strict: the browser arrives here by a cross-site redirect
		// from Google, and Strict would drop the cookie on that navigation.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

// ownReturnTo refuses to bounce a browser anywhere except back to the app it
// came from — an open redirector on the sign-in path would hand every
// phishing page a krill.run URL to hide behind.
func (s *Server) ownReturnTo(rd, app string) bool {
	if rd == "" {
		return false
	}
	u, err := url.Parse(rd)
	if err != nil || u.Scheme != s.cfg.Scheme {
		return false
	}
	got, ok := s.appFromHost(u.Host)
	return ok && got == app
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func firstOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// deny answers a refusal in whatever form the caller can read, and always
// with the same vocabulary a person can act on. F4 fails on "any question
// that has to be answered before they succeed", so these pages say what
// happened and what to do, and never show a stack trace or an error code
// alone.
func (s *Server) deny(w http.ResponseWriter, r *http.Request, code int, title, detail string) {
	if !wantsHTML(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": title, "detail": stripTags(detail)})
		return
	}
	s.page(w, code, title, detail)
}

func (s *Server) page(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, pageHTML, html.EscapeString(title), html.EscapeString(title), detail)
}

func stripTags(s string) string {
	var sb strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// pageHTML is theme-aware and self-contained, matching the project's
// convention for documents that must render anywhere with no external
// requests.
const pageHTML = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s · Krill</title>
<style>
  :root { color-scheme: light dark; --bg:#fbfbfa; --fg:#1c1c1a; --mut:#6b6b66; --line:#e3e3df; }
  @media (prefers-color-scheme: dark) { :root { --bg:#16161a; --fg:#e8e8e6; --mut:#9a9a95; --line:#2c2c31; } }
  body { margin:0; min-height:100vh; display:grid; place-items:center; background:var(--bg); color:var(--fg);
         font:16px/1.55 ui-sans-serif,-apple-system,"Segoe UI",Roboto,sans-serif; padding:2rem; }
  main { max-width:32rem; text-align:center; }
  h1 { font-size:1.35rem; margin:0 0 .6rem; letter-spacing:-.01em; }
  p { color:var(--mut); margin:0; }
  .mark { font-size:.75rem; letter-spacing:.14em; text-transform:uppercase; color:var(--mut);
          border-top:1px solid var(--line); margin-top:2.5rem; padding-top:1rem; }
</style>
<main>
  <h1>%s</h1>
  <p>%s</p>
  <div class="mark">krill</div>
</main>
`
