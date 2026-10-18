package worker

// Lifecycle handler tests: app_delete tears containers and rows down
// and regenerates routes; domain_sync replays a route change into the
// proxy. Both run against the fake Docker and fake Caddy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// lifecycleFixture wires a Deployer with a fake Docker and Caddy plus
// one app, so the teardown handlers have something to act on.
type lifecycleFixture struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	fk  *fakeDocker
	fc  *fakeCaddy
	dpl *Deployer
	app *store.App
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	ctx := context.Background()
	s := testStore(t)
	fk := newFakeDocker()
	fc := &fakeCaddy{}
	dpl := NewDeployer(s, fk, slog.New(slog.DiscardHandler), "test-worker")
	dpl.Caddy = fc
	app, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: "web", ConfigJSON: happyConfig, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	return &lifecycleFixture{t: t, ctx: ctx, s: s, fk: fk, fc: fc, dpl: dpl, app: app}
}

// addContainer plants a managed container carrying the app's label so
// the teardown sweep has a target.
func (f *lifecycleFixture) addContainer(appID, id, state string) {
	f.fk.mu.Lock()
	defer f.fk.mu.Unlock()
	f.fk.containers[id] = &fakeContainer{
		id: id, name: id, state: state,
		labels: map[string]string{
			policy.LabelManaged: policy.ManagedValue,
			policy.LabelApp:     appID,
		},
	}
}

// routableDeployment creates a succeeded deployment holding a host port
// and attaches one hostname, so the proxy has a route to drop.
func (f *lifecycleFixture) routableDeployment(hostname string, port int) {
	f.t.Helper()
	depID := store.NewID()
	job, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType,
		PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, depID),
		MaxAttempts: 5, CreatedBy: "cli",
	})
	if err != nil {
		f.t.Fatalf("create job: %v", err)
	}
	if _, err := f.s.CreateDeployment(f.ctx, store.CreateDeploymentInput{
		ID: depID, AppID: f.app.ID, JobID: job.ID, Kind: "deploy",
		ImageRef:           "traefik/whoami:v1.8",
		ConfigSnapshotJSON: happyConfig,
		ConfigVersion:      f.app.ConfigVersion,
	}); err != nil {
		f.t.Fatalf("create deployment: %v", err)
	}
	if _, err := f.s.DB().Exec(
		`UPDATE deployments SET host_port = ? WHERE id = ?`, port, depID); err != nil {
		f.t.Fatalf("assign port: %v", err)
	}
	if err := f.s.SucceedDeployment(f.ctx, f.app.ID, depID); err != nil {
		f.t.Fatalf("succeed deployment: %v", err)
	}
	if _, _, err := f.s.AddDomain(f.ctx, f.app.ID, hostname); err != nil {
		f.t.Fatalf("add domain: %v", err)
	}
	// Routes only include active domains; a fresh attach is pending_dns
	// until DNS checks pass, which this fake host never runs.
	dom, err := f.s.GetDomainByHost(f.ctx, hostname)
	if err != nil {
		f.t.Fatalf("get domain: %v", err)
	}
	if err := f.s.SetDomainStatus(f.ctx, dom.ID, store.DomainActive); err != nil {
		f.t.Fatalf("activate domain: %v", err)
	}
}

func appDeleteJob(appID string) *store.Job {
	payload, _ := json.Marshal(map[string]string{"app_id": appID})
	return &store.Job{ID: store.NewID(), Type: AppDeleteJobType, PayloadJSON: string(payload)}
}

func TestAppDeleteRemovesContainersRowsAndRoutes(t *testing.T) {
	f := newLifecycleFixture(t)
	// One running container and one already-exited leftover, both the
	// app's; a foreign app's container must survive the sweep.
	f.addContainer(f.app.ID, "cid-app", "running")
	f.addContainer(f.app.ID, "cid-left", "exited")
	f.addContainer("another-app", "cid-other", "running")
	// A routable deployment so the proxy has a reason to include the
	// app before teardown, then drop it after.
	f.routableDeployment("web.example.com", 20080)
	// Apply an initial proxy config while the app still routes. The
	// teardown must then issue a second apply that drops the app.
	if err := RegenerateRoutes(f.ctx, f.s, f.fc, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed initial routes: %v", err)
	}
	appliesBefore := len(f.fc.applies)
	if appliesBefore == 0 {
		t.Fatal("seed route apply produced no proxy config")
	}

	if err := f.dpl.AppDeleteHandler()(f.ctx, appDeleteJob(f.app.ID)); err != nil {
		t.Fatalf("app delete: %v", err)
	}

	// Both of the app's containers are gone; the other app's stays.
	if _, ok := f.fk.containers["cid-app"]; ok {
		t.Error("running app container survived teardown")
	}
	if _, ok := f.fk.containers["cid-left"]; ok {
		t.Error("exited leftover container survived teardown")
	}
	if _, ok := f.fk.containers["cid-other"]; !ok {
		t.Error("foreign app container was removed; sweep is not app-scoped")
	}

	// The app row and its dependents are gone.
	if _, err := f.s.GetAppByID(f.ctx, f.app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("app after delete = %v, want ErrNotFound", err)
	}
	var domains int
	if err := f.s.DB().QueryRow(
		`SELECT COUNT(*) FROM domains WHERE app_id = ?`, f.app.ID).Scan(&domains); err != nil {
		t.Fatalf("count domains: %v", err)
	}
	if domains != 0 {
		t.Errorf("domain rows after delete = %d, want 0", domains)
	}

	// A second proxy apply happened and it no longer routes the app.
	if len(f.fc.applies) <= appliesBefore {
		t.Fatalf("teardown issued no new proxy apply (%d before, %d after)",
			appliesBefore, len(f.fc.applies))
	}
	last, _ := json.Marshal(f.fc.applies[len(f.fc.applies)-1])
	if strings.Contains(string(last), "web.example.com") {
		t.Errorf("final proxy config still routes the deleted app: %s", last)
	}
}

