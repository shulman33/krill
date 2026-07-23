package admin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/builder"
	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/registry"
)

// liveBackend boots a real HTTP listener per app so deploy's verification
// wake exercises the true probe path.
type liveBackend struct {
	mu       sync.Mutex
	addrs    map[string]string
	goldens  map[string]string
	failBoot map[string]bool // apps whose guests "crash"
	serial   map[string]string
	dir      string
}

type liveInstance struct{ srv *http.Server }

func (i *liveInstance) Kill() { i.srv.Close() }

func newLiveBackend(dir string) *liveBackend {
	return &liveBackend{addrs: map[string]string{}, goldens: map[string]string{},
		failBoot: map[string]bool{}, serial: map[string]string{}, dir: dir}
}

func (b *liveBackend) Install(app registry.App, golden string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.goldens[app.Name] = golden
	return nil
}
func (b *liveBackend) Prepare(registry.App) error    { return nil }
func (b *liveBackend) HasSnapshot(registry.App) bool { return false }
func (b *liveBackend) Replace(app registry.App, golden string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.goldens[app.Name] = golden
	return nil
}
func (b *liveBackend) SerialLogPath(app registry.App) string {
	return filepath.Join(b.dir, app.Name+".serial.log")
}
func (b *liveBackend) ColdBoot(_ context.Context, app registry.App) (lifecycle.Instance, error) {
	b.mu.Lock()
	crash := b.failBoot[app.Name]
	log := b.serial[app.Name]
	b.mu.Unlock()
	if crash {
		os.WriteFile(b.SerialLogPath(app), []byte(log), 0o644)
		return nil, errors.New("guest exited")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok %s", app.Name)
	})}
	go srv.Serve(ln)
	b.mu.Lock()
	b.addrs[app.Name] = ln.Addr().String()
	b.mu.Unlock()
	return &liveInstance{srv: srv}, nil
}
func (b *liveBackend) Restore(ctx context.Context, app registry.App) (lifecycle.Instance, error) {
	return b.ColdBoot(ctx, app)
}
func (b *liveBackend) Snapshot(context.Context, registry.App, lifecycle.Instance) error { return nil }
func (b *liveBackend) GuestAddr(app registry.App) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if a, ok := b.addrs[app.Name]; ok {
		return a
	}
	return "127.0.0.1:1"
}
func (b *liveBackend) Purge(registry.App) error { return nil }

// fakeBuilder returns a canned result without touching docker.
type fakeBuilder struct {
	failWith *builder.BuildError
	port     int
	dir      string
	builds   int
}

func (f *fakeBuilder) Build(_ context.Context, name, contextDir string, sizeMB int) (*builder.Result, error) {
	f.builds++
	if f.failWith != nil {
		return nil, f.failWith
	}
	// The context must actually have been extracted for us.
	if _, err := os.Stat(filepath.Join(contextDir, "Dockerfile")); err != nil {
		return nil, fmt.Errorf("context not extracted: %w", err)
	}
	golden := filepath.Join(f.dir, fmt.Sprintf("golden-%s-%d.ext4", name, f.builds))
	if err := os.WriteFile(golden, []byte("ext4"), 0o644); err != nil {
		return nil, err
	}
	if sizeMB == 0 {
		sizeMB = 1024
	}
	return &builder.Result{GoldenPath: golden, GuestPort: f.port, SizeMB: sizeMB, BuildLog: "ok"}, nil
}

func setup(t *testing.T) (*httptest.Server, *liveBackend, *fakeBuilder) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	be := newLiveBackend(dir)
	sup := lifecycle.New(reg, be, lifecycle.Config{
		WakeTimeout: 5 * time.Second, FreezeTimeout: 5 * time.Second, IdleTimeout: time.Hour,
	}, slog.Default())
	if err := sup.Reconcile(); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBuilder{dir: dir, port: 8000}
	srv := httptest.NewServer(New(sup, fb, DeployConfig{
		WorkDir:       dir,
		BaseHost:      "krill.local",
		RouterAddr:    ":8080",
		BuildTimeout:  time.Minute,
		VerifyTimeout: 5 * time.Second,
	}, slog.Default()))
	t.Cleanup(srv.Close)
	return srv, be, fb
}

func contextTarGz(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return bytes.NewReader(buf.Bytes())
}

func postDeploy(t *testing.T, srv *httptest.Server, name, query string, body io.Reader) (int, deployResp, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/apps/"+name+"/deploy"+query, "application/gzip", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var dr deployResp
	json.Unmarshal(raw, &dr)
	return resp.StatusCode, dr, string(raw)
}

func TestDeployNewAppEndToEnd(t *testing.T) {
	srv, be, _ := setup(t)
	code, dr, raw := postDeploy(t, srv, "guestbook", "",
		contextTarGz(t, map[string]string{"Dockerfile": "FROM x", "app.py": "print"}))
	if code != http.StatusCreated {
		t.Fatalf("status %d: %s", code, raw)
	}
	if !dr.Created || !dr.Ready {
		t.Fatalf("created=%v ready=%v: %s", dr.Created, dr.Ready, raw)
	}
	if dr.URL != "http://guestbook.krill.local:8080/" {
		t.Fatalf("url = %q", dr.URL)
	}
	if dr.App.State != lifecycle.StateActive {
		t.Fatalf("app state = %s, want ACTIVE after verification wake", dr.App.State)
	}
	if be.goldens["guestbook"] == "" {
		t.Fatal("golden never installed")
	}
}

