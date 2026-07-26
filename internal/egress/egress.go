// Package egress is F6: what a guest is allowed to send.
//
// The starting position is unusual and worth stating plainly — guests
// currently have NO outbound connectivity at all, because no NAT was ever
// configured. That is not a security posture, it is an accident of M1 not
// needing one. The moment anything needs outbound (and the builder VM is
// exactly what needs it first), the accident stops protecting us. So the
// baseline lands in the same change as the first masquerade rule, never
// after it.
//
// The shape is nftables, because that is the proven component for this job
// (posture map, today-risk #2) and because a separate `inet krill` table can
// drop packets without touching whatever ufw has already put on the box:
// nftables evaluates every table, and one drop verdict ends the packet no
// matter how many other tables said accept.
//
// What the rules encode, in the order they matter:
//
//  1. App guests reach NOTHING outbound by default.
//  2. No guest ever reaches another guest's subnet, the host's own
//     addresses, the RFC1918 world behind the host, or link-local
//     169.254.0.0/16. The metadata-service drop is N/A on Hetzner bare metal
//     and is written anyway: the rule has to travel if this ever runs on a
//     cloud host, and that is precisely the day nobody will remember to add
//     it.
//  3. SMTP is closed on every port anyone actually uses for it. Hetzner
//     blocks 25/465 provider-side; 587 and 2525 are ours to own.
//  4. Builder VMs reach a container registry and a resolver, and nothing
//     else. They are the only guests with any egress at all.
//  5. Everything that is allowed is rate-limited and counted, so "the limit
//     engaged" is a number you can read rather than a claim.
package egress

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// TableName is the nftables table this package owns entirely. It is created
// and replaced atomically; nothing else should write to it.
const TableName = "krill"

// AppTapPrefix and BuilderTapPrefix are how the rules tell an app guest from
// a builder guest. Two prefixes rather than one plus a list, so a rule can
// name the class instead of enumerating members that change on every deploy.
const (
	AppTapPrefix     = "krill"
	BuilderTapPrefix = "krillb"
)

// AppSubnets and BuilderSubnets are the ranges Derive hands out. They appear
// in the ruleset as the definition of "another guest's subnet".
const (
	AppSubnets     = "172.16.0.0/16"
	BuilderSubnets = "172.17.0.0/16"
)

type Config struct {
	// AppEgress lets ordinary app guests talk to the internet. Off by
	// default and expected to stay off: nothing an app does needs it, and
	// every abuse story on a single-IP host starts here. When on, apps get
	// the same prohibitions as builders plus the rate limit — F6's "a guest
	// granted egress still cannot reach 25/465/587".
	AppEgress bool
	// Registries are the container registries a builder VM may reach on 443.
	Registries []string
	// BuildAllow are additional hostnames the operator has chosen to permit
	// builders to reach on 443 — distro mirrors, PyPI, npm.
	//
	// This exists because of a real tension F6 does not resolve on its own:
	// "the registry and nothing else" is the right posture, and almost every
	// real Dockerfile also runs `apt-get install` or `pip install`. Both
	// lists feed one nft set, so the mechanism is unchanged; the split is so
	// a unit file says out loud which hosts are the registry and which are a
	// deliberate widening. An allowlist of named package sources is still not
	// the "general internet access" F6 FAILs on — but it is a bigger surface
	// than the default, and it should be a decision rather than a discovery.
	BuildAllow []string
	// Resolvers are the DNS servers a builder may query on 53.
	Resolvers []string
	// RatePerSecond and Burst bound any permitted egress, per guest
	// interface. A limit that cannot be exceeded is F6's phrasing; the
	// counter beside it is what makes that observable.
	RatePerSecond int
	Burst         int
	// ResolveTimeout bounds the registry lookup at ruleset-build time.
	ResolveTimeout time.Duration
}

func Default() Config {
	return Config{
		AppEgress: false,
		// Docker Hub's three names: the registry itself, its token service,
		// and the CDN that actually serves layers. A registry reachable
		// without its auth endpoint is a registry that cannot be pulled from.
		Registries:     []string{"registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com"},
		Resolvers:      []string{"1.1.1.1", "8.8.8.8"},
		RatePerSecond:  200,
		Burst:          400,
		ResolveTimeout: 10 * time.Second,
	}
}

