package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeAPI records every call to a unix-socket Firecracker API lookalike.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string // "PUT /machine-config {json}"
	srv   *http.Server
}

func newFakeAPI(t *testing.T, sock string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		compact, _ := json.Marshal(body)
		f.mu.Lock()
		f.calls = append(f.calls, fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, compact))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeAPI) snapshotCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func testMachine(t *testing.T) (*Machine, *fakeAPI) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fc.sock")
	api := newFakeAPI(t, sock)
	// White-box: a Machine with a client but no process — these tests
	// exercise the API sequence, not process management.
	return &Machine{sock: sock, client: NewClient(sock)}, api
}

// TestConfigureSequence pins the exact call order proven by wake-bench:
// machine-config → boot-source → drive → net → balloon. Reordering these
// is a Firecracker API error at best and a silently misconfigured VM at
// worst — this test makes the port un-driftable.
func TestConfigureSequence(t *testing.T) {
	m, api := testMachine(t)
	err := m.Configure(context.Background(), VMConfig{
		VCPUs: 2, MemMiB: 512,
		KernelPath: "/srv/fc/vmlinux", BootArgs: "quiet init=/init.sh",
		RootfsPath: "/srv/krill/apps/x/disk.ext4",
		TapDev:     "krill0", GuestMAC: "06:00:AC:10:00:02",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		`PUT /machine-config {"mem_size_mib":512,"smt":false,"vcpu_count":2}`,
		`PUT /boot-source {"boot_args":"quiet init=/init.sh","kernel_image_path":"/srv/fc/vmlinux"}`,
		`PUT /drives/rootfs {"drive_id":"rootfs","is_read_only":false,"is_root_device":true,"path_on_host":"/srv/krill/apps/x/disk.ext4"}`,
		`PUT /network-interfaces/eth0 {"guest_mac":"06:00:AC:10:00:02","host_dev_name":"krill0","iface_id":"eth0"}`,
		`PUT /balloon {"amount_mib":0,"deflate_on_oom":true,"stats_polling_interval_s":1}`,
		`PUT /actions {"action_type":"InstanceStart"}`,
	}
	got := api.snapshotCalls()
	if len(got) != len(want) {
		t.Fatalf("got %d calls:\n%v\nwant %d:\n%v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

func TestSnapshotAndRestoreCalls(t *testing.T) {
	m, api := testMachine(t)
	ctx := context.Background()
	if err := m.SetBalloon(ctx, 384); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBalloon(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateSnapshot(ctx, "/snap/vmstate", "/snap/mem"); err != nil {
		t.Fatal(err)
	}
	if err := m.LoadSnapshot(ctx, "/snap/vmstate", "/snap/mem"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		`PATCH /balloon {"amount_mib":384}`,
		`PATCH /balloon {"amount_mib":0}`,
		`PATCH /vm {"state":"Paused"}`,
		`PUT /snapshot/create {"mem_file_path":"/snap/mem","snapshot_path":"/snap/vmstate","snapshot_type":"Full"}`,
		`PUT /snapshot/load {"mem_backend":{"backend_path":"/snap/mem","backend_type":"File"},"resume_vm":true,"snapshot_path":"/snap/vmstate"}`,
	}
	got := api.snapshotCalls()
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// TestAPIErrorsSurface: Firecracker's error body is the only diagnostic
// there is; it must reach the caller.
func TestAPIErrorsSurface(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"fault_message":"The requested operation is not supported after starting the microVM."}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	m := &Machine{sock: sock, client: NewClient(sock)}
	err = m.Pause(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if want := "not supported after starting"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q should contain the API fault message %q", err, want)
	}
}
