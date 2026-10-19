package store

// A deploy job and its deployment row must appear together: the worker
// claims jobs in a poll loop, and a job visible without its deployment
// would fail the pipeline at its first lookup.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCreateDeploymentJobCommitsBothRows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	depID := NewID()

	dep, job, err := s.CreateDeploymentJob(ctx,
		CreateDeploymentInput{
			ID: depID, AppID: appID, Kind: "deploy",
			ImageRef: "nginx@sha256:abc", ConfigSnapshotJSON: "{}", ConfigVersion: 1,
		},
		CreateJobInput{
			AppID: &appID, Type: "deploy",
			PayloadJSON: `{"deployment_id":"` + depID + `"}`,
			CreatedBy:   "test",
		})
	if err != nil {
		t.Fatalf("CreateDeploymentJob: %v", err)
	}
	if dep.State != DepPending || dep.JobID != job.ID {
		t.Errorf("deployment = %+v (job %s), want pending tied to job %s",
			dep, dep.JobID, job.ID)
	}
	if job.State != JobQueued {
		t.Errorf("job state = %v, want queued", job.State)
	}

	// The worker's claim path resolves the payload's deployment.
	claimed, err := s.Claim(ctx, "worker-1", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var pl struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal([]byte(claimed.PayloadJSON), &pl); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if pl.DeploymentID != depID {
		t.Fatalf("payload deployment %q, want %q", pl.DeploymentID, depID)
	}
	if _, err := s.GetDeployment(ctx, pl.DeploymentID); err != nil {
		t.Fatalf("the claimed job's deployment is not readable: %v", err)
	}
}

func TestCreateDeploymentJobRejectsIdempotentJobs(t *testing.T) {
	s := openTest(t)
	appID := createApp(t, s, "web")
	key := "k1"
	_, _, err := s.CreateDeploymentJob(context.Background(),
		CreateDeploymentInput{ID: NewID(), AppID: appID, Kind: "deploy"},
		CreateJobInput{AppID: &appID, Type: "deploy", IdempotencyKey: &key})
	if err == nil {
		t.Fatal("an idempotent job must be refused: its duplicate semantics cannot hold a fresh deployment")
	}
}

// TestNoDeployJobVisibleWithoutItsDeployment is the regression for the
// enqueue race: the old two-transaction order committed the job first,
// so a claiming worker could observe the job while its deployment row
// was still uncommitted and fail with "deployment does not exist".
func TestNoDeployJobVisibleWithoutItsDeployment(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	const rounds = 300
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var raceErr error

	// The claimer runs exactly like the worker: claim, decode, resolve.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Claim legitimately returns (nil, nil) when nothing is
			// claimable or the claim race was lost.
			job, err := s.Claim(ctx, "claimer", time.Now(), time.Minute)
			if err != nil || job == nil {
				continue
			}
			var pl struct {
				DeploymentID string `json:"deployment_id"`
			}
			if err := json.Unmarshal([]byte(job.PayloadJSON), &pl); err != nil {
				continue
			}
			if _, err := s.GetDeployment(ctx, pl.DeploymentID); err != nil {
				raceErr = fmt.Errorf("claimed job %s references deployment %s: %v",
					job.ID, pl.DeploymentID, err)
				return
			}
		}
	}()

	for i := 0; i < rounds; i++ {
		depID := NewID()
		_, _, err := s.CreateDeploymentJob(ctx,
			CreateDeploymentInput{
				ID: depID, AppID: appID, Kind: "deploy",
				ImageRef: "nginx", ConfigSnapshotJSON: "{}", ConfigVersion: 1,
			},
			CreateJobInput{
				AppID: &appID, Type: "deploy",
				PayloadJSON: `{"deployment_id":"` + depID + `"}`,
				CreatedBy:   "test",
			})
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if raceErr != nil {
		t.Fatal(raceErr)
	}
}
