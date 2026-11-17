package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/tinyship/tinyship/internal/caddy"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// fakeCaddy records every applied server config and returns its hash,
// like the production client does after verification.
type fakeCaddy struct {
	mu      sync.Mutex
	current map[string]any
	applies []map[string]any
	hashes  []string
	err     error
	readErr error

	calls   int            // ApplyServer invocations, including failed ones
	failAt  int            // 1-based invocation that must fail (needs failErr)
	failErr error          // the error failAt returns
	onApply func(call int) // observes each successful apply, called unlocked
}

func (f *fakeCaddy) CurrentConfig(_ context.Context) (map[string]any, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, "", f.readErr
	}
	return f.current, "", nil
}

func (f *fakeCaddy) ApplyServer(_ context.Context, server map[string]any) (string, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	if call == f.failAt && f.failErr != nil {
		err := f.failErr
		f.mu.Unlock()
		return "", err
	}
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return "", err
	}
	h, err := caddy.Hash(server)
	if err != nil {
		f.mu.Unlock()
		return "", err
	}
	f.applies = append(f.applies, server)
	f.hashes = append(f.hashes, h)
	f.current = server
	hook := f.onApply
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return h, nil
}

// forgetCurrent models a proxy restart that came back with only its
// baseline config, which is how a service or host restart loses routes
// that were applied through the admin API.
func (f *fakeCaddy) forgetCurrent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = nil
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

func TestDeployRouteSwitchSocketPermissionDeniedIsPermanent(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.attachDomain("web.example.com")
	f.dplr.Caddy = &fakeCaddy{err: &caddy.UnreachableError{Err: &os.PathError{
		Op: "dial", Path: "/run/caddy/admin.sock", Err: syscall.EACCES}}}
	f.dplr.Probe = func(context.Context, string) (int, error) { return 200, nil }

	err := f.run(1, 5)
	if err == nil {
		t.Fatal("deploy = nil, want E_CADDY_APPLY")
	}
	if !strings.Contains(err.Error(), "E_CADDY_APPLY") {
		t.Errorf("err = %v, want E_CADDY_APPLY", err)
	}
	if Retryable(err) {
		t.Errorf("err = %v, want permanent: retrying cannot change the socket mode", err)
	}
	d := f.deployment()
	if d.State != store.DepFailed {
		t.Errorf("state = %q, want failed", d.State)
	}
	if d.FailureStep == nil || *d.FailureStep != "route_switch" {
		t.Errorf("failure step = %v, want route_switch", d.FailureStep)
	}
}

