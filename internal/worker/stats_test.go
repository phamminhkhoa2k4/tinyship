package worker

import (
	"context"
	"log/slog"
	"testing"
)

func TestStatsSamplerReportsMemoryAndNearLimit(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	f.fk.mu.Lock()
	f.fk.statMemBytes = 60 << 20 // 60 of the configured 64 MiB
	f.fk.statMemLimit = 64 << 20
	f.fk.statCPUPct = 12.5
	f.fk.mu.Unlock()

	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	apps, err := sampler.Sample(f.ctx)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("apps = %d, want 1", len(apps))
	}
	got := apps[0]
	if got.Name != "web" || got.State != "running" {
		t.Errorf("app = %+v, want web running", got)
	}
	if got.MemUsageMB != 60 || got.MemLimitMB != 64 {
		t.Errorf("memory = %d/%d MiB, want 60/64", got.MemUsageMB, got.MemLimitMB)
	}
	if !got.NearLimit {
		t.Errorf("near_limit = false for 60/64 MiB")
	}
	if got.CPUPercent != 12.5 {
		t.Errorf("cpu = %v, want 12.5", got.CPUPercent)
	}

	// The durable half is stored for the next reader.
	stored, err := f.s.GetContainerState(context.Background(), f.app.ID)
	if err != nil {
		t.Fatalf("stored state: %v", err)
	}
	if stored.MemUsageMB != 60 || stored.State != "running" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestStatsSamplerCountsOOMOnce(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	apps, err := sampler.Sample(f.ctx)
	if err != nil {
		t.Fatalf("first sample: %v", err)
	}
	if apps[0].OOMKillCount != 0 {
		t.Fatalf("initial oom count = %d, want 0", apps[0].OOMKillCount)
	}

	// The running container is killed by the kernel.
	d := f.deployment()
	f.fk.mu.Lock()
	c := f.fk.containers[*d.ContainerID]
	c.oom = true
	c.state = "exited"
	f.fk.mu.Unlock()

	apps, err = sampler.Sample(f.ctx)
	if err != nil {
		t.Fatalf("second sample: %v", err)
	}
	if apps[0].OOMKillCount != 1 || apps[0].State != "exited" {
		t.Fatalf("after kill = %+v, want one OOM and exited", apps[0])
	}

	// Re-sampling the same dead container must not count it again.
	apps, err = sampler.Sample(f.ctx)
	if err != nil {
		t.Fatalf("third sample: %v", err)
	}
	if apps[0].OOMKillCount != 1 {
		t.Errorf("oom count = %d after re-sample, want 1", apps[0].OOMKillCount)
	}
}
