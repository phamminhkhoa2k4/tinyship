package api

// API tests: create, deploy queue, history, log proxy over the worker
// socket. Everything runs against a temp store; the log proxy
// talks to a scripted worker server on a temp Unix socket.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/worker"
)

const testConfig = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080
}`

func testServer(t *testing.T, runDir string) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tinyship.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	mux := newMux(s, runDir, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func errCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := decodeBody(t, resp)
	errObj, _ := body["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func createApp(t *testing.T, srv *httptest.Server, cfg string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/apps", "application/json", strings.NewReader(cfg))
	if err != nil {
		t.Fatalf("POST /apps: %v", err)
	}
	return resp
}

// --- healthz ------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	for _, key := range []string{"version", "commit", "time"} {
		if _, ok := body[key]; !ok {
			t.Errorf("body missing %q", key)
		}
	}
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	resp, err := http.Post(srv.URL+"/healthz", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// --- POST /apps ------------------------------------------------------

func TestCreateAppValidatesStrictly(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	resp := createApp(t, srv, testConfig)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", resp.StatusCode, drain(resp))
	}
	body := decodeBody(t, resp)
	if body["name"] != "web" || body["image"] != "traefik/whoami" {
		t.Errorf("created app = %v", body)
	}
	if body["config"] == nil {
		t.Error("response must echo the stored config")
	}

	// Unknown field: 400 naming the field.
	resp = createApp(t, srv, `{"name":"x1","image":"nginx","port":80,"privalige":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "E_VALIDATION" {
		t.Errorf("code = %q, want E_VALIDATION", code)
	}

	// Invalid config: 400 with the failing rule.
	resp = createApp(t, srv, `{"name":"UPPER","image":"nginx","port":80}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: status = %d, want 400", resp.StatusCode)
	}

	// Duplicate name: 409 (names are unique).
	resp = createApp(t, srv, testConfig)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: status = %d, want 409; body %s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()
}

func TestCreateAppReturnsWarnings(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	// `tag: latest` (mutable) triggers W_MUTABLE_TAG.
	resp := createApp(t, srv, `{"name":"warned","image":"nginx","tag":"latest","port":80}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d; body %s", resp.StatusCode, drain(resp))
	}
	body := decodeBody(t, resp)
	warnings, _ := body["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if strings.Contains(fmt.Sprint(w), "W_MUTABLE_TAG") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want W_MUTABLE_TAG", warnings)
	}
}

// --- GET apps ------------------------------------------------------------------

