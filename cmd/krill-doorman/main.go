// krill-doorman — Krill's front door.
//
// Caddy terminates TLS and forward_auths every request here; only a 200 from
// this process lets a request reach krilld's router, which is the only thing
// that can wake an app. It runs UNPRIVILEGED and owns its own database:
// krilld is root (taps, mkfs.ext4, docker), and the internet-facing OAuth
// surface must not live inside it (ROADMAP decision #9).
//
//	--listen   the forward_auth + browser surface Caddy proxies to
//	--control  the operator API `krill share` talks to (loopback)
//
// Both default to loopback. The router never un-loopbacks either: Caddy binds
// 443 and proxies inward, so the "expose the router" step earlier plans
// treated as the last dangerous commit simply never happens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shulman33/krill/internal/doorman"
	"github.com/shulman33/krill/internal/objstore"
)

func main() {
	if err := run(); err != nil {
		slog.Error("krill-doorman exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		stateDir  = flag.String("state-dir", "/var/lib/krill-doorman", "doorman state root (database, signing key)")
		listen    = flag.String("listen", "127.0.0.1:9090", "browser + forward_auth surface (Caddy proxies here)")
		control   = flag.String("control", "127.0.0.1:9092", "operator API (`krill share` talks to this)")
		admin     = flag.String("krilld-admin", "http://127.0.0.1:9091", "krilld's admin API")
		baseHost  = flag.String("base-host", "krill.run", "domain apps serve under (<app>.<base-host>)")
		authHost  = flag.String("auth-host", "", "the single host Google redirects to (default auth.<base-host>)")
		scheme    = flag.String("scheme", "https", "how the outside world reaches the doorman (http only for pre-exposure testing)")
		cookieSec = flag.Bool("cookie-secure", true, "set Secure and the __Host- cookie prefix (off only for plain-http testing)")
		owners    = flag.String("owners", "", "comma-separated emails holding the edit plane on every app")
		store     = flag.String("objstore", "", "where revocations become durable: file:///path or gs://bucket/prefix")
		clientID  = flag.String("google-client-id", "", "Google OAuth client id (env KRILL_GOOGLE_CLIENT_ID)")
		clientSec = flag.String("google-client-secret", "", "Google OAuth client secret (env KRILL_GOOGLE_CLIENT_SECRET)")
		snapEvery = flag.Duration("snapshot-interval", time.Hour, "target age of the newest doorman database snapshot (0 = off)")
		snapKeep  = flag.Int("snapshot-keep", 14, "how many database snapshots to retain")
		restore   = flag.Bool("restore", false, "restore the database from the newest snapshot before starting (F2 step 3)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	dbPath := filepath.Join(*stateDir, "doorman.db")

	var obj objstore.Store
	if *store != "" {
		s, err := objstore.Open(*store)
		if err != nil {
			return err
		}
		obj = s
	} else {
		// Loud, not fatal: the doorman must still come up so an operator can
		// look at it, but F2 cannot pass in this state and every revoke will
		// be refused rather than quietly kept on the box being revoked from.
		log.Error("NO OBJECT STORE (--objstore): revocation has nowhere durable to land, " +
			"so every revoke will be REFUSED. F2 cannot pass in this configuration.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Restore before opening: the database file is replaced wholesale.
	if obj != nil {
		want := *restore
		if !want {
			if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
				want = true
			}
		}
		if want {
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			info, err := doorman.RestoreLatest(rctx, obj, dbPath)
			cancel()
			switch {
			case err != nil && *restore:
				return fmt.Errorf("--restore: %w", err)
			case err != nil:
				log.Info("no doorman database and nothing to restore: starting empty", "reason", err)
			default:
				log.Warn("doorman database RESTORED from the object store",
					"key", info.Key, "taken_at", info.TakenAt, "shares", info.Shares,
					"grants", info.Grants, "revocations_at_snapshot", info.Revocations)
			}
		}
	}

	st, err := doorman.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	key, err := doorman.LoadOrCreateKey(*stateDir)
	if err != nil {
		return err
	}
	// Rewrite the public half every start: krilld reads this file to bake
	// krill_idkey= into each guest's boot args, and a stale copy would make
	// every app reject every identity token.
	if err := os.WriteFile(filepath.Join(*stateDir, "identity.pub"),
		[]byte(key.PublicB64()+"\n"), 0o644); err != nil {
		return err
	}

	rev := doorman.NewRevoker(st, obj, log)
	// The replay that makes F2 step 3 true. It runs before a single request is
	// served, so a doorman restored from any snapshot is at least as revoked
	// as the log — never less.
	if obj != nil {
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		n, err := rev.Sync(sctx)
		cancel()
		if err != nil {
			log.Error("REVOCATION LOG UNREADABLE — this doorman may be serving access "+
				"that has already been revoked elsewhere", "err", err)
		} else {
			log.Info("revocation log replayed", "applied", n)
		}
	}

	snap := doorman.NewSnapshotter(st, obj, dbPath, filepath.Join(*stateDir, "work"),
		*snapEvery, *snapKeep, log)

	cfg := doorman.Config{
		BaseHost: *baseHost, AuthHost: *authHost, Scheme: *scheme,
		CookieSecure: *cookieSec, Owners: splitList(*owners), AdminAddr: *admin,
	}
	if cfg.AuthHost == "" {
		cfg.AuthHost = "auth." + cfg.BaseHost
	}

	apps := doorman.NewKrilldApps(*admin)
	auth := &lazyGoogle{
		clientID:     envOr(*clientID, "KRILL_GOOGLE_CLIENT_ID"),
		clientSecret: envOr(*clientSec, "KRILL_GOOGLE_CLIENT_SECRET"),
		redirect:     cfg.Scheme + "://" + cfg.AuthHost + "/_krill/auth/callback",
		log:          log,
	}
	if auth.clientID == "" || auth.clientSecret == "" {
		log.Error("NO GOOGLE OAUTH CLIENT (--google-client-id/--google-client-secret): " +
			"nobody can sign in. Existing sessions and revocation still work.")
	}

	pub := doorman.NewServer(cfg, st, rev, key, auth, apps, apps, log)
	ctl := doorman.NewControl(cfg, st, rev, snap, apps, key, log)

	go rev.RunSync(ctx, 5*time.Minute)
	go snap.Run(ctx)
	go sweep(ctx, st)

	pubSrv := &http.Server{Addr: *listen, Handler: pub, ReadHeaderTimeout: 10 * time.Second}
	ctlSrv := &http.Server{Addr: *control, Handler: ctl, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []struct {
		name string
		srv  *http.Server
	}{{"doorman", pubSrv}, {"control", ctlSrv}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s listener: %w", s.name, err)
			}
		}()
	}
	log.Info("krill-doorman up", "listen", *listen, "control", *control,
		"base_host", cfg.BaseHost, "auth_host", cfg.AuthHost, "scheme", cfg.Scheme,
		"owners", len(cfg.Owners), "identity_key", key.KeyID,
		"revoke_durable", obj != nil)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = pubSrv.Shutdown(shCtx)
	_ = ctlSrv.Shutdown(shCtx)
	wg.Wait()
	return nil
}

// lazyGoogle defers OIDC discovery to the first sign-in.
//
// Deliberate: the doorman runs under Restart=always, and a Google outage (or
// a box booting before its network is up) must not turn into a crash loop
// that also takes down revocation — the one function that has to work when
// things are going badly.
type lazyGoogle struct {
	clientID     string
	clientSecret string
	redirect     string
	log          *slog.Logger

	mu    sync.Mutex
	inner *doorman.GoogleAuth
}

func (l *lazyGoogle) get(ctx context.Context) (*doorman.GoogleAuth, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inner != nil {
		return l.inner, nil
	}
	g, err := doorman.NewGoogleAuth(ctx, l.clientID, l.clientSecret, l.redirect)
	if err != nil {
		return nil, err
	}
	l.log.Info("google oidc ready", "redirect_uri", l.redirect)
	l.inner = g
	return g, nil
}

func (l *lazyGoogle) AuthCodeURL(state, nonce string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	g, err := l.get(ctx)
	if err != nil {
		l.log.Error("google oidc unavailable", "err", err)
		// A URL that goes nowhere is better than a panic; the browser lands
		// on the doorman's own error page rather than a blank tab.
		return "/_krill/auth/unavailable"
	}
	return g.AuthCodeURL(state, nonce)
}

func (l *lazyGoogle) Exchange(ctx context.Context, code, nonce string) (doorman.Identity, error) {
	g, err := l.get(ctx)
	if err != nil {
		return doorman.Identity{}, err
	}
	return g.Exchange(ctx, code, nonce)
}

// sweep drops expired flows and sessions. Revocations are never swept.
func sweep(ctx context.Context, st *doorman.Store) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st.SweepExpired(time.Now())
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(val, key string) string {
	if val != "" {
		return val
	}
	return os.Getenv(key)
}
