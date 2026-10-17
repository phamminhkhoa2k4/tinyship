package store

// Budget settings, running-app totals, and rollback-source lookup tests.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBudgetSettingsDefaults(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	bs, err := s.GetBudgetSettings(ctx)
	if err != nil {
		t.Fatalf("GetBudgetSettings: %v", err)
	}
	if bs.BudgetMB != 0 {
		t.Errorf("BudgetMB = %d, want 0 (auto from MemTotal)", bs.BudgetMB)
	}
	if bs.MinFreeDiskMB != DefaultMinFreeDiskMB {
		t.Errorf("MinFreeDiskMB = %d, want %d", bs.MinFreeDiskMB, DefaultMinFreeDiskMB)
	}
	if bs.CPUOvercommit != DefaultCPUOvercommit {
		t.Errorf("CPUOvercommit = %g, want %g", bs.CPUOvercommit, DefaultCPUOvercommit)
	}
}

func TestBudgetSettingsOverrideAndValidate(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.SetBudgetMB(ctx, 2048); err != nil {
		t.Fatalf("SetBudgetMB: %v", err)
	}
	if err := s.SetMinFreeDiskMB(ctx, 5120); err != nil {
		t.Fatalf("SetMinFreeDiskMB: %v", err)
	}
	if err := s.PutSetting(ctx, CPUOvercommitKey, "1.5"); err != nil {
		t.Fatalf("put cpu overcommit: %v", err)
	}
	bs, err := s.GetBudgetSettings(ctx)
	if err != nil {
		t.Fatalf("GetBudgetSettings: %v", err)
	}
	if bs.BudgetMB != 2048 || bs.MinFreeDiskMB != 5120 || bs.CPUOvercommit != 1.5 {
		t.Errorf("settings = %+v, want 2048/5120/1.5", bs)
	}

	// 0 restores the auto budget.
	if err := s.SetBudgetMB(ctx, 0); err != nil {
		t.Fatalf("reset budget: %v", err)
	}
	bs, _ = s.GetBudgetSettings(ctx)
	if bs.BudgetMB != 0 {
		t.Errorf("BudgetMB after reset = %d, want 0", bs.BudgetMB)
	}

	// Negative values are rejected before touching the store.
	if err := s.SetBudgetMB(ctx, -1); err == nil {
		t.Error("negative budget accepted; want rejection")
	}
}

func TestBudgetSettingsRejectGarbage(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if err := s.PutSetting(ctx, BudgetMBKey, "lots"); err != nil {
		t.Fatalf("poison budget: %v", err)
	}
	if _, err := s.GetBudgetSettings(ctx); err == nil {
		t.Error("garbage budget accepted; want a loud error")
	}
	if err := s.PutSetting(ctx, CPUOvercommitKey, "0"); err != nil {
		t.Fatalf("poison overcommit: %v", err)
	}
	if _, err := s.GetBudgetSettings(ctx); err == nil {
		t.Error("zero cpu overcommit accepted; want rejection")
	}
}

