package worker

// Admission-control boundary tests. Each case mirrors a boundary the
// budget algorithm documents: exact fit, one MiB over, a raised grant on
// a running app, a deleted app returning its grant, two apps sharing one
// budget, auto lowering to stop-first, and an auto budget that follows
// MemTotal after a host upgrade. Force, the disk floor, and the CPU
// warning have their own cases at the end.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

// memConfig builds a healthy config with a chosen memory grant.
func memConfig(name string, memMB int) string {
	return fmt.Sprintf(`{
	"name": %q,
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	%s,
	"resources": {"memory_mb": %d, "cpu_millis": 100, "pids_limit": 64}
}`, name, execHealthFast, memMB)
}

// cpuConfig builds a healthy config with chosen memory and CPU grants.
func cpuConfig(name string, memMB, cpuMillis int) string {
	return fmt.Sprintf(`{
	"name": %q,
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	%s,
	"resources": {"memory_mb": %d, "cpu_millis": %d, "pids_limit": 64}
}`, name, execHealthFast, memMB, cpuMillis)
}

// queueRedeploy stores a new config on the app and queues the next
// deployment the way the API does, so the pipeline runs with an already
// deployed predecessor (old container in play for peak accounting).
func (f *deployFixture) queueRedeploy(cfgJSON string, force bool) {
	f.t.Helper()
	if _, err := f.s.DB().Exec(
		`UPDATE apps SET config_json = ?, config_version = config_version + 1 WHERE id = ?`,
		cfgJSON, f.app.ID); err != nil {
		f.t.Fatalf("update app config: %v", err)
	}
	app, err := f.s.GetAppByID(f.ctx, f.app.ID)
	if err != nil {
		f.t.Fatalf("reload app: %v", err)
	}
	f.app = app

	depID := store.NewID()
	payload := fmt.Sprintf(`{"deployment_id":%q`, depID)
	if force {
		payload += `,"force":true`
	}
	payload += `}`
	job, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType,
		PayloadJSON: payload, MaxAttempts: 5, CreatedBy: "cli",
	})
	if err != nil {
		f.t.Fatalf("create redeploy job: %v", err)
	}
	prev := f.depID
	dep, err := f.s.CreateDeployment(f.ctx, store.CreateDeploymentInput{
		ID: depID, AppID: f.app.ID, JobID: job.ID, Kind: "deploy",
		ImageRef:           "traefik/whoami:v1.8",
		ConfigSnapshotJSON: cfgJSON, ConfigVersion: f.app.ConfigVersion,
		PreviousDeploymentID: &prev,
	})
	if err != nil {
		f.t.Fatalf("create redeploy deployment: %v", err)
	}
	f.depID, f.job, f.depRec = depID, job, dep
}

// admissionDetail returns the detail map of the admission ok event.
func (f *deployFixture) admissionDetail() map[string]any {
	f.t.Helper()
	events, err := f.s.ListDeploymentEvents(f.ctx, f.depID)
	if err != nil {
		f.t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if ev.Step != "admission" || ev.Status != "ok" || len(ev.Detail) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Detail, &m); err != nil {
			f.t.Fatalf("parse admission detail: %v", err)
		}
		return m
	}
	f.t.Fatal("no admission ok event with detail")
	return nil
}

func TestAdmissionAcceptsExactlyAtBudget(t *testing.T) {
	f := newDeployFixture(t, memConfig("web", 256))
	if err := f.s.SetBudgetMB(f.ctx, 256); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	// First deploy: nobody else is running, needed is the new grant.
	// 0 + 256 == 256 must be accepted.
	if err := f.run(1, 1); err != nil {
		t.Fatalf("deploy at exact budget: %v", err)
	}
	d := f.deployment()
	if d.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded", d.State)
	}
	detail := f.admissionDetail()
	if detail["budget_mb"].(float64) != 256 || detail["needed_mb"].(float64) != 256 {
		t.Errorf("admission detail = %v, want budget 256 needed 256", detail)
	}
}

