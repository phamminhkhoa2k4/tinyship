package api

// Listen gating tests: the API serves on loopback by default, refuses
// a non-loopback address without the explicit opt-in, and reports its
// TCP listen address on /healthz so the CLI can surface a warning.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7878":    true,
		"localhost:7878":    true,
		"[::1]:7878":        true,
		"127.0.0.53:1":      true,
		"0.0.0.0:7878":      false,
		":7878":             false,
		"[::]:7878":         false,
		"192.168.1.10:7878": false,
		"example.com:7878":  false,
		"garbage":           false,
		"":                  false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestRunRefusesNonLoopbackWithoutAllowRemote(t *testing.T) {
	t.Setenv("TINYSHIP_DB", filepath.Join(t.TempDir(), "tinyship.db"))
	t.Setenv("TINYSHIP_RUN_DIR", t.TempDir())
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.10:0"} {
		if code := Run(context.Background(), []string{"--listen", addr}); code != 2 {
			t.Errorf("Run(--listen %s) = %d, want 2 (refused without --allow-remote)", addr, code)
		}
	}
}

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	t.Setenv("TINYSHIP_DB", filepath.Join(t.TempDir(), "tinyship.db"))
	if code := Run(context.Background(), []string{"bogus"}); code != 2 {
		t.Errorf("Run(bogus) = %d, want 2", code)
	}
}

// freePort reserves and releases a port so a test can predict Run's bind.
func freePort(t *testing.T, host string) int {
	t.Helper()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// runServing starts Run in the background on addr, waits until /healthz
// answers, then cancels and returns Run's exit code.
func runServing(t *testing.T, addr string, args ...string) (map[string]any, int) {
	t.Helper()
	t.Setenv("TINYSHIP_DB", filepath.Join(t.TempDir(), "tinyship.db"))
	t.Setenv("TINYSHIP_RUN_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args) }()

	host, port, _ := net.SplitHostPort(addr)
	probeHost := host
	if probeHost == "0.0.0.0" || probeHost == "" || probeHost == "::" {
		probeHost = "127.0.0.1"
	}
	probe := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(probeHost, port))
	var body map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(probe)
		if err == nil {
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if body["status"] != "ok" {
		cancel()
		t.Fatalf("server never became healthy at %s", probe)
	}
	cancel()
	select {
	case code := <-done:
		return body, code
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down after cancel")
		return nil, -1
	}
}

func TestRunServesLoopbackByDefaultFlags(t *testing.T) {
	port := freePort(t, "127.0.0.1")
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	body, code := runServing(t, addr, "--listen", addr)
	if code != 0 {
		t.Errorf("Run = %d, want 0 (clean shutdown)", code)
	}
	if body["tcp_listen"] != addr {
		t.Errorf("healthz tcp_listen = %v, want %s", body["tcp_listen"], addr)
	}
}

func TestRunAllowRemoteBindsNonLoopback(t *testing.T) {
	port := freePort(t, "0.0.0.0")
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	body, code := runServing(t, addr, "--listen", addr, "--allow-remote")
	if code != 0 {
		t.Errorf("Run = %d, want 0 (clean shutdown)", code)
	}
	if body["tcp_listen"] != addr {
		t.Errorf("healthz tcp_listen = %v, want %s", body["tcp_listen"], addr)
	}
}

func TestHealthzReportsCustomTCPListen(t *testing.T) {
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tinyship.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	mux := newMuxWith(s, t.TempDir(), slog.New(slog.DiscardHandler),
		func(context.Context, string) ([]net.IP, error) { return nil, nil }, nil,
		"0.0.0.0:7878")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body := decodeBody(t, resp)
	if body["tcp_listen"] != "0.0.0.0:7878" {
		t.Errorf("tcp_listen = %v, want 0.0.0.0:7878", body["tcp_listen"])
	}
}