func TestListRunningAppsExcludesStoppedAndDeleting(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	running := createApp(t, s, "a-running")
	stopped := createApp(t, s, "b-stopped")
	deleting := createApp(t, s, "c-deleting")
	if _, err := s.db.Exec(
		`UPDATE apps SET desired_state='stopped' WHERE id = ?`, stopped); err != nil {
		t.Fatalf("stop app: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE apps SET deleting=1 WHERE id = ?`, deleting); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}

	apps, err := s.ListRunningApps(ctx)
	if err != nil {
		t.Fatalf("ListRunningApps: %v", err)
	}
	if len(apps) != 1 || apps[0].ID != running {
		t.Errorf("running apps = %v, want only %s", apps, running)
	}
}

// --- rollback source ------------------------------------------------

// insertJobForApp adds the deploy job row a deployment must reference.
func insertJobForApp(ctx context.Context, s *Store, jobID, appID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, app_id, type, payload_json, next_run_at, created_by, created_at, updated_at)
		VALUES (?, ?, 'deploy', '{}', ?, 'cli', ?, ?)`,
		jobID, appID, nowMS(), nowMS(), nowMS())
	return err
}

func nowMS() int64 { return time.Now().UTC().UnixMilli() }

func TestFindRollbackSourcePrefersLatestGood(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	d1 := newDeployment(t, s, appID) // gains a digest below
	d2 := newDeployment(t, s, appID)
	d3 := newDeployment(t, s, appID)
	for _, d := range []*Deployment{d1, d2, d3} {
		if _, err := s.db.Exec(
			`UPDATE deployments SET image_digest = ? WHERE id = ?`,
			"sha256:"+d.ID, d.ID); err != nil {
			t.Fatalf("set digest: %v", err)
		}
	}
	// d1 succeeded, then d2 superseded it, then d3 is current.
	if err := s.SucceedDeployment(ctx, appID, d1.ID); err != nil {
		t.Fatalf("succeed d1: %v", err)
	}
	if err := s.SucceedDeployment(ctx, appID, d2.ID); err != nil {
		t.Fatalf("succeed d2: %v", err)
	}
	if err := s.SucceedDeployment(ctx, appID, d3.ID); err != nil {
		t.Fatalf("succeed d3: %v", err)
	}

	// From the current deployment: the previous good release is d2.
	src, err := s.FindRollbackSource(ctx, appID, d3.ID)
	if err != nil {
		t.Fatalf("FindRollbackSource: %v", err)
	}
	if src.ID != d2.ID {
		t.Errorf("rollback source = %s (seq %d), want %s (seq %d)",
			src.ID, src.Seq, d2.ID, d2.Seq)
	}
	// The lookup always means "latest known-good other than X", so
	// excluding the oldest release yields the newest one (d3).
	src, err = s.FindRollbackSource(ctx, appID, d1.ID)
	if err != nil {
		t.Fatalf("FindRollbackSource from d1: %v", err)
	}
	if src.ID != d3.ID {
		t.Errorf("rollback source = %s, want %s", src.ID, d3.ID)
	}
}

func TestFindRollbackSourceNeedsDigestAndGoodState(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	// A failed deployment with a digest is not a rollback target.
	failed := newDeployment(t, s, appID)
	if _, err := s.db.Exec(
		`UPDATE deployments SET image_digest='sha256:x', state='failed' WHERE id = ?`,
		failed.ID); err != nil {
		t.Fatalf("fail deployment: %v", err)
	}
	// A succeeded deployment without a digest cannot be redeployed by
	// digest and must be skipped.
	noDigest := newDeployment(t, s, appID)
	if err := s.SucceedDeployment(ctx, appID, noDigest.ID); err != nil {
		t.Fatalf("succeed no-digest: %v", err)
	}

	if _, err := s.FindRollbackSource(ctx, appID, noDigest.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindRollbackSource = %v, want ErrNotFound", err)
	}
}

func TestFindDeploymentBySeq(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d := newDeployment(t, s, appID)

	got, err := s.FindDeploymentBySeq(ctx, appID, d.Seq)
	if err != nil {
		t.Fatalf("FindDeploymentBySeq: %v", err)
	}
	if got.ID != d.ID {
		t.Errorf("deployment = %s, want %s", got.ID, d.ID)
	}
	if _, err := s.FindDeploymentBySeq(ctx, appID, d.Seq+999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing seq = %v, want ErrNotFound", err)
	}
}

func TestCreateDeploymentRecordsRollbackOf(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	target := newDeployment(t, s, appID)

	jobID := NewID()
	if err := insertJobForApp(ctx, s, jobID, appID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	rb, err := s.CreateDeployment(ctx, CreateDeploymentInput{
		ID: NewID(), AppID: appID, JobID: jobID, Kind: "rollback",
		ImageRef: target.ImageRef, ConfigSnapshotJSON: target.ConfigSnapshotJSON,
		ConfigVersion: target.ConfigVersion, RollbackOfID: &target.ID,
	})
	if err != nil {
		t.Fatalf("CreateDeployment rollback: %v", err)
	}
	if rb.RollbackOfID == nil || *rb.RollbackOfID != target.ID {
		t.Errorf("rollback_of_id = %v, want %s", rb.RollbackOfID, target.ID)
	}
}
