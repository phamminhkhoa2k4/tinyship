package cli

// Right-size and dry-run command tests over a stub API: the flags
// shape the requests, the output explains the numbers, and a refused
// apply exits non-zero with the API's cause.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func rightsizeViewJSON(verdict string, peak, current, suggested int, confidence string) map[string]any {
	return map[string]any{
		"app": "web", "window_days": 7, "samples": 168,
		"observed_from": "2026-11-09T12:00:00Z", "observed_to": "2026-11-16T12:00:00Z",
		"peak_mb": peak, "current_mb": current, "suggested_mb": suggested,
		"verdict": verdict, "confidence": confidence,
		"reasons": []string{
			"observed peak 100 MiB across 168 samples",
			"peak x1.25 = 125 MiB, rounded up to 128 MiB (never below the observed peak)",
		},
		"budget_mb": 768, "free_mb": 512,
	}
}

func TestRightsizeRendersSuggestion(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apps/web/rightsize" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
			return
		}
		writeJSON(w, http.StatusOK, rightsizeViewJSON("lower", 100, 256, 128, "full"))
	}))

	code, stdout, stderr := run(t, "rightsize", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"Right-size for web (window: last 7 days)",
		"observed:  100 MiB peak",
		"current:   256 MiB",
		"suggested: 128 MiB",
		"lower the limit by 128 MiB",
		"budget:    512 MiB free of 768 MiB",
		"never below the observed peak",
		"To apply it: tinyship rightsize web --apply",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestRightsizeNoDataPrintsReason(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"app": "web", "window_days": 7, "samples": 0,
			"verdict": "no_data", "confidence": "none", "current_mb": 256,
			"reasons": []string{"no samples in the last 7 days; let the sampler run first"},
		})
	}))

	code, stdout, stderr := run(t, "rightsize", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no samples in the last 7 days") {
		t.Errorf("stdout should explain the empty window:\n%s", stdout)
	}
	if !strings.Contains(stdout, "current limit: 256 MiB") {
		t.Errorf("stdout should still show the current limit:\n%s", stdout)
	}
	if strings.Contains(stdout, "To apply it") {
		t.Errorf("no-data view must not offer --apply:\n%s", stdout)
	}
}

func TestRightsizeApplyQueuesDeploy(t *testing.T) {
	var posted int
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apps/web/rightsize" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
			return
		}
		posted++
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 3,
			"state": "pending", "old_memory_mb": 256, "new_memory_mb": 128,
		})
	}))

	code, stdout, stderr := run(t, "rightsize", "web", "--apply")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if posted != 1 {
		t.Errorf("POST count = %d, want 1", posted)
	}
	if !strings.Contains(stdout, "Applied to web: memory 256 → 128 MiB; queued deploy (seq 3, pending)") {
		t.Errorf("stdout should report the applied change:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Follow it with: tinyship deployments web") {
		t.Errorf("stdout should hint the follow-up command:\n%s", stdout)
	}
}

func TestRightsizeApplyFailureExitsNonZero(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "E_BUDGET_EXCEEDED",
			"400 MiB committed + 128 MiB suggested = 528 MiB, budget 512 MiB, over by 16 MiB")
	}))

	code, _, stderr := run(t, "rightsize", "web", "--apply")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_BUDGET_EXCEEDED") || !strings.Contains(stderr, "over by 16 MiB") {
		t.Errorf("stderr should carry the refusal:\n%s", stderr)
	}
}

func TestRightsizeRequiresExactlyOneName(t *testing.T) {
	code, _, stderr := run(t, "rightsize")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if !strings.Contains(stderr, "exactly one app name") {
		t.Errorf("stderr = %q", stderr)
	}
}

// --- app create --dry-run ----------------------------------------------------------

func dryRunBudgetHandler(t *testing.T, freeMB, committedMB int, nameExists bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/budget":
			writeJSON(w, http.StatusOK, map[string]any{
				"budget_mb": 768, "budget_source": "auto", "mem_total_mb": 1536,
				"committed_mb": committedMB, "free_mb": freeMB,
				"min_free_disk_mb": 3072, "cpu_overcommit": 2.0,
				"disk_forecast": map[string]any{"enough": false, "threshold_days": 14},
				"apps":          []any{},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apps/"):
			if nameExists {
				writeJSON(w, http.StatusOK, map[string]any{"app": map[string]any{"name": "web"}})
			} else {
				writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
			}
		case r.Method == http.MethodPost && r.URL.Path == "/apps":
			t.Error("dry run must never POST /apps")
			writeErr(w, http.StatusInternalServerError, "E_INTERNAL", "created")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	})
}

func TestAppCreateDryRunReportsBudget(t *testing.T) {
	apiFor(t, dryRunBudgetHandler(t, 512, 256, false))

	code, stdout, stderr := run(t, "app", "create", "--name", "web",
		"--image", "nginx", "--port", "80", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		`Dry run: app "web" was not created.`,
		"requested: 256 MiB of RAM for the new app",
		"budget:    768 MiB (auto) — 256 MiB committed by running apps, 512 MiB free",
		"after:     256 MiB free — the deploy would fit the budget",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestAppCreateDryRunOverBudgetSaysRefused(t *testing.T) {
	apiFor(t, dryRunBudgetHandler(t, 128, 640, false))

	code, stdout, stderr := run(t, "app", "create", "--name", "web",
		"--image", "nginx", "--port", "80", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "over by 128 MiB — admission control would refuse the deploy") {
		t.Errorf("stdout should predict the refusal:\n%s", stdout)
	}
}

func TestAppCreateDryRunReportsNameConflict(t *testing.T) {
	apiFor(t, dryRunBudgetHandler(t, 512, 256, true))

	code, stdout, stderr := run(t, "app", "create", "--name", "web",
		"--image", "nginx", "--port", "80", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `an app named "web" already exists`) {
		t.Errorf("stdout should report the conflict:\n%s", stdout)
	}
}

func TestAppCreateDryRunRejectsInvalidConfig(t *testing.T) {
	apiFor(t, dryRunBudgetHandler(t, 512, 256, false))

	code, _, stderr := run(t, "app", "create", "--name", "UPPER",
		"--image", "nginx", "--port", "80", "--dry-run")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "invalid config") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestAppCreateDryRunJSON(t *testing.T) {
	apiFor(t, dryRunBudgetHandler(t, 512, 256, false))

	code, stdout, stderr := run(t, "app", "create", "--name", "web",
		"--image", "nginx", "--port", "80", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if out["created"] != false || out["fits"] != true || out["requested_mb"] != float64(256) {
		t.Errorf("report = %v", out)
	}
}
