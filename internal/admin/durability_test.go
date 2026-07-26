package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/regbackup"
	"github.com/shulman33/krill/internal/registry"
)

func setupDurability(t *testing.T, wired bool) (*httptest.Server, objstore.Store) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "krill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Create(registry.App{
		Name: "guestbook", VCPUs: 1, MemMiB: 128, GuestPort: 8080, State: "COLD",
	}); err != nil {
		t.Fatal(err)
	}
	sup := lifecycle.New(reg, newLiveBackend(dir), lifecycle.Config{
		WakeTimeout: 5 * time.Second, FreezeTimeout: 5 * time.Second, IdleTimeout: time.Hour,
	}, slog.Default())
	s := New(sup, &fakeBuilder{dir: dir, port: 8000}, DeployConfig{WorkDir: dir}, slog.Default())
	var store objstore.Store
	if wired {
		store = objstore.NewMem()
		s.SetDurability(Durability{
			Spec: "mem:", Store: store, BackupSpec: "mem:",
			Backups: regbackup.New(regbackup.Config{
				Store: store, Source: reg, WorkDir: filepath.Join(dir, "backup"),
				CellGen: 2, Interval: 24 * time.Hour, Keep: 3,
			}),
		})
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, store
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if into != nil {
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("decoding %s: %v (%s)", url, err, raw)
		}
	}
	return resp.StatusCode
}

func TestObjstoreCheckEndpoint(t *testing.T) {
	srv, _ := setupDurability(t, true)
	var body struct {
		Spec  string `json:"spec"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if code := getJSON(t, srv.URL+"/v1/objstore", &body); code != http.StatusOK {
		t.Fatalf("status %d, body %+v", code, body)
	}
	if !body.OK || body.Spec != "mem:" {
		t.Fatalf("body = %+v, want ok with spec mem:", body)
	}
}

func TestRegistryBackupEndpoints(t *testing.T) {
	srv, store := setupDurability(t, true)

	// Nothing shipped yet.
	var list []regbackup.Info
	if code := getJSON(t, srv.URL+"/v1/registry/backups", &list); code != http.StatusOK || len(list) != 0 {
		t.Fatalf("initial list: status %d, %d entries", code, len(list))
	}

	resp, err := http.Post(srv.URL+"/v1/registry/backup", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST backup: %s: %s", resp.Status, raw)
	}
	var info regbackup.Info
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info.Apps != 1 || info.CellGen != 2 || info.RestoreCellGen != 3 {
		t.Errorf("info = %+v, want 1 app, cell_gen 2, restore_cell_gen 3", info)
	}
	if _, _, err := store.Get(t.Context(), info.Key); err != nil {
		t.Errorf("backup object %s not in the store: %v", info.Key, err)
	}
	if code := getJSON(t, srv.URL+"/v1/registry/backups", &list); code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list after backup: status %d, %d entries", code, len(list))
	}
}

// With --data-plane off (or backups disabled) the routes must say so plainly
// rather than 404 or pretend success.
func TestDurabilityRoutesWhenUnwired(t *testing.T) {
	srv, _ := setupDurability(t, false)
	if code := getJSON(t, srv.URL+"/v1/objstore", nil); code != http.StatusServiceUnavailable {
		t.Errorf("GET /v1/objstore = %d, want 503", code)
	}
	if code := getJSON(t, srv.URL+"/v1/registry/backups", nil); code != http.StatusServiceUnavailable {
		t.Errorf("GET /v1/registry/backups = %d, want 503", code)
	}
	resp, err := http.Post(srv.URL+"/v1/registry/backup", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("POST /v1/registry/backup = %d, want 503", resp.StatusCode)
	}
}
