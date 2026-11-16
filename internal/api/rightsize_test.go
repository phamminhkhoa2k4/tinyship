package api

// Right-size endpoints: the suggestion view from real samples, and the
// apply path's refusals (no data, nothing to change, a job already in
// flight, the budget) before it ever rewrites a config.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

// seedRightsizeSamples writes first→last samples so the window has a
// known peak and span.
func seedRightsizeSamples(t *testing.T, s *store.Store, appID string,
	first, last time.Time, peakBytes int64) {
	t.Helper()
	ctx := context.Background()
	mk := func(at time.Time, mem int64) {
		if err := s.AppendMetricSample(ctx, store.MetricSample{
			AppID: appID, At: at, MemBytes: mem, MemLimitBytes: 256 << 20,
		}); err != nil {
			t.Fatalf("seed sample: %v", err)
		}
	}
	mk(first, 40<<20)
	mk(first.Add(last.Sub(first)/2), peakBytes)
	mk(last, 50<<20)
}

func TestRightsizeViewWithoutSamples(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	if resp := createApp(t, srv, testConfig); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	resp, err := http.Get(srv.URL + "/apps/web/rightsize")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	if body["verdict"] != "no_data" || body["confidence"] != "none" {
		t.Errorf("verdict/confidence = %v/%v, want no_data/none",
			body["verdict"], body["confidence"])
	}
	if body["current_mb"] != float64(256) {
		t.Errorf("current_mb = %v, want the default 256", body["current_mb"])
	}
	reasons, _ := body["reasons"].([]any)
	if len(reasons) == 0 {
		t.Error("no-data view must carry a reason")
	}
}

func TestRightsizeSuggestsFromObservedPeak(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	if resp := createApp(t, srv, testConfig); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	app, err := s.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	now := time.Now().UTC()
	// A 100 MiB peak observed across (almost) the whole 7-day window:
	// 100 × 1.25 = 125 → 128, with mature data.
	seedRightsizeSamples(t, s, app.ID, now.Add(-7*24*time.Hour+5*time.Minute), now, 100<<20)

	resp, err := http.Get(srv.URL + "/apps/web/rightsize")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body := decodeBody(t, resp)
	if body["verdict"] != "lower" {
		t.Errorf("verdict = %v, want lower", body["verdict"])
	}
	if body["suggested_mb"] != float64(128) || body["peak_mb"] != float64(100) {
		t.Errorf("suggested/peak = %v/%v, want 128/100",
			body["suggested_mb"], body["peak_mb"])
	}
	if body["confidence"] != "full" {
		t.Errorf("confidence = %v, want full (8 days observed)", body["confidence"])
	}
}

func TestRightsizeShortHistoryIsLowConfidence(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	if resp := createApp(t, srv, testConfig); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	app, _ := s.GetAppByName(context.Background(), "web")
	now := time.Now().UTC()
	seedRightsizeSamples(t, s, app.ID, now.AddDate(0, 0, -2), now, 100<<20)

	resp, err := http.Get(srv.URL + "/apps/web/rightsize")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body := decodeBody(t, resp)
	if body["confidence"] != "low" {
		t.Errorf("confidence = %v, want low (2 days observed)", body["confidence"])
	}
	// Low confidence never weakens the floor: still above the peak.
	if sug, _ := body["suggested_mb"].(float64); sug < 100 {
		t.Errorf("suggested_mb = %v, never below the 100 MiB peak", body["suggested_mb"])
	}
	reasons, _ := body["reasons"].([]any)
	found := false
	for _, r := range reasons {
		if s, ok := r.(string); ok && strings.Contains(s, "low confidence") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons = %v, want a low-confidence note", reasons)
	}
}

func TestApplyRightsizeRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("no data", func(t *testing.T) {
		srv, _ := testServer(t, t.TempDir())
		createApp(t, srv, testConfig)
		resp, err := http.Post(srv.URL+"/apps/web/rightsize", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		code := errCode(t, resp)
		if resp.StatusCode != http.StatusConflict || code != "E_NO_DATA" {
			t.Errorf("status/code = %d/%s, want 409/E_NO_DATA", resp.StatusCode, code)
		}
	})

	t.Run("already right-sized", func(t *testing.T) {
		srv, s := testServer(t, t.TempDir())
		createApp(t, srv, testConfig)
		app, _ := s.GetAppByName(ctx, "web")
		now := time.Now().UTC()
		// A 205 MiB peak suggests exactly the current 256 MiB.
		seedRightsizeSamples(t, s, app.ID, now.Add(-7*24*time.Hour+5*time.Minute), now, 205<<20)
		resp, err := http.Post(srv.URL+"/apps/web/rightsize", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		code := errCode(t, resp)
		if resp.StatusCode != http.StatusConflict || code != "E_NO_CHANGE" {
			t.Errorf("status/code = %d/%s, want 409/E_NO_CHANGE", resp.StatusCode, code)
		}
	})

	t.Run("job in flight", func(t *testing.T) {
		srv, s := testServer(t, t.TempDir())
		createApp(t, srv, testConfig)
		app, _ := s.GetAppByName(ctx, "web")
		now := time.Now().UTC()
		seedRightsizeSamples(t, s, app.ID, now.Add(-7*24*time.Hour+5*time.Minute), now, 100<<20)
		if _, err := s.CreateJob(ctx, store.CreateJobInput{
			AppID: &app.ID, Type: "deploy", PayloadJSON: "{}", CreatedBy: "test",
		}); err != nil {
			t.Fatalf("create job: %v", err)
		}
		resp, err := http.Post(srv.URL+"/apps/web/rightsize", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		code := errCode(t, resp)
		if resp.StatusCode != http.StatusConflict || code != "E_CONFLICT" {
			t.Errorf("status/code = %d/%s, want 409/E_CONFLICT", resp.StatusCode, code)
		}
	})

	t.Run("over budget", func(t *testing.T) {
		srv, s := testServer(t, t.TempDir())
		createApp(t, srv, testConfig)
		// A second running app already holds 400 of the 512 MiB budget;
		// the 128 MiB suggestion would push it to 528.
		other := `{"name":"hog","image":"nginx","port":80,"resources":{"memory_mb":400}}`
		if resp := createApp(t, srv, other); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create other: %d %s", resp.StatusCode, drain(resp))
		}
		if err := s.SetBudgetMB(ctx, 512); err != nil {
			t.Fatalf("set budget: %v", err)
		}
		app, _ := s.GetAppByName(ctx, "web")
		now := time.Now().UTC()
		seedRightsizeSamples(t, s, app.ID, now.Add(-7*24*time.Hour+5*time.Minute), now, 100<<20)
		resp, err := http.Post(srv.URL+"/apps/web/rightsize", "application/json", nil)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		code := errCode(t, resp)
		if resp.StatusCode != http.StatusConflict || code != "E_BUDGET_EXCEEDED" {
			t.Errorf("status/code = %d/%s, want 409/E_BUDGET_EXCEEDED",
				resp.StatusCode, code)
		}
		// The refusal must not have rewritten anything.
		app, _ = s.GetAppByName(ctx, "web")
		if app.ConfigVersion != 1 {
			t.Errorf("config_version = %d, want 1 (unchanged)", app.ConfigVersion)
		}
	})
}

func TestApplyRightsizeUpdatesConfigAndQueuesDeploy(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	createApp(t, srv, testConfig)
	app, _ := s.GetAppByName(context.Background(), "web")
	now := time.Now().UTC()
	seedRightsizeSamples(t, s, app.ID, now.Add(-7*24*time.Hour+5*time.Minute), now, 100<<20)

	resp, err := http.Post(srv.URL+"/apps/web/rightsize", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, drain(resp))
	}
	body := decodeBody(t, resp)
	if body["old_memory_mb"] != float64(256) || body["new_memory_mb"] != float64(128) {
		t.Errorf("old/new = %v/%v, want 256/128", body["old_memory_mb"], body["new_memory_mb"])
	}

	app, _ = s.GetAppByName(context.Background(), "web")
	if app.ConfigVersion != 2 {
		t.Errorf("config_version = %d, want 2", app.ConfigVersion)
	}
	if !strings.Contains(app.ConfigJSON, `"memory_mb":128`) {
		t.Errorf("config_json = %s, want memory_mb 128", app.ConfigJSON)
	}
	jobID, _ := body["job_id"].(string)
	job, err := s.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("job: %v", err)
	}
	if job.State != store.JobQueued {
		t.Errorf("job state = %s, want queued", job.State)
	}
	n, err := s.CountAuditEvents(context.Background(), "app.rightsize", app.ID)
	if err != nil || n != 1 {
		t.Errorf("audit rows = %d, err = %v; want 1", n, err)
	}
}

func TestBudgetExposesDiskForecast(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	now := time.Now().UTC().Truncate(time.Hour)
	// Five days of decline: now 4500 MiB, 900 MiB/day → fills in ≈5
	// days, under the 14-day default lead time.
	for i := 5 * 24; i >= 0; i-- {
		if err := s.AppendDiskSample(context.Background(), store.DiskSample{
			At:      now.Add(-time.Duration(i) * time.Hour),
			FreeMB:  4500 + int64(900*(float64(i)/24)),
			TotalMB: 40000,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	resp, err := http.Get(srv.URL + "/budget")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body := decodeBody(t, resp)
	fc, _ := body["disk_forecast"].(map[string]any)
	if fc == nil {
		t.Fatalf("budget has no disk_forecast: %v", body)
	}
	if fc["enough"] != true || fc["filling"] != true {
		t.Errorf("enough/filling = %v/%v, want true/true", fc["enough"], fc["filling"])
	}
	if fc["threshold_days"] != float64(14) {
		t.Errorf("threshold_days = %v, want 14", fc["threshold_days"])
	}
	if days, _ := fc["days_low"].(float64); days < 4.5 || days > 5.5 {
		t.Errorf("days_low = %v, want ≈5", fc["days_low"])
	}
	if hi, _ := fc["days_high"].(float64); hi < 4.5 {
		t.Errorf("days_high = %v, want a range", fc["days_high"])
	}
}

func TestSetBudgetDiskForecastDays(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	resp, err := http.Post(srv.URL+"/budget", "application/json",
		strings.NewReader(`{"disk_forecast_days":30}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := decodeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, body)
	}
	fc, _ := body["disk_forecast"].(map[string]any)
	if fc["threshold_days"] != float64(30) {
		t.Errorf("threshold_days = %v, want 30", fc["threshold_days"])
	}

	for _, bad := range []string{`{"disk_forecast_days":0}`, `{"disk_forecast_days":91}`} {
		resp, _ := http.Post(srv.URL+"/budget", "application/json", strings.NewReader(bad))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp, _ = http.Post(srv.URL+"/budget", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
