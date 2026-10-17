package cli

// Command-level tests for the CLI surface: each command is
// exercised against a scripted API behind TINYSHIP_API, asserting the exact
// request shape, the rendered output, and the exit code contract
// (0 ok / 1 failure / 2 usage).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
)

// apiFor points the CLI at a scripted API and returns it.
func apiFor(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("TINYSHIP_API", srv.URL)
	return srv
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// --- app create ------------------------------------------------------------------

func TestAppCreateFromFlagsSendsStrictConfig(t *testing.T) {
	var got appconfig.Config
	var rawBody []byte
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apps" {
			t.Errorf("request = %s %s, want POST /apps", r.Method, r.URL.Path)
		}
		rawBody, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "01ABC", "name": "web", "image": "ghcr.io/acme/web", "tag": "v1.2.3",
			"port": 8080, "warnings": []string{"W_MUTABLE_TAG: not applicable?"},
		})
	}))

	code, stdout, stderr := run(t,
		"app", "create",
		"--name", "web",
		"--image", "ghcr.io/acme/web",
		"--tag", "v1.2.3",
		"--port", "8080",
		"--memory-mb", "128",
		"--strategy", "stop-first",
		"--volume", "data:/var/lib/app",
		"--health-type", "http",
		"--health-path", "/healthz",
	)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	// The payload must survive the API's strict parser.
	cfg, err := appconfig.Parse(rawBody)
	if err != nil {
		t.Fatalf("API would reject %s: %v", rawBody, err)
	}
	got = *cfg
	if got.Name != "web" || got.Image != "ghcr.io/acme/web" || got.Tag != "v1.2.3" || got.Port != 8080 {
		t.Errorf("config = %+v", got)
	}
	if got.Resources.MemoryMB != 128 || got.Strategy != "stop-first" {
		t.Errorf("resources/strategy not forwarded: %+v", got)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Name != "data" || got.Volumes[0].Path != "/var/lib/app" {
		t.Errorf("volumes = %+v", got.Volumes)
	}
	if got.Health == nil || got.Health.Type != "http" || got.Health.Path != "/healthz" {
		t.Errorf("health = %+v", got.Health)
	}
	for _, want := range []string{"Created app web", "ghcr.io/acme/web:v1.2.3", "tinyship deploy web --wait"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestAppCreateUsageErrors(t *testing.T) {
	// Required flags missing: 2 before any network call.
	code, _, stderr := run(t, "app", "create", "--name", "web")
	if code != 2 {
		t.Errorf("missing flags: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--name, --image and --port") {
		t.Errorf("stderr = %q", stderr)
	}
	// Unknown flag: 2.
	code, _, _ = run(t, "app", "create", "--name", "a", "--image", "b", "--port", "80", "--nope")
	if code != 2 {
		t.Errorf("unknown flag: exit = %d, want 2", code)
	}
	// Extra positional argument: 2.
	code, _, _ = run(t, "app", "create", "--name", "a", "--image", "b", "--port", "80", "extra")
	if code != 2 {
		t.Errorf("extra arg: exit = %d, want 2", code)
	}
	// Missing subcommand: 2 and list the valid ones.
	code, _, stderr = run(t, "app")
	if code != 2 {
		t.Errorf("bare app: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "create") || !strings.Contains(stderr, "list") {
		t.Errorf("bare app stderr should list subcommands: %q", stderr)
	}
}

func TestAppCreateAPIErrorExitsOneWithCause(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "E_VALIDATION", `app "web" already exists`)
	}))
	code, _, stderr := run(t,
		"app", "create", "--name", "web", "--image", "nginx", "--port", "80")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_VALIDATION") || !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want code and cause", stderr)
	}
}

func TestAppCreateFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"name":"fromfile","image":"nginx","tag":"v1","port":9000,"resources":{"memory_mb":64,"cpu_millis":100,"pids_limit":64}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var sent []byte
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "01X", "name": "fromfile", "image": "nginx", "tag": "v1", "port": 9000,
		})
	}))

	code, stdout, stderr := run(t, "app", "create", "-f", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if _, err := appconfig.Parse(sent); err != nil {
		t.Errorf("file payload rejected by strict parser: %v", err)
	}
	if !strings.Contains(stdout, "Created app fromfile") {
		t.Errorf("stdout = %q", stdout)
	}

	// A missing file exits 1 with the OS error, no panic.
	code, _, stderr = run(t, "app", "create", "-f", filepath.Join(t.TempDir(), "nope.json"))
	if code != 1 {
		t.Errorf("missing file: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "nope.json") {
		t.Errorf("stderr = %q, want the filename", stderr)
	}
}

// --- app list / show ---------------------------------------------------------------

func TestAppListRendersTableAndJSON(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"apps": []map[string]any{
			{"id": "01A", "name": "alpha", "image": "nginx", "tag": "v1", "port": 80,
				"desired_state": "running", "current_deployment_id": "01DEPLOY123"},
			{"id": "01B", "name": "beta", "image": "redis", "tag": "latest", "port": 6379,
				"desired_state": "stopped"},
		}})
	}))

	code, stdout, stderr := run(t, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"NAME", "alpha", "nginx:v1", "01DEPLOY12"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
	// Apps without a deployment show a dash, not a panic.
	if !strings.Contains(stdout, "-") {
		t.Errorf("no-deployment cell should be a dash:\n%s", stdout)
	}

	code, stdout, _ = run(t, "app", "list", "--json")
	if code != 0 {
		t.Fatalf("--json exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("--json output invalid: %v (%s)", err, stdout)
	}
}

func TestAppListEmptyHint(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"apps": []any{}})
	}))
	code, stdout, _ := run(t, "app", "list")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "No apps yet") || !strings.Contains(stdout, "tinyship app create") {
		t.Errorf("empty list should suggest the next command:\n%s", stdout)
	}
}

func TestAppShowRendersCurrentDeployment(t *testing.T) {
	digest := "sha256:aaaa"
	port := 7878
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps/web" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"app": map[string]any{
				"id": "01A", "name": "web", "desired_state": "running",
				"config":                map[string]any{"name": "web", "image": "nginx", "tag": "v1", "port": 80},
				"current_deployment_id": "01DEPLOY123",
			},
			"current_deployment": map[string]any{
				"id": "01DEPLOY123", "seq": 3, "state": "succeeded",
				"image_ref": "nginx:v1", "image_digest": digest, "host_port": port,
			},
		})
	}))

	code, stdout, stderr := run(t, "app", "show", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"App web", `"image": "nginx"`, digest, "127.0.0.1", "succeeded"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestAppShowNotFoundExitsOne(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
	}))
	code, _, stderr := run(t, "app", "show", "ghost")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_NOT_FOUND") {
		t.Errorf("stderr = %q", stderr)
	}
	code, _, _ = run(t, "app", "show")
	if code != 2 {
		t.Errorf("no name: exit = %d, want 2", code)
	}
}

// --- deploy --------------------------------------------------------------------------

func TestDeployWaitSucceededPrintsTimeline(t *testing.T) {
	var calls atomic.Int32
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			writeJSON(w, http.StatusAccepted, map[string]any{
				"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 1, "state": "pending",
			})
		case strings.HasSuffix(r.URL.Path, "/deployments/01DEPLOY"):
			// First poll running, then succeeded: proves --wait polls.
			if calls.Add(1) == 1 {
				writeJSON(w, http.StatusOK, map[string]any{
					"id": "01DEPLOY", "seq": 1, "state": "running", "image_ref": "nginx:v1",
					"events": []map[string]any{{"step": "validate", "status": "ok"}},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "01DEPLOY", "seq": 1, "state": "succeeded",
				"image_ref": "nginx:v1", "image_digest": "sha256:bbb",
				"host_port": 32768,
				"events": []map[string]any{
					{"step": "validate", "status": "ok"},
					{"step": "pull", "status": "ok", "detail": map[string]any{"digest": "sha256:bbb"}},
					{"step": "health", "status": "ok"},
				},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "deploy", "web", "--wait")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	for _, want := range []string{"SUCCEED", "sha256:bbb", "127.0.0.1:32768", "validate", "health"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("polls = %d, want 2 (running then succeeded)", calls.Load())
	}
}

func TestDeployWaitFailureExitsOneWithStepAndCode(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 2, "state": "pending",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "01DEPLOY", "seq": 2, "state": "failed",
			"image_ref":    "nginx:v1",
			"failure_step": "health", "failure_code": "E_HEALTH_UNSTABLE",
			"failure_message": "health check did not pass within the overall window (last error: connection refused)",
			"events": []map[string]any{
				{"step": "start", "status": "ok"},
				{"step": "health", "status": "fail"},
			},
		})
	}))

	code, stdout, _ := run(t, "deploy", "web", "--wait")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout = %q", code, stdout)
	}
	for _, want := range []string{"FAILED", "health", "E_HEALTH_UNSTABLE", "connection refused", "tinyship deployments web"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestDeployWithoutWaitQueues(t *testing.T) {
	var posted bool
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = true
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 1, "state": "pending",
		})
	}))
	code, stdout, stderr := run(t, "deploy", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !posted {
		t.Fatal("no POST reached the API")
	}
	if !strings.Contains(stdout, "Queued deploy of web") || !strings.Contains(stdout, "--wait") {
		t.Errorf("stdout = %q, want queue confirmation + follow-up hint", stdout)
	}
}

