// Package router is D2 from the architecture doc as code: the wake-on-request
// reverse proxy. A request for a FROZEN app is held while the supervisor
// restores the snapshot, then forwarded — the caller never learns the app
// was asleep except by reading the latency.
//
// Routing: the first DNS label of the Host header is the app name
// (counter.krill.example.com → "counter"; counter.localhost works out of the
// box for local demos).
package router

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/registry"
)

// maxBufferedBody bounds request-body buffering. Buffering makes requests
// replayable across the connect-retry window; small-software requests are
// small.
const maxBufferedBody = 32 << 20

// connectGrace is how long the proxy keeps retrying refused connections
// after the supervisor reports the app ACTIVE — covers the gap between a
// cold-booted guest's TCP listen and its accept loop actually serving.
const connectGrace = 3 * time.Second

type Router struct {
	sup *lifecycle.Supervisor
	log *slog.Logger
	// SyncAck is D1 as a proxy behavior: before a response is released to
	// the client, the app's committed writes must be durable at the object
	// store. The hold happens in ModifyResponse — after the guest answered,
	// before one byte reaches the client — so an ack can never outrun its
	// durability. Read-only requests cost one WAL scan and no store I/O.
	SyncAck bool
	// RouteSuffixes pins which Host suffixes route, e.g. {"krill.run"}.
	//
	// Empty means any suffix routes, which is M1-M3 behavior and is what the
	// gate suites depend on (they send krill.local at a daemon whose
	// --base-host is krill.run, and that is correct — the suffix has never
	// been part of routing). The doorman pins the suffix unconditionally
	// upstream of here, so this is defense in depth for the loopback router:
	// F3's requirement is met at the front door, and again here.
	RouteSuffixes []string
}

func New(sup *lifecycle.Supervisor, log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{sup: sup, log: log}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := appName(r.Host)
	if name == "" || !rt.suffixAllowed(r.Host) {
		http.Error(w, "krill: no app in Host header (use <app>.<host>)", http.StatusNotFound)
		return
	}

	// Buffer the body so connect-level retries can replay it.
	if r.Body != nil && r.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBufferedBody+1))
		if err != nil {
			http.Error(w, "krill: reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxBufferedBody {
			http.Error(w, "krill: request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}

	// The hold: returns once the app is ACTIVE, waking it if needed.
	t0 := time.Now()
	addr, release, err := rt.sup.Acquire(r.Context(), name)
	acquireDur := time.Since(t0)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrNotFound):
			http.Error(w, fmt.Sprintf("krill: no app named %q", name), http.StatusNotFound)
		case errors.Is(err, r.Context().Err()):
			http.Error(w, "krill: client gave up while app was waking", http.StatusGatewayTimeout)
		default:
			rt.log.Error("wake for request failed", "app", name, "err", err)
			http.Error(w, "krill: app failed to wake: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	defer release()

	target := &url.URL{Scheme: "http", Host: addr}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = r.Host // the guest sees the public host, not 172.16.x.2
		},
		Transport: &retryTransport{base: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext,
			// Keep-alives to guests are safe: the supervisor won't freeze an
			// app with requests in flight, and a snapshotted guest's dead
			// conns surface as retryable dial errors on the next request.
		}},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rt.log.Error("proxy", "app", name, "err", err)
			http.Error(w, "krill: app did not answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	if rt.SyncAck {
		proxy.ModifyResponse = func(*http.Response) error {
			if err := rt.sup.SyncData(r.Context(), name); err != nil {
				return fmt.Errorf("write not durable: %w", err)
			}
			return nil
		}
	}
	proxy.ServeHTTP(w, r)

	// Wake-path observability: any request that waited on machinery gets a
	// breakdown, so a slow wake is attributable (supervisor vs proxy leg)
	// from the daemon log alone.
	if total := time.Since(t0); acquireDur > 20*time.Millisecond || total > 150*time.Millisecond {
		rt.log.Info("slow request", "app", name,
			"acquire_ms", acquireDur.Milliseconds(),
			"proxy_ms", (total - acquireDur).Milliseconds())
	}
}

// suffixAllowed applies the optional Host-suffix pin. With no suffixes
// configured every Host with a valid app label routes, which is how M1-M3
// behaved and what the gate suites still exercise.
func (rt *Router) suffixAllowed(host string) bool {
	if len(rt.RouteSuffixes) == 0 {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range rt.RouteSuffixes {
		suffix = strings.ToLower(strings.TrimPrefix(suffix, "."))
		// Exactly one label in front of the suffix: "<app>.<suffix>" routes,
		// "<app>.<anything>.<suffix>" does not.
		if rest, ok := strings.CutSuffix(host, "."+suffix); ok && !strings.Contains(rest, ".") {
			return true
		}
	}
	return false
}

// appName extracts the first DNS label from a Host header value. Bare hosts
// ("localhost", an IP) have no app label and route nowhere.
func appName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if net.ParseIP(host) != nil {
		return ""
	}
	first, rest, found := strings.Cut(host, ".")
	if !found || rest == "" {
		return ""
	}
	if !registry.ValidName(first) {
		return ""
	}
	return first
}

// retryTransport retries connect-level failures (refused/reset before any
// bytes of the request were written) for a short grace window. This is the
// second half of "hold the request": a guest that is up but not yet
// accepting gets its accept loop raced politely instead of 502ing.
type retryTransport struct {
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline := time.Now().Add(connectGrace)
	for {
		attempt := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attempt.Body = body
		}
		resp, err := t.base.RoundTrip(attempt)
		if err == nil || !isConnectErr(err) || time.Now().After(deadline) {
			return resp, err
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func isConnectErr(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}
