package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreadableToken points TINYSHIP_TOKEN_FILE at a file the test cannot
// read — the state a fresh install leaves a user in before their next
// login.
func unreadableToken(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte("ts_test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tf, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINYSHIP_TOKEN_FILE", tf)
}

// When the API rejects a request that carried no bearer because the
// token file is unreadable, the operator gets the permission cause (a
// fresh login or sudo) instead of a misleading rotation hint.
func TestDoUnreadableTokenReportsLocalCauseOn401(t *testing.T) {
	unreadableToken(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"E_UNAUTHORIZED","message":"missing admin token"}}`))
	}))
	defer srv.Close()
	t.Setenv("TINYSHIP_API", srv.URL)
	c := newAPIClient()
	var out map[string]any
	err := c.doJSON(context.Background(), http.MethodGet, "/healthz", nil, &out)
	if !errors.Is(err, ErrTokenUnreadable) {
		t.Fatalf("want ErrTokenUnreadable, got %v", err)
	}
	for _, want := range []string{"permission denied", "log out and back in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q misses %q", err, want)
		}
	}
}

// The unreadable file never blocks a request the API accepts: the local
// cause only replaces a 401.
func TestDoUnreadableTokenDoesNotBlockAcceptedRequests(t *testing.T) {
	unreadableToken(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	t.Setenv("TINYSHIP_API", srv.URL)
	c := newAPIClient()
	resp, err := c.do(context.Background(), http.MethodGet, "/healthz", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
}

// A missing token file keeps the previous behavior: the request goes out
// with no bearer and the API's own guidance decides the message.
func TestDoMissingTokenKeepsAPI401Message(t *testing.T) {
	t.Setenv("TINYSHIP_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"E_UNAUTHORIZED","message":"missing admin token; run 'tinyship token rotate'"}}`))
	}))
	defer srv.Close()
	t.Setenv("TINYSHIP_API", srv.URL)
	c := newAPIClient()
	var out map[string]any
	err := c.doJSON(context.Background(), http.MethodGet, "/healthz", nil, &out)
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, ErrTokenUnreadable) {
		t.Fatalf("local cause must not fire for a missing file: %v", err)
	}
	if !strings.Contains(err.Error(), "E_UNAUTHORIZED") {
		t.Errorf("error %q misses the API code", err)
	}
}
