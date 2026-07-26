package egress

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

// The ruleset is policy expressed as text, so the tests read it as text.
// What they cannot do is prove the kernel agrees — that is F6 on hardware,
// where the assertions are packets rather than lines.

type fakeRunner struct {
	stdin string
	args  []string
	calls int
}

func (f *fakeRunner) Run(_ context.Context, _ string, stdin string, args ...string) (string, error) {
	f.stdin, f.args, f.calls = stdin, args, f.calls+1
	return "", nil
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.ParseIP(a))
	}
	return out, nil
}

func testManager(t *testing.T, mutate func(*Config)) (*Manager, *fakeRunner) {
	t.Helper()
	cfg := Default()
	cfg.Registries = []string{"registry.example", "cdn.example"}
	cfg.Resolvers = []string{"1.1.1.1"}
	if mutate != nil {
		mutate(&cfg)
	}
	run := &fakeRunner{}
	res := fakeResolver{
		"registry.example": {"203.0.113.10", "2001:db8::10"},
		"cdn.example":      {"198.51.100.7"},
	}
	return New(cfg, run, res), run
}

func ruleset(t *testing.T, m *Manager) string {
	t.Helper()
	rs, err := m.Ruleset(context.Background())
	if err != nil {
		t.Fatalf("ruleset: %v", err)
	}
	return rs
}

// The default posture: apps reach nothing, and every prohibition F6 names is
// present whether or not this host could ever hit it.
func TestDefaultBaselineDeniesEverythingAppsCouldWant(t *testing.T) {
	m, _ := testManager(t, nil)
	rs := ruleset(t, m)

	for _, want := range []string{
		`iifname "krill*" counter drop comment "app egress denied"`,
		`iifname "krill*" oifname "krill*" counter drop comment "guest->guest denied"`,
		`tcp dport { 25, 465, 587, 2525 } counter drop`,
		"169.254.0.0/16", // the metadata range: N/A here, written anyway
		"127.0.0.0/8",
		"10.0.0.0/8",
		"192.168.0.0/16",
		`iifname "krill*" ct state established,related accept`,
		`iifname "krill*" counter drop comment "guest->host denied"`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("baseline is missing %q\n---\n%s", want, rs)
		}
	}
	// Apps get no NAT at all when their egress is off: nothing to masquerade.
	if strings.Contains(rs, "ip saddr "+AppSubnets) {
		t.Error("apps are masqueraded even though app egress is off")
	}
	if !strings.Contains(rs, "ip saddr "+BuilderSubnets) {
		t.Error("builders have no masquerade, so no build could pull an image")
	}
}

// The builder's allowance is a set of resolved addresses on 443 plus DNS —
// never "all of :443", which would be the general internet access F6 FAILs on.
func TestBuilderReachesTheRegistryAndNothingElse(t *testing.T) {
	m, _ := testManager(t, nil)
	rs := ruleset(t, m)

	if !strings.Contains(rs, "elements = { 198.51.100.7, 203.0.113.10 }") {
		t.Errorf("registry set does not hold the resolved v4 addresses\n---\n%s", rs)
	}
	if !strings.Contains(rs, "elements = { 2001:db8::10 }") {
		t.Errorf("registry set does not hold the resolved v6 address\n---\n%s", rs)
	}
	if !strings.Contains(rs, `iifname "krillb*" ip daddr @registry4 tcp dport 443`) {
		t.Error("builders cannot reach the registry")
	}
	if !strings.Contains(rs, `iifname "krillb*" counter drop comment "builder->everything else denied"`) {
		t.Error("builders have no catch-all drop")
	}
	// The drop must come after the accepts, or the accepts are dead lines.
	accept := strings.Index(rs, `@registry4 tcp dport 443`)
	drop := strings.Index(rs, `"builder->everything else denied"`)
	if accept < 0 || drop < 0 || drop < accept {
		t.Fatalf("builder rules are ordered wrong: accept at %d, drop at %d", accept, drop)
	}
	// And the prohibitions must come before both, so no builder accept can
	// reopen SMTP or another guest's subnet.
	smtp := strings.Index(rs, `tcp dport { 25, 465, 587, 2525 }`)
	if smtp < 0 || smtp > accept {
		t.Fatalf("the SMTP drop lands after the builder accepts (%d vs %d)", smtp, accept)
	}
}

