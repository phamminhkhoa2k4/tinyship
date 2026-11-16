package worker

// Disk-forecast behavior: hourly sampling of the trend rows, the
// edge-triggered fill-up warning, and re-arming when the trend
// recovers so a hovering disk warns once, not every tick.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

// seedDiskTrend writes hours+1 hourly samples where free space was
// rateMBPerDay higher per day in the past (positive = filling), ending
// at nowFreeMB at the current hour.
func seedDiskTrend(t *testing.T, s *store.Store, nowFreeMB int64, rateMBPerDay float64, hours int) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Hour)
	for i := hours; i >= 0; i-- {
		free := nowFreeMB + int64(rateMBPerDay*(float64(i)/24))
		if err := s.AppendDiskSample(context.Background(), store.DiskSample{
			At:      now.Add(-time.Duration(i) * time.Hour),
			FreeMB:  free,
			TotalMB: 40000,
		}); err != nil {
			t.Fatalf("seed sample: %v", err)
		}
	}
}

func TestNotifyDispatcherSamplesDiskHourly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := dispatcher(t, s)
	d.DiskFree = func(string) (int, error) { return 9999, nil }
	calls := 0
	d.DiskUsage = func(string) (int64, int64, error) {
		calls++
		return 4500, 20000, nil
	}

	d.RunOnce(ctx)
	d.RunOnce(ctx) // paced: no second sample within the hour
	samples, err := s.ListDiskSamples(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(samples) != 1 || calls != 1 {
		t.Fatalf("samples = %d, probe calls = %d; want one each (hourly pacing)",
			len(samples), calls)
	}
	if samples[0].FreeMB != 4500 || samples[0].TotalMB != 20000 {
		t.Errorf("sample = %+v, want free 4500 / total 20000", samples[0])
	}
}

func TestNotifyDispatcherForecastIsEdgeTriggered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SetDiskForecastDays(ctx, 14); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	// Five days of decline: now 4500 MiB, 900 MiB/day → fills in
	// about five days, well under the 14-day lead time.
	seedDiskTrend(t, s, 4500, 900, 5*24)

	d := dispatcher(t, s)
	d.DiskFree = func(string) (int, error) { return 4500, nil }
	d.DiskUsage = func(string) (int64, int64, error) { return 4500, 40000, nil }

	d.RunOnce(ctx)
	events, err := s.ListNotifyEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 || events[0].Type != "disk_forecast" {
		t.Fatalf("events = %+v, want one disk_forecast", events)
	}
	detail := map[string]any{}
	_ = json.Unmarshal([]byte(events[0].Detail), &detail)
	if days, ok := detail["days_low"].(float64); !ok || days < 4.5 || days > 5.5 {
		t.Errorf("days_low = %v, want ≈5", detail["days_low"])
	}
	if detail["threshold_days"] != float64(14) {
		t.Errorf("threshold_days = %v, want 14", detail["threshold_days"])
	}

	// Still filling: the edge stays set, no repeat enqueue.
	d.RunOnce(ctx)
	if events, _ = s.ListNotifyEvents(ctx, 10); len(events) != 1 {
		t.Fatalf("still-filling produced another event: %d rows", len(events))
	}

	// Recover: the trend turns upward, which re-arms the edge without
	// firing anything.
	if err := s.PruneDiskSamples(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	seedDiskTrend(t, s, 9000, -900, 5*24)
	d.RunOnce(ctx)
	if events, _ = s.ListNotifyEvents(ctx, 10); len(events) != 1 {
		t.Fatalf("recovery produced an event: %d rows", len(events))
	}

	// Declining again: the edge fires and the queue collapses the
	// repeat onto the original row.
	if err := s.PruneDiskSamples(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	seedDiskTrend(t, s, 4500, 900, 5*24)
	d.RunOnce(ctx)
	events, _ = s.ListNotifyEvents(ctx, 10)
	if len(events) != 1 {
		t.Fatalf("re-crossing queued a new row: %d rows", len(events))
	}
	if events[0].DupCount != 2 {
		t.Errorf("dup_count = %d, want 2 (the repeat collapsed)", events[0].DupCount)
	}
}
