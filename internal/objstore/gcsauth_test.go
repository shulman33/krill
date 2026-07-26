package objstore

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSA is a generated service-account key plus the token endpoint that
// will trade its signed assertions for an access token — the whole
// server-to-server flow, offline.
type fakeSA struct {
	email     string
	credPath  string
	accessTok string

	mu        sync.Mutex
	exchanges int
}

func newFakeSA(t *testing.T) *fakeSA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	sa := &fakeSA{
		email:     "krill-fsn1@example.iam.gserviceaccount.com",
		accessTok: "ya29.fake-access-token",
	}
	// Captured by reference: the handler only runs after the assignment below.
	var tokenURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", got)
		}
		if err := verifyAssertion(r.PostForm.Get("assertion"), &key.PublicKey, sa.email, tokenURI); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			t.Errorf("assertion rejected: %v", err)
			return
		}
		sa.mu.Lock()
		sa.exchanges++
		sa.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600,"token_type":"Bearer"}`, sa.accessTok)
	}))
	t.Cleanup(srv.Close)
	tokenURI = srv.URL

	sa.credPath = filepath.Join(t.TempDir(), "sa.json")
	cred, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "ycombinator-test",
		"private_key_id": "kid-1",
		"private_key":    string(keyPEM),
		"client_email":   sa.email,
		"token_uri":      srv.URL,
	})
	if err := os.WriteFile(sa.credPath, cred, 0o600); err != nil {
		t.Fatal(err)
	}
	return sa
}

func verifyAssertion(assertion string, pub *rsa.PublicKey, wantIss, wantAud string) error {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return fmt.Errorf("assertion has %d parts, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("signature not base64url: %w", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("bad RS256 signature: %w", err)
	}
	var hdr struct{ Alg, Typ, Kid string }
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return err
	}
	if hdr.Alg != "RS256" || hdr.Kid != "kid-1" {
		return fmt.Errorf("header alg=%q kid=%q", hdr.Alg, hdr.Kid)
	}
	var cl struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Aud   string `json:"aud"`
		Iat   int64  `json:"iat"`
		Exp   int64  `json:"exp"`
	}
	raw, _ = base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(raw, &cl); err != nil {
		return err
	}
	if cl.Iss != wantIss {
		return fmt.Errorf("iss = %q, want %q", cl.Iss, wantIss)
	}
	if cl.Aud != wantAud {
		return fmt.Errorf("aud = %q, want the token endpoint %q", cl.Aud, wantAud)
	}
	if cl.Scope != gcsScope {
		return fmt.Errorf("scope = %q, want %q", cl.Scope, gcsScope)
	}
	now := time.Now().Unix()
	if cl.Iat > now+60 || cl.Exp <= now || cl.Exp > now+3601 {
		return fmt.Errorf("iat=%d exp=%d vs now=%d", cl.Iat, cl.Exp, now)
	}
	return nil
}

// TestGCSServiceAccountAuth is the production path on non-GCP hardware: a
// JSON key on disk, no metadata server, no gcloud.
func TestGCSServiceAccountAuth(t *testing.T) {
	sa := newFakeSA(t)
	g, _ := newFakeGCSStore(t, sa.accessTok)
	g.CredentialsFile = sa.credPath
	ctx := context.Background()

	if err := g.Put(ctx, "a/b", []byte("hi")); err != nil {
		t.Fatalf("Put with service-account auth: %v", err)
	}
	got, _, err := g.Get(ctx, "a/b")
	if err != nil || string(got) != "hi" {
		t.Fatalf("Get: %q %v", got, err)
	}
	// A token good for an hour must be reused, not re-minted per request.
	if _, err := g.List(ctx, "a/"); err != nil {
		t.Fatalf("List: %v", err)
	}
	sa.mu.Lock()
	n := sa.exchanges
	sa.mu.Unlock()
	if n != 1 {
		t.Errorf("token exchanges = %d, want 1 (token not cached)", n)
	}
	if via := g.AuthVia(); !strings.Contains(via, sa.email) {
		t.Errorf("AuthVia() = %q, want it to name the service account", via)
	}
	if d := g.Describe(); !strings.HasPrefix(d, "gs://bkt/pre/fix") {
		t.Errorf("Describe() = %q", d)
	}
}

// The env-var path is how the systemd unit actually configures this.
func TestGCSCredentialsFromEnv(t *testing.T) {
	sa := newFakeSA(t)
	for _, env := range []string{"KRILL_GCS_CREDENTIALS", "GOOGLE_APPLICATION_CREDENTIALS"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("KRILL_GCS_TOKEN", "")
			t.Setenv("KRILL_GCS_CREDENTIALS", "")
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
			t.Setenv(env, sa.credPath)
			g, _ := newFakeGCSStore(t, sa.accessTok)
			if err := g.Put(context.Background(), "x", []byte("y")); err != nil {
				t.Fatalf("Put: %v", err)
			}
		})
	}
}

// A configured-but-wrong credential must fail loudly rather than fall through
// to some other ambient identity with different powers.
func TestGCSCredentialsErrors(t *testing.T) {
	dir := t.TempDir()
	userCred := filepath.Join(dir, "user.json")
	os.WriteFile(userCred, []byte(`{"type":"authorized_user","client_id":"x"}`), 0o600)
	badPEM := filepath.Join(dir, "bad.json")
	os.WriteFile(badPEM, []byte(`{"type":"service_account","client_email":"a@b","private_key":"not-a-pem"}`), 0o600)

	for name, tc := range map[string]struct{ path, want string }{
		"missing file":    {filepath.Join(dir, "nope.json"), "reading credentials"},
		"authorized_user": {userCred, "need a service_account key"},
		"unparsable key":  {badPEM, "not PEM"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KRILL_GCS_TOKEN", "")
			g, _ := newFakeGCSStore(t, "irrelevant")
			g.CredentialsFile = tc.path
			err := g.Put(context.Background(), "a", []byte("b"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
