package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- apps ----------------------------------------------------------

func TestCreateAppAndLookup(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	app, err := s.CreateApp(ctx, CreateAppInput{
		Name: "web", ConfigJSON: `{"name":"web","image":"nginx","port":80}`,
		Actor: "cli",
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if app.ID == "" || app.Name != "web" || app.ConfigVersion != 1 {
		t.Errorf("app = %+v", app)
	}
	if app.DesiredState != "running" || app.Deleting {
		t.Errorf("defaults: desired=%q deleting=%v", app.DesiredState, app.Deleting)
	}

	byName, err := s.GetAppByName(ctx, "web")
	if err != nil || byName.ID != app.ID {
		t.Errorf("GetAppByName = %v, %v", byName, err)
	}
	byID, err := s.GetAppByID(ctx, app.ID)
	if err != nil || byID.Name != "web" {
		t.Errorf("GetAppByID = %v, %v", byID, err)
	}
	if _, err := s.GetAppByName(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing app = %v, want ErrNotFound", err)
	}
}

func TestCreateAppDuplicateName(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.CreateApp(ctx, CreateAppInput{Name: "web", ConfigJSON: "{}", Actor: "cli"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.CreateApp(ctx, CreateAppInput{Name: "web", ConfigJSON: "{}", Actor: "cli"})
	if !errors.Is(err, ErrAppNameExists) {
		t.Errorf("duplicate = %v, want ErrAppNameExists", err)
	}
}

func TestListAppsOrdered(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if _, err := s.CreateApp(ctx, CreateAppInput{Name: n, ConfigJSON: "{}", Actor: "cli"}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	apps, err := s.ListApps(ctx)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 3 || apps[0].Name != "alpha" || apps[2].Name != "zeta" {
		t.Errorf("order = %v", []string{apps[0].Name, apps[1].Name, apps[2].Name})
	}
}

// --- deployments lifecycle --------------------------------------

func newDeployment(t *testing.T, s *Store, appID string) *Deployment {
	t.Helper()
	jobID := NewID()
	now := time.Now().UTC().UnixMilli()
	if _, err := s.db.Exec(`
		INSERT INTO jobs (id, app_id, type, payload_json, next_run_at, created_by, created_at, updated_at)
		VALUES (?, ?, 'deploy', '{}', ?, 'cli', ?, ?)`, jobID, appID, now, now, now); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	d, err := s.CreateDeployment(context.Background(), CreateDeploymentInput{
		ID: NewID(), AppID: appID, JobID: jobID, Kind: "deploy",
		ImageRef:           "nginx@sha256:" + strings.Repeat("ab", 32),
		ConfigSnapshotJSON: `{"name":"web"}`, ConfigVersion: 1,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	return d
}

func TestDeploymentSeqIncrements(t *testing.T) {
	s := openTest(t)
	appID := createApp(t, s, "web")
	d1 := newDeployment(t, s, appID)
	d2 := newDeployment(t, s, appID)
	if d1.Seq != 1 || d2.Seq != 2 {
		t.Errorf("seq = %d, %d; want 1, 2 (UNIQUE(app_id, seq))", d1.Seq, d2.Seq)
	}
	if d1.State != DepPending {
		t.Errorf("initial state = %q, want pending", d1.State)
	}
}

func TestMarkDeploymentRunningIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d := newDeployment(t, s, appID)

	if err := s.MarkDeploymentRunning(ctx, d.ID); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, err := s.GetDeployment(ctx, d.ID)
	if err != nil || first.State != DepRunning || first.StartedAt == nil {
		t.Fatalf("after first: %+v %v", first, err)
	}
	// Re-run: must not error or move started_at.
	time.Sleep(5 * time.Millisecond)
	if err := s.MarkDeploymentRunning(ctx, d.ID); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, _ := s.GetDeployment(ctx, d.ID)
	if !second.StartedAt.Equal(*first.StartedAt) {
		t.Errorf("started_at moved on re-run: %v → %v", first.StartedAt, second.StartedAt)
	}
	if err := s.MarkDeploymentRunning(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id = %v, want ErrNotFound", err)
	}
}

func TestAllocateHostPortRangeUniquenessAndIdempotence(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d1 := newDeployment(t, s, appID)
	d2 := newDeployment(t, s, appID)

	p1, err := s.AllocateHostPort(ctx, d1.ID)
	if err != nil {
		t.Fatalf("alloc 1: %v", err)
	}
	if p1 < 20000 || p1 > 29999 {
		t.Errorf("port %d outside managed range 20000-29999", p1)
	}
	// Idempotent: second admission call keeps the same port.
	again, err := s.AllocateHostPort(ctx, d1.ID)
	if err != nil || again != p1 {
		t.Errorf("re-alloc = %d, %v; want %d, nil", again, err, p1)
	}
	p2, err := s.AllocateHostPort(ctx, d2.ID)
	if err != nil {
		t.Fatalf("alloc 2: %v", err)
	}
	if p2 == p1 {
		t.Errorf("both deployments got port %d (deployments_port uniqueness)", p1)
	}

	// A failed/superseded deployment's port is reusable; an active one is not.
	if err := s.FailDeployment(ctx, d1.ID, "health", "E_HEALTH_FAILED", "no"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	p3, err := s.AllocateHostPort(ctx, newDeployment(t, s, appID).ID)
	if err != nil {
		t.Fatalf("alloc 3: %v", err)
	}
	if p3 == p2 {
		t.Errorf("active port %d reallocated", p2)
	}
}

func TestFailDeploymentRecordsStepAndStays(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d := newDeployment(t, s, appID)

	if err := s.MarkDeploymentRunning(ctx, d.ID); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := s.FailDeployment(ctx, d.ID, "health", "E_HEALTH_FAILED", "probe never passed"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != DepFailed || got.FailureStep == nil || *got.FailureStep != "health" ||
		got.FailureCode == nil || *got.FailureCode != "E_HEALTH_FAILED" || got.FinishedAt == nil {
		t.Errorf("failed deployment = %+v", got)
	}
	// First failure wins (a later re-run cannot overwrite the record).
	if err := s.FailDeployment(ctx, d.ID, "create", "E_CREATE", "later"); err != nil {
		t.Fatalf("second fail: %v", err)
	}
	got, _ = s.GetDeployment(ctx, d.ID)
	if *got.FailureStep != "health" {
		t.Errorf("failure record overwritten: step=%q", *got.FailureStep)
	}
}

func TestSucceedDeploymentSupersedesPreviousAndIsRerunnable(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	first := newDeployment(t, s, appID)
	if err := s.MarkDeploymentRunning(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SucceedDeployment(ctx, appID, first.ID); err != nil {
		t.Fatalf("succeed first: %v", err)
	}
	app, _ := s.GetAppByID(ctx, appID)
	if app.CurrentDeploymentID == nil || *app.CurrentDeploymentID != first.ID {
		t.Fatalf("current = %v, want first", app.CurrentDeploymentID)
	}

	second := newDeployment(t, s, appID)
	if err := s.MarkDeploymentRunning(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SucceedDeployment(ctx, appID, second.ID); err != nil {
		t.Fatalf("succeed second: %v", err)
	}
	// Re-run after a simulated crash before job finish: safe.
	if err := s.SucceedDeployment(ctx, appID, second.ID); err != nil {
		t.Fatalf("re-run finalize: %v", err)
	}

	prev, _ := s.GetDeployment(ctx, first.ID)
	if prev.State != DepSuperseded {
		t.Errorf("previous state = %q, want superseded", prev.State)
	}
	cur, _ := s.GetDeployment(ctx, second.ID)
	if cur.State != DepSucceeded || cur.FinishedAt == nil {
		t.Errorf("current = %+v", cur)
	}
	app, _ = s.GetAppByID(ctx, appID)
	if *app.CurrentDeploymentID != second.ID {
		t.Errorf("current deployment = %v, want second", *app.CurrentDeploymentID)
	}
}

// --- timeline --------------------------------------------------

func TestDeploymentTimelineOrderAndDetail(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d := newDeployment(t, s, appID)

	steps := []struct{ step, status string }{
		{"validate", EvStart}, {"validate", EvOK},
		{"pull", EvStart}, {"pull", EvOK},
		{"health", EvFail},
	}
	for _, st := range steps {
		if err := s.AppendDeploymentEvent(ctx, d.ID, st.step, st.status,
			map[string]any{"step": st.step}); err != nil {
			t.Fatalf("append %s/%s: %v", st.step, st.status, err)
		}
	}
	events, err := s.ListDeploymentEvents(ctx, d.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != len(steps) {
		t.Fatalf("events = %d, want %d", len(events), len(steps))
	}
	for i, ev := range events {
		if ev.Step != steps[i].step || ev.Status != steps[i].status {
			t.Errorf("event[%d] = %s/%s, want %s/%s", i, ev.Step, ev.Status, steps[i].step, steps[i].status)
		}
		if ev.TS.IsZero() {
			t.Errorf("event[%d] missing timestamp", i)
		}
		if !strings.Contains(string(ev.Detail), `"step"`) {
			t.Errorf("event[%d] detail = %q", i, ev.Detail)
		}
	}
}

// --- ownership ------------------------------------------

func TestDeploymentOwnsContainer(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d := newDeployment(t, s, appID)

	if err := s.SetDeploymentContainer(ctx, d.ID, "c-123", "ts-web-1"); err != nil {
		t.Fatalf("set container: %v", err)
	}
	ok, err := s.DeploymentOwnsContainer(ctx, appID, "c-123")
	if err != nil || !ok {
		t.Errorf("own container = %v, %v; want true", ok, err)
	}
	// Wrong app: refused even though the container exists on another app.
	otherApp := createApp(t, s, "other")
	if ok, _ := s.DeploymentOwnsContainer(ctx, otherApp, "c-123"); ok {
		t.Error("container ownership leaked across apps")
	}
	if ok, _ := s.DeploymentOwnsContainer(ctx, appID, "c-unknown"); ok {
		t.Error("unknown container reported as owned")
	}
}

// --- audit append-only -----------------------------------------

func TestAuditIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.AppendAudit(ctx, "worker", "container.refuse", "container", "c-1",
		"denied", map[string]any{"code": "E_NOT_MANAGED"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := s.db.Exec(`UPDATE audit_events SET result = 'allowed'`); err == nil {
		t.Error("UPDATE on audit_events must be rejected (append-only trigger)")
	}
	if _, err := s.db.Exec(`DELETE FROM audit_events`); err == nil {
		t.Error("DELETE on audit_events must be rejected (append-only trigger)")
	}
}

// --- app env ---------------------------------------------------
// Env coverage lives with the env store tests: round trip, kind
// switching, and mismatched payload rejection.

// --- active domains ------------------

func TestListActiveDomains(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	now := time.Now().UTC().UnixMilli()
	for _, row := range []struct {
		host, status string
	}{
		{"blog.example.com", "active"},
		{"pending.example.com", "pending_dns"},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO domains (id, app_id, hostname, status, created_at) VALUES (?, ?, ?, ?, ?)`,
			NewID(), appID, row.host, row.status, now); err != nil {
			t.Fatalf("insert domain: %v", err)
		}
	}
	hosts, err := s.ListActiveDomains(ctx, appID)
	if err != nil {
		t.Fatalf("ListActiveDomains: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "blog.example.com" {
		t.Errorf("active hosts = %v, want [blog.example.com]", hosts)
	}
}
