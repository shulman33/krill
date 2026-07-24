package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/dataplane"
	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/registry"
)

func TestAppName(t *testing.T) {
	cases := map[string]string{
		"counter.localhost:8080":    "counter",
		"counter.krill.example.com": "counter",
		"counter.localhost":         "counter",
		"localhost:8080":            "",
		"localhost":                 "",
		"127.0.0.1:8080":            "",
		"[::1]:8080":                "",
		"Bad_Name.localhost":        "",
		"":                          "",
		"a.b":                       "a",
	}
	for host, want := range cases {
		if got := appName(host); got != want {
			t.Errorf("appName(%q) = %q, want %q", host, got, want)
		}
	}
}

// echoBackend boots real HTTP servers that echo app name, method, path and
// body — enough to prove the proxy forwards everything that matters.
type echoBackend struct {
	mu    sync.Mutex
	addrs map[string]string
	snaps map[string]bool
}

type echoInstance struct{ srv *http.Server }

func (i *echoInstance) Kill() { i.srv.Close() }

func (b *echoBackend) spawn(app registry.App) (lifecycle.Instance, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s|%s|%s|%s|host=%s", app.Name, r.Method, r.URL.Path, body, r.Host)
	})}
	go srv.Serve(ln)
	b.mu.Lock()
	b.addrs[app.Name] = ln.Addr().String()
	b.mu.Unlock()
	return &echoInstance{srv: srv}, nil
}

func (b *echoBackend) Install(registry.App, string) error { return nil }
func (b *echoBackend) Prepare(registry.App) error         { return nil }
func (b *echoBackend) HasSnapshot(a registry.App) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snaps[a.Name]
}
func (b *echoBackend) ColdBoot(_ context.Context, a registry.App) (lifecycle.Instance, error) {
	return b.spawn(a)
}
func (b *echoBackend) Restore(_ context.Context, a registry.App) (lifecycle.Instance, error) {
	return b.spawn(a)
}
func (b *echoBackend) Snapshot(_ context.Context, a registry.App, _ lifecycle.Instance) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snaps[a.Name] = true
	return nil
}
func (b *echoBackend) GuestAddr(a registry.App) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if addr, ok := b.addrs[a.Name]; ok {
		return addr
	}
	return "127.0.0.1:1"
}
func (b *echoBackend) Purge(registry.App) error            { return nil }
func (b *echoBackend) Replace(registry.App, string) error  { return nil }
func (b *echoBackend) SerialLogPath(a registry.App) string { return "/tmp/echo-" + a.Name + ".log" }

func setup(t *testing.T) (*httptest.Server, *lifecycle.Supervisor) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	sup := lifecycle.New(reg, &echoBackend{addrs: map[string]string{}, snaps: map[string]bool{}},
		lifecycle.Config{WakeTimeout: 5 * time.Second, FreezeTimeout: 5 * time.Second, IdleTimeout: time.Hour},
		slog.Default())
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(sup, slog.Default()))
	t.Cleanup(srv.Close)
	return srv, sup
}

func doReq(t *testing.T, srv *httptest.Server, method, host, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

func TestWakeOnRequest(t *testing.T) {
	srv, sup := setup(t)
	if _, err := sup.Register(registry.App{Name: "counter", VCPUs: 1, MemMiB: 512, GuestPort: 8000}, "x"); err != nil {
		t.Fatal(err)
	}

	// A request to a COLD app boots it and gets a real answer — the caller
	// never sees the machinery.
	code, body := doReq(t, srv, http.MethodPost, "counter.localhost", "/inc", "by-2")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %q", code, body)
	}
	if body != "counter|POST|/inc|by-2|host=counter.localhost" {
		t.Fatalf("proxied response mangled: %q", body)
	}

	// Freeze it; the next request must wake it transparently (A1's shape).
	if err := sup.Freeze("counter"); err != nil {
		t.Fatal(err)
	}
	code, body = doReq(t, srv, http.MethodGet, "counter.localhost", "/", "")
	if code != http.StatusOK || !strings.HasPrefix(body, "counter|GET|/") {
		t.Fatalf("wake-on-request failed: %d %q", code, body)
	}
	if st, _ := sup.Status("counter"); st.State != lifecycle.StateActive {
		t.Fatalf("state = %s, want ACTIVE", st.State)
	}
}

func TestUnknownAppAndBadHosts(t *testing.T) {
	srv, _ := setup(t)
	if code, _ := doReq(t, srv, http.MethodGet, "ghost.localhost", "/", ""); code != http.StatusNotFound {
		t.Fatalf("unknown app: status = %d, want 404", code)
	}
	if code, _ := doReq(t, srv, http.MethodGet, "localhost", "/", ""); code != http.StatusNotFound {
		t.Fatalf("bare host: status = %d, want 404", code)
	}
}

func TestConcurrentRequestsShareOneWake(t *testing.T) {
	srv, sup := setup(t)
	if _, err := sup.Register(registry.App{Name: "counter", VCPUs: 1, MemMiB: 512, GuestPort: 8000}, "x"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := doReq(t, srv, http.MethodGet, "counter.localhost", "/", "")
			if code != http.StatusOK {
				errs <- errors.New(body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// syncDP implements just enough of lifecycle.DataPlane to count and fail
// Sync calls — the router's D1 hold.
type syncDP struct {
	mu    sync.Mutex
	syncs int
	fail  error
}

func (d *syncDP) PrepareWake(context.Context, registry.App) (bool, error) { return false, nil }
func (d *syncDP) StartShipping(registry.App, func(error)) error           { return nil }
func (d *syncDP) StopShipping(context.Context, registry.App, bool) error  { return nil }
func (d *syncDP) Sync(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.syncs++
	return d.fail
}
func (d *syncDP) BranchRestore(context.Context, registry.App, uint64, time.Time) (string, uint64, error) {
	return "", 0, nil
}
func (d *syncDP) StreamStatus(context.Context, string, string) (*dataplane.Manifest, error) {
	return nil, nil
}
func (d *syncDP) PurgeApp(context.Context, string) error { return nil }

// TestSyncAckHoldsResponses: with SyncAck on, every proxied response passes
// through the durability hold; a failing hold means the client gets a 502,
// never an unacknowledged-durability 200.
func TestSyncAckHoldsResponses(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	sup := lifecycle.New(reg, &echoBackend{addrs: map[string]string{}, snaps: map[string]bool{}},
		lifecycle.Config{WakeTimeout: 5 * time.Second, FreezeTimeout: 5 * time.Second, IdleTimeout: time.Hour},
		slog.Default())
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	dp := &syncDP{}
	sup.SetDataPlane(dp)
	rt := New(sup, slog.Default())
	rt.SyncAck = true
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)

	if _, err := sup.Register(registry.App{Name: "echo", VCPUs: 1, MemMiB: 64, GuestPort: 1}, "g"); err != nil {
		t.Fatal(err)
	}
	code, _ := doReq(t, srv, "GET", "echo.localhost", "/x", "")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	dp.mu.Lock()
	n := dp.syncs
	dp.mu.Unlock()
	if n != 1 {
		t.Fatalf("sync calls = %d, want 1 (one hold per response)", n)
	}

	// A failing hold surfaces as a gateway error, not a false ack.
	dp.mu.Lock()
	dp.fail = errors.New("gateway unreachable")
	dp.mu.Unlock()
	code, body := doReq(t, srv, "POST", "echo.localhost", "/x", "data")
	if code != http.StatusBadGateway {
		t.Fatalf("status %d (%q), want 502 when durability cannot be confirmed", code, body)
	}
}
