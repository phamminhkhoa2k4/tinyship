package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tinyship/tinyship/internal/caddy"
	"github.com/tinyship/tinyship/internal/store"
)

// fakeCaddy records every applied server config and returns its hash,
// like the production client does after verification.
type fakeCaddy struct {
	mu      sync.Mutex
	applies []map[string]any
	hashes  []string
	err     error
}

func (f *fakeCaddy) ApplyServer(_ context.Context, server map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	h, err := caddy.Hash(server)
	if err != nil {
		return "", err
	}
	f.applies = append(f.applies, server)
	f.hashes = append(f.hashes, h)
	return h, nil
}

func (f *fakeCaddy) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.applies) == 0 {
		return nil
	}
	return f.applies[len(f.applies)-1]
}

func (f *fakeCaddy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applies)
}

// attachDomain adds a hostname to the app as active.
func (f *deployFixture) attachDomain(hostname string) {
	f.t.Helper()
	d, _, err := f.s.AddDomain(f.ctx, f.app.ID, hostname)
	if err != nil {
		f.t.Fatalf("AddDomain: %v", err)
	}
	if err := f.s.SetDomainStatus(f.ctx, d.ID, store.DomainActive); err != nil {
		f.t.Fatalf("activate domain: %v", err)
	}
}

// routeBody renders the last applied config as JSON for assertions.
func routeBody(t *testing.T, server map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(server)
	if err != nil {
		t.Fatalf("marshal applied config: %v", err)
	}
	return string(raw)
}

func TestDeployWithDomainSwitchesRouteAndPassesFinalCheck(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.attachDomain("web.example.com")
	fc := &fakeCaddy{}
	f.dplr.Caddy = fc
	f.dplr.Probe = func(_ context.Context, host string) (int, error) {
		if host != "web.example.com" {
			t.Errorf("probe host = %q, want web.example.com", host)
		}
		return 200, nil
	}

	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	assertStep(t, []string{
		"validate:ok", "admission:ok", "pull:ok", "stop_old:skip",
		"create:ok", "start:ok", "health:ok",
		"route_switch:ok", "final_check:ok", "retire_old:skip",
		"finalize:ok",
	}, f.timeline())

	if fc.count() != 1 {
		t.Fatalf("applies = %d, want 1", fc.count())
	}
	body := routeBody(t, fc.last())
	if !strings.Contains(body, "web.example.com") {
		t.Errorf("applied config misses hostname: %s", body)
	}
	d := f.deployment()
	if d.HostPort == nil || !strings.Contains(body, "127.0.0.1:"+strconv.Itoa(*d.HostPort)) {
		t.Errorf("applied config does not point at the new port %v: %s", d.HostPort, body)
	}
	// The applied hash is recorded for boot regeneration.
	hash, err := f.s.GetSetting(f.ctx, CaddyConfigHashKey)
	if err != nil || hash != fc.hashes[0] {
		t.Errorf("settings hash = %q, %v; want %q", hash, err, fc.hashes[0])
	}
}

func TestDeployFinalCheckFailureRestoresRouteAndFails(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.attachDomain("web.example.com")
	fc := &fakeCaddy{}
	f.dplr.Caddy = fc
	f.dplr.Probe = func(context.Context, string) (int, error) { return 502, nil }

	err := f.run(1, 1)
	if err == nil {
		t.Fatal("deploy = nil, want E_FINAL_CHECK_FAILED")
	}
	if !strings.Contains(err.Error(), "E_FINAL_CHECK_FAILED") {
		t.Errorf("err = %v, want E_FINAL_CHECK_FAILED", err)
	}

	d := f.deployment()
	if d.State != store.DepFailed || d.FailureStep == nil || *d.FailureStep != "final_check" {
		t.Errorf("deployment = %+v, want failed at final_check", d)
	}
	// First deploy: the restore drops the domain from routing entirely
	// (nothing good to fall back to), so the last applied config must
	// not mention the hostname.
	if fc.count() != 2 {
		t.Fatalf("applies = %d, want 2 (switch then restore)", fc.count())
	}
	if body := routeBody(t, fc.last()); strings.Contains(body, "web.example.com") {
		t.Errorf("restored config still routes the domain: %s", body)
	}
	// The failed run's new container was cleaned up.
	f.fk.mu.Lock()
	remaining := len(f.fk.containers)
	f.fk.mu.Unlock()
	if remaining != 0 {
		t.Errorf("containers left = %d, want 0 after recovery", remaining)
	}
}

