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
	"errors"
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

// ErrTokenUnreadable marks a token file that exists but cannot be read,
// so callers report the local permission cause instead of blaming the
// API for a request that never carried a credential.
var ErrTokenUnreadable = errors.New("cannot read the admin token")

type apiClient struct {
	base string
	// token is the bearer credential read from tokenPath; empty when the
	// file is missing (the API then rejects and guides).
	token string
	// tokenErr is non-nil only when the file exists but cannot be read;
	// requests then fail with that cause rather than an empty bearer.
	tokenErr  error
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
	tok, terr := readTokenFile(tokenPath)
	return &apiClient{
		base:      base,
		token:     tok,
		tokenErr:  terr,
		tokenPath: tokenPath,
		regular:   &http.Client{Timeout: 30 * time.Second},
		stream:    &http.Client{},
	}
}

// readTokenFile returns the trimmed token from path. A missing file is
// not an error (the 401 the API returns then tells the operator how to
// issue one); a file that exists but cannot be read — the installer's
// admin group only reaches a fresh login — is returned as an error so
// the real cause reaches the operator instead of a generic 401.
func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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

// unauthorizedCause returns the local token-read error when the API
// rejected a request that carried no bearer because the token file is
// unreadable: the operator needs the permission cause (a fresh login or
// sudo), not a generic E_UNAUTHORIZED suggesting a rotation.
func (c *apiClient) unauthorizedCause(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized && c.tokenErr != nil {
		return fmt.Errorf("%w: %v — log out and back in once after install, or run this command with sudo",
			ErrTokenUnreadable, c.tokenErr)
	}
	return nil
}

// do performs a bounded request and returns the response; callers must
// close the body. Non-2xx responses are returned as-is for apiError.
func (c *apiClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.regular.Do(req)
	if err != nil {
		return nil, err
	}
	if cause := c.unauthorizedCause(resp); cause != nil {
		resp.Body.Close()
		return nil, cause
	}
	return resp, nil
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
