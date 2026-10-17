package worker

// Rollback pipeline tests: a rollback deployment must revalidate and
// run the TARGET release's config snapshot (image, digest, port,
// resources) rather than the app's current config, while env vars stay
// live from the store. Rolling back the image never rolls back the
// operator's current settings.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// queueRollback stores a rollback deployment whose snapshot pins a
// chosen digest and port, pointing at f's predecessor.
func (f *deployFixture) queueRollback(snapshotJSON, digest string) {
	f.t.Helper()
	depID := store.NewID()
	job, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType,
		PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, depID),
		MaxAttempts: 5, CreatedBy: "cli",
	})
	if err != nil {
		f.t.Fatalf("create rollback job: %v", err)
	}
	prev := f.depID
	dep, err := f.s.CreateDeployment(f.ctx, store.CreateDeploymentInput{
		ID: depID, AppID: f.app.ID, JobID: job.ID, Kind: "rollback",
		ImageRef:             "traefik/whoami@" + digest,
		ConfigSnapshotJSON:   snapshotJSON,
		ConfigVersion:        1,
		PreviousDeploymentID: &prev,
	})
	if err != nil {
		f.t.Fatalf("create rollback deployment: %v", err)
	}
	f.depID, f.job, f.depRec = depID, job, dep
}

func TestRollbackRunsTargetSnapshotNotCurrentConfig(t *testing.T) {
	// The app's current config says port 8080 and 256 MiB; the rollback
	// target ran port 9090 with 64 MiB and its own digest.
	f := newDeployFixture(t, memConfig("web", 256))
	targetDigest := "sha256:" + strings.Repeat("ab", 32)
	const targetSnapshot = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.7",
	"port": 9090,
	` + execHealthFast + `,
	"resources": {"memory_mb": 64, "cpu_millis": 100, "pids_limit": 64}
}`
	// The API pins the target's digest into the snapshot it stores, so
	// the pipeline pulls by digest even though the release once ran a
	// tag. Mirror that here.
	targetCfg, err := appconfig.Parse([]byte(targetSnapshot))
	if err != nil {
		t.Fatalf("parse target snapshot: %v", err)
	}
	targetCfg.Digest = targetDigest
	targetCfg.Tag = ""
	pinned, err := json.Marshal(targetCfg)
	if err != nil {
		t.Fatalf("marshal pinned snapshot: %v", err)
	}
	f.queueRollback(string(pinned), targetDigest)

	// Live env set after the target release: it must still apply.
	now := time.Now().UTC().UnixMilli()
	if _, err := f.s.DB().Exec(
		`INSERT INTO app_env (app_id, key, kind, value_plain, updated_at)
		 VALUES (?, 'MODE', 'plain', 'current', ?)`, f.app.ID, now); err != nil {
		t.Fatalf("insert env: %v", err)
	}

	var captured []string
	var capturedSpec *policy.ContainerSpec
	recorder := &specCapture{
		inner:        f.fk,
		onCreate:     func(env []string) { captured = env },
		onCreateSpec: func(spec *policy.ContainerSpec) { capturedSpec = spec },
	}
	f.dplr.Docker = recorder

	if err := f.run(1, 1); err != nil {
		t.Fatalf("rollback deploy: %v", err)
	}
	if d := f.deployment(); d.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded", d.State)
	}
	// The pull pins the target's digest, never the current tag.
	if f.fk.lastPullRef != "traefik/whoami@"+targetDigest {
		t.Errorf("pull ref = %q, want the target's digest", f.fk.lastPullRef)
	}
	// The container runs the target's port, not the current config's.
	if capturedSpec == nil {
		t.Fatal("no container spec was captured")
	}
	if len(capturedSpec.Ports) != 1 || capturedSpec.Ports[0].ContainerPort != 9090 {
		t.Errorf("container port = %v, want the snapshot's 9090", capturedSpec.Ports)
	}
	// Memory comes from the target snapshot too.
	if capturedSpec.MemoryBytes != 64*1024*1024 {
		t.Errorf("memory = %d, want the snapshot's 64 MiB", capturedSpec.MemoryBytes)
	}
	// Env is live, not rolled back with the image.
	if len(captured) != 1 || captured[0] != "MODE=current" {
		t.Errorf("container env = %v, want the current MODE=current", captured)
	}
}