func TestDeployUsageAndAPIErrors(t *testing.T) {
	code, _, stderr := run(t, "deploy")
	if code != 2 {
		t.Errorf("no name: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "exactly one app name") {
		t.Errorf("stderr = %q", stderr)
	}

	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "E_NOT_FOUND",
			`no such app "ghost"; create it with "tinyship app create"`)
	}))
	code, _, stderr = run(t, "deploy", "ghost")
	if code != 1 {
		t.Errorf("unknown app: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "tinyship app create") {
		t.Errorf("stderr should carry the next step: %q", stderr)
	}
}

// --- logs --------------------------------------------------------------------------------

func TestLogsForwardsQueryAndPrintsBody(t *testing.T) {
	var gotQuery string
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ready to serve\nsecond line\n")
	}))

	code, stdout, stderr := run(t, "logs", "web", "--tail", "25", "--since", "15m")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "ready to serve") {
		t.Errorf("stdout = %q", stdout)
	}
	if gotQuery != "tail=25&since=15m" {
		t.Errorf("query = %q, want tail=25&since=15m", gotQuery)
	}

	// follow adds follow=1.
	run(t, "logs", "web", "--follow")
	if !strings.Contains(gotQuery, "follow=1") {
		t.Errorf("query = %q, want follow=1", gotQuery)
	}
}

func TestLogsUsageAndAPIErrors(t *testing.T) {
	code, _, stderr := run(t, "logs")
	if code != 2 {
		t.Errorf("no name: exit = %d, want 2", code)
	}
	code, _, stderr = run(t, "logs", "web", "--since", "banana")
	if code != 2 {
		t.Errorf("bad since: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Go duration") {
		t.Errorf("stderr = %q", stderr)
	}

	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "E_NO_DEPLOYMENT",
			`app "web" has no deployed container yet; run "tinyship deploy web"`)
	}))
	code, _, stderr = run(t, "logs", "web")
	if code != 1 {
		t.Errorf("409: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_NO_DEPLOYMENT") {
		t.Errorf("stderr = %q", stderr)
	}
}

// --- deployments --------------------------------------------------------------------------

func TestDeploymentsTableAndJSON(t *testing.T) {
	created := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps/web/deployments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"deployments": []map[string]any{
			{"id": "01AAAAAAAAAAAAAAAAAAAAAAAAA", "seq": 2, "state": "failed",
				"image_ref": "nginx:v2", "created_at": created.Format(time.RFC3339)},
			{"id": "01BBBBBBBBBBBBBBBBBBBBBBBBB", "seq": 1, "state": "succeeded",
				"image_ref": "nginx:v1", "created_at": created.Format(time.RFC3339)},
		}})
	}))

	code, stdout, stderr := run(t, "deployments", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"SEQ", "failed", "succeeded", "nginx:v1", "01AAAAAAAA"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = run(t, "deployments", "web", "--json")
	if code != 0 {
		t.Fatalf("--json exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Errorf("invalid JSON: %v (%s)", err, stdout)
	}
}

func TestDeploymentsEmpty(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"deployments": []any{}})
	}))
	code, stdout, _ := run(t, "deployments", "web")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "No deployments") {
		t.Errorf("stdout = %q", stdout)
	}
}

// --- status ---------------------------------------------------------------------------------

