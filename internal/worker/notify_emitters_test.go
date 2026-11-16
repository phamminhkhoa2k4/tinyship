package worker

// Emitter tests: the three worker-side producers queue their events at
// the right moment — a terminal deploy failure, a successful rollback,
// and the rising edge of the OOM counter — without ever affecting the
// operation itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/store"
)

func notifyEventsOfType(t *testing.T, s *store.Store, typ string) []*store.NotifyEvent {
	t.Helper()
	events, err := s.ListNotifyEvents(context.Background(), 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []*store.NotifyEvent
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func TestDeployFailureQueuesNotification(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.fk.pullErr = errors.New("manifest unknown: manifest unknown")
	if err := f.run(1, 5); err == nil {
		t.Fatal("expected failure")
	}

	events := notifyEventsOfType(t, f.s, "deploy_failed")
	if len(events) != 1 {
		t.Fatalf("deploy_failed events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.AppName != "web" || ev.Title == "" {
		t.Errorf("app/title = %q/%q", ev.AppName, ev.Title)
	}
	detail := map[string]any{}
	if err := json.Unmarshal([]byte(ev.Detail), &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["step"] != "pull" || detail["code"] != "E_IMAGE_NOT_FOUND" {
		t.Errorf("detail = %v, want step pull / E_IMAGE_NOT_FOUND", detail)
	}
	if !strings.Contains(fmt.Sprint(detail["error"]), "manifest unknown") {
		t.Errorf("error detail = %v", detail["error"])
	}
	if !strings.HasSuffix(ev.DedupKey, ":"+f.depID) {
		t.Errorf("dedup_key = %q, want one event per deployment", ev.DedupKey)
	}

	// A transient failure with attempts left is not terminal: no event
	// until the deploy actually fails for good.
	f2 := newDeployFixture(t, happyConfig)
	f2.fk.pullErr = errors.New("read tcp: connection reset by peer")
	if err := f2.run(1, 5); err == nil {
		t.Fatal("expected retryable failure")
	}
	if n := len(notifyEventsOfType(t, f2.s, "deploy_failed")); n != 0 {
		t.Errorf("retryable failure queued %d events, want 0", n)
	}
}

func TestRollbackSuccessQueuesNotification(t *testing.T) {
	// Same fixture shape with a rollback-kind deployment.
	s := testStore(t)
	ctx := context.Background()
	cfgJSON := happyConfig
	cfg, err := appconfig.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	app, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: cfg.Name, ConfigJSON: cfgJSON, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	depID := store.NewID()
	job, err := s.CreateJob(ctx, store.CreateJobInput{
		AppID: &app.ID, Type: DeployJobType,
		PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, depID),
		MaxAttempts: 5, CreatedBy: "cli",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		ID: depID, AppID: app.ID, JobID: job.ID, Kind: "rollback",
		ImageRef:           cfg.Image + ":v1.8",
		ConfigSnapshotJSON: cfgJSON,
		ConfigVersion:      app.ConfigVersion,
	}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	fk := newFakeDocker()
	dplr := NewDeployer(s, fk, slog.New(slog.DiscardHandler), "test-worker")
	pinHostFacts(dplr)
	dplr.HealthConsecutive = 1
	j := *job
	j.Attempts, j.MaxAttempts = 1, 5
	if err := dplr.Handler()(ctx, &j); err != nil {
		t.Fatalf("rollback deploy: %v", err)
	}

	events := notifyEventsOfType(t, s, "rollback")
	if len(events) != 1 {
		t.Fatalf("rollback events = %d, want 1", len(events))
	}
	detail := map[string]any{}
	_ = json.Unmarshal([]byte(events[0].Detail), &detail)
	if auto, ok := detail["auto"].(bool); !ok || auto {
		t.Errorf("detail.auto = %v, want false until auto-rollback exists", detail["auto"])
	}

	// A plain deploy of the same shape must not queue a rollback event.
	if n := len(notifyEventsOfType(t, s, "deploy_failed")); n != 0 {
		t.Errorf("deploy_failed events after a good rollback = %d, want 0", n)
	}
}

func TestStatsSampleQueuesOomNotificationOnRisingCountOnly(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	if n := len(notifyEventsOfType(t, f.s, "oom")); n != 0 {
		t.Fatalf("healthy sample queued %d oom events", n)
	}

	d := f.deployment()
	f.fk.mu.Lock()
	c := f.fk.containers[*d.ContainerID]
	c.oom = true
	c.state = "exited"
	f.fk.mu.Unlock()

	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("second sample: %v", err)
	}
	events := notifyEventsOfType(t, f.s, "oom")
	if len(events) != 1 {
		t.Fatalf("oom events after the kill = %d, want 1", len(events))
	}
	if events[0].AppName != "web" {
		t.Errorf("app = %q, want web", events[0].AppName)
	}
	detail := map[string]any{}
	_ = json.Unmarshal([]byte(events[0].Detail), &detail)
	if detail["oom_count"] != float64(1) {
		t.Errorf("oom_count = %v, want 1", detail["oom_count"])
	}

	// Re-sampling the same dead container must not notify again.
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("third sample: %v", err)
	}
	if n := len(notifyEventsOfType(t, f.s, "oom")); n != 1 {
		t.Errorf("oom events after re-sample = %d, want still 1", n)
	}
}
