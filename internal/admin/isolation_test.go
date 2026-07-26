package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/registry"
)

// F5's central rule is one sentence — a deploy that arrived over the network
// never reaches host docker — and these tests are that sentence asserted from
// both sides: the untrusted request must not build on the host, and it must
// not be silently downgraded when isolation is unavailable either.
//
// What they cannot prove is that a microVM contains a hostile build. That is
// F5 on hardware, with m4-gates/examples/hostile.

// setupIso is setup() with a handle on the *Server, so the isolation policy
// can be changed between requests.
func setupIso(t *testing.T) (*httptest.Server, *Server, *fakeBuilder) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	sup := lifecycle.New(reg, newLiveBackend(dir), lifecycle.Config{
		WakeTimeout: 5 * time.Second, FreezeTimeout: 5 * time.Second, IdleTimeout: time.Hour,
	}, slog.Default())
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	host := &fakeBuilder{dir: dir, port: 8000}
	s := New(sup, host, DeployConfig{
		WorkDir: dir, BaseHost: "krill.local", RouterAddr: ":8080",
		BuildTimeout: time.Minute, VerifyTimeout: 5 * time.Second,
	}, slog.Default())
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s, host
}

func deployUntrusted(t *testing.T, srv *httptest.Server, name string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/apps/"+name+"/deploy", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set(UntrustedHeader, "1")
	req.Header.Set(DeployByHeader, "stranger@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

var minimalContext = map[string]string{"Dockerfile": "FROM scratch\nCMD [\"/x\"]\n"}

func TestUntrustedDeployIsRefusedWhenThereIsNoBuilderVM(t *testing.T) {
	srv, _, host := setupIso(t)

	code, body := deployUntrusted(t, srv, "hostile", contextTarGz(t, minimalContext))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("untrusted deploy with no builder VM = %d, want 503\n%s", code, body)
	}
	// The point of the whole gate: it did not build on the host instead.
	if host.builds != 0 {
		t.Fatalf("the host builder ran %d time(s) for an untrusted deploy — F5 FAIL", host.builds)
	}
}

func TestUntrustedDeployGoesToTheIsolatedBuilder(t *testing.T) {
	srv, s, host := setupIso(t)
	iso := &fakeBuilder{dir: t.TempDir(), port: 8000}
	s.SetIsolatedBuilder(iso, IsolationUntrusted)

	code, body := deployUntrusted(t, srv, "hostile", contextTarGz(t, minimalContext))
	if code >= 300 {
		t.Fatalf("untrusted deploy = %d\n%s", code, body)
	}
	if iso.builds != 1 {
		t.Fatalf("the isolated builder ran %d time(s), want 1", iso.builds)
	}
	if host.builds != 0 {
		t.Fatalf("the host builder ran %d time(s) for an untrusted deploy — F5 FAIL", host.builds)
	}
}

// The operator's own deploys keep the fast host path by default, and the
// response says which path ran so nobody has to read a log to find out.
func TestOperatorDeployUsesTheHostBuilderUnlessIsolationIsAll(t *testing.T) {
	srv, s, host := setupIso(t)
	iso := &fakeBuilder{dir: t.TempDir(), port: 8000}
	s.SetIsolatedBuilder(iso, IsolationUntrusted)

	_, dr, raw := postDeploy(t, srv, "trusted", "", contextTarGz(t, minimalContext))
	if host.builds != 1 || iso.builds != 0 {
		t.Fatalf("operator deploy: host=%d isolated=%d, want 1/0\n%s", host.builds, iso.builds, raw)
	}
	if dr.Isolated {
		t.Error("the response claims an operator deploy was isolated")
	}

	// --build-isolation=all makes even Sam's deploys go through the VM.
	s.SetIsolatedBuilder(iso, IsolationAll)
	_, dr2, raw2 := postDeploy(t, srv, "trusted", "", contextTarGz(t, minimalContext))
	if iso.builds != 1 {
		t.Fatalf("with isolation=all the isolated builder ran %d time(s)\n%s", iso.builds, raw2)
	}
	if !dr2.Isolated {
		t.Error("the response does not report an isolated build")
	}
}

// Forging the header can only cost you the host path, never gain it. The
// admin API is loopback-only, so this is about making the safe direction the
// only direction rather than about defending the header itself.
func TestTheUntrustedHeaderOnlyEverRaisesIsolation(t *testing.T) {
	srv, s, host := setupIso(t)
	s.SetIsolatedBuilder(nil, IsolationOff)
	code, _ := deployUntrusted(t, srv, "hostile", contextTarGz(t, minimalContext))
	if code != http.StatusServiceUnavailable || host.builds != 0 {
		t.Fatalf("isolation=off still built an untrusted deploy on the host (code %d, builds %d)",
			code, host.builds)
	}
}
