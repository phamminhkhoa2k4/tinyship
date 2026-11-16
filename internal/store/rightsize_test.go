package store

// The data path right-sizing reads: window aggregates over metric
// samples, the host disk trend rows, the config update apply performs,
// the pending-job guard, and the forecast lead-time setting.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetricStatsForWindowAggregate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "stats-app")

	base := time.Now().UTC().Truncate(time.Hour)
	in := []MetricSample{
		{AppID: appID, At: base.Add(-6 * time.Hour), CPUPercent: 10, MemBytes: 80 << 20},
		{AppID: appID, At: base.Add(-3 * time.Hour), CPUPercent: 55, MemBytes: 210 << 20},
		// An hourly rollup row counts too: MAX must see through the
		// fold that preserves the peak.
		{AppID: appID, At: base.Add(-2 * time.Hour), Aggregated: true, CPUPercent: 30, MemBytes: 240 << 20},
		{AppID: appID, At: base, CPUPercent: 20, MemBytes: 100 << 20},
	}
	for _, m := range in {
		if err := s.AppendMetricSample(ctx, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	stats, err := s.MetricStatsFor(ctx, appID, base.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Samples != 4 {
		t.Errorf("samples = %d, want 4", stats.Samples)
	}
	if stats.PeakMemBytes != 240<<20 {
		t.Errorf("peak = %d, want %d (240 MiB)", stats.PeakMemBytes, 240<<20)
	}
	if stats.PeakCPU != 55 {
		t.Errorf("peak cpu = %v, want 55", stats.PeakCPU)
	}
	if !stats.FirstAt.Equal(base.Add(-6*time.Hour)) || !stats.LastAt.Equal(base) {
		t.Errorf("span = %s..%s, want %s..%s",
			stats.FirstAt, stats.LastAt, base.Add(-6*time.Hour), base)
	}

	// A window that starts after every sample sees nothing — this is
	// the empty-window case rightsize reports as "no data".
	empty, err := s.MetricStatsFor(ctx, appID, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("empty stats: %v", err)
	}
	if empty.Samples != 0 || !empty.FirstAt.IsZero() {
		t.Errorf("empty window = %+v, want zero samples and zero times", empty)
	}
}

func TestDiskSamplesRoundTripAndPrune(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	old := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Hour)
	fresh := time.Now().UTC().Truncate(time.Hour)
	for _, d := range []DiskSample{
		{At: old, FreeMB: 9000, TotalMB: 40000},
		{At: fresh, FreeMB: 4500, TotalMB: 40000},
	} {
		if err := s.AppendDiskSample(ctx, d); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Re-sampling the same second overwrites, not duplicates.
	if err := s.AppendDiskSample(ctx, DiskSample{At: fresh, FreeMB: 4400, TotalMB: 40000}); err != nil {
		t.Fatalf("re-append: %v", err)
	}

	got, err := s.ListDiskSamples(ctx, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2", len(got))
	}
	if got[1].FreeMB != 4400 {
		t.Errorf("free = %d, want 4400 (the re-sample won)", got[1].FreeMB)
	}

	if err := s.PruneDiskSamples(ctx, fresh.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, _ = s.ListDiskSamples(ctx, time.Time{})
	if len(got) != 1 || !got[0].At.Equal(fresh) {
		t.Errorf("after prune = %+v, want only the fresh sample", got)
	}
}

func TestUpdateAppConfigBumpsVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "cfg-app")

	if err := s.UpdateAppConfig(ctx, appID, `{"name":"cfg-app","memory":1}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	app, err := s.GetAppByID(ctx, appID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if app.ConfigVersion != 2 {
		t.Errorf("config_version = %d, want 2", app.ConfigVersion)
	}
	if app.ConfigJSON != `{"name":"cfg-app","memory":1}` {
		t.Errorf("config_json = %q", app.ConfigJSON)
	}

	if err := s.UpdateAppConfig(ctx, "no-such-app", "{}"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing app: err = %v, want ErrNotFound", err)
	}
}

func TestHasPendingJobGuardsThePipeline(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "job-app")

	pending, err := s.HasPendingJob(ctx, appID)
	if err != nil || pending {
		t.Fatalf("no jobs: pending = %v, err = %v; want false", pending, err)
	}
	if _, err := s.CreateJob(ctx, CreateJobInput{
		AppID: &appID, Type: "deploy", PayloadJSON: "{}", CreatedBy: "test",
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	pending, err = s.HasPendingJob(ctx, appID)
	if err != nil || !pending {
		t.Fatalf("queued job: pending = %v, err = %v; want true", pending, err)
	}
}

func TestDiskForecastDaysSetting(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	settings, err := s.GetBudgetSettings(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if settings.DiskForecastDays != DefaultDiskForecastDays {
		t.Errorf("default = %d, want %d", settings.DiskForecastDays, DefaultDiskForecastDays)
	}
	if err := s.SetDiskForecastDays(ctx, 30); err != nil {
		t.Fatalf("set: %v", err)
	}
	settings, _ = s.GetBudgetSettings(ctx)
	if settings.DiskForecastDays != 30 {
		t.Errorf("after set = %d, want 30", settings.DiskForecastDays)
	}
	for _, bad := range []int{0, -1, MaxDiskForecastDays + 1} {
		if err := s.SetDiskForecastDays(ctx, bad); err == nil {
			t.Errorf("set %d: want an error (out of range)", bad)
		}
	}
}