func TestDeployFinalCheckProbeErrorFails(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.attachDomain("web.example.com")
	f.dplr.Caddy = &fakeCaddy{}
	f.dplr.Probe = func(context.Context, string) (int, error) {
		return 0, context.DeadlineExceeded
	}

	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_FINAL_CHECK_FAILED") {
		t.Errorf("deploy = %v, want E_FINAL_CHECK_FAILED on probe error", err)
	}
}

func TestDeployDomainWithoutRouteEngineFailsLoudly(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.attachDomain("web.example.com")
	// Caddy stays nil: no route engine configured.

	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_CADDY_APPLY") {
		t.Errorf("deploy = %v, want E_CADDY_APPLY", err)
	}
	d := f.deployment()
	if d.State != store.DepFailed {
		t.Errorf("state = %q, want failed", d.State)
	}
}

func TestDeployRouteSwitchRetriesWhenCaddyUnreachable(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.attachDomain("web.example.com")
	f.dplr.Caddy = &fakeCaddy{err: &caddy.UnreachableError{Err: errors.New("connection refused")}}
	f.dplr.Probe = func(context.Context, string) (int, error) { return 200, nil }

	// Attempt 1 of 5: unreachable is transient, so the job requeues.
	if err := f.run(1, 5); !Retryable(err) {
		t.Errorf("deploy = %v, want retryable (Caddy restarting)", err)
	}
}

func TestBuildOptionsFromEnv(t *testing.T) {
	// Installed-host defaults: no env set.
	opts := buildOptions(nil)
	if opts.Listen != "" || opts.DisableAutoHTTPS || opts.ServeTLS || opts.Override != nil {
		t.Errorf("zero env = %+v, want zero options", opts)
	}

	t.Setenv("TINYSHIP_CADDY_LISTEN", ":8443")
	t.Setenv("TINYSHIP_CADDY_AUTO_HTTPS", "off")
	opts = buildOptions(&caddy.Override{AppID: "appA", HostPort: 20017})
	if opts.Listen != ":8443" || !opts.DisableAutoHTTPS || opts.ServeTLS || opts.Override == nil {
		t.Errorf("env options = %+v, want listen :8443, auto-https off, override kept", opts)
	}

	// The test-CA profile: automation stays on, the server terminates
	// TLS, and the override survives.
	t.Setenv("TINYSHIP_CADDY_AUTO_HTTPS", "internal")
	opts = buildOptions(&caddy.Override{AppID: "appA", HostPort: 20017})
	if opts.Listen != ":8443" || opts.DisableAutoHTTPS || !opts.ServeTLS || opts.Override == nil {
		t.Errorf("internal options = %+v, want listen :8443, TLS served, override kept", opts)
	}
}

func TestDefaultProbeSwitchesToTLSWithAURL(t *testing.T) {
	var seenHost string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusTeapot)
	}))
	defer ts.Close()

	// httptest's URL is https://127.0.0.1:<port>; the probe trusts the
	// routing, not the issuer, so the self-signed certificate must not
	// matter.
	t.Setenv("TINYSHIP_CADDY_PROBE", ts.URL)
	got, err := DefaultProbe(context.Background(), "app.example.test")
	if err != nil {
		t.Fatalf("DefaultProbe over https: %v", err)
	}
	if got != http.StatusTeapot {
		t.Errorf("probe status = %d, want %d", got, http.StatusTeapot)
	}
	if seenHost != "app.example.test" {
		t.Errorf("Host header = %q, want the routed hostname", seenHost)
	}

	// An explicit http:// prefix keeps the plain scheme.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer plain.Close()
	t.Setenv("TINYSHIP_CADDY_PROBE", plain.URL)
	got, err = DefaultProbe(context.Background(), "app.example.test")
	if err != nil {
		t.Fatalf("DefaultProbe over http: %v", err)
	}
	if got != http.StatusOK {
		t.Errorf("http probe status = %d, want 200", got)
	}
}

