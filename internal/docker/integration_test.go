package docker

// Integration tests against a real Docker daemon. Every test skips with
// a clear message when the daemon is unavailable, so unit-only
// environments stay green — such coverage is only claimed done with a
// real end-to-end deploy.
//
// These tests construct ContainerSpec values directly (still through the
// closed spec type) and never import the Docker client themselves — the
// static client-scope test scans test files too, and this package is the
// proof that real-daemon coverage is achievable through the wrapper alone.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/health"
	"github.com/tinyship/tinyship/internal/policy"
)

// requireDocker returns a live wrapper or skips the test with a clear
// diagnostic when no daemon is reachable.
func requireDocker(t *testing.T) *Wrapper {
	t.Helper()
	w, err := New(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Skipf("docker: no daemon reachable, skipping integration proof: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.CheckEngine(ctx); err != nil {
		w.Close()
		t.Skipf("docker: engine unavailable, skipping integration proof: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// freeManagedPort picks an unused port from the TinyShip managed range
func freeManagedPort(t *testing.T) int {
	t.Helper()
	for try := 0; try < 50; try++ {
		candidate := policy.MinHostPort + (time.Now().Nanosecond()+try*7)%(policy.MaxHostPort-policy.MinHostPort+1)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
		if err == nil {
			ln.Close()
			return candidate
		}
	}
	t.Fatal("no free port in managed range 20000-29999")
	return 0
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func uniqueAppName() string {
	return fmt.Sprintf("it%d", time.Now().UnixNano()%1e9)
}

// TestIntegrationHTTPLifecycle is the core lifecycle test: pull a public
// image, pin by digest, create/start under policy, HTTP health from the
// host, logs, ownership checks, stop, remove — all against the real daemon.
func TestIntegrationHTTPLifecycle(t *testing.T) {
	w := requireDocker(t)
	ctx := testCtx(t)

	// 1. Pull public image and pin by digest.
	digest, err := w.PullImage(ctx, "traefik/whoami:latest", nil)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("pull returned digest %q, want sha256:<64 hex>", digest)
	}

	// 2. Ensure network (idempotent, ICC disabled) and volume prefix rule.
	if err := w.EnsureNetwork(ctx); err != nil {
		t.Fatalf("ensure network: %v", err)
	}
	if err := w.EnsureNetwork(ctx); err != nil {
		t.Fatalf("ensure network not idempotent: %v", err)
	}
	if err := w.EnsureVolume(ctx, "foreign-volume"); err == nil || !strings.Contains(err.Error(), "prefix") {
		t.Errorf("EnsureVolume without ts- prefix = %v, want prefix refusal", err)
	}

	// 3. Build the closed spec from a validated config (the production path).
	appName := uniqueAppName()
	cfg := &appconfig.Config{
		Name: "web", Image: "traefik/whoami", Tag: "latest",
		Port: 80, Strategy: "auto",
		Resources:  appconfig.Resources{MemoryMB: 64, CPUMillis: 200, PidsLimit: 64},
		StopGraceS: 5,
	}
	if _, err := appconfig.Validate(cfg); err != nil {
		t.Fatalf("config validate: %v", err)
	}
	hostPort := freeManagedPort(t)
	spec, err := policy.Build(cfg, policy.Instance{
		AppID: "app-int", AppName: appName, DeploymentID: "dep-int", Seq: 1,
		Digest: digest, ConfigHash: policy.ConfigHash(cfg),
		HostPort: hostPort,
		Env:      []string{"WHOAMI_NAME=tinyship"},
	})
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}

	// 4. Create + start; always clean up.
	containerID, err := w.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.RemoveContainer(cleanup, containerID, true)
	})
	if err := w.StartContainer(ctx, containerID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 5. Ownership checks pass for our own container.
	owns := func(context.Context, string, string) (bool, error) { return true, nil }
	if err := AssertOwned(ctx, w.InspectContainer, "app-int", containerID, owns); err != nil {
		t.Fatalf("ownership of own container: %v", err)
	}
	// Wrong app id must be refused (check #2 against the real daemon).
	if err := AssertOwned(ctx, w.InspectContainer, "some-other-app", containerID, owns); err == nil {
		t.Error("ownership check accepted a container from another app")
	}
	// Not managed by TinyShip: covered by TestIntegrationExecProbeAndForeign
	// below, which creates a real unlabelled container on the daemon.

	// 6. HTTP health from the host over the loopback binding.
	healthCfg := &appconfig.Health{
		Type: "http", Path: "/", ExpectStatus: [2]int{200, 299},
		Port: 80, IntervalS: 2, TimeoutS: 2, Retries: 20,
	}
	target := fmt.Sprintf("127.0.0.1:%d", hostPort)
	res := health.Wait(ctx, target, healthCfg, nil)
	if !res.Healthy {
		t.Fatalf("HTTP health via 127.0.0.1:%d never passed: %s", hostPort, res.Reason)
	}
	// TCP probe must agree (the port is genuinely bound loopback-only).
	tcpRes := health.Probe(ctx, target, &appconfig.Health{Type: "tcp", TimeoutS: 2, IntervalS: 5, Retries: 1}, nil)
	if !tcpRes.Healthy {
		t.Errorf("TCP health: %s", tcpRes.Reason)
	}

	// 7. Logs after generating traffic.
	if err := w.StopContainer(ctx, containerID, 3*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Restart to capture fresh request logs deterministically.
	if err := w.StartContainer(ctx, containerID); err != nil {
		t.Fatalf("restart: %v", err)
	}
	resp, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err == nil {
		fmt.Fprintf(resp, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
		io.Copy(io.Discard, io.LimitReader(resp, 4096))
		resp.Close()
	}
	time.Sleep(500 * time.Millisecond)

	rc, err := w.ContainerLogs(ctx, containerID, 50, 0, false)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	logBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("logs read: %v", err)
	}
	if len(logBytes) == 0 {
		t.Error("container logs are empty after requests")
	}

	// 8. Inspect shows running state with the managed labels.
	info, err := w.InspectContainer(ctx, containerID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.Labels[policy.LabelManaged] != policy.ManagedValue {
		t.Errorf("managed label = %q", info.Labels[policy.LabelManaged])
	}
	if info.State != "running" {
		t.Errorf("state = %q, want running", info.State)
	}

	// 9. Stop for real (grace) — proves StopContainer against the daemon.
	if err := w.StopContainer(ctx, containerID, 3*time.Second); err != nil {
		t.Fatalf("final stop: %v", err)
	}
	info, err = w.InspectContainer(ctx, containerID)
	if err != nil {
		t.Fatalf("inspect after stop: %v", err)
	}
	if info.State == "running" {
		t.Error("container still running after stop")
	}
}

// TestIntegrationExecProbeAndForeignContainer covers the exec health path
// and the E_NOT_MANAGED refusal against the real daemon.
func TestIntegrationExecProbeAndForeignContainer(t *testing.T) {
	w := requireDocker(t)
	ctx := testCtx(t)

	digest, err := w.PullImage(ctx, "alpine:3.20", nil)
	if err != nil {
		t.Fatalf("pull alpine: %v", err)
	}

	appName := uniqueAppName()
	cfg := &appconfig.Config{
		Name: "job", Image: "alpine", Tag: "3.20",
		Port: 8080, Strategy: "auto",
		Command:    []string{"/bin/sh", "-c", "echo tinyship-integration-marker; sleep 600"},
		Resources:  appconfig.Resources{MemoryMB: 64, CPUMillis: 200, PidsLimit: 64},
		StopGraceS: 5,
	}
	if _, err := appconfig.Validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	spec, err := policy.Build(cfg, policy.Instance{
		AppID: "app-exec", AppName: appName, DeploymentID: "dep-exec", Seq: 1,
		Digest: digest, ConfigHash: policy.ConfigHash(cfg),
		HostPort: freeManagedPort(t),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	containerID, err := w.CreateContainer(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.RemoveContainer(cleanup, containerID, true)
	})
	if err := w.StartContainer(ctx, containerID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Exec health probe: /bin/true exits 0 inside alpine.
	execHealth := &appconfig.Health{
		Type: "exec", Command: []string{"/bin/true"},
		Port: 8080, IntervalS: 2, TimeoutS: 5, Retries: 5,
	}
	execTransport := func(c context.Context, command []string, timeout time.Duration) (int, error) {
		return w.ExecProbe(c, containerID, command, timeout)
	}
	res := health.Wait(ctx, "127.0.0.1:0", execHealth, execTransport)
	if !res.Healthy {
		t.Fatalf("exec health: %s", res.Reason)
	}
	// Exec health with a failing command must fail with its exit code.
	execFail := &appconfig.Health{
		Type: "exec", Command: []string{"/bin/false"},
		Port: 8080, IntervalS: 2, TimeoutS: 5, Retries: 2,
	}
	res = health.Wait(ctx, "127.0.0.1:0", execFail, execTransport)
	if res.Healthy || !strings.Contains(res.Reason, health.EExecNonZero) {
		t.Errorf("failing exec probe: healthy=%v reason=%q", res.Healthy, res.Reason)
	}

	// Foreign container: created through the closed spec but carrying no
	// TinyShip labels — exactly what another tool would leave behind. All
	// ownership checks must refuse it.
	foreign := *spec // copy the struct; the original must stay intact
	foreign.Name = spec.Name + "-foreign"
	foreign.Labels = map[string]string{"com.example.tool": "someone-else"}
	foreignID, err := w.CreateContainer(ctx, &foreign)
	if err != nil {
		t.Fatalf("create foreign: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = w.RemoveContainer(cleanup, foreignID, true)
	})
	owns := func(context.Context, string, string) (bool, error) { return true, nil }
	err = AssertOwned(ctx, w.InspectContainer, "app-exec", foreignID, owns)
	if err == nil || !strings.Contains(err.Error(), "E_NOT_MANAGED") {
		t.Errorf("foreign container ownership = %v, want E_NOT_MANAGED", err)
	}
	// And the managed list must not return it.
	list, err := w.ListManagedContainers(ctx)
	if err != nil {
		t.Fatalf("list managed: %v", err)
	}
	for _, c := range list {
		if c.ID == foreignID {
			t.Error("ListManagedContainers returned an unmanaged container")
		}
	}

	// Logs contain the marker the container wrote.
	deadline := time.Now().Add(15 * time.Second)
	var logBytes []byte
	for time.Now().Before(deadline) {
		rc, err := w.ContainerLogs(ctx, containerID, 20, 0, false)
		if err != nil {
			t.Fatalf("logs: %v", err)
		}
		logBytes, _ = io.ReadAll(rc)
		rc.Close()
		if strings.Contains(string(logBytes), "tinyship-integration-marker") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !strings.Contains(string(logBytes), "tinyship-integration-marker") {
		t.Errorf("logs missing marker, got: %.200s", string(logBytes))
	}
}
