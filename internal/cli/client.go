package cli

// apiClient is the CLI's thin HTTP client for the local control API. The
// base URL comes from TINYSHIP_API (default the loopback control
// address). Admin authentication uses a bearer token read from a system
// file (TINYSHIP_TOKEN_FILE overrides it for tests and staging); the API
// rejects requests without one, so the client sends it whenever present.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultAPIBase matches api.DefaultTCPAddr; duplicated here so the CLI
// does not import the server package (CLI and API are separate
// trust positions).
const DefaultAPIBase = "http://127.0.0.1:7878"

// DefaultTokenFile is where the admin token lives on a host managed by
// the installer. Mode 0640, owner root, group tinyship-admin: readable
// by the CLI's normal operators, not by other local users.
const DefaultTokenFile = "/etc/tinyship/token"

type apiClient struct {
	base string
	// token is the bearer credential read from tokenPath; empty when the
	// file is missing or unreadable (the API then rejects and guides).
	token     string
	tokenPath string
	// regular bounds ordinary calls; streams (logs --follow, deploy
	// --wait polls) use their own clients/contexts.
	regular *http.Client
	// stream has no timeout: log follow and long polls stay open.
	stream *http.Client
}

func newAPIClient() *apiClient {
	base := os.Getenv("TINYSHIP_API")
	if base == "" {
		base = DefaultAPIBase
	}
	tokenPath := os.Getenv("TINYSHIP_TOKEN_FILE")
	if tokenPath == "" {
		tokenPath = DefaultTokenFile
	}
	return &apiClient{
		base:      base,
		token:     readTokenFile(tokenPath),
		tokenPath: tokenPath,
		regular:   &http.Client{Timeout: 30 * time.Second},
		stream:    &http.Client{},
	}
}

// readTokenFile returns the trimmed token from path, or "" when the file
// cannot be read (the 401 the API returns then tells the operator how to
// re-issue a token).
func readTokenFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// errEnvelope is the API's uniform error shape.
type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiError renders a non-2xx response as "code: message" (
// state the cause; the API already puts the next step in the message).
func apiError(resp *http.Response) error {
	var env errEnvelope
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
}

func (c *apiClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// do performs a bounded request and returns the response; callers must
// close the body. Non-2xx responses are returned as-is for apiError.
func (c *apiClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return c.regular.Do(req)
}

// doJSON performs a request and decodes a 2xx JSON body into out.
func (c *apiClient) doJSON(ctx context.Context, method, path string, body []byte, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