func TestAppDeleteIsIdempotentAcrossRetries(t *testing.T) {
	f := newLifecycleFixture(t)
	f.addContainer(f.app.ID, "cid-app", "running")

	if err := f.dpl.AppDeleteHandler()(f.ctx, appDeleteJob(f.app.ID)); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// A crash could lose the ack and requeue the job: the retry must
	// still succeed even though every row is already gone.
	if err := f.dpl.AppDeleteHandler()(f.ctx, appDeleteJob(f.app.ID)); err != nil {
		t.Fatalf("retry delete must converge, got: %v", err)
	}
	if _, ok := f.fk.containers["cid-app"]; ok {
		t.Error("container survived the retry sweep")
	}
}

func TestAppDeleteRejectsBadPayload(t *testing.T) {
	f := newLifecycleFixture(t)
	for name, payload := range map[string]string{
		"missing app_id": `{}`,
		"unknown field":  `{"app_id":"x","extra":true}`,
		"not json":       `nope`,
	} {
		t.Run(name, func(t *testing.T) {
			job := &store.Job{ID: "j", Type: AppDeleteJobType, PayloadJSON: payload}
			err := f.dpl.AppDeleteHandler()(f.ctx, job)
			if err == nil || Retryable(err) || !strings.Contains(err.Error(), "E_VALIDATION") {
				t.Errorf("err = %v, want permanent E_VALIDATION", err)
			}
			if f.fk.creates != 0 || f.fk.removed != nil {
				t.Error("a bad payload must cause no side effects")
			}
		})
	}
}

func TestAppDeleteRetriesTransientDockerFailure(t *testing.T) {
	f := newLifecycleFixture(t)
	f.addContainer(f.app.ID, "cid-app", "running")
	// Break removal; the daemon outage must requeue, not fail.
	f.fk.mu.Lock()
	f.fk.removeErr = fmt.Errorf("read unix /var/run/docker.sock: connection reset")
	f.fk.mu.Unlock()

	err := f.dpl.AppDeleteHandler()(f.ctx, appDeleteJob(f.app.ID))
	if err == nil {
		t.Fatal("expected a retryable error")
	}
	if !Retryable(err) {
		t.Errorf("err = %v, want RetryableError", err)
	}
	// The app row is still present: rows purge only after containers,
	// so a retry has something left to clean up.
	if _, err := f.s.GetAppByID(f.ctx, f.app.ID); err != nil {
		t.Errorf("app row should survive a retried teardown: %v", err)
	}
}

func TestDomainSyncRegeneratesRoutes(t *testing.T) {
	f := newLifecycleFixture(t)
	// The API already deleted the row; the worker only replays routes.
	f.routableDeployment("keep.example.com", 20081)
	// Seed one proxy apply while the hostname still routes, so the
	// replay has a recorded hash to diverge from.
	if err := RegenerateRoutes(f.ctx, f.s, f.fc, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed initial routes: %v", err)
	}
	appliesBefore := len(f.fc.applies)
	if appliesBefore == 0 {
		t.Fatal("seed route apply produced no proxy config")
	}
	// Detach one hostname so there is a change to replay.
	if err := f.s.DeleteDomain(f.ctx, f.app.ID, "keep.example.com"); err != nil {
		t.Fatalf("delete domain: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"app_id": f.app.ID})
	job := &store.Job{ID: store.NewID(), Type: DomainSyncJobType, PayloadJSON: string(payload)}
	if err := f.dpl.DomainSyncHandler()(f.ctx, job); err != nil {
		t.Fatalf("domain sync: %v", err)
	}
	// The replay issued a fresh apply that drops the removed hostname.
	if len(f.fc.applies) <= appliesBefore {
		t.Fatalf("domain sync issued no new proxy apply (%d before, %d after)",
			appliesBefore, len(f.fc.applies))
	}
	last, _ := json.Marshal(f.fc.applies[len(f.fc.applies)-1])
	if strings.Contains(string(last), "keep.example.com") {
		t.Errorf("proxy still routes the removed hostname: %s", last)
	}
}

func TestDomainSyncWithoutRouteEngineFails(t *testing.T) {
	f := newLifecycleFixture(t)
	f.dpl.Caddy = nil
	payload, _ := json.Marshal(map[string]string{"app_id": f.app.ID})
	job := &store.Job{ID: store.NewID(), Type: DomainSyncJobType, PayloadJSON: string(payload)}
	err := f.dpl.DomainSyncHandler()(f.ctx, job)
	if err == nil || Retryable(err) || !strings.Contains(err.Error(), "E_CADDY_APPLY") {
		t.Errorf("err = %v, want permanent E_CADDY_APPLY", err)
	}
}