// Runner executes a host command. The indirection exists so the ruleset can
// be generated and asserted without root, Linux, or nftables — which is the
// only way this package is testable on the machine it is written on.
type Runner interface {
	Run(ctx context.Context, name string, stdin string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// Resolver maps a hostname to addresses. Swappable for the same reason.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

type netResolver struct{}

func (netResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

type Manager struct {
	cfg  Config
	run  Runner
	res  Resolver
	nft  string
	last string // the ruleset currently applied, for Applied()
}

func New(cfg Config, run Runner, res Resolver) *Manager {
	if run == nil {
		run = ExecRunner{}
	}
	if res == nil {
		res = netResolver{}
	}
	if cfg.RatePerSecond <= 0 {
		cfg.RatePerSecond = Default().RatePerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = cfg.RatePerSecond * 2
	}
	if cfg.ResolveTimeout <= 0 {
		cfg.ResolveTimeout = Default().ResolveTimeout
	}
	return &Manager{cfg: cfg, run: run, res: res, nft: "nft"}
}

// Apply installs the whole baseline in one atomic transaction.
//
// Atomic matters more than it looks: applied rule by rule, there is a window
// in which masquerade exists and the drops do not, and that window is a guest
// with unrestricted internet access. `nft -f` with a delete-then-create pair
// in one file never opens it.
func (m *Manager) Apply(ctx context.Context) error {
	ruleset, err := m.Ruleset(ctx)
	if err != nil {
		return err
	}
	if _, err := m.run.Run(ctx, m.nft, ruleset, "-f", "-"); err != nil {
		return fmt.Errorf("applying the egress baseline: %w", err)
	}
	m.last = ruleset
	return nil
}

// Applied returns the ruleset text last handed to nft, for the gate scripts
// and for `krilld -print-egress`.
func (m *Manager) Applied() string { return m.last }

// Ruleset renders the baseline. Exported so it can be printed, diffed and
// asserted without applying anything.
func (m *Manager) Ruleset(ctx context.Context) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, m.cfg.ResolveTimeout)
	defer cancel()
	registry4, registry6 := m.resolveRegistries(rctx)
	if len(registry4) == 0 && len(registry6) == 0 && len(m.cfg.Registries)+len(m.cfg.BuildAllow) > 0 {
		// A builder that cannot reach a registry cannot build. Failing here is
		// better than installing a ruleset that silently breaks every deploy.
		return "", fmt.Errorf("egress: none of %v resolved; refusing to install a ruleset "+
			"that would make every build fail", append(m.cfg.Registries, m.cfg.BuildAllow...))
	}
	resolvers := m.resolvers()

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# Generated by krilld (internal/egress). Do not edit: it is replaced")
	w("# wholesale on every daemon start. F6's baseline.")
	w("table inet %s {", TableName)
	w("  set registry4 {")
	w("    type ipv4_addr")
	if len(registry4) > 0 {
		w("    elements = { %s }", strings.Join(registry4, ", "))
	}
	w("  }")
	w("  set registry6 {")
	w("    type ipv6_addr")
	if len(registry6) > 0 {
		w("    elements = { %s }", strings.Join(registry6, ", "))
	}
	w("  }")
	w("  set resolvers {")
	w("    type ipv4_addr")
	if len(resolvers) > 0 {
		w("    elements = { %s }", strings.Join(resolvers, ", "))
	}
	w("  }")
	w("")
	w("  # Guests may not open connections to the host. Replies to flows the")
	w("  # host started (the router proxying a request inward) are conntrack,")
	w("  # not new connections, so this does not touch the product. It does")
	w("  # close sshd, which listens on 0.0.0.0 and is otherwise one hop from")
	w("  # every guest via its own default gateway.")
	w("  chain input {")
	w("    type filter hook input priority filter; policy accept;")
	w(`    iifname "%s*" ct state established,related accept`, AppTapPrefix)
	w(`    iifname "%s*" counter drop comment "guest->host denied"`, AppTapPrefix)
	w("  }")
	w("")
	w("  chain forward {")
	w("    type filter hook forward priority filter; policy accept;")
	w("    # Order is the policy. Prohibitions first, so no later accept can")
	w("    # reopen something an earlier line closed.")
	w(`    iifname "%s*" oifname "%s*" counter drop comment "guest->guest denied"`, AppTapPrefix, AppTapPrefix)
	w(`    iifname "%s*" ip daddr { %s } counter drop comment "guest->other subnets denied"`,
		AppTapPrefix, strings.Join(deniedDestinations(), ", "))
	w(`    iifname "%s*" ip6 daddr { fe80::/10, fc00::/7, ::1/128 } counter drop comment "guest->v6 local denied"`, AppTapPrefix)
	w(`    iifname "%s*" tcp dport { 25, 465, 587, 2525 } counter drop comment "smtp denied"`, AppTapPrefix)
	w("")
	w("    # Builder VMs: a registry, a resolver, nothing else.")
	w(`    iifname "%s*" ip daddr @registry4 tcp dport 443 %s counter accept comment "builder->registry"`,
		BuilderTapPrefix, m.limit())
	w(`    iifname "%s*" ip6 daddr @registry6 tcp dport 443 %s counter accept comment "builder->registry v6"`,
		BuilderTapPrefix, m.limit())
	w(`    iifname "%s*" ip daddr @resolvers udp dport 53 %s counter accept comment "builder->dns"`,
		BuilderTapPrefix, m.limit())
	w(`    iifname "%s*" ip daddr @resolvers tcp dport 53 %s counter accept comment "builder->dns tcp"`,
		BuilderTapPrefix, m.limit())
	w(`    iifname "%s*" counter drop comment "builder->everything else denied"`, BuilderTapPrefix)
	w("")
	if m.cfg.AppEgress {
		w("    # --app-egress is on: apps may reach the internet, still without")
		w("    # SMTP, other guests, or anything local, and always rate-limited.")
		w(`    iifname "%s*" %s counter accept comment "app egress (limited)"`, AppTapPrefix, m.limit())
		w(`    iifname "%s*" counter drop comment "app egress over limit"`, AppTapPrefix)
	} else {
		w("    # Apps are silent. This is the default and should stay it.")
		w(`    iifname "%s*" counter drop comment "app egress denied"`, AppTapPrefix)
	}
	w("  }")
	w("")
	w("  chain postrouting {")
	w("    type nat hook postrouting priority srcnat; policy accept;")
	w("    # Masquerade only what the forward chain has already agreed to let")
	w("    # out. Builders always; apps only when their egress is enabled.")
	w(`    ip saddr %s oifname != "%s*" counter masquerade`, BuilderSubnets, AppTapPrefix)
	if m.cfg.AppEgress {
		w(`    ip saddr %s oifname != "%s*" counter masquerade`, AppSubnets, AppTapPrefix)
	}
	w("  }")
	w("}")
	// The delete must precede the definition in the same file: nft applies a
	// file as one transaction, so this is replace-not-merge with no gap.
	return fmt.Sprintf("table inet %s\ndelete table inet %s\n\n%s", TableName, TableName, b.String()), nil
}

func (m *Manager) limit() string {
	return fmt.Sprintf("limit rate %d/second burst %d packets", m.cfg.RatePerSecond, m.cfg.Burst)
}

// deniedDestinations is everywhere a guest must never reach: the host's own
// loopback, every private range (which includes both guest subnet blocks and
// whatever LAN the host sits on), carrier NAT space, and link-local — the
// last of which is the cloud metadata service, N/A here and written anyway.
func deniedDestinations() []string {
	return []string{
		"127.0.0.0/8",
		"169.254.0.0/16",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10",
	}
}

func (m *Manager) resolvers() []string {
	var out []string
	for _, r := range m.cfg.Resolvers {
		if ip := net.ParseIP(strings.TrimSpace(r)); ip != nil && ip.To4() != nil {
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}

// resolveRegistries turns registry hostnames into address-set elements.
//
// The honest caveat, recorded here rather than discovered later: a CDN can
// return addresses this lookup never saw, so the set is refreshed on a timer
// while builds are possible. The alternative — allowing all of :443 — is
// "general internet access", which F6 FAILs on explicitly.
func (m *Manager) resolveRegistries(ctx context.Context) (v4, v6 []string) {
	seen4, seen6 := map[string]bool{}, map[string]bool{}
	for _, host := range append(append([]string{}, m.cfg.Registries...), m.cfg.BuildAllow...) {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			addSeen(ip, seen4, seen6)
			continue
		}
		ips, err := m.res.LookupIP(ctx, host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			addSeen(ip, seen4, seen6)
		}
	}
	return sortedKeys(seen4), sortedKeys(seen6)
}

func addSeen(ip net.IP, seen4, seen6 map[string]bool) {
	if v4 := ip.To4(); v4 != nil {
		seen4[v4.String()] = true
		return
	}
	seen6[ip.String()] = true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RunRefresh keeps the registry set current. Builders are short-lived, so the
// window that matters is "a build started after DNS moved", not "a set that
// is stale forever".
func (m *Manager) RunRefresh(ctx context.Context, every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.Apply(ctx)
		}
	}
}

// Counters reads the named counters back, which is how a gate script proves
// a rule engaged rather than merely existed.
func (m *Manager) Counters(ctx context.Context) (string, error) {
	return m.run.Run(ctx, m.nft, "", "-a", "list", "table", "inet", TableName)
}
