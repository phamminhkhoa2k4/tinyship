package worker

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
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

func TestStatsSamplerCoversEveryApp(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	cfg2 := strings.Replace(happyConfig, `"name": "web"`, `"name": "api2"`, 1)
	app2, err := f.s.CreateApp(context.Background(), store.CreateAppInput{
		Name: "api2", ConfigJSON: cfg2, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create second app: %v", err)
	}
	f.fk.addContainer("second-container", "ts-api2", "running", map[string]string{
		policy.LabelManaged: policy.ManagedValue,
		policy.LabelApp:     app2.ID,
	})
	f.fk.mu.Lock()
	f.fk.statMemBytes = 16 << 20
	f.fk.statMemLimit = 64 << 20
	f.fk.mu.Unlock()

	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	apps, err := sampler.Sample(f.ctx)
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("apps = %d, want both apps", len(apps))
	}
	for _, app := range apps {
		if app.State != "running" || app.MemUsageMB != 16 {
			t.Errorf("app %q = %+v, want running with 16 MiB", app.Name, app)
		}
	}
}

func TestStatsSamplerRecordsMetricHistory(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	f.fk.mu.Lock()
	f.fk.statMemBytes = 60 << 20
	f.fk.statMemLimit = 64 << 20
	f.fk.statCPUPct = 12.5
	f.fk.mu.Unlock()

	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	got, err := f.s.ListMetricSamples(f.ctx, f.app.ID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("samples = %d, want 1", len(got))
	}
	if got[0].MemBytes != 60<<20 || got[0].CPUPercent != 12.5 {
		t.Errorf("sample = %+v, want 60MiB at 12.5%%", got[0])
	}
	if got[0].MemLimitBytes != 64<<20 {
		t.Errorf("limit = %d, want 64MiB", got[0].MemLimitBytes)
	}

	// The default interval (30s) gates the next append on the same
	// sampler: an immediate second pass stores nothing new.
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("second sample: %v", err)
	}
	got, err = f.s.ListMetricSamples(f.ctx, f.app.ID, time.Time{})
	if err != nil || len(got) != 1 {
		t.Errorf("samples after immediate re-sample = %d, err %v, want still 1", len(got), err)
	}
}

func TestStatsSamplerSkipsDisabledMetrics(t *testing.T) {
	disabled := strings.Replace(happyConfig, "{", `{"metrics": {"enabled": false},`, 1)
	f := newDeployFixture(t, disabled)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	f.fk.mu.Lock()
	f.fk.statMemBytes = 32 << 20
	f.fk.statMemLimit = 64 << 20
	f.fk.mu.Unlock()

	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	got, err := f.s.ListMetricSamples(f.ctx, f.app.ID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("samples = %d for a disabled app, want 0", len(got))
	}
}

func TestStatsSamplerSkipsAppsWithoutFreshStats(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// The container is not running, so no fresh reading exists: a
	// stopped app must pause its history instead of storing a stale
	// value.
	d := f.deployment()
	f.fk.mu.Lock()
	f.fk.containers[*d.ContainerID].state = "exited"
	f.fk.mu.Unlock()

	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	got, err := f.s.ListMetricSamples(f.ctx, f.app.ID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("samples = %d for a stopped app, want 0", len(got))
	}
}

func TestStatsSamplerSweepRollsUpAndPrunes(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	f.fk.mu.Lock()
	f.fk.statMemBytes = 8 << 20
	f.fk.statMemLimit = 64 << 20
	f.fk.mu.Unlock()

	now := time.Now().UTC().Truncate(time.Second)
	fold := now.Add(-50 * time.Hour)       // past the raw window
	prune := now.Add(-40 * 24 * time.Hour) // past the 30-day cap
	for _, at := range []time.Time{fold, prune} {
		if err := f.s.AppendMetricSample(f.ctx, store.MetricSample{
			AppID: f.app.ID, At: at, MemBytes: 1 << 20,
		}); err != nil {
			t.Fatalf("plant sample: %v", err)
		}
	}

	// A fresh sampler has never swept, so its first pass runs both the
	// rollup and the prune.
	sampler := NewStatsSampler(f.s, f.fk, slog.New(slog.DiscardHandler))
	if _, err := sampler.Sample(f.ctx); err != nil {
		t.Fatalf("sample: %v", err)
	}
	got, err := f.s.ListMetricSamples(f.ctx, f.app.ID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var folded, prunedAway, fresh int
	for _, m := range got {
		switch {
		case m.Aggregated:
			folded++
			if !m.At.Equal(fold.Truncate(time.Hour)) {
				t.Errorf("rollup at %v, want hour %v", m.At, fold.Truncate(time.Hour))
			}
		case m.At.Equal(prune):
			prunedAway++
		case m.At.Equal(now):
			fresh++
		}
	}
	if folded != 1 || prunedAway != 0 || fresh != 1 {
		t.Errorf("after sweep: folded=%d pruned=%d fresh=%d, want 1/0/1 (rows: %+v)",
			folded, prunedAway, fresh, got)
	}
}