// Granting apps egress must not grant them the things nobody may have.
func TestAppEgressStillDeniesTheDangerousThings(t *testing.T) {
	m, _ := testManager(t, func(c *Config) { c.AppEgress = true })
	rs := ruleset(t, m)

	if !strings.Contains(rs, `comment "app egress (limited)"`) {
		t.Fatal("app egress was enabled but no accept rule appeared")
	}
	if !strings.Contains(rs, "ip saddr "+AppSubnets) {
		t.Error("app egress is allowed but not masqueraded, so it cannot work")
	}
	smtp := strings.Index(rs, `tcp dport { 25, 465, 587, 2525 }`)
	appAccept := strings.Index(rs, `comment "app egress (limited)"`)
	if smtp < 0 || appAccept < 0 || smtp > appAccept {
		t.Fatal("app egress is permitted before SMTP is denied: 587 would be open")
	}
	if !strings.Contains(rs, "limit rate") {
		t.Error("app egress is not rate-limited")
	}
}

// The limit is what F6 step 3 drives past, so it has to be in the rule that
// actually permits traffic rather than decorative.
func TestRateLimitAppearsOnEveryAcceptThatLetsPacketsOut(t *testing.T) {
	m, _ := testManager(t, func(c *Config) {
		c.AppEgress = true
		c.RatePerSecond = 5
		c.Burst = 7
	})
	rs := ruleset(t, m)
	for _, line := range strings.Split(rs, "\n") {
		if !strings.Contains(line, "accept") || !strings.Contains(line, "iifname") {
			continue
		}
		if strings.Contains(line, "ct state established") {
			continue // reply traffic to host-initiated flows is not egress
		}
		if !strings.Contains(line, "limit rate 5/second burst 7 packets") {
			t.Errorf("an accept rule has no rate limit:\n  %s", line)
		}
	}
}

// Applying must be one transaction. Applied rule-by-rule there is a window
// where masquerade exists and the drops do not.
func TestApplyIsASingleAtomicTransaction(t *testing.T) {
	m, run := testManager(t, nil)
	if err := m.Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if run.calls != 1 {
		t.Fatalf("apply ran nft %d times, want exactly 1", run.calls)
	}
	if got := strings.Join(run.args, " "); got != "-f -" {
		t.Fatalf("apply invoked nft with %q, want a file on stdin", got)
	}
	// Replace, not merge: the old table is deleted in the same file.
	del := strings.Index(run.stdin, "delete table inet "+TableName)
	create := strings.Index(run.stdin, "table inet "+TableName+" {")
	if del < 0 || create < 0 || del > create {
		t.Fatalf("the ruleset does not delete-then-create the table:\n%s", run.stdin)
	}
	if m.Applied() != run.stdin {
		t.Error("Applied() does not report what was handed to nft")
	}
}

// A registry that resolves to nothing means every build fails. Installing the
// ruleset anyway would turn that into a mystery; refusing names it.
func TestUnresolvableRegistryRefusesRatherThanSilentlyBreakingBuilds(t *testing.T) {
	m := New(Config{Registries: []string{"nowhere.invalid"}}, &fakeRunner{}, fakeResolver{})
	if _, err := m.Ruleset(context.Background()); err == nil {
		t.Fatal("a ruleset was produced with an empty registry set")
	}
}

// No registries configured at all is a legitimate posture — total silence,
// which is what a host with builds disabled should have.
func TestNoRegistriesIsAllowedAndDeniesEverything(t *testing.T) {
	m := New(Config{}, &fakeRunner{}, fakeResolver{})
	rs, err := m.Ruleset(context.Background())
	if err != nil {
		t.Fatalf("ruleset: %v", err)
	}
	if !strings.Contains(rs, `comment "builder->everything else denied"`) {
		t.Error("with no registry, builders should still be explicitly denied")
	}
}
