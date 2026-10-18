package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMarkAppDeleting(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	if err := s.MarkAppDeleting(ctx, appID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	app, err := s.GetAppByID(ctx, appID)
	if err != nil || !app.Deleting {
		t.Fatalf("app = %+v err=%v, want deleting=true", app, err)
	}
	// Budget math must stop counting the app the moment it is marked.
	running, err := s.ListRunningApps(ctx)
	if err != nil || len(running) != 0 {
		t.Errorf("running = %+v err=%v, want empty while deleting", running, err)
	}
	if err := s.MarkAppDeleting(ctx, "no-such-app"); !errors.Is(err, ErrNotFound) {
		t.Errorf("mark missing = %v, want ErrNotFound", err)
	}
}

// TestPurgeAppRemovesDependents covers the FK order the teardown job
// depends on: children first, then the app; jobs survive detached.
func TestPurgeAppRemovesDependents(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	dep := newDeployment(t, s, appID)
	if _, err := s.db.Exec(`
		INSERT INTO deployment_events (deployment_id, ts, step, status)
		VALUES (?, ?, 'pull', 'ok')`, dep.ID, time.Now().UTC().UnixMilli()); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if _, _, err := s.AddDomain(ctx, appID, "web.example.com"); err != nil {
		t.Fatalf("add domain: %v", err)
	}
	if err := s.SetAppEnv(ctx, appID, "TOKEN", EnvSecret, "", []byte("sealed")); err != nil {
		t.Fatalf("set env: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO container_state (app_id, state, updated_at)
		VALUES (?, 'running', ?)`, appID, time.Now().UTC().UnixMilli()); err != nil {
		t.Fatalf("insert container state: %v", err)
	}

	if err := s.PurgeApp(ctx, appID); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if _, err := s.GetAppByID(ctx, appID); !errors.Is(err, ErrNotFound) {
		t.Errorf("app after purge = %v, want ErrNotFound", err)
	}
	for _, q := range []struct {
		name, query string
	}{
		{"domains", `SELECT COUNT(*) FROM domains WHERE app_id = '` + appID + `'`},
		{"deployments", `SELECT COUNT(*) FROM deployments WHERE app_id = '` + appID + `'`},
		{"deployment_events", `SELECT COUNT(*) FROM deployment_events`},
		{"container_state", `SELECT COUNT(*) FROM container_state WHERE app_id = '` + appID + `'`},
		{"app_env", `SELECT COUNT(*) FROM app_env WHERE app_id = '` + appID + `'`},
	} {
		var n int
		if err := s.db.QueryRow(q.query).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.name, err)
		}
		if n != 0 {
			t.Errorf("%s rows after purge = %d, want 0", q.name, n)
		}
	}

	// The deploy job that backed the deployment keeps its row but loses
	// its app reference, or the FK would block the app delete.
	var jobAppID *string
	var jobState string
	if err := s.db.QueryRow(
		`SELECT app_id, state FROM jobs WHERE type = 'deploy'`).Scan(&jobAppID, &jobState); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if jobAppID != nil {
		t.Errorf("job app_id = %q, want NULL after purge", *jobAppID)
	}
}

func TestPurgeAppMissing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.PurgeApp(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("purge missing = %v, want ErrNotFound", err)
	}
}
