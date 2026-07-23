// Package network gives every app its own tap device and /30 subnet:
// app with subnet index N lives in 172.16.N.0/30 — host side .1, guest .2.
//
// Two rules here were learned the hard way during the wake-path benchmark
// and are load-bearing for wake latency:
//
//  1. Tap MACs are DETERMINISTIC. A restored guest's memory image contains
//     its ARP cache; if the host side comes back with a random MAC, every
//     restored guest is deaf for ~30s until its stale entry expires.
//  2. The guest's neighbor entry is PINNED (nud permanent). Otherwise the
//     first packet after a restore races kernel ARP resolution, whose
//     who-has retry is exactly 1s — a bimodal +1000ms tax on wakes.
package network

import (
	"fmt"
	"os/exec"
	"strings"
)

// AppNet is the fully derived network identity for one subnet index.
type AppNet struct {
	SubnetIdx int
	TapName   string
	HostIP    string // .1, on the host tap
	GuestIP   string // .2, the address krilld proxies to
	HostCIDR  string // HostIP with the /30 mask, for `ip addr add`
	TapMAC    string
	GuestMAC  string
}

// Derive maps a subnet index to its network identity. Pure function — this
// is the single source of truth for the addressing scheme.
func Derive(idx int) (AppNet, error) {
	if idx < 0 || idx > 255 {
		return AppNet{}, fmt.Errorf("subnet index %d out of range [0,255]", idx)
	}
	return AppNet{
		SubnetIdx: idx,
		TapName:   fmt.Sprintf("krill%d", idx),
		HostIP:    fmt.Sprintf("172.16.%d.1", idx),
		GuestIP:   fmt.Sprintf("172.16.%d.2", idx),
		HostCIDR:  fmt.Sprintf("172.16.%d.1/30", idx),
		// Same scheme as the benchmark (AC:10 = 172.16), one byte of subnet
		// index. Locally-administered, unicast prefixes 02: and 06:.
		TapMAC:   fmt.Sprintf("02:FC:AC:10:%02X:01", idx),
		GuestMAC: fmt.Sprintf("06:00:AC:10:%02X:02", idx),
	}, nil
}

// Runner executes a host network command. Indirection exists so tests can
// record calls instead of needing root and Linux.
type Runner interface {
	Run(args ...string) (string, error)
}

// ExecRunner runs `ip ...` for real.
type ExecRunner struct{}

func (ExecRunner) Run(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

type Manager struct {
	run Runner
}

func NewManager(r Runner) *Manager {
	if r == nil {
		r = ExecRunner{}
	}
	return &Manager{run: r}
}

// EnsureTap makes the app's tap exist, addressed, up, and ARP-pinned.
// Idempotent: safe to call on every daemon start and before every boot.
func (m *Manager) EnsureTap(n AppNet) error {
	if _, err := m.run.Run("ip", "link", "show", n.TapName); err != nil {
		steps := [][]string{
			{"ip", "tuntap", "add", n.TapName, "mode", "tap"},
			{"ip", "link", "set", n.TapName, "address", n.TapMAC},
			{"ip", "addr", "add", n.HostCIDR, "dev", n.TapName},
			{"ip", "link", "set", n.TapName, "up"},
		}
		for _, s := range steps {
			if _, err := m.run.Run(s...); err != nil {
				return err
			}
		}
	}
	// Always re-pin: neighbor entries don't survive tap re-creation.
	_, err := m.run.Run("ip", "neigh", "replace", n.GuestIP,
		"lladdr", n.GuestMAC, "dev", n.TapName, "nud", "permanent")
	return err
}

// DeleteTap removes the app's tap (used on app deletion). Missing tap is not
// an error.
func (m *Manager) DeleteTap(n AppNet) error {
	if _, err := m.run.Run("ip", "link", "show", n.TapName); err != nil {
		return nil
	}
	_, err := m.run.Run("ip", "link", "del", n.TapName)
	return err
}
