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
	"net/url"
	"os"
	"strings"
	"time"
)

// Service-account auth: the only credential form that works on a host that
// is not a GCE instance and has no gcloud installed — i.e. the production
// Hetzner box. A JSON key file gives us an RSA private key; we self-sign a
// JWT with it and trade the JWT for an access token (RFC 7523, the
// "jwt-bearer" grant Google documents for server-to-server auth). Stdlib
// only: crypto/rsa + encoding/pem are all the official SDK would use here.
//
// Scope is deliberately narrow — object read/write in the buckets the key's
// IAM binding allows. Bucket creation and IAM are operator work, never
// krilld's, so no storage-admin scope is ever requested.
const gcsScope = "https://www.googleapis.com/auth/devstorage.read_write"

const defaultTokenURI = "https://oauth2.googleapis.com/token"

// CredentialsPath returns the service-account JSON key path from the
// environment: KRILL_GCS_CREDENTIALS wins so a Krill-specific key can sit
// beside an unrelated GOOGLE_APPLICATION_CREDENTIALS on a shared box.
func CredentialsPath() string {
	for _, env := range []string{"KRILL_GCS_CREDENTIALS", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if p := os.Getenv(env); p != "" {
			return p
		}
	}
	return ""
}

type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`

	key *rsa.PrivateKey
}

func loadServiceAccount(path string) (*serviceAccount, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("objstore gcs: reading credentials %s: %w", path, err)
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("objstore gcs: parsing credentials %s: %w", path, err)
	}
	if sa.Type != "service_account" {
		// An "authorized_user" file here is the classic mistake: it works on a
		// laptop via gcloud and silently has no bearing on a daemon.
		return nil, fmt.Errorf("objstore gcs: credentials %s are type %q, need a service_account key", path, sa.Type)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("objstore gcs: credentials %s missing client_email or private_key", path)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokenURI
	}
	blk, _ := pem.Decode([]byte(sa.PrivateKey))
	if blk == nil {
		return nil, fmt.Errorf("objstore gcs: credentials %s: private_key is not PEM", path)
	}
	// Google issues PKCS#8; accept PKCS#1 too so a hand-rolled key works.
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("objstore gcs: credentials %s: private key is %T, need RSA", path, k)
		}
		sa.key = rk
	} else if rk, err2 := x509.ParsePKCS1PrivateKey(blk.Bytes); err2 == nil {
		sa.key = rk
	} else {
		return nil, fmt.Errorf("objstore gcs: credentials %s: unparsable private key: %w", path, err)
	}
	return &sa, nil
}

// assertion builds the signed JWT. exp is capped at one hour, the maximum
// Google accepts.
func (sa *serviceAccount) assertion(now time.Time) (string, error) {
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if sa.PrivateKeyID != "" {
		hdr["kid"] = sa.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   sa.ClientEmail,
		"scope": gcsScope,
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hj, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	cj, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hj) + "." + enc.EncodeToString(cj)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sa.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("objstore gcs: signing assertion: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// token exchanges the assertion for an access token and its lifetime.
func (sa *serviceAccount) token(ctx context.Context, c *http.Client, now time.Time) (string, time.Duration, error) {
	assertion, err := sa.assertion(now)
	if err != nil {
		return "", 0, err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("objstore gcs: token exchange for %s: %w", sa.ClientEmail, err)
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", 0, fmt.Errorf("objstore gcs: token exchange http %d: unparsable response: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		// The two failures worth naming: a key deleted in IAM, and clock skew
		// large enough that "iat" lands in the future ("invalid_grant").
		return "", 0, fmt.Errorf("objstore gcs: token exchange for %s: http %d: %s %s",
			sa.ClientEmail, resp.StatusCode, tr.Error, tr.ErrorDesc)
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return tr.AccessToken, ttl, nil
}
