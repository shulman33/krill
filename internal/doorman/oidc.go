package doorman

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Identity is what a completed sign-in establishes. Nothing more is asked
// for and nothing more is stored.
type Identity struct {
	// Subject is Google's "sub": stable for the life of the account. The ACL
	// is keyed on it because an email address can be changed or reassigned.
	Subject string
	Email   string
	Name    string
}

// Authenticator is the OAuth/OIDC round trip. It is an interface so the gate
// tests can drive the entire doorman — claim, session, revoke, replay —
// against a fake provider, without a browser or a Google client.
type Authenticator interface {
	// AuthCodeURL is where the browser is sent. state and nonce are
	// single-use values the caller has already persisted.
	AuthCodeURL(state, nonce string) string
	// Exchange trades the callback's code for a verified identity, checking
	// the nonce binding.
	Exchange(ctx context.Context, code, nonce string) (Identity, error)
}

// GoogleAuth is the bought half of the doorman (decision #8): the OIDC flow,
// Google's discovery document, and JWKS-backed ID-token verification all come
// from x/oauth2 + coreos/go-oidc. Auth plumbing is the highest
// security-bug-density code there is and none of it is differentiated.
type GoogleAuth struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	cfg      oauth2.Config
}

// GoogleIssuer is the discovery root. Google's ID tokens carry either this or
// the bare "accounts.google.com"; go-oidc handles that quirk itself.
const GoogleIssuer = "https://accounts.google.com"

// NewGoogleAuth builds the flow. redirectURL must be the single URI
// registered on the OAuth client — one fixed host, never per-app, because
// Google matches redirect URIs exactly and registering a wildcard of app
// subdomains is not possible.
//
// Scopes are openid/email/profile and nothing else, deliberately: those are
// non-sensitive, so the client needs no Google verification review, and F4
// says the friend must succeed "without a security warning of any kind."
func NewGoogleAuth(ctx context.Context, clientID, clientSecret, redirectURL string) (*GoogleAuth, error) {
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("doorman: --google-client-id and --google-client-secret are required")
	}
	provider, err := oidc.NewProvider(ctx, GoogleIssuer)
	if err != nil {
		return nil, fmt.Errorf("doorman: OIDC discovery at %s: %w", GoogleIssuer, err)
	}
	return &GoogleAuth{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		cfg: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
	}, nil
}

func (g *GoogleAuth) AuthCodeURL(state, nonce string) string {
	return g.cfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		// prompt=select_account: on a shared phone the friend must be able to
		// pick which Google account is claiming the link, rather than being
		// silently signed in as whoever last used the browser.
		oauth2.SetAuthURLParam("prompt", "select_account"))
}

func (g *GoogleAuth) Exchange(ctx context.Context, code, nonce string) (Identity, error) {
	tok, err := g.cfg.Exchange(ctx, code)
	if err != nil {
		return Identity{}, fmt.Errorf("code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, errors.New("no id_token in the token response")
	}
	idTok, err := g.verifier.Verify(ctx, rawID)
	if err != nil {
		return Identity{}, fmt.Errorf("verifying id_token: %w", err)
	}
	if idTok.Nonce != nonce {
		// The nonce ties this ID token to the authorization request this
		// browser started. Without the check, an ID token obtained elsewhere
		// could be injected into someone else's callback.
		return Identity{}, errors.New("id_token nonce does not match this sign-in")
	}
	var claims struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return Identity{}, err
	}
	if claims.Sub == "" {
		return Identity{}, errors.New("id_token has no subject")
	}
	if claims.Email != "" && !claims.EmailVerified {
		// An unverified address must never appear as X-App-User: the whole
		// point of F1 is that the app can believe who it is talking to.
		return Identity{}, fmt.Errorf("google reports %q as unverified", claims.Email)
	}
	return Identity{Subject: claims.Sub, Email: claims.Email, Name: claims.Name}, nil
}
