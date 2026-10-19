package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

func managedInfo(id, state string) docker.ContainerInfo {
	return docker.ContainerInfo{
		ID:    "ctr-" + id,
		State: state,
		Labels: map[string]string{
			policy.LabelManaged: policy.ManagedValue,
			policy.LabelApp:     id,
		},
	}
}

func TestRunningAppSetKeepsOnlyRunningLabeledContainers(t *testing.T) {
	infos := []docker.ContainerInfo{
		managedInfo("app-up", "running"),
		managedInfo("app-exited", "exited"),
		{ID: "no-label", State: "running", Labels: map[string]string{}},
	}
	got := runningAppSet(infos)
	if len(got) != 1 || !got["app-up"] {
		t.Errorf("runningAppSet = %v, want exactly [app-up]", got)
	}
}

// seededAppStore returns a store holding one app that wants to run.
func seededAppStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	s := testStore(t)
	app, err := s.CreateApp(context.Background(), store.CreateAppInput{
		Name: "web", ConfigJSON: `{"name":"web","image":"nginx"}`, Actor: "test",
	})
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return s, app.ID
}

// collectReconcile logs one report and returns the level it used.
func collectReconcile(t *testing.T, s *store.Store, running map[string]bool) (slog.Level, string) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logReconcileReport(context.Background(), s, running, logger)
	rec := buf.String()
	switch {
	case strings.Contains(rec, "level=WARN"):
		return slog.LevelWarn, rec
	case strings.Contains(rec, "level=INFO"):
		return slog.LevelInfo, rec
	}
	return slog.LevelDebug, rec
}

func TestLogReconcileReportWarnsOnlyOnDrift(t *testing.T) {
	s, appID := seededAppStore(t)

	// The app wants to run with no container behind it: drift, at warn
	// level, with the app named so the operator knows what to deploy.
	lvl, rec := collectReconcile(t, s, map[string]bool{})
	if lvl != slog.LevelWarn {
		t.Fatalf("missing container: level = %v, want WARN; record=%s", lvl, rec)
	}
	if !strings.Contains(rec, "need_deploy=web") {
		t.Errorf("record does not name the app to deploy: %s", rec)
	}
	if !strings.Contains(rec, "report only") {
		t.Errorf("record must state nothing was remediated: %s", rec)
	}

	// The container is up: in sync, at info level.
	lvl, rec = collectReconcile(t, s, map[string]bool{appID: true})
	if lvl != slog.LevelInfo {
		t.Fatalf("in-sync state: level = %v, want INFO; record=%s", lvl, rec)
	}
	if !strings.Contains(rec, "up_to_date=web") || !strings.Contains(rec, "need_deploy=-") {
		t.Errorf("in-sync record = %s, want up_to_date=web and need_deploy=-", rec)
	}

	// A running container the database ignores is drift as well.
	lvl, rec = collectReconcile(t, s, map[string]bool{appID: true, "stray": true})
	if lvl != slog.LevelWarn || !strings.Contains(rec, "unknown_running=stray") {
		t.Errorf("orphan record = %s (level %v), want WARN with unknown_running=stray", rec, lvl)
	}
}

func TestBootReconcileSkipsWithoutDocker(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	// A worker started without a Docker client must not panic and must
	// not pretend it checked anything.
	bootReconcile(context.Background(), testStore(t), nil, logger)
	if buf.Len() != 0 {
		t.Errorf("nil docker logged %q, want silence", buf.String())
	}
}

func TestJoinOrDash(t *testing.T) {
	if got := joinOrDash(nil); got != "-" {
		t.Errorf("joinOrDash(nil) = %q, want -", got)
	}
	if got := joinOrDash([]string{"a", "b"}); got != "a,b" {
		t.Errorf("joinOrDash = %q, want a,b", got)
	}
}