func TestDeployRecoveryRestoresRoutesBeforeRemovingTheNewContainer(t *testing.T) {
	f := newDeployFixture(t, writableVolumeConfig)
	f.attachDomain("web.example.com")
	fc := &fakeCaddy{}
	f.dplr.Caddy = fc
	f.dplr.Probe = func(context.Context, string) (int, error) { return 200, nil }
	if err := f.run(1, 1); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	first := f.deployment()
	oldID := *first.ContainerID
	oldPort := *first.HostPort

	f.queueRedeploy(writableVolumeConfig, false)
	// Call 3 is final_check's own restore: it fails, so the recovery
	// (call 4) is the one that must bring the routes back.
	fc.failAt, fc.failErr = 3, errors.New("verify read after apply failed")
	fc.onApply = func(call int) {
		if call != 4 {
			return
		}
		if got := f.fk.stateOf(oldID); got != "running" {
			t.Errorf("previous container state at route restore = %q, want running first", got)
		}
		if n := f.fk.containerCount(); n != 2 {
			t.Errorf("containers at route restore = %d, want 2 (the new one goes last)", n)
		}
	}
	f.dplr.Probe = func(context.Context, string) (int, error) { return 502, nil }

	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_FINAL_CHECK_FAILED") {
		t.Fatalf("deploy = %v, want E_FINAL_CHECK_FAILED", err)
	}
	if fc.calls != 4 || fc.count() != 3 {
		t.Errorf("calls = %d, applies = %d, want 4 and 3 (one restore failed)",
			fc.calls, fc.count())
	}
	d := f.deployment()
	if d.State != store.DepFailed || d.FailureStep == nil || *d.FailureStep != "final_check" {
		t.Fatalf("deployment = %q/%v, want failed/final_check", d.State, d.FailureStep)
	}
	if n := f.fk.containerCount(); n != 1 || f.fk.stateOf(oldID) != "running" {
		t.Errorf("containers = %d (previous state %q), want only the running previous one",
			n, f.fk.stateOf(oldID))
	}
	body := routeBody(t, fc.last())
	if !strings.Contains(body, "web.example.com") ||
		!strings.Contains(body, "127.0.0.1:"+strconv.Itoa(oldPort)) {
		t.Errorf("last applied config does not route back to port %d: %s", oldPort, body)
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
	var seenHost, seenSNI string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusTeapot)
	}))
	ts.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		seenSNI = chi.ServerName
		return nil, nil
	}}
	ts.StartTLS()
	defer ts.Close()

	// httptest's URL is https://127.0.0.1:<port>; the probe trusts the
	// routing, not the issuer, so the self-signed certificate must not
	// matter. SNI must still carry the hostname: the proxy refuses a
	// handshake whose server name matches no route.
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
	if seenSNI != "app.example.test" {
		t.Errorf("SNI = %q, want the routed hostname", seenSNI)
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

func TestDefaultProbeFollowsRedirectWithoutTrustStore(t *testing.T) {
	// A proxy with automatic HTTPS answers a plain probe with a redirect
	// to https, and the target may present a locally issued certificate
	// (a hostname whose DNS still points elsewhere). The followed
	// request must still prove routing: trust-store contents must not
	// decide a deploy, while the handshake keeps carrying the route's
	// hostname as SNI.
	var seenSNI string
	tlsSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	tlsSrv.TLS = &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		seenSNI = chi.ServerName
		return nil, nil
	}}
	tlsSrv.StartTLS()
	defer tlsSrv.Close()

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tlsSrv.URL+"/", http.StatusPermanentRedirect)
	}))
	defer plain.Close()

	t.Setenv("TINYSHIP_CADDY_PROBE", plain.URL)
	got, err := DefaultProbe(context.Background(), "app.example.test")
	if err != nil {
		t.Fatalf("probe after http->https redirect: %v", err)
	}
	if got != http.StatusOK {
		t.Errorf("status = %d, want 200 from the routed app", got)
	}
	if seenSNI != "app.example.test" {
		t.Errorf("SNI = %q, want the routed hostname", seenSNI)
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

func TestRegenerateRoutesRestoresLostLiveConfig(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	fc := &fakeCaddy{}
	log := slog.New(slog.DiscardHandler)

	createRoutedApp(t, s, "web", "web.example.com", 20017)
	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("initial regenerate: %v", err)
	}
	if fc.count() != 1 {
		t.Fatalf("initial applies = %d, want 1", fc.count())
	}

	// The recorded hash still matches the database, but the proxy came
	// back from a restart with only its baseline config.
	fc.forgetCurrent()
	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("regenerate after losing live config: %v", err)
	}
	if fc.count() != 2 {
		t.Fatalf("applies after losing live config = %d, want 2", fc.count())
	}
	if body := routeBody(t, fc.last()); !strings.Contains(body, "web.example.com") {
		t.Errorf("restored config misses hostname: %s", body)
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

// --- hibernation routing ------------------------------------------------------

func TestRegenerateRoutesPointsHibernatingAppsAtTheRelay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	fc := &fakeCaddy{}
	log := slog.New(slog.DiscardHandler)

	// A plain app keeps its container port.
	createRoutedApp(t, s, "plain", "plain.example.com", 20001)

	// A hibernating app is routed at the relay, awake or asleep, so
	// no proxy reconfiguration is ever needed to wake it.
	hib, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: "hib", ConfigJSON: hibConfig("hib", 128, 60, 30), Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create hibernating app: %v", err)
	}
	dep := makeDeployment(t, s, hib.ID, 20002)
	if err := s.SucceedDeployment(ctx, hib.ID, dep); err != nil {
		t.Fatalf("succeed deployment: %v", err)
	}
	dom, _, err := s.AddDomain(ctx, hib.ID, "hib.example.com")
	if err != nil {
		t.Fatalf("add domain: %v", err)
	}
	if err := s.SetDomainStatus(ctx, dom.ID, store.DomainActive); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if err := RegenerateRoutes(ctx, s, fc, log); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	body := routeBody(t, fc.last())
	if !strings.Contains(body, "127.0.0.1:"+strconv.Itoa(policy.RelayPort)) {
		t.Errorf("config must dial the relay for the hibernating app: %s", body)
	}
	if !strings.Contains(body, "127.0.0.1:20001") {
		t.Errorf("config must keep the plain app on its port: %s", body)
	}
	if strings.Contains(body, "127.0.0.1:20002") {
		t.Errorf("config must not dial the hibernating app's own port: %s", body)
	}
}

func TestRelayTargetsKeepAnArmedMachineOnTheRelay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Hibernation on: the route must point at the relay.
	on, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: "on-app", ConfigJSON: hibConfig("on-app", 128, 60, 30), Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// Hibernation off and never slept: the container serves directly.
	off, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: "off-app", ConfigJSON: `{"name":"off-app","image":"nginx"}`, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// Turned off while asleep: the machine is still armed, and
	// dropping the relay route would strand the stopped container.
	stranded, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: "stranded", ConfigJSON: `{"name":"stranded","image":"nginx"}`, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := s.SetHibernateState(ctx, stranded.ID, store.HibernateSleeping); err != nil {
		t.Fatalf("arm machine: %v", err)
	}

	targets, err := relayTargets(ctx, s)
	if err != nil {
		t.Fatalf("relay targets: %v", err)
	}
	if !targets[on.ID] {
		t.Error("an app with hibernation on must be relay-routed")
	}
	if targets[off.ID] {
		t.Error("an app without hibernation must keep its container port")
	}
	if !targets[stranded.ID] {
		t.Error("an armed machine keeps the relay route until it retires")
	}
}

