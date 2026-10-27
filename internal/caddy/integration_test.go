package caddy

// Real-Caddy integration: applies a server config through the admin
// socket exactly like the deploy pipeline and proves both the create
// and the replace path against a live Caddy. The replace path is the
// one that separates POST from the create-only PUT verb, so this test
// is the evidence that an atomic update of a live subtree works.
//
// The test skips with a clear message when no caddy binary is on the
// machine; unit-only environments stay green.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireCaddy returns the caddy binary path or skips the test.
func requireCaddy(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("TINYSHIP_CADDY_BIN"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("caddy")
	if err != nil {
		t.Skip("caddy binary not found; set TINYSHIP_CADDY_BIN to enable the real-Caddy proof")
	}
	return bin
}

// freePort grabs an ephemeral port and releases it for Caddy to bind.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// startCaddy boots Caddy with an empty server set on a fresh admin
// socket and returns the socket path.
func startCaddy(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")
	cfg := filepath.Join(dir, "caddy.json")
	base := fmt.Sprintf(
		`{"admin":{"listen":"unix/%s","origins":["%s"]},"apps":{"http":{"servers":{}}}}`,
		sock, adminHost)
	if err := os.WriteFile(cfg, []byte(base), 0o600); err != nil {
		t.Fatalf("write caddy config: %v", err)
	}
	var out bytes.Buffer
	cmd := exec.Command(bin, "run", "--config", cfg)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start caddy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("caddy output:\n%s", out.String())
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("caddy exited before opening the admin socket:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("caddy admin socket never appeared:\n%s", out.String())
	return ""
}

func staticServer(port int, status int) map[string]any {
	return map[string]any{
		"listen": []string{fmt.Sprintf("127.0.0.1:%d", port)},
		"routes": []any{map[string]any{
			"match": []any{map[string]any{"host": []string{"it.invalid"}}},
			"handle": []any{map[string]any{
				"handler":     "static_response",
				"status_code": status,
			}},
		}},
		// Certificates are not part of this proof; disable automatic
		// HTTPS so the test never talks to an ACME server.
		"automatic_https": map[string]any{"disable": true},
	}
}

func TestApplyServerAgainstRealCaddy(t *testing.T) {
	bin := requireCaddy(t)
	sock := startCaddy(t, bin)
	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	port := freePort(t)

	// First apply creates the subtree.
	first := staticServer(port, 404)
	hash1, err := c.ApplyServer(ctx, first)
	if err != nil {
		t.Fatalf("create apply: %v", err)
	}
	if len(hash1) != 64 {
		t.Errorf("hash = %q, want 64 hex chars", hash1)
	}

	// Second apply must atomically replace the live subtree: this is
	// exactly where the create-only PUT verb fails with 409 on a real
	// Caddy, so a green run here is the POST-replace proof.
	second := staticServer(port, 503)
	hash2, err := c.ApplyServer(ctx, second)
	if err != nil {
		t.Fatalf("replace apply: %v", err)
	}
	if hash2 == hash1 {
		t.Error("replacing the config did not change its hash")
	}

	// The live config equals the desired one after canonical encoding,
	// which is the invariant the pipeline's verification step relies on.
	got, etag, err := c.CurrentConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("CurrentConfig = %v, %v", got, err)
	}
	if etag == "" {
		t.Error("live config has no Etag; the concurrency dance cannot run")
	}
	gotRaw, _ := json.Marshal(got)
	wantRaw, _ := json.Marshal(second)
	if !bytes.Equal(gotRaw, wantRaw) {
		t.Errorf("live config diverged from desired:\ngot  %s\nwant %s", gotRaw, wantRaw)
	}
}
