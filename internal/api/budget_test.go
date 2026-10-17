package api

// Budget view and deploy-force tests: the endpoint reports the same
// accounting the worker enforces, and force flows into the job payload
// the worker decodes.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const budgetConfig = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	"resources": {"memory_mb": 256, "cpu_millis": 100, "pids_limit": 64}
}`

func TestBudgetViewEmptyHost(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, err := http.Get(srv.URL + "/budget")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	body := decodeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %v", resp.StatusCode, body)
	}
	if body["budget_source"] != "auto" {
		t.Errorf("budget_source = %v, want auto with no override", body["budget_source"])
	}
	if committed, _ := body["committed_mb"].(float64); committed != 0 {
		t.Errorf("committed_mb = %v, want 0 on an empty host", body["committed_mb"])
	}
	if apps, _ := body["apps"].([]any); len(apps) != 0 {
		t.Errorf("apps = %v, want empty", body["apps"])
	}
	// The auto derivation must be positive on any host that can run
	// tests, and never exceed MemTotal minus the host reserve.
	budget, _ := body["budget_mb"].(float64)
	memTotal, _ := body["mem_total_mb"].(float64)
	if budget <= 0 || budget > memTotal {
		t.Errorf("budget_mb = %v with mem_total_mb = %v, want 0 < budget <= memTotal", budget, memTotal)
	}
}

func TestBudgetViewCountsRunningGrants(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	if err := s.SetBudgetMB(context.Background(), 2048); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	resp := createApp(t, srv, budgetConfig)
	resp.Body.Close()

	resp, err := http.Get(srv.URL + "/budget")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	body := decodeBody(t, resp)
	if body["budget_source"] != "configured" {
		t.Errorf("budget_source = %v, want configured", body["budget_source"])
	}
	if body["budget_mb"] != float64(2048) {
		t.Errorf("budget_mb = %v, want 2048", body["budget_mb"])
	}
	if body["committed_mb"] != float64(256) {
		t.Errorf("committed_mb = %v, want the app's 256 MiB grant", body["committed_mb"])
	}
	if body["free_mb"] != float64(1792) {
		t.Errorf("free_mb = %v, want 1792", body["free_mb"])
	}
	apps, _ := body["apps"].([]any)
	if len(apps) != 1 {
		t.Fatalf("apps = %v, want one grant", body["apps"])
	}
	grant := apps[0].(map[string]any)
	if grant["name"] != "web" || grant["memory_mb"] != float64(256) {
		t.Errorf("grant = %v, want web/256", grant)
	}
}

func TestBudgetViewExcludesStoppedApps(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	resp := createApp(t, srv, budgetConfig)
	resp.Body.Close()
	if _, err := s.DB().Exec(
		`UPDATE apps SET desired_state = 'stopped' WHERE name = 'web'`); err != nil {
		t.Fatalf("stop app: %v", err)
	}
	resp, err := http.Get(srv.URL + "/budget")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	body := decodeBody(t, resp)
	if body["committed_mb"] != float64(0) {
		t.Errorf("committed_mb = %v, want 0: a stopped app holds no grant", body["committed_mb"])
	}
}

func TestDeployForceFlowsIntoJobPayload(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()

	resp, err := http.Post(srv.URL+"/apps/web/deploy", "application/json",
		strings.NewReader(`{"force":true}`))
	if err != nil {
		t.Fatalf("forced deploy: %v", err)
	}
	body := decodeBody(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %v", resp.StatusCode, body)
	}
	if body["forced"] != true {
		t.Errorf("forced = %v, want true", body["forced"])
	}
	job, err := s.GetJob(context.Background(), body["job_id"].(string))
	if err != nil {
		t.Fatalf("job row: %v", err)
	}
	if !strings.Contains(job.PayloadJSON, `"force":true`) {
		t.Errorf("payload = %s, want force:true", job.PayloadJSON)
	}
}

func TestDeployEmptyBodyKeepsDefaults(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()

	resp, err := http.Post(srv.URL+"/apps/web/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	body := decodeBody(t, resp)
	if body["forced"] != false {
		t.Errorf("forced = %v, want false for an empty body", body["forced"])
	}
	job, err := s.GetJob(context.Background(), body["job_id"].(string))
	if err != nil {
		t.Fatalf("job row: %v", err)
	}
	if strings.Contains(job.PayloadJSON, "force") {
		t.Errorf("payload = %s, want no force key", job.PayloadJSON)
	}
}

func TestDeployRejectsUnknownBodyField(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()

	resp, err := http.Post(srv.URL+"/apps/web/deploy", "application/json",
		strings.NewReader(`{"frce":true}`))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a misspelled field; body %s",
			resp.StatusCode, drain(resp))
	}
}

func TestSetBudgetOverride(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	resp, err := http.Post(srv.URL+"/budget", "application/json",
		strings.NewReader(`{"budget_mb":1024}`))
	if err != nil {
		t.Fatalf("set budget: %v", err)
	}
	body := decodeBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %v", resp.StatusCode, body)
	}
	if body["budget_mb"] != float64(1024) || body["budget_source"] != "configured" {
		t.Errorf("view = %v, want configured 1024", body)
	}
	// The override persists for the worker's admission check.
	settings, err := s.GetBudgetSettings(context.Background())
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if settings.BudgetMB != 1024 {
		t.Errorf("stored budget = %d, want 1024", settings.BudgetMB)
	}
	// The change is audited.
	n, err := s.CountAuditEvents(context.Background(), "budget.set", "")
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("budget.set audit events = %d, want 1", n)
	}
}

func TestSetBudgetZeroRestoresAuto(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, err := http.Post(srv.URL+"/budget", "application/json",
		strings.NewReader(`{"budget_mb":0}`))
	if err != nil {
		t.Fatalf("set budget: %v", err)
	}
	body := decodeBody(t, resp)
	if body["budget_source"] != "auto" {
		t.Errorf("budget_source = %v, want auto after reset", body["budget_source"])
	}
}

func TestSetBudgetValidatesInput(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	for _, tc := range []struct {
		body string
		why  string
	}{
		{`{"budget_mb":-5}`, "negative budget"},
		{`{}`, "missing budget_mb"},
		{`{"budget":"lots"}`, "wrong type"},
	} {
		resp, err := http.Post(srv.URL+"/budget", "application/json",
			strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.why, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body %s", tc.why, resp.StatusCode, drain(resp))
		}
	}
}
