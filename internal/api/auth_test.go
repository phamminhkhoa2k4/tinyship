package api

// Authentication middleware tests: healthz stays open, every other route
// requires a valid admin bearer token, bad attempts are throttled per
// source, and the authenticated caller is recorded as the audit actor.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

// fastArgon lowers the token hash cost so an auth request does not dominate
// the test; production keeps the stronger default.
var fastArgon = store.ArgonParams{
	MemoryKiB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8,
}

func authServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir()+"/a.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.SetArgon2Params(fastArgon)
	t.Cleanup(func() { s.Close() })
	raw, err := s.CreateToken(context.Background(), "admin")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	mux := newMuxWith(s, t.TempDir(), slog.New(slog.DiscardHandler),
		func(context.Context, string) ([]net.IP, error) { return nil, nil }, s,
		DefaultTCPAddr)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s, raw
}

func authReq(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestAuthHealthzStaysOpen(t *testing.T) {
	srv, _, _ := authServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 (must stay open)", resp.StatusCode)
	}
}

func TestAuthRejectsWithoutToken(t *testing.T) {
	srv, _, _ := authServer(t)
	resp := authReq(t, srv.URL+"/apps", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "E_UNAUTHORIZED" {
		t.Errorf("code = %q, want E_UNAUTHORIZED", code)
	}
}

func TestAuthRejectsMalformedHeader(t *testing.T) {
	srv, _, _ := authServer(t)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/apps", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic xyz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("malformed auth status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthAcceptsValidToken(t *testing.T) {
	srv, _, raw := authServer(t)
	resp := authReq(t, srv.URL+"/apps", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200; body %s", resp.StatusCode, drain(resp))
	}
	if code := errCode(t, resp); code != "" {
		t.Errorf("valid request returned error code %q", code)
	}
}

func TestAuthRejectsWrongToken(t *testing.T) {
	srv, _, _ := authServer(t)
	resp := authReq(t, srv.URL+"/apps", "ts_totallywrong")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthRecordsAuthenticatedActor(t *testing.T) {
	srv, s, raw := authServer(t)
	// Create an app so the env write has a real target.
	post, err := http.NewRequest(http.MethodPost, srv.URL+"/apps", strings.NewReader(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Authorization", "Bearer "+raw)
	postResp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusCreated {
		t.Fatalf("create app status = %d, want 201", postResp.StatusCode)
	}

	// A mutating call with a valid token records the token name as actor.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/apps/web/env/KEY",
		strings.NewReader(`{"value":"v","kind":"plain"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("env set: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("env set status = %d, want 200", resp.StatusCode)
	}
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT actor FROM audit_events WHERE action = 'env.set'`)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no audit row written for env.set")
	}
	var actor string
	_ = rows.Scan(&actor)
	if actor != "admin" {
		t.Errorf("audit actor = %q, want the token name admin", actor)
	}
}

func TestAuthRateLimitsBadAttempts(t *testing.T) {
	srv, _, raw := authServer(t)
	// Fail enough times to cross the limiter threshold.
	for i := 0; i < newAuthLimiter().maxFails; i++ {
		resp := authReq(t, srv.URL+"/apps", "ts_wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// Even the correct token is throttled once the source is locked.
	resp := authReq(t, srv.URL+"/apps", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("post-lock status = %d, want 429", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "E_RATE_LIMITED" {
		t.Errorf("code = %q, want E_RATE_LIMITED", code)
	}
}

func TestAuthRateLimitAllowsValidBeforeThreshold(t *testing.T) {
	srv, _, raw := authServer(t)
	// A couple of misses still let the real admin through.
	for i := 0; i < newAuthLimiter().maxFails/2; i++ {
		resp := authReq(t, srv.URL+"/apps", "ts_wrong")
		resp.Body.Close()
	}
	resp := authReq(t, srv.URL+"/apps", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("valid token after a few misses: status = %d, want 200", resp.StatusCode)
	}
}
