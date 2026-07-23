package network

import (
	"errors"
	"strings"
	"testing"
)

func TestDerive(t *testing.T) {
	n, err := Derive(7)
	if err != nil {
		t.Fatal(err)
	}
	want := AppNet{
		SubnetIdx: 7,
		TapName:   "krill7",
		HostIP:    "172.16.7.1",
		GuestIP:   "172.16.7.2",
		HostCIDR:  "172.16.7.1/30",
		TapMAC:    "02:FC:AC:10:07:01",
		GuestMAC:  "06:00:AC:10:07:02",
	}
	if n != want {
		t.Fatalf("Derive(7) = %+v, want %+v", n, want)
	}
	// MACs must be unique across the full index space (the snapshot embeds
	// the guest's ARP cache — a MAC collision would cross-wire two apps).
	seen := map[string]bool{}
	for i := 0; i <= 255; i++ {
		n, err := Derive(i)
		if err != nil {
			t.Fatal(err)
		}
		if seen[n.TapMAC] || seen[n.GuestMAC] {
			t.Fatalf("MAC collision at index %d", i)
		}
		seen[n.TapMAC], seen[n.GuestMAC] = true, true
	}
	for _, bad := range []int{-1, 256} {
		if _, err := Derive(bad); err == nil {
			t.Errorf("Derive(%d) should fail", bad)
		}
	}
}

// fakeRunner scripts the `ip link show` existence probe and records
// everything else.
type fakeRunner struct {
	tapExists bool
	calls     []string
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if strings.HasPrefix(cmd, "ip link show") && !f.tapExists {
		return "", errors.New("does not exist")
	}
	return "", nil
}

func TestEnsureTapCreatesAndPins(t *testing.T) {
	run := &fakeRunner{tapExists: false}
	m := NewManager(run)
	n, _ := Derive(3)
	if err := m.EnsureTap(n); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip link show krill3",
		"ip tuntap add krill3 mode tap",
		"ip link set krill3 address 02:FC:AC:10:03:01",
		"ip addr add 172.16.3.1/30 dev krill3",
		"ip link set krill3 up",
		"ip neigh replace 172.16.3.2 lladdr 06:00:AC:10:03:02 dev krill3 nud permanent",
	}
	if len(run.calls) != len(want) {
		t.Fatalf("calls = %q, want %q", run.calls, want)
	}
	for i := range want {
		if run.calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, run.calls[i], want[i])
		}
	}
}

func TestEnsureTapIdempotentButAlwaysRepins(t *testing.T) {
	run := &fakeRunner{tapExists: true}
	m := NewManager(run)
	n, _ := Derive(3)
	if err := m.EnsureTap(n); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 {
		t.Fatalf("existing tap should only probe + re-pin, got %q", run.calls)
	}
	if !strings.HasPrefix(run.calls[1], "ip neigh replace") {
		t.Fatalf("neighbor pin must happen even when the tap exists, got %q", run.calls[1])
	}
}