func TestListAndShowApps(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	for _, name := range []string{"zeta", "alpha"} {
		resp := createApp(t, srv,
			fmt.Sprintf(`{"name":%q,"image":"nginx","tag":"v1","port":80}`, name))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, resp.StatusCode, drain(resp))
		}
		resp.Body.Close()
	}

	resp, err := http.Get(srv.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	body := decodeBody(t, resp)
	apps, _ := body["apps"].([]any)
	if len(apps) != 2 {
		t.Fatalf("apps = %v, want 2", body)
	}
	first := apps[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Errorf("first app = %v, want alpha (ordered by name)", first["name"])
	}

	resp, err = http.Get(srv.URL + "/apps/web")
	if err != nil {
		t.Fatalf("GET missing app: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing app: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(srv.URL + "/apps/alpha")
	if err != nil {
		t.Fatalf("GET /apps/alpha: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body = decodeBody(t, resp)
	appObj, _ := body["app"].(map[string]any)
	if appObj == nil || appObj["name"] != "alpha" {
		t.Errorf("body = %v", body)
	}
}

// --- POST deploy ------------------------------------------------------

func TestDeployQueuesJobAndDeployment(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()

	resp, err := http.Post(srv.URL+"/apps/web/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy status = %d, want 202; body %s", resp.StatusCode, drain(resp))
	}
	body := decodeBody(t, resp)
	depID, _ := body["deployment_id"].(string)
	jobID, _ := body["job_id"].(string)
	if depID == "" || jobID == "" {
		t.Fatalf("deploy response = %v", body)
	}
	if body["seq"] != float64(1) {
		t.Errorf("seq = %v, want 1", body["seq"])
	}

	ctx := context.Background()
	dep, err := s.GetDeployment(ctx, depID)
	if err != nil {
		t.Fatalf("deployment row: %v", err)
	}
	if dep.State != store.DepPending || dep.JobID != jobID {
		t.Errorf("deployment = %+v", dep)
	}
	if dep.ImageRef != "traefik/whoami:v1.8" {
		t.Errorf("image_ref = %q, want digest>tag>bare derivation", dep.ImageRef)
	}
	job, err := s.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("job row: %v", err)
	}
	if job.Type != worker.DeployJobType || !strings.Contains(job.PayloadJSON, depID) {
		t.Errorf("job = %+v", job)
	}

	// previous_deployment_id points at the app's LIVE deployment (the one
	// retire_old must replace); a pending request with nothing running has
	// no previous. Simulate the worker finishing seq 1 first.
	if err := s.SucceedDeployment(ctx, appIDOf(t, s, depID), depID); err != nil {
		t.Fatalf("finalize first deploy: %v", err)
	}
	resp, err = http.Post(srv.URL+"/apps/web/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("deploy 2: %v", err)
	}
	body = decodeBody(t, resp)
	resp.Body.Close()
	dep2, err := s.GetDeployment(ctx, body["deployment_id"].(string))
	if err != nil {
		t.Fatalf("deployment 2 row: %v", err)
	}
	if dep2.Seq != 2 {
		t.Errorf("seq = %d, want 2", dep2.Seq)
	}
	if dep2.PreviousDeploymentID == nil || *dep2.PreviousDeploymentID != depID {
		t.Errorf("previous_deployment_id = %v, want %s", dep2.PreviousDeploymentID, depID)
	}
}

// appIDOf resolves the app that owns depID (small test helper).
func appIDOf(t *testing.T, s *store.Store, depID string) string {
	t.Helper()
	dep, err := s.GetDeployment(context.Background(), depID)
	if err != nil {
		t.Fatal(err)
	}
	return dep.AppID
}

func TestDeployUnknownApp404(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, err := http.Post(srv.URL+"/apps/ghost/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- deployment history ----------------------------------------------

func TestDeploymentsEndpoints(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()
	resp, err := http.Post(srv.URL+"/apps/web/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	body := decodeBody(t, resp)
	depID := body["deployment_id"].(string)

	// Simulate a finished timeline; the API must echo it in order.
	ctx := context.Background()
	for _, ev := range []struct{ step, status string }{
		{"validate", store.EvStart}, {"validate", store.EvOK},
		{"pull", store.EvOK}, {"health", store.EvFail},
	} {
		if err := s.AppendDeploymentEvent(ctx, depID, ev.step, ev.status,
			map[string]any{"step": ev.step}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}

	resp, err = http.Get(srv.URL + "/apps/web/deployments")
	if err != nil {
		t.Fatalf("GET deployments: %v", err)
	}
	body = decodeBody(t, resp)
	deps, _ := body["deployments"].([]any)
	if len(deps) != 1 {
		t.Fatalf("deployments = %v", body)
	}

	resp, err = http.Get(srv.URL + "/deployments/" + depID)
	if err != nil {
		t.Fatalf("GET deployment: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body = decodeBody(t, resp)
	events, _ := body["events"].([]any)
	if len(events) != 4 {
		t.Fatalf("events = %v, want 4 timeline rows", events)
	}
	last := events[3].(map[string]any)
	if last["step"] != "health" || last["status"] != "fail" {
		t.Errorf("last event = %v, want health/fail", last)
	}

	resp, err = http.Get(srv.URL + "/deployments/does-not-exist")
	if err != nil {
		t.Fatalf("GET missing deployment: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing deployment: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- logs proxy -------------------------------------------------

// fakeWorker serves /logs over a Unix socket and records the query it saw.
type fakeWorker struct {
	query    url.Values
	status   int
	body     string
	requests int
}

func (f *fakeWorker) start(t *testing.T, runDir string) {
	t.Helper()
	sock := filepath.Join(runDir, worker.LogSockName)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen worker sock: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		f.query = r.URL.Query()
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		if f.body != "" {
			_, _ = io.WriteString(w, f.body)
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Close()
		_ = os.Remove(sock)
	})
}

func deployAndAttachContainer(t *testing.T, srv *httptest.Server, s *store.Store) (appID, containerID string) {
	t.Helper()
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()
	resp, err := http.Post(srv.URL+"/apps/web/deploy", "application/json", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	body := decodeBody(t, resp)
	depID := body["deployment_id"].(string)
	ctx := context.Background()
	dep, err := s.GetDeployment(ctx, depID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeploymentContainer(ctx, dep.ID, "cid-123", "ts-web-1"); err != nil {
		t.Fatal(err)
	}
	// Finalize so apps.current_deployment_id points at the container the
	// logs route reads (the same state a successful deploy leaves behind).
	if err := s.SucceedDeployment(ctx, dep.AppID, dep.ID); err != nil {
		t.Fatal(err)
	}
	return dep.AppID, "cid-123"
}

func TestLogsProxyForwardsToWorkerSocket(t *testing.T) {
	runDir := t.TempDir()
	fw := &fakeWorker{body: "line one\nline two\n"}
	fw.start(t, runDir)
	srv, s := testServer(t, runDir)
	appID, containerID := deployAndAttachContainer(t, srv, s)

	resp, err := http.Get(srv.URL + "/apps/web/logs?tail=7&since=15m&follow=1&sudo=1")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "line two") {
		t.Errorf("proxied body = %q", body)
	}
	if fw.query.Get("app_id") != appID || fw.query.Get("container_id") != containerID {
		t.Errorf("worker saw app_id=%q container_id=%q, want %q/%q",
			fw.query.Get("app_id"), fw.query.Get("container_id"), appID, containerID)
	}
	if fw.query.Get("tail") != "7" || fw.query.Get("since") != "15m" || fw.query.Get("follow") != "1" {
		t.Errorf("worker saw %v, want tail/since/follow forwarded", fw.query)
	}
	if fw.query.Has("sudo") {
		t.Error("undocumented query parameter was forwarded to the worker")
	}
}

func TestLogsProxyErrorPaths(t *testing.T) {
	runDir := t.TempDir()
	fw := &fakeWorker{status: http.StatusForbidden,
		body: `{"error":{"code":"E_NOT_MANAGED","message":"denied"}}`}
	fw.start(t, runDir)
	srv, s := testServer(t, runDir)

	// No deployment yet: 409 before any worker round-trip.
	resp := createApp(t, srv, testConfig)
	resp.Body.Close()
	resp, err := http.Get(srv.URL + "/apps/web/logs")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("no-deployment logs: status = %d, want 409", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "E_NO_DEPLOYMENT" {
		t.Errorf("code = %q, want E_NO_DEPLOYMENT", code)
	}
	if fw.requests != 0 {
		t.Error("worker was contacted for an app without a container")
	}

	// Unknown app: 404.
	resp, err = http.Get(srv.URL + "/apps/ghost/logs")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown app logs: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Worker refusal (E_NOT_MANAGED) passes through with its status+code.
	_, _ = deployAndAttachContainer(t, srv, s)
	resp, err = http.Get(srv.URL + "/apps/web/logs")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("refusal passthrough: status = %d, body = %s", resp.StatusCode, body)
	}
	if code := errCode(t, resp); code != "E_NOT_MANAGED" {
		t.Errorf("code = %q, want E_NOT_MANAGED passthrough", code)
	}
}

func TestLogsProxyWithoutWorkerIs503(t *testing.T) {
	runDir := t.TempDir() // no worker socket in there
	srv, s := testServer(t, runDir)
	appID, _ := deployAndAttachContainer(t, srv, s)
	_ = appID

	resp, err := http.Get(srv.URL + "/apps/web/logs")
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "E_WORKER_UNAVAILABLE" {
		t.Errorf("code = %q, want E_WORKER_UNAVAILABLE", code)
	}
}

func drain(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}
