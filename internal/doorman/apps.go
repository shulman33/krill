package doorman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrKrilldDown means the doorman could not establish whether an app exists.
// It is deliberately distinct from "no such app": one is a 503, the other a
// 404, and conflating them would let a krilld restart silently prune an ACL.
var ErrKrilldDown = errors.New("krilld is not answering")

// AppSource answers "is this a real app on this host". The doorman asks
// krilld rather than keeping its own copy of the catalog, because a stale
// copy is exactly how an unknown Host header ends up routing.
type AppSource interface {
	Exists(ctx context.Context, name string) (bool, error)
}

// KrilldApps is the production AppSource: krilld's loopback admin API with a
// short cache in front of it.
//
// The cache exists for one specific failure the design has to survive —
// krilld restarting under Restart=always while a browser is mid-request. A
// positive answer is good for PositiveTTL, so a restart that takes a couple
// of seconds is invisible. Negative answers are cached far more briefly, so
// a freshly deployed app becomes reachable almost immediately.
type KrilldApps struct {
	Admin       string
	HTTP        *http.Client
	PositiveTTL time.Duration
	NegativeTTL time.Duration

	mu    sync.Mutex
	cache map[string]appCacheEntry
}

type appCacheEntry struct {
	exists bool
	until  time.Time
}

func NewKrilldApps(admin string) *KrilldApps {
	return &KrilldApps{
		Admin:       admin,
		HTTP:        &http.Client{Timeout: 5 * time.Second},
		PositiveTTL: 30 * time.Second,
		NegativeTTL: 3 * time.Second,
		cache:       map[string]appCacheEntry{},
	}
}

func (k *KrilldApps) Exists(ctx context.Context, name string) (bool, error) {
	now := time.Now()
	k.mu.Lock()
	if e, ok := k.cache[name]; ok && now.Before(e.until) {
		k.mu.Unlock()
		return e.exists, nil
	}
	k.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.Admin+"/v1/apps/"+name, nil)
	if err != nil {
		return false, err
	}
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrKrilldDown, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	var exists bool
	var ttl time.Duration
	switch {
	case resp.StatusCode == http.StatusOK:
		exists, ttl = true, k.PositiveTTL
	case resp.StatusCode == http.StatusNotFound:
		exists, ttl = false, k.NegativeTTL
	default:
		// A 500 from krilld is not evidence of anything about this app.
		return false, fmt.Errorf("%w: admin API returned %s", ErrKrilldDown, resp.Status)
	}
	k.mu.Lock()
	k.cache[name] = appCacheEntry{exists: exists, until: now.Add(ttl)}
	k.mu.Unlock()
	return exists, nil
}

// Forget drops a cached answer — called after the doorman itself changes
// something (a deploy through the edit plane) so the next lookup is fresh.
func (k *KrilldApps) Forget(name string) {
	k.mu.Lock()
	delete(k.cache, name)
	k.mu.Unlock()
}

// StaticApps is an AppSource over a fixed set, for tests and for running the
// doorman without krilld.
type StaticApps map[string]bool

func (s StaticApps) Exists(_ context.Context, name string) (bool, error) {
	return s[name], nil
}

// appStatus is the slice of krilld's status payload the doorman surfaces on
// the data plane.
type appStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// krilldGet proxies a GET to krilld's admin API and returns the raw body.
// Used by the data share plane, which is a read-only window onto endpoints
// the operator already has.
func (k *KrilldApps) krilldGet(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.Admin+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrKrilldDown, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return body, resp.StatusCode, err
}

// Status returns an app's lifecycle state, for the data plane's status view.
func (k *KrilldApps) Status(ctx context.Context, name string) (appStatus, error) {
	var st appStatus
	body, code, err := k.krilldGet(ctx, "/v1/apps/"+name)
	if err != nil {
		return st, err
	}
	if code != http.StatusOK {
		return st, fmt.Errorf("krilld returned %d for %s", code, name)
	}
	return st, json.Unmarshal(body, &st)
}
