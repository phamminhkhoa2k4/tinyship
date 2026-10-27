package caddy

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

// ServerPath is the admin API path of the `tinyship` HTTP server object.
// The worker replaces only this subtree; the base config written by the
// installer stays untouched.
const ServerPath = "/config/apps/http/servers/tinyship"

// maxApplyAttempts bounds the Etag conflict loop: another writer changed
// the config underneath us; regenerate a few times before giving up.
const maxApplyAttempts = 3

// DefaultSocket is the Caddy admin socket on an installed host.
// TINYSHIP_CADDY_ADMIN overrides it for development and tests.
const DefaultSocket = "/run/caddy/admin.sock"

// adminHost is the Host header used on the unix socket. The installer
// allows it in the admin origins list because Caddy 2.6 rejects an
// admin request whose Host is not explicitly allowed.
const adminHost = "caddy"

// Client talks to the Caddy admin API over its Unix socket. It can
// read the current server config, replace it atomically with Etag
// concurrency control, and report the applied config's hash.
type Client struct {
	socket string
	httpc  *http.Client
}

// NewClient builds a client for the admin socket at path.
func NewClient(path string) *Client {
	return &Client{
		socket: path,
		httpc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				},
			},
			// Admin calls are small and local; a stuck Caddy must not
			// wedge a deploy past its step budget.
			Timeout: 10 * time.Second,
		},
	}
}

// CurrentConfig fetches the live server config and its Etag. A missing
// subtree returns (nil, "") so callers can treat first apply as a create.
func (c *Client) CurrentConfig(ctx context.Context) (map[string]any, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+adminHost+ServerPath, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, "", &UnreachableError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("caddy: get %s: unexpected status %d", ServerPath, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", fmt.Errorf("caddy: read %s: %w", ServerPath, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, "", fmt.Errorf("caddy: decode current config: %w", err)
	}
	// Caddy 2.6 sends the ETag as an HTTP trailer, so it is populated
	// only after the body has been consumed. Newer versions send it as
	// a normal header; accept either location.
	etag := resp.Header.Get("Etag")
	if etag == "" {
		etag = resp.Trailer.Get("Etag")
	}
	return cfg, etag, nil
}

// ApplyServer atomically replaces the server subtree with the generated
// config. It follows the Etag dance: read the current Etag, POST the
// replacement with If-Match, retry on 412, then verify the live config
// hashes to the desired one. Any mismatch is an E_CADDY_APPLY error and
// leaves the previously active config serving.
func (c *Client) ApplyServer(ctx context.Context, server map[string]any) (string, error) {
	want, err := json.Marshal(server)
	if err != nil {
		return "", fmt.Errorf("caddy: encode server config: %w", err)
	}
	wantHash, err := Hash(server)
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 0; attempt < maxApplyAttempts; attempt++ {
		_, etag, err := c.CurrentConfig(ctx)
		if err != nil {
			return "", err
		}
		status, err := c.replaceServer(ctx, etag, want)
		if err != nil {
			return "", err
		}
		switch status {
		case http.StatusPreconditionFailed:
			// Someone else changed the config; regenerate the Etag and
			// retry with the same desired content.
			lastErr = fmt.Errorf("caddy: config changed concurrently (attempt %d)", attempt+1)
			continue
		case http.StatusOK, http.StatusCreated:
			// Applied; fall through to verification below.
		default:
			return "", fmt.Errorf("caddy: post %s: unexpected status %d", ServerPath, status)
		}

		got, _, err := c.CurrentConfig(ctx)
		if err != nil {
			return "", err
		}
		gotRaw, err := json.Marshal(got)
		if err != nil {
			return "", fmt.Errorf("caddy: re-encode applied config: %w", err)
		}
		if !bytes.Equal(gotRaw, want) {
			gotHash, _ := Hash(got)
			return "", fmt.Errorf("E_CADDY_APPLY: applied config hash %s does not match "+
				"desired %s; the live config may have been changed by another writer",
				gotHash, wantHash)
		}
		return wantHash, nil
	}
	return "", fmt.Errorf("E_CADDY_APPLY: gave up after %d attempts: %v",
		maxApplyAttempts, lastErr)
}

// UnreachableError marks a failure to reach the admin socket at all
// (Caddy absent, socket missing, transport error). Callers may retry it;
// a rejected or mismatched config is never wrapped in it.
type UnreachableError struct{ Err error }

func (e *UnreachableError) Error() string {
	return "caddy: admin socket unreachable: " + e.Err.Error()
}
func (e *UnreachableError) Unwrap() error { return e.Err }

// replaceServer sends the replacement. POST is the admin API's replace
// verb: PUT is create-only and answers 409 once the subtree exists, so
// an atomic update of a live config must POST. If-Match enforces
// optimistic concurrency (absent Etag means unconditional replace).
func (c *Client) replaceServer(ctx context.Context, etag string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+adminHost+ServerPath, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, &UnreachableError{err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}()
	return resp.StatusCode, nil
}