func TestAdmissionRejectsOneMiBOverBudget(t *testing.T) {
	f := newDeployFixture(t, memConfig("web", 257))
	if err := f.s.SetBudgetMB(f.ctx, 256); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_BUDGET_EXCEEDED") {
		t.Fatalf("deploy over by 1 MiB: err = %v, want E_BUDGET_EXCEEDED", err)
	}
	d := f.deployment()
	if d.State != "failed" || d.FailureStep == nil || *d.FailureStep != "admission" ||
		d.FailureCode == nil || *d.FailureCode != "E_BUDGET_EXCEEDED" {
		t.Fatalf("deployment = %q step %v code %v, want failed at admission",
			d.State, d.FailureStep, d.FailureCode)
	}
	if d.FailureMessage == nil || !strings.Contains(*d.FailureMessage, "over by 1 MiB") {
		t.Errorf("failure message = %v, must name the 1 MiB overrun", d.FailureMessage)
	}
	if f.fk.pulls != 0 {
		t.Errorf("pulls = %d, want 0: a refused deploy must not pull", f.fk.pulls)
	}
}

func TestAdmissionCountsRaisedGrantOnRunningApp(t *testing.T) {
	f := newDeployFixture(t, memConfig("web", 128))
	if err := f.s.SetBudgetMB(f.ctx, 300); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if err := f.run(1, 1); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// Raising the grant to 256 makes the parallel peak 128+256=384,
	// which no longer fits; auto lowers to stop-first (needed 256).
	f.queueRedeploy(memConfig("web", 256), false)
	if err := f.run(1, 1); err != nil {
		t.Fatalf("redeploy with raised grant: %v", err)
	}
	d := f.deployment()
	if d.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded", d.State)
	}
	if d.StrategyEffective == nil || *d.StrategyEffective != "stop-first" {
		t.Errorf("strategy = %v, want auto lowered to stop-first", d.StrategyEffective)
	}
	detail := f.admissionDetail()
	if detail["needed_mb"].(float64) != 256 {
		t.Errorf("needed_mb = %v, want 256 after the downgrade", detail["needed_mb"])
	}
}

func TestAdmissionReturnsBudgetWhenAppIsDeleted(t *testing.T) {
	f := newDeployFixtureIn(t, testStore(t), memConfig("web", 256))
	if err := f.s.SetBudgetMB(f.ctx, 256); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	// A second running app holds a grant that leaves no room for web.
	other := newDeployFixtureIn(t, f.s, memConfig("api", 256))
	_ = other
	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_BUDGET_EXCEEDED") {
		t.Fatalf("deploy while api holds a grant: err = %v, want rejection", err)
	}
	// Deleting api returns its grant; web now fits exactly.
	if _, err := f.s.DB().Exec(
		`UPDATE apps SET deleting = 1 WHERE name = 'api'`); err != nil {
		t.Fatalf("delete api: %v", err)
	}
	f.queueRedeploy(memConfig("web", 256), false)
	if err := f.run(1, 1); err != nil {
		t.Fatalf("deploy after api deleted: %v", err)
	}
	if d := f.deployment(); d.State != "succeeded" {
		t.Errorf("state = %q, want succeeded after budget returned", d.State)
	}
}

func TestAdmissionTwoAppsShareOneBudget(t *testing.T) {
	f := newDeployFixtureIn(t, testStore(t), memConfig("web", 256))
	if err := f.s.SetBudgetMB(f.ctx, 512); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	// web and api each hold 256: together they exactly fill the budget.
	api := newDeployFixtureIn(t, f.s, memConfig("api", 256))

	if err := f.run(1, 1); err != nil {
		t.Fatalf("deploy web: %v", err)
	}
	if err := api.run(1, 1); err != nil {
		t.Fatalf("deploy api: %v", err)
	}
	// A third app cannot slip past the two committed grants.
	third := newDeployFixtureIn(t, f.s, memConfig("cache", 64))
	err := third.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_BUDGET_EXCEEDED") {
		t.Fatalf("third deploy: err = %v, want rejection", err)
	}
	// Freeing one grant lets the third app in.
	if _, err := f.s.DB().Exec(
		`UPDATE apps SET desired_state = 'stopped' WHERE name = 'api'`); err != nil {
		t.Fatalf("stop api: %v", err)
	}
	third.queueRedeploy(memConfig("cache", 64), false)
	if err := third.run(1, 1); err != nil {
		t.Fatalf("third deploy after api stopped: %v", err)
	}
}