func TestWithRelayRewritesOnlyTargetUpstreams(t *testing.T) {
	rows := []store.RouteRow{
		{DomainID: "d1", AppID: "a1", Hostname: "one.example.com", HostPort: 20001},
		{DomainID: "d2", AppID: "a2", Hostname: "two.example.com", HostPort: 20002},
	}
	out := withRelay(rows, map[string]bool{"a1": true})

	if out[0].HostPort != policy.RelayPort {
		t.Errorf("target upstream = %d, want the relay %d", out[0].HostPort, policy.RelayPort)
	}
	if out[1].HostPort != 20002 {
		t.Errorf("other upstream = %d, want its own port", out[1].HostPort)
	}
	if rows[0].HostPort != 20001 {
		t.Error("the caller's rows must not be rewritten in place")
	}
	// With no targets the rows pass through untouched.
	if same := withRelay(rows, nil); len(same) != 2 || same[0].HostPort != 20001 {
		t.Errorf("no targets must leave the rows alone: %+v", same)
	}
}

// --- deploy ↔ relay -----------------------------------------------------------

// upstreamDial reads where the proxy would dial for a hostname in an
// applied config, exactly as the final check's probe would reach it.
func upstreamDial(t *testing.T, server map[string]any, host string) string {
	t.Helper()
	routes, _ := server["routes"].([]any)
	for _, raw := range routes {
		route, _ := raw.(map[string]any)
		matches, _ := route["match"].([]any)
		for _, rawM := range matches {
			m, _ := rawM.(map[string]any)
			var hosts []string
			switch h := m["host"].(type) {
			case []string:
				hosts = h
			case []any:
				for _, v := range h {
					if s, ok := v.(string); ok {
						hosts = append(hosts, s)
					}
				}
			}
			for _, h := range hosts {
				if h != host {
					continue
				}
				handles, _ := route["handle"].([]any)
				for _, rawH := range handles {
					h, _ := rawH.(map[string]any)
					ups, _ := h["upstreams"].([]any)
					for _, rawU := range ups {
						u, _ := rawU.(map[string]any)
						if dial, ok := u["dial"].(string); ok {
							return dial
						}
					}
				}
			}
		}
	}
	t.Fatalf("no route for %q in applied config %s", host, routeBody(t, server))
	return ""
}

// TestDeployOfAHibernatingAppProbesTheContainerNotTheRelay: the deploy
// job is in flight while final_check probes through the proxy, and the
// relay answers every request during a job with 503. The route switch
// must therefore aim this app at the new container for the whole
// pipeline — whatever its hibernation position — and the route-sync
// tick then moves the route onto the relay once the job is done.
func TestDeployOfAHibernatingAppProbesTheContainerNotTheRelay(t *testing.T) {
	for _, state := range []string{"", store.HibernateAwake, store.HibernateSleeping} {
		t.Run("state="+state, func(t *testing.T) {
			f := newDeployFixtureIn(t, testStore(t), hibConfig("web", 128, 60, 30))
			f.attachDomain("web.example.com")
			if state != "" {
				if err := f.s.SetHibernateState(f.ctx, f.app.ID, state); err != nil {
					t.Fatalf("set hibernate state: %v", err)
				}
			}
			fc := &fakeCaddy{}
			f.dplr.Caddy = fc
			var probedDial string
			f.dplr.Probe = func(_ context.Context, host string) (int, error) {
				probedDial = upstreamDial(t, fc.last(), host)
				if probedDial == fmt.Sprintf("127.0.0.1:%d", policy.RelayPort) {
					// What the relay would answer with: the deploy
					// job owns the app, so final_check would fail.
					return http.StatusServiceUnavailable, nil
				}
				return 200, nil
			}

			if err := f.run(1, 5); err != nil {
				t.Fatalf("deploy: %v", err)
			}
			dep := f.deployment()
			if dep.HostPort == nil {
				t.Fatal("deployment has no host port")
			}
			want := fmt.Sprintf("127.0.0.1:%d", *dep.HostPort)
			if probedDial != want {
				t.Errorf("final check probed %q, want the new container %q", probedDial, want)
			}

			// Once the job is done, the route-sync tick converges the
			// route onto the relay — the assumption the finalize idle
			// clock restart leans on.
			if err := RegenerateRoutes(f.ctx, f.s, fc, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatalf("route sync after deploy: %v", err)
			}
			relayDial := fmt.Sprintf("127.0.0.1:%d", policy.RelayPort)
			if got := upstreamDial(t, fc.last(), "web.example.com"); got != relayDial {
				t.Errorf("route after sync = %q, want the relay %q", got, relayDial)
			}
		})
	}
}
