package worker

import (
	"context"
	"log/slog"
	"testing"

	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/store"
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

func TestOOMCountAccumulatesAcrossContainers(t *testing.T) {
	fk := newFakeDocker()
	fk.addContainer("seen-exit", "ts-seen", "exited", nil)
	fk.containers["seen-exit"].oom = true
	fk.addContainer("unseen-exit", "ts-unseen", "exited", nil)
	fk.containers["unseen-exit"].oom = true
	s := NewStatsSampler(nil, fk, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	cases := []struct {
		name    string
		prev    *store.ContainerState
		current docker.ContainerInfo
		want    int
	}{
		{
			name:    "an observed exit is not counted again when the container is replaced",
			prev:    &store.ContainerState{ContainerID: "seen-exit", State: "exited", OOMKillCount: 1},
			current: docker.ContainerInfo{ID: "new", State: "running"},
			want:    1,
		},
		{
			name:    "a replacement counts the previous container's unsampled kill",
			prev:    &store.ContainerState{ContainerID: "unseen-exit", State: "running", OOMKillCount: 0},
			current: docker.ContainerInfo{ID: "new", State: "running"},
			want:    1,
		},
		{
			name:    "the same container transitions from running to killed once",
			prev:    &store.ContainerState{ContainerID: "unseen-exit", State: "running", OOMKillCount: 0},
			current: docker.ContainerInfo{ID: "unseen-exit", State: "exited", OOMKilled: true},
			want:    1,
		},
		{
			name:    "a brand-new container that is already killed counts once",
			prev:    nil,
			current: docker.ContainerInfo{ID: "fresh", State: "exited", OOMKilled: true},
			want:    1,
		},
	}
	for _, tc := range cases {
		if got := s.oomCount(ctx, tc.prev, tc.current); got != tc.want {
			t.Errorf("%s: count = %d, want %d", tc.name, got, tc.want)
		}
	}
}
