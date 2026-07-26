package doorman

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMintForPythonVerifier is a bridge, not a gate: it emits a real token so
// the example apps' pure-Python verifier can be checked against Go's ed25519
// rather than assumed compatible. Run with KRILL_EMIT_TOKEN=<path>.
func TestMintForPythonVerifier(t *testing.T) {
	out := os.Getenv("KRILL_EMIT_TOKEN")
	if out == "" {
		t.Skip("set KRILL_EMIT_TOKEN=<path> to emit a token for the python verifier check")
	}
	k := NewTestKey()
	tok, err := k.Mint("https://auth.krill.run", "watchlist", Grant{
		Subject: "sub-123", Email: "friend@example.com", Name: "Friend", Plane: PlaneUse,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("%s\n%s\n%s\n", k.PublicB64(), tok, NewTestKey().PublicB64())
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
