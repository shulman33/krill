package doorman

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TokenTTL is how long a minted identity token is good for (decision #10c).
//
// Five minutes, and minted per request — the opposite end of the scale from
// the 30-day session, for the opposite reason. The session's counterparty is
// a browser the doorman can cut off at any moment (F2). The token's
// counterparty is the GUEST, which runs agent-written code the platform does
// not trust; a leaked token must therefore die on its own, quickly. Signing
// is ~50 µs of ed25519, so caching one would be optimizing the wrong thing.
const TokenTTL = 5 * time.Minute

// IdentityKey signs the per-request tokens guests use to believe X-App-User.
//
// Ed25519, not RSA, for one practical reason beyond speed: the public key is
// 32 bytes, which base64s to 44 characters and therefore fits in the guest's
// KERNEL COMMAND LINE. That is how a guest with no outbound network at all
// (F6: apps stay silent) can still verify a token offline — krilld passes
// krill_idkey=<b64> at boot exactly the way it already passes the network
// contract, and the generated init exports it. No JWKS fetch, no egress hole
// cut through the baseline to reach the doorman.
type IdentityKey struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	// KeyID lets a future rotation exist without a format change. Guests may
	// ignore it; the JWKS endpoint publishes it.
	KeyID string
}

// LoadOrCreateKey reads the signing key from dir, generating one on first
// start. The private key is 0600 and never leaves the doorman's user.
func LoadOrCreateKey(dir string) (*IdentityKey, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	privPath := filepath.Join(dir, "identity.key")
	raw, err := os.ReadFile(privPath)
	if err == nil {
		seed, decErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("doorman: %s is not a valid ed25519 seed", privPath)
		}
		return newKey(ed25519.NewKeyFromSeed(seed)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := os.WriteFile(privPath, []byte(base64.RawURLEncoding.EncodeToString(seed)), 0o600); err != nil {
		return nil, err
	}
	k := newKey(ed25519.NewKeyFromSeed(seed))
	// The public half is written world-readable on purpose: krilld (a
	// different user) reads it to bake krill_idkey= into every guest's boot
	// args. It is a public key; publishing it is its job.
	if err := os.WriteFile(filepath.Join(dir, "identity.pub"), []byte(k.PublicB64()+"\n"), 0o644); err != nil {
		return nil, err
	}
	return k, nil
}

func newKey(priv ed25519.PrivateKey) *IdentityKey {
	pub := priv.Public().(ed25519.PublicKey)
	sum := base64.RawURLEncoding.EncodeToString(pub)
	return &IdentityKey{priv: priv, pub: pub, KeyID: sum[:8]}
}

// NewTestKey builds an in-memory key. Exported for the gate tooling and tests.
func NewTestKey() *IdentityKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return newKey(priv)
}

// PublicB64 is the raw 32-byte public key, base64url, no padding — the exact
// string that travels on the guest kernel command line.
func (k *IdentityKey) PublicB64() string {
	return base64.RawURLEncoding.EncodeToString(k.pub)
}

// Claims is what a guest gets to believe about its caller.
type Claims struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	// Audience is EXACTLY ONE app name. F3 step 3 replays a token against a
	// second app and requires rejection; a token that named several apps
	// could not fail that test, so the field is a string, not a list.
	Audience string `json:"aud"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Plane    string `json:"plane"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	TokenID  string `json:"jti"`
}

// Mint signs a token for one identity on one app.
func (k *IdentityKey) Mint(issuer, app string, g Grant, now time.Time) (string, error) {
	c := Claims{
		Issuer: issuer, Subject: g.Subject, Audience: app,
		Email: g.Email, Name: g.Name, Plane: string(g.Plane),
		IssuedAt: now.Unix(), Expires: now.Add(TokenTTL).Unix(), TokenID: shortID(),
	}
	hdr, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": k.KeyID})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(body)
	sig := ed25519.Sign(k.priv, []byte(signing))
	return signing + "." + enc.EncodeToString(sig), nil
}

// JWKS publishes the public key in the standard shape, for tooling that
// prefers a fetch over a kernel argument. Guests do not need it — see the
// IdentityKey comment — but the gate scripts and any future dashboard do.
func (k *IdentityKey) JWKS() []byte {
	doc := map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA",
		"kid": k.KeyID, "x": k.PublicB64(),
	}}}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return out
}

// VerifyToken is the check every guest must perform, written once here so the
// gate tooling and the example apps agree on what "verified" means.
//
// The three clauses are not optional and each maps to a gate: the signature
// (F1 — "arriving as a token the app can verify, not a bare header"), the
// audience (F3 step 3 — one app), and the expiry (F3's replay window).
func VerifyToken(publicB64, token, wantAudience string, now time.Time) (Claims, error) {
	pubRaw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(publicB64))
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		return Claims{}, errors.New("bad public key")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed token")
	}
	// Pin the algorithm from the key type rather than trusting the header.
	// Reading "alg" out of an attacker-supplied header is the oldest JWT bug
	// there is; here the verifier knows it holds an ed25519 key and will only
	// ever run ed25519.Verify.
	var hdr struct {
		Alg string `json:"alg"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "EdDSA" {
		return Claims{}, errors.New("unsupported token algorithm")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, errors.New("malformed signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(pubRaw), []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, errors.New("bad signature")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("malformed claims")
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return Claims{}, err
	}
	if wantAudience != "" && c.Audience != wantAudience {
		return Claims{}, fmt.Errorf("token audience %q is not %q", c.Audience, wantAudience)
	}
	if now.Unix() >= c.Expires {
		return Claims{}, errors.New("token expired")
	}
	return c, nil
}