func TestStatusReportsHealthAndApps(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "ok", "version": "0.1.0-dev", "commit": "abc123",
				"time": time.Now().UTC().Format(time.RFC3339),
			})
		case "/apps":
			writeJSON(w, http.StatusOK, map[string]any{"apps": []map[string]any{
				{"name": "web", "image": "nginx", "tag": "v1", "desired_state": "running",
					"current_deployment_id": "01DEPLOY123"},
			}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "status")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"API     OK (version 0.1.0-dev, commit abc123)", "Apps    1", "web", "nginx:v1", "running"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("--json exit = %d", code)
	}
	var got struct {
		API  map[string]any `json:"api"`
		Apps []any          `json:"apps"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, stdout)
	}
	if got.API["status"] != "ok" || len(got.Apps) != 1 {
		t.Errorf("json = %s", stdout)
	}
}

func TestStatusUnreachableExitsOne(t *testing.T) {
	// Bind a port then close it: connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()
	t.Setenv("TINYSHIP_API", dead)

	code, _, stderr := run(t, "status")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "tinyship api") {
		t.Errorf("stderr should tell the user how to start the API: %q", stderr)
	}
}

// --- budget --------------------------------------------------------------------

func TestBudgetRendersGrantsAndFree(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/budget" {
			t.Errorf("request = %s %s, want GET /budget", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"budget_mb": 2048, "budget_source": "configured", "mem_total_mb": 4096,
			"committed_mb": 768, "free_mb": 1280, "min_free_disk_mb": 3072,
			"cpu_overcommit": 2.0,
			"apps": []map[string]any{
				{"name": "web", "memory_mb": 512},
				{"name": "api", "memory_mb": 256},
			},
		})
	}))

	code, stdout, stderr := run(t, "budget")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"2048 MiB", "configured", "Committed  768 MiB", "Free       1280 MiB", "web", "api"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestBudgetSetMBPostsOverride(t *testing.T) {
	var got map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/budget":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			got = body
			writeJSON(w, http.StatusOK, map[string]any{
				"budget_mb": 1024, "budget_source": "configured", "mem_total_mb": 4096,
				"committed_mb": 0, "free_mb": 1024, "apps": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/budget":
			writeJSON(w, http.StatusOK, map[string]any{
				"budget_mb": 1024, "budget_source": "configured", "mem_total_mb": 4096,
				"committed_mb": 0, "free_mb": 1024, "apps": []map[string]any{},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "budget", "--set-mb", "1024")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if got == nil || got["budget_mb"] != float64(1024) {
		t.Errorf("posted body = %v, want budget_mb 1024", got)
	}
	if !strings.Contains(stdout, "Budget set to 1024 MiB") {
		t.Errorf("stdout missing confirmation:\n%s", stdout)
	}
}

func TestBudgetRejectsPositionalArgument(t *testing.T) {
	code, _, stderr := run(t, "budget", "extra")
	if code != 2 {
		t.Errorf("exit = %d, want 2 for usage error", code)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("stderr = %q", stderr)
	}
}

// --- deploy --force -------------------------------------------------------------

func TestDeployForceSendsBodyAndWarns(t *testing.T) {
	var rawBody []byte
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apps/web/deploy" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		rawBody, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 3, "state": "pending",
			"forced": true,
		})
	}))

	code, stdout, stderr := run(t, "deploy", "web", "--force")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if !strings.Contains(string(rawBody), `"force":true`) {
		t.Errorf("body = %s, want force:true", rawBody)
	}
	if !strings.Contains(stdout, "Warning: --force") {
		t.Errorf("stdout missing the force warning:\n%s", stdout)
	}
}

func TestDeployWithoutForceSendsNoBody(t *testing.T) {
	var rawBody []byte
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 1, "state": "pending",
		})
	}))

	code, stdout, _ := run(t, "deploy", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stdout = %q", code, stdout)
	}
	if len(rawBody) != 0 {
		t.Errorf("body = %s, want empty without --force", rawBody)
	}
	if strings.Contains(stdout, "Warning") {
		t.Errorf("stdout should not warn without --force:\n%s", stdout)
	}
}

func TestDeployWaitBudgetFailureExitsOne(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"deployment_id": "01DEPLOY", "job_id": "01JOB", "seq": 1, "state": "pending",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "01DEPLOY", "seq": 1, "state": "failed",
			"failure_step": "admission", "failure_code": "E_BUDGET_EXCEEDED",
			"failure_message": "host RAM budget exceeded: over by 128 MiB",
			"events": []map[string]any{
				{"step": "validate", "status": "ok"},
				{"step": "admission", "status": "fail"},
			},
		})
	}))

	code, stdout, _ := run(t, "deploy", "web", "--wait")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout = %q", code, stdout)
	}
	for _, want := range []string{"admission", "E_BUDGET_EXCEEDED", "over by 128 MiB"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// --- rollback --------------------------------------------------------------------

// rollbackAPI serves the two calls the CLI makes: app show (to check
// for named volumes) and the rollback queue.
func rollbackAPI(t *testing.T, volumes []map[string]any, queue func(w http.ResponseWriter)) {
	t.Helper()
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/web":
			cfg := map[string]any{"name": "web", "image": "nginx", "tag": "v1", "port": 80}
			if volumes != nil {
				cfg["volumes"] = volumes
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"app": map[string]any{"name": "web", "config": cfg},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/apps/web/rollback":
			queue(w)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))
}

func TestRollbackQueuesAndExplainsEnvScope(t *testing.T) {
	rollbackAPI(t, nil, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01RB", "seq": 4, "state": "pending", "kind": "rollback",
			"rollback_of_seq": 2, "image_ref": "nginx@sha256:aaa",
			"warnings": []string{},
		})
	})

	code, stdout, stderr := run(t, "rollback", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	for _, want := range []string{"to seq 2", "01RB", "Env vars are NOT rolled back"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestRollbackRequiresYesForWritableVolume(t *testing.T) {
	volumes := []map[string]any{{"name": "data", "path": "/data"}}
	rollbackAPI(t, volumes, func(w http.ResponseWriter) {
		t.Error("rollback must not be queued without --yes when volumes exist")
		writeErr(w, http.StatusConflict, "E_CONFLICT", "x")
	})

	code, _, stderr := run(t, "rollback", "web")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (confirmation required)", code)
	}
	if !strings.Contains(stderr, "does NOT revert") || !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr should warn about volume data and mention --yes:\n%s", stderr)
	}
}

func TestRollbackYesProceedsWithWritableVolume(t *testing.T) {
	volumes := []map[string]any{{"name": "data", "path": "/data"}}
	rollbackAPI(t, volumes, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01RB", "seq": 5, "state": "pending", "kind": "rollback",
			"rollback_of_seq": 3,
			"warnings":        []string{"W_VOLUME_DATA_RETAINED: named volume data keeps its data"},
		})
	})

	code, stdout, stderr := run(t, "rollback", "web", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "W_VOLUME_DATA_RETAINED") {
		t.Errorf("stdout should repeat the volume warning:\n%s", stdout)
	}
}

func TestRollbackReadOnlyVolumeNeedsNoYes(t *testing.T) {
	volumes := []map[string]any{{"name": "cfg", "path": "/cfg", "read_only": true}}
	rollbackAPI(t, volumes, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": "01RB", "seq": 6, "state": "pending", "kind": "rollback",
			"rollback_of_seq": 1, "warnings": []string{},
		})
	})

	code, _, stderr := run(t, "rollback", "web")
	if code != 0 {
		t.Errorf("exit = %d, want 0 for a read-only volume; stderr = %q", code, stderr)
	}
}

func TestRollbackWaitFailureExitsOne(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/web":
			writeJSON(w, http.StatusOK, map[string]any{
				"app": map[string]any{"name": "web", "config": map[string]any{
					"name": "web", "image": "nginx", "port": 80}},
			})
		case r.Method == http.MethodPost:
			writeJSON(w, http.StatusAccepted, map[string]any{
				"deployment_id": "01RB", "seq": 7, "state": "pending", "kind": "rollback",
				"rollback_of_seq": 2, "warnings": []string{},
			})
		case strings.HasSuffix(r.URL.Path, "/deployments/01RB"):
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "01RB", "seq": 7, "state": "failed",
				"failure_step": "pull", "failure_code": "E_IMAGE_NOT_FOUND",
				"failure_message": "manifest unknown",
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, _ := run(t, "rollback", "web", "--wait")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout = %q", code, stdout)
	}
	if !strings.Contains(stdout, "E_IMAGE_NOT_FOUND") || !strings.Contains(stdout, "pull") {
		t.Errorf("stdout should show the pull failure:\n%s", stdout)
	}
}

func TestRollbackRequiresOneAppName(t *testing.T) {
	code, _, stderr := run(t, "rollback")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "exactly one app name") {
		t.Errorf("stderr = %q", stderr)
	}
}