func TestRegenerateRoutesAppliesWhenHashDiffers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	fc := &fakeCaddy{}
	log := slog.New(slog.DiscardHandler)

	// Fresh host: nothing routed, nothing recorded — no-op.
	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("fresh regenerate: %v", err)
	}
	if fc.count() != 0 {
		t.Fatalf("fresh applies = %d, want 0", fc.count())
	}

	// One routable row appears (as after a succeeded deploy): apply.
	app := createRoutedApp(t, s, "web", "web.example.com", 20017)
	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if fc.count() != 1 {
		t.Fatalf("applies = %d, want 1", fc.count())
	}
	if body := routeBody(t, fc.last()); !strings.Contains(body, "web.example.com") {
		t.Errorf("regenerated config misses hostname: %s", body)
	}
	hash, err := s.GetSetting(ctx, CaddyConfigHashKey)
	if err != nil || hash != fc.hashes[0] {
		t.Errorf("settings hash = %q, %v; want %q", hash, err, fc.hashes[0])
	}

	// Same database again: the hash matches, no second apply.
	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("second regenerate: %v", err)
	}
	if fc.count() != 1 {
		t.Errorf("applies after no-op regenerate = %d, want 1", fc.count())
	}

	// A new deployment moves the port: the hash differs, apply again.
	moveCurrentDeployment(t, s, app, 20099)
	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("regenerate after move: %v", err)
	}
	if fc.count() != 2 {
		t.Fatalf("applies after port move = %d, want 2", fc.count())
	}
	if body := routeBody(t, fc.last()); !strings.Contains(body, "127.0.0.1:20099") {
		t.Errorf("regenerated config misses new port: %s", body)
	}
}

// createRoutedApp builds the database shape of a succeeded deploy with
// an active domain: app + current deployment holding the host port.
func createRoutedApp(t *testing.T, s *store.Store, name, hostname string, port int) *store.App {
	t.Helper()
	ctx := context.Background()
	app, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: name, ConfigJSON: `{"name":"` + name + `","image":"nginx"}`, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	dep := makeDeployment(t, s, app.ID, port)
	if err := s.SucceedDeployment(ctx, app.ID, dep); err != nil {
		t.Fatalf("succeed deployment: %v", err)
	}
	d, _, err := s.AddDomain(ctx, app.ID, hostname)
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if err := s.SetDomainStatus(ctx, d.ID, store.DomainActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, err := s.GetAppByID(ctx, app.ID)
	if err != nil {
		t.Fatalf("reload app: %v", err)
	}
	return got
}

// makeDeployment inserts a succeeded-deploy-shaped row with a host port.
func makeDeployment(t *testing.T, s *store.Store, appID string, port int) string {
	t.Helper()
	ctx := context.Background()
	job, err := s.CreateJob(ctx, store.CreateJobInput{
		AppID: &appID, Type: DeployJobType, PayloadJSON: "{}", CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	dep, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		ID: store.NewID(), AppID: appID, JobID: job.ID, Kind: "deploy",
		ImageRef:           "nginx@sha256:" + strings.Repeat("cd", 32),
		ConfigSnapshotJSON: `{"name":"` + appID + `"}`, ConfigVersion: 1,
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if _, err := s.DB().Exec(
		`UPDATE deployments SET host_port = ? WHERE id = ?`, port, dep.ID); err != nil {
		t.Fatalf("assign port: %v", err)
	}
	return dep.ID
}

// moveCurrentDeployment supersedes the app's current deployment with a
// new one on a different port, like a second succeeded deploy would.
func moveCurrentDeployment(t *testing.T, s *store.Store, app *store.App, port int) {
	t.Helper()
	ctx := context.Background()
	dep := makeDeployment(t, s, app.ID, port)
	if err := s.SucceedDeployment(ctx, app.ID, dep); err != nil {
		t.Fatalf("succeed moved deployment: %v", err)
	}
}