func TestAdmissionExplicitStartFirstNeverDowngrades(t *testing.T) {
	const cfg = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	` + execHealthFast + `,
	"strategy": "start-first",
	"resources": {"memory_mb": 256, "cpu_millis": 100, "pids_limit": 64}
}`
	f := newDeployFixture(t, cfg)
	if err := f.s.SetBudgetMB(f.ctx, 300); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if err := f.run(1, 1); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	// Peak 256+256=512 exceeds 300. The strategy was chosen explicitly,
	// so it must be refused rather than silently lowered.
	f.queueRedeploy(cfg, false)
	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_BUDGET_EXCEEDED") {
		t.Fatalf("explicit start-first over peak: err = %v, want rejection", err)
	}
	if d := f.deployment(); d.StrategyEffective == nil || *d.StrategyEffective != "start-first" {
		t.Errorf("strategy = %v, want untouched start-first", d.StrategyEffective)
	}
}

func TestAdmissionAutoBudgetFollowsMemTotal(t *testing.T) {
	f := newDeployFixtureIn(t, testStore(t), memConfig("web", 768))
	// No explicit budget: 2048 MiB host yields min(1024, 1024) = 1024.
	f.dplr.MemTotalMB = func() (int, error) { return 2048, nil }
	if err := f.run(1, 1); err != nil {
		t.Fatalf("deploy web on small host: %v", err)
	}
	// api does not fit beside web's grant (768+768=1536 > 1024).
	api := newDeployFixtureIn(t, f.s, memConfig("api", 768))
	api.dplr.MemTotalMB = func() (int, error) { return 2048, nil }
	err := api.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_BUDGET_EXCEEDED") {
		t.Fatalf("deploy api on small host: err = %v, want rejection", err)
	}
	// The VPS grows: without touching any setting the budget follows
	// MemTotal (4096 MiB host yields min(2048, 3072) = 2048).
	f.dplr.MemTotalMB = func() (int, error) { return 4096, nil }
	api.dplr.MemTotalMB = func() (int, error) { return 4096, nil }
	api.queueRedeploy(memConfig("api", 768), false)
	if err := api.run(1, 1); err != nil {
		t.Fatalf("deploy api after host upgrade: %v", err)
	}
}

func TestAdmissionForcePassesBudgetButRecordsAudit(t *testing.T) {
	f := newDeployFixture(t, memConfig("web", 512))
	if err := f.s.SetBudgetMB(f.ctx, 256); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	f.queueRedeploy(memConfig("web", 512), true)
	if err := f.run(1, 1); err != nil {
		t.Fatalf("forced deploy over budget: %v", err)
	}
	d := f.deployment()
	if d.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded under force", d.State)
	}
	detail := f.admissionDetail()
	warnings, _ := detail["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if w == "W_BUDGET_FORCED" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want W_BUDGET_FORCED", warnings)
	}
	n, err := f.s.CountAuditEvents(f.ctx, "admission.force", f.depID)
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("admission.force audit events = %d, want 1", n)
	}
}

func TestAdmissionDiskFloorRejectsEvenUnderForce(t *testing.T) {
	f := newDeployFixture(t, memConfig("web", 64))
	// The floor defaults to 3072 MiB; report less free space.
	f.dplr.DiskFreeMB = func(string) (int, error) { return 100, nil }
	f.queueRedeploy(memConfig("web", 64), true)
	err := f.run(1, 1)
	if err == nil || !strings.Contains(err.Error(), "E_DISK_LOW") {
		t.Fatalf("low disk under force: err = %v, want E_DISK_LOW", err)
	}
	if d := f.deployment(); d.State != "failed" ||
		d.FailureCode == nil || *d.FailureCode != "E_DISK_LOW" {
		t.Errorf("deployment = %q code %v, want failed with E_DISK_LOW", d.State, d.FailureCode)
	}
	if f.fk.pulls != 0 {
		t.Errorf("pulls = %d, want 0: the disk check runs before the pull", f.fk.pulls)
	}
}

func TestAdmissionCPUOvercommitWarns(t *testing.T) {
	// One vCPU with the default 2.0 overcommit caps the sum at 2000
	// cpu_millis; the two grants together exceed it.
	f := newDeployFixtureIn(t, testStore(t), cpuConfig("web", 64, 1500))
	f.dplr.NumCPUs = func() int { return 1 }
	api := newDeployFixtureIn(t, f.s, cpuConfig("api", 64, 1000))
	api.dplr.NumCPUs = func() int { return 1 }

	if err := f.run(1, 1); err != nil {
		t.Fatalf("deploy web: %v", err)
	}
	if err := api.run(1, 1); err != nil {
		t.Fatalf("deploy api: %v", err)
	}
	detail := api.admissionDetail()
	warnings, _ := detail["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if w == "W_CPU_OVERCOMMIT" {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want W_CPU_OVERCOMMIT", warnings)
	}
	if d := api.deployment(); d.State != "succeeded" {
		t.Errorf("state = %q, want succeeded: CPU overcommit only warns", d.State)
	}
}
