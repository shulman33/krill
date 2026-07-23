package objstore

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GCS is the real-object-store backend, via the XML API with plain net/http:
// conditional PUTs (x-goog-if-generation-match) give the manifest CAS that
// E4 requires, and skipping the official SDK keeps krilld's dependency
// closure tiny. Auth resolves, in order: KRILL_GCS_TOKEN (static, tests),
// the GCE metadata server (production on GCP), `gcloud auth
// print-access-token` (the dev-box pattern from the ROADMAP's bq gotcha).
type GCS struct {
	Bucket string
	Prefix string
	// Endpoint overrides https://storage.googleapis.com in tests.
	Endpoint string
	// HTTPClient defaults to a 30s-timeout client.
	HTTPClient *http.Client
	// TokenFunc overrides auth resolution in tests.
	TokenFunc func(ctx context.Context) (string, error)

	mu       sync.Mutex
	tok      string
	tokUntil time.Time
}

func NewGCS(bucket, prefix string) *GCS { return &GCS{Bucket: bucket, Prefix: prefix} }

func (s *GCS) objURL(key string) string {
	ep := s.Endpoint
	if ep == "" {
		ep = "https://storage.googleapis.com"
	}
	name := key
	if s.Prefix != "" {
		name = strings.TrimSuffix(s.Prefix, "/") + "/" + key
	}
	segs := strings.Split(name, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return ep + "/" + s.Bucket + "/" + strings.Join(segs, "/")
}

func (s *GCS) do(ctx context.Context, method, u string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	tok, err := s.token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := s.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return c.Do(req)
}

func (s *GCS) Get(ctx context.Context, key string) ([]byte, int64, error) {
	if err := validKey(key); err != nil {
		return nil, 0, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.objURL(key), nil, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, 0, ErrNotFound
	default:
		return nil, 0, gcsErr("GET", key, resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	gen, _ := strconv.ParseInt(resp.Header.Get("x-goog-generation"), 10, 64)
	return data, gen, nil
}

func (s *GCS) Put(ctx context.Context, key string, data []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodPut, s.objURL(key), data, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return gcsErr("PUT", key, resp)
	}
	return nil
}

func (s *GCS) PutCAS(ctx context.Context, key string, data []byte, expect int64) (int64, error) {
	if err := validKey(key); err != nil {
		return 0, err
	}
	resp, err := s.do(ctx, http.MethodPut, s.objURL(key), data, map[string]string{
		// expect 0 = "object must not exist": exactly GCS's semantics too.
		"x-goog-if-generation-match": strconv.FormatInt(expect, 10),
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		gen, _ := strconv.ParseInt(resp.Header.Get("x-goog-generation"), 10, 64)
		return gen, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return 0, fmt.Errorf("%w: key %s (http %d)", ErrCASConflict, key, resp.StatusCode)
	default:
		return 0, gcsErr("PUT(cas)", key, resp)
	}
}

func (s *GCS) List(ctx context.Context, prefix string) ([]string, error) {
	ep := s.Endpoint
	if ep == "" {
		ep = "https://storage.googleapis.com"
	}
	full := prefix
	if s.Prefix != "" {
		full = strings.TrimSuffix(s.Prefix, "/") + "/" + prefix
	}
	var keys []string
	marker := ""
	for {
		u := ep + "/" + s.Bucket + "?prefix=" + url.QueryEscape(full)
		if marker != "" {
			u += "&marker=" + url.QueryEscape(marker)
		}
		resp, err := s.do(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return nil, gcsErr("LIST", full, resp)
		}
		var lr struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			NextMarker  string `xml:"NextMarker"`
			IsTruncated bool   `xml:"IsTruncated"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&lr)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("objstore gcs: parsing list response: %w", err)
		}
		strip := ""
		if s.Prefix != "" {
			strip = strings.TrimSuffix(s.Prefix, "/") + "/"
		}
		for _, c := range lr.Contents {
			keys = append(keys, strings.TrimPrefix(c.Key, strip))
			marker = c.Key
		}
		if !lr.IsTruncated {
			return keys, nil
		}
		if lr.NextMarker != "" {
			marker = lr.NextMarker
		}
	}
}

func (s *GCS) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.objURL(key), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return gcsErr("DELETE", key, resp)
	}
	return nil
}

func gcsErr(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("objstore gcs: %s %s: http %d: %s", op, key, resp.StatusCode, strings.TrimSpace(string(body)))
}

// token caches whichever auth source works until shortly before expiry.
func (s *GCS) token(ctx context.Context) (string, error) {
	if s.TokenFunc != nil {
		return s.TokenFunc(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && time.Now().Before(s.tokUntil) {
		return s.tok, nil
	}
	if t := os.Getenv("KRILL_GCS_TOKEN"); t != "" {
		s.tok, s.tokUntil = t, time.Now().Add(time.Hour)
		return t, nil
	}
	if t, ttl, err := metadataToken(ctx); err == nil {
		s.tok, s.tokUntil = t, time.Now().Add(ttl-time.Minute)
		return t, nil
	}
	out, err := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token").Output()
	if err != nil {
		return "", fmt.Errorf("objstore gcs: no auth (set KRILL_GCS_TOKEN, run on GCE, or install gcloud): %w", err)
	}
	s.tok, s.tokUntil = strings.TrimSpace(string(out)), time.Now().Add(30*time.Minute)
	return s.tok, nil
}

func metadataToken(ctx context.Context) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("metadata server: http %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", 0, err
	}
	return tr.AccessToken, time.Duration(tr.ExpiresIn) * time.Second, nil
}
