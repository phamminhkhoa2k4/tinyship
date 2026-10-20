package worker

// Log server tests (API→worker read-only socket; ownership;
// refusals are audited).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// unixClient talks HTTP over the worker's Unix socket.
func unixClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// startLogServer brings up the log server on a temp run dir and returns a
// client plus a cleanup func.
func startLogServer(t *testing.T, f *deployFixture) (*http.Client, string, func()) {
	t.Helper()
	ls, err := NewLogServer(f.s, f.fk, t.TempDir(), "test-worker",
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewLogServer: %v", err)
	}
	ls.Stats = NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	ls.GC = NewGCRunner(f.s, f.fk, "test-worker", slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := ls.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	// Wait for the socket to accept connections.
	client := unixClient(ls.SockPath)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := client.Get("http://unix/healthz"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("log socket never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cleanup := func() {
		cancel()
		ls.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("log server did not stop")
		}
	}
	return client, ls.SockPath, cleanup
}

func logURL(containerID, appID, query string) string {
	return "http://unix/logs?app_id=" + appID + "&container_id=" + containerID + query
}

func TestLogServerStreamsOwnedContainerLogs(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	d := f.deployment()
	client, _, cleanup := startLogServer(t, f)
	defer cleanup()

	resp, err := client.Get(logURL(*d.ContainerID, f.app.ID, ""))
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s, want 200", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ready to serve") {
		t.Errorf("body = %q, want container log lines", body)
	}
	// Default tail is applied when unspecified.
	if f.fk.logTail != DefaultLogTail {
		t.Errorf("tail sent to docker = %d, want default %d", f.fk.logTail, DefaultLogTail)
	}
}

func TestLogServerPassesTailSinceAndFollowThrough(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	d := f.deployment()
	client, _, cleanup := startLogServer(t, f)
	defer cleanup()

	resp, err := client.Get(logURL(*d.ContainerID, f.app.ID, "&tail=5&since=15m&follow=1"))
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if f.fk.logTail != 5 || f.fk.logSince != 15*time.Minute || !f.fk.logFollow {
		t.Errorf("docker saw tail=%d since=%v follow=%v, want 5/15m/true",
			f.fk.logTail, f.fk.logSince, f.fk.logFollow)
	}

	// tail=0 must reach docker as 0 (the wrapper maps ≤0 to "all").
	resp, err = client.Get(logURL(*d.ContainerID, f.app.ID, "&tail=0"))
	if err != nil {
		t.Fatalf("GET logs tail=0: %v", err)
	}
	resp.Body.Close()
	if f.fk.logTail != 0 {
		t.Errorf("tail=0 reached docker as %d", f.fk.logTail)
	}
}

func TestLogServerRefusesForeignContainersAndAudits(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	client, _, cleanup := startLogServer(t, f)
	defer cleanup()

	cases := []struct {
		name        string
		containerID string
		appID       string
	}{
		{"wrong app id", *f.deployment().ContainerID, "app-of-someone-else"},
		{"unmanaged container", "foreign-1", f.app.ID},
		{"missing container", "nope-404", f.app.ID},
	}
	// A foreign container: real but carrying someone else's app label.
	f.fk.addContainer("foreign-1", "not-tinyship", "running", map[string]string{
		"com.example.tool": "true", // deliberately no tinyship labels
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Get(logURL(tc.containerID, tc.appID, ""))
			if err != nil {
				t.Fatalf("GET logs: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, body = %s, want 403", resp.StatusCode, body)
			}
			var payload struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if payload.Error.Code != "E_NOT_MANAGED" {
				t.Errorf("code = %q, want E_NOT_MANAGED", payload.Error.Code)
			}
			// refusal audited with target container.
			n, err := f.s.CountAuditEvents(f.ctx, "logs.read", tc.containerID)
			if err != nil {
				t.Fatalf("count audit: %v", err)
			}
			if n == 0 {
				t.Errorf("no audit row for refused read of %s", tc.containerID)
			}
		})
	}
}

func TestLogServerStatsEndpoint(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	f.fk.mu.Lock()
	f.fk.statMemBytes = 8 << 20
	f.fk.statMemLimit = 64 << 20
	f.fk.mu.Unlock()

	client, _, cleanup := startLogServer(t, f)
	defer cleanup()

	resp, err := client.Get("http://unix/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s, want 200", resp.StatusCode, body)
	}
	var payload struct {
		Apps []AppStats `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Apps) != 1 || payload.Apps[0].Name != "web" {
		t.Fatalf("apps = %+v", payload.Apps)
	}
	if payload.Apps[0].MemUsageMB != 8 || payload.Apps[0].NearLimit {
		t.Errorf("stats = %+v, want 8 MiB and not near the limit", payload.Apps[0])
	}
}

func TestLogServerGCEndpoint(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	client, _, cleanup := startLogServer(t, f)
	defer cleanup()

	post := func(body string) *http.Response {
		t.Helper()
		resp, err := client.Post("http://unix/gc", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /gc: %v", err)
		}
		return resp
	}

	// Unknown fields are rejected before the collector runs.
	resp := post(`{"sudo":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field: status = %d, want 400", resp.StatusCode)
	}

	// Unknown app names are a lookup failure, not an empty success.
	resp = post(`{"app":"ghost"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown app: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// A dry run reports without removing; this deployment is inside the
	// default retention window, so there is nothing to list.
	resp = post(`{"dry_run":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dry run: status = %d, body = %s", resp.StatusCode, body)
	}
	var res GCResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.DryRun || len(res.Removed) != 0 {
		t.Errorf("dry run result = %+v, want dry_run with no candidates", res)
	}
}

func TestLogServerValidatesQuery(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	d := f.deployment()
	client, _, cleanup := startLogServer(t, f)
	defer cleanup()

	bad := []string{
		"http://unix/logs", // no ids
		logURL(*d.ContainerID, f.app.ID, "&tail=abc"),     // bad tail
		logURL(*d.ContainerID, f.app.ID, "&since=banana"), // bad since
		logURL(*d.ContainerID, f.app.ID, "&since=-3m"),    // negative since
	}
	for _, u := range bad {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", u, resp.StatusCode)
		}
	}
	// None of the invalid requests may have reached the daemon.
	if f.fk.logTail != 0 {
		t.Errorf("invalid requests reached docker (tail=%d)", f.fk.logTail)
	}
}
