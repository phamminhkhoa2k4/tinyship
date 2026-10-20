package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContainerStateUpsertAndRead(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "webapp")

	if _, err := s.GetContainerState(ctx, appID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetContainerState before sample = %v, want ErrNotFound", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	in := ContainerState{
		AppID: appID, ContainerID: "cid-1", State: "running", Health: "healthy",
		MemUsageMB: 42, OOMKillCount: 0, UpdatedAt: now,
	}
	if err := s.UpsertContainerState(ctx, in); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	got, err := s.GetContainerState(ctx, appID)
	if err != nil {
		t.Fatalf("get after insert: %v", err)
	}
	if got.ContainerID != "cid-1" || got.MemUsageMB != 42 || got.State != "running" {
		t.Errorf("stored sample = %+v", got)
	}

	// A second sample for the same app replaces the row.
	in.ContainerID = "cid-2"
	in.MemUsageMB = 7
	in.OOMKillCount = 1
	in.UpdatedAt = now.Add(time.Second)
	if err := s.UpsertContainerState(ctx, in); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	all, err := s.ListContainerStates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[appID].ContainerID != "cid-2" || all[appID].OOMKillCount != 1 {
		t.Errorf("listed states = %+v", all)
	}
}

func TestAppHasActiveJob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "webapp")

	active, err := s.AppHasActiveJob(ctx, appID)
	if err != nil || active {
		t.Fatalf("no jobs: active=%v err=%v", active, err)
	}

	job, err := s.CreateJob(ctx, CreateJobInput{
		AppID: &appID, Type: "deploy", PayloadJSON: `{}`, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if active, err = s.AppHasActiveJob(ctx, appID); err != nil || !active {
		t.Fatalf("queued job: active=%v err=%v", active, err)
	}

	if _, err := s.Claim(ctx, "test-worker", time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if active, err = s.AppHasActiveJob(ctx, appID); err != nil || !active {
		t.Fatalf("running job: active=%v err=%v", active, err)
	}

	if err := s.FinishJob(ctx, job.ID, "test-worker", JobSucceeded, "", time.Now().UTC()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if active, err = s.AppHasActiveJob(ctx, appID); err != nil || active {
		t.Fatalf("finished job: active=%v err=%v", active, err)
	}
}
