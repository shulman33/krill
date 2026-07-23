// Package firecracker drives one Firecracker microVM per Machine through the
// VMM's HTTP API on a unix socket. It is a direct port of the call sequence
// proven by wake-bench/lib.sh — no SDK, no abstraction over calls we don't
// make.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client is a minimal JSON-over-unix-socket client for the Firecracker API.
type Client struct {
	http *http.Client
}

func NewClient(sockPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
			// Individual API calls are fast; snapshot/create writes the whole
			// guest RAM to disk and gets the long end of this.
			Timeout: 60 * time.Second,
		},
	}
}

// call issues one API request and fails on any non-2xx response, echoing the
// body — Firecracker's error strings are the only diagnostics there are.
func (c *Client) call(ctx context.Context, method, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fc %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fc %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return nil
}

func (c *Client) put(ctx context.Context, path string, body any) error {
	return c.call(ctx, http.MethodPut, path, body)
}

func (c *Client) patch(ctx context.Context, path string, body any) error {
	return c.call(ctx, http.MethodPatch, path, body)
}
