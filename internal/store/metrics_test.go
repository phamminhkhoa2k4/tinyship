package store

import (
	"context"
	"testing"
	"time"
)

func TestMetricSampleAppendAndList(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "metrics-app")

	base := time.Unix(1_760_000_000, 0).UTC() // aligned hour boundary
	in := []MetricSample{
		{AppID: appID, At: base.Add(30 * time.Second), CPUPercent: 12.5, MemBytes: 60 << 20, MemLimitBytes: 64 << 20},
		{AppID: appID, At: base.Add(60 * time.Second), CPUPercent: 40, MemBytes: 90 << 20, MemLimitBytes: 64 << 20},
	}
	for _, m := range in {
		if err := s.AppendMetricSample(ctx, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Re-appending the same second is idempotent, not an error.
	if err := s.AppendMetricSample(ctx, in[0]); err != nil {
		t.Fatalf("re-append: %v", err)
	}

	got, err := s.ListMetricSamples(ctx, appID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %d, want 2", len(got))
	}
	if got[0].At != base.Add(30*time.Second) || got[1].At != base.Add(60*time.Second) {
		t.Errorf("order = %v, want oldest first", []time.Time{got[0].At, got[1].At})
	}
	if got[1].MemBytes != 90<<20 || got[1].CPUPercent != 40 {
		t.Errorf("second sample = %+v", got[1])
	}

	// A foreign app's history never shows up in this app's list.
	other := createApp(t, s, "other")
	if err := s.AppendMetricSample(ctx, MetricSample{AppID: other, At: base, MemBytes: 1}); err != nil {
		t.Fatalf("append other: %v", err)
	}
	got, err = s.ListMetricSamples(ctx, appID, time.Time{})
	if err != nil || len(got) != 2 {
		t.Errorf("list after other append = %d samples, err %v", len(got), err)
	}
}

func TestRollupFoldsOldRawIntoHourly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "rollup-app")

	now := time.Now().UTC().Truncate(time.Second)
	hour := now.Add(-50 * time.Hour).Truncate(time.Hour)
	oldA := hour.Add(30 * time.Second)
	oldB := hour.Add(90 * time.Second)
	recent := now.Add(-1 * time.Hour)

	for _, m := range []MetricSample{
		{AppID: appID, At: oldA, CPUPercent: 10, MemBytes: 50 << 20},
		{AppID: appID, At: oldB, CPUPercent: 30, MemBytes: 70 << 20},
		{AppID: appID, At: recent, CPUPercent: 5, MemBytes: 40 << 20},
	} {
		if err := s.AppendMetricSample(ctx, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	cutoff := now.Add(-48 * time.Hour)
	if err := s.RollupMetricSamples(ctx, cutoff); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	// Running it again finds nothing to fold and must stay clean.
	if err := s.RollupMetricSamples(ctx, cutoff); err != nil {
		t.Fatalf("second rollup: %v", err)
	}

	got, err := s.ListMetricSamples(ctx, appID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("samples after rollup = %d, want 1 rollup + 1 recent raw", len(got))
	}
	rollup := got[0]
	if !rollup.Aggregated || !rollup.At.Equal(hour) {
		t.Errorf("first row = %+v, want hourly rollup at %v", rollup, hour)
	}
	if rollup.MemBytes != 70<<20 { // MAX survives the fold
		t.Errorf("rollup mem = %d, want peak 70MiB", rollup.MemBytes)
	}
	if rollup.CPUPercent != 20 { // AVG of 10 and 30
		t.Errorf("rollup cpu = %v, want 20", rollup.CPUPercent)
	}
	if got[1].Aggregated || !got[1].At.Equal(recent.UTC()) {
		t.Errorf("second row = %+v, want recent raw sample untouched", got[1])
	}
}

func TestPruneEnforcesRetentionPerApp(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "prune-app")
	other := createApp(t, s, "prune-other")

	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-40 * 24 * time.Hour)
	fresh := now.Add(-1 * time.Hour)
	for _, m := range []MetricSample{
		{AppID: appID, At: old, MemBytes: 1},
		{AppID: appID, At: fresh, MemBytes: 2},
		{AppID: other, At: old, MemBytes: 3},
	} {
		if err := s.AppendMetricSample(ctx, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if err := s.PruneMetricSamples(ctx, appID, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got, err := s.ListMetricSamples(ctx, appID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || !got[0].At.Equal(fresh) {
		t.Errorf("after prune = %+v, want only the fresh sample", got)
	}
	otherGot, err := s.ListMetricSamples(ctx, other, time.Time{})
	if err != nil || len(otherGot) != 1 {
		t.Errorf("other app after prune = %d samples, err %v (prune is per app)", len(otherGot), err)
	}
}

func TestRawAppendNeverOverwritesRollup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	appID := createApp(t, s, "conflict-app")

	hour := time.Unix(1_760_000_000, 0).UTC() // exact hour boundary
	if _, err := s.db.Exec(`
		INSERT INTO app_metric_samples
			(app_id, ts, aggregated, cpu_percent, mem_bytes, mem_limit_bytes)
		VALUES (?, ?, 1, 11, 999, 0)`, appID, hour.Unix()); err != nil {
		t.Fatalf("insert rollup: %v", err)
	}
	if err := s.AppendMetricSample(ctx, MetricSample{
		AppID: appID, At: hour, CPUPercent: 99, MemBytes: 123,
	}); err != nil {
		t.Fatalf("append at rollup ts: %v", err)
	}

	got, err := s.ListMetricSamples(ctx, appID, time.Time{})
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %d rows, err %v", len(got), err)
	}
	if !got[0].Aggregated || got[0].MemBytes != 999 || got[0].CPUPercent != 11 {
		t.Errorf("rollup row = %+v, want untouched aggregate", got[0])
	}
}