func TestRedeployKeepsSubnetAndReturnsOK(t *testing.T) {
	srv, be, _ := setup(t)
	_, dr1, _ := postDeploy(t, srv, "app", "",
		contextTarGz(t, map[string]string{"Dockerfile": "FROM x"}))
	code, dr2, raw := postDeploy(t, srv, "app", "?mem_mib=1024",
		contextTarGz(t, map[string]string{"Dockerfile": "FROM y"}))
	if code != http.StatusOK || dr2.Created {
		t.Fatalf("redeploy: code=%d created=%v: %s", code, dr2.Created, raw)
	}
	if dr2.App.SubnetIdx != dr1.App.SubnetIdx {
		t.Fatalf("subnet moved on redeploy: %d -> %d", dr1.App.SubnetIdx, dr2.App.SubnetIdx)
	}
	if dr2.App.MemMiB != 1024 || dr2.App.VCPUs != dr1.App.VCPUs {
		t.Fatalf("spec: mem=%d vcpus=%d", dr2.App.MemMiB, dr2.App.VCPUs)
	}
	if !dr2.Ready {
		t.Fatalf("not ready: %s", raw)
	}
	if len(be.goldens) != 1 {
		t.Fatalf("goldens: %v", be.goldens)
	}
}

func TestDeployBuildFailureReturns422WithLog(t *testing.T) {
	srv, _, fb := setup(t)
	fb.failWith = &builder.BuildError{Stage: "docker build",
		Log: "Step 3/4: RUN pip install nope\nERROR: not found", Err: errors.New("exit status 1")}
	code, _, raw := postDeploy(t, srv, "bad", "",
		contextTarGz(t, map[string]string{"Dockerfile": "FROM x"}))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", code, raw)
	}
	var body map[string]string
	json.Unmarshal([]byte(raw), &body)
	if body["stage"] != "docker build" || body["build_log"] == "" {
		t.Fatalf("422 body missing structure: %s", raw)
	}
}

func TestDeployCrashingGuestReportsErrorsStructured(t *testing.T) {
	srv, be, _ := setup(t)
	be.mu.Lock()
	be.failBoot["broken"] = true
	be.serial["broken"] = "Traceback (most recent call last):\n  File \"/srv/app.py\", line 10, in <module>\n    app = FastAPI()\nNameError: name 'FastAPI' is not defined\n"
	be.mu.Unlock()

	code, dr, raw := postDeploy(t, srv, "broken", "",
		contextTarGz(t, map[string]string{"Dockerfile": "FROM x"}))
	if code != http.StatusCreated {
		t.Fatalf("deploy itself should succeed (build was fine): %d %s", code, raw)
	}
	if dr.Ready {
		t.Fatalf("ready=true for a crashing guest: %s", raw)
	}
	if dr.WakeError == "" {
		t.Fatalf("missing wake_error: %s", raw)
	}
	if len(dr.Errors) == 0 || dr.Errors[0].Kind != "python_traceback" {
		t.Fatalf("structured errors missing: %s", raw)
	}
	// The self-heal loop: fixing the app and redeploying reports ready.
	be.mu.Lock()
	be.failBoot["broken"] = false
	be.mu.Unlock()
	code, dr, raw = postDeploy(t, srv, "broken", "",
		contextTarGz(t, map[string]string{"Dockerfile": "FROM fixed"}))
	if code != http.StatusOK || !dr.Ready {
		t.Fatalf("fixed redeploy: code=%d ready=%v: %s", code, dr.Ready, raw)
	}
}

func TestDeployRejectsGarbageContext(t *testing.T) {
	srv, _, fb := setup(t)
	code, _, raw := postDeploy(t, srv, "x", "", bytes.NewReader([]byte("not a tarball")))
	if code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", code, raw)
	}
	if fb.builds != 0 {
		t.Fatal("builder ran on garbage input")
	}
}

func TestDeployPortPreference(t *testing.T) {
	srv, _, fb := setup(t)
	fb.port = 3000 // EXPOSEd port
	_, dr, raw := postDeploy(t, srv, "a", "", contextTarGz(t, map[string]string{"Dockerfile": "F"}))
	if dr.App.GuestPort != 3000 {
		t.Fatalf("EXPOSE port not used: %s", raw)
	}
	_, dr, raw = postDeploy(t, srv, "b", "?guest_port=9999", contextTarGz(t, map[string]string{"Dockerfile": "F"}))
	if dr.App.GuestPort != 9999 {
		t.Fatalf("explicit port not used: %s", raw)
	}
}

func TestLogsEndpoint(t *testing.T) {
	srv, be, _ := setup(t)
	postDeploy(t, srv, "app", "?verify=false", contextTarGz(t, map[string]string{"Dockerfile": "F"}))
	os.WriteFile(be.SerialLogPath(registry.App{Name: "app"}),
		[]byte("boot line\npanic: oh no\n"), 0o644)

	resp, err := http.Get(srv.URL + "/v1/apps/app/logs?tail=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Lines  []string `json:"lines"`
		Errors []struct {
			Kind string `json:"kind"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Lines) != 1 || body.Lines[0] != "panic: oh no" {
		t.Fatalf("tail: %+v", body.Lines)
	}
	// The error was parsed from the wider window even though tail=1.
	if len(body.Errors) != 1 || body.Errors[0].Kind != "panic" {
		t.Fatalf("errors: %+v", body.Errors)
	}
}

func TestLogsUnknownApp(t *testing.T) {
	srv, _, _ := setup(t)
	resp, err := http.Get(srv.URL + "/v1/apps/ghost/logs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}
