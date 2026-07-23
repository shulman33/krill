package sim

import (
	"os"
	"strconv"
	"testing"
)

// seedBudget: the default is CI-friendly; the C4 gate runs the full 10k
// via KRILL_SIM_SEEDS=10000.
func seedBudget(t *testing.T, def int) int {
	if v := os.Getenv("KRILL_SIM_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("KRILL_SIM_SEEDS=%q: %v", v, err)
		}
		return n
	}
	if testing.Short() {
		return def / 10
	}
	return def
}

// TestPositiveInvariants is C4's positive half: with all three fences on,
// no seeded schedule may violate any invariant.
func TestPositiveInvariants(t *testing.T) {
	n := seedBudget(t, 2000)
	cfg := Default()
	for seed := 0; seed < n; seed++ {
		res := Run(int64(seed), cfg)
		if res.Violation != nil {
			t.Fatalf("seed %d violated an invariant with ALL fences on:\n  %v\ntrace:\n%s",
				seed, res.Violation, traceDump(res))
		}
	}
}

// The negative configs, one fence off at a time — each MUST find its
// violation, reproducing the TLC counterexamples against the real code.
// A fence that cannot fail when disabled is a fence the harness no longer
// exercises.
func TestNegativeGatewayFencing(t *testing.T) {
	cfg := Default()
	cfg.GatewayFencing = false
	requireViolation(t, cfg, "GatewayFencing off (PT-1, the partition zombie)")
}

func TestNegativeReplayOnRestore(t *testing.T) {
	cfg := Default()
	cfg.ReplayOnRestore = false
	requireViolation(t, cfg, "ReplayOnRestore off (the E6 stale-restore bug)")
}

func TestNegativeRegistrationFencing(t *testing.T) {
	cfg := Default()
	cfg.RegistrationFencing = false
	requireViolation(t, cfg, "RegistrationFencing off (PT-9, the slow waker)")
}

func requireViolation(t *testing.T, cfg Config, what string) {
	t.Helper()
	n := seedBudget(t, 2000)
	for seed := 0; seed < n; seed++ {
		if res := Run(int64(seed), cfg); res.Violation != nil {
			t.Logf("%s: violation found at seed %d after %d steps:\n  %v\ntrace:\n%s",
				what, seed, res.Steps, res.Violation, traceDump(res))
			return
		}
	}
	t.Fatalf("%s: NO violation in %d seeds — the harness is not exercising this fence", what, n)
}

func traceDump(r Result) string {
	out := ""
	start := 0
	if len(r.Trace) > 50 {
		start = len(r.Trace) - 50
	}
	for i := start; i < len(r.Trace); i++ {
		out += "  " + r.Trace[i] + "\n"
	}
	return out
}

// TestDeterminism: same seed, same config → identical trace and outcome.
// Resumability and debuggability both hang off this property.
func TestDeterminism(t *testing.T) {
	cfg := Default()
	for seed := int64(0); seed < 50; seed++ {
		a, b := Run(seed, cfg), Run(seed, cfg)
		if len(a.Trace) != len(b.Trace) || a.Steps != b.Steps {
			t.Fatalf("seed %d: runs diverged (%d vs %d trace entries)", seed, len(a.Trace), len(b.Trace))
		}
		for i := range a.Trace {
			if a.Trace[i] != b.Trace[i] {
				t.Fatalf("seed %d: traces diverge at step %d: %q vs %q", seed, i, a.Trace[i], b.Trace[i])
			}
		}
	}
}
