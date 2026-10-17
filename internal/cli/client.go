package cli

// apiClient is the CLI's thin HTTP client for the local control API. The
// base URL comes from TINYSHIP_API (default the loopback control
// address); there is no auth yet, so the CLI relies on the API being
// bound to 127.0.0.1 only.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultAPIBase matches api.DefaultTCPAddr; duplicated here so the CLI
// does not import the server package (CLI and API are separate
// trust positions).
const DefaultAPIBase = "http://127.0.0.1:7878"

type apiClient struct {
	base string
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
	return &apiClient{
		base:    base,
		regular: &http.Client{Timeout: 30 * time.Second},
		stream:  &http.Client{},
	}
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
