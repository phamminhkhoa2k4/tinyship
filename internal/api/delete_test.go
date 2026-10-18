package api

// DELETE endpoint tests: app teardown marks the row and queues a job;
// domain detach removes the row and queues a route replay. Both assert
// the durable effect (marking + queued job), not the worker's cleanup,
// which internal/worker covers.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const deleteTestConfig = `{"name":"web","image":"nginx","tag":"1.25","port":8080}`

func doDelete(t *testing.T, method, url string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp, decodeBody(t, resp)
}

func TestDeleteAppMarksRowAndQueuesJob(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	createApp(t, srv, deleteTestConfig).Body.Close()

	resp, body := doDelete(t, http.MethodDelete, srv.URL+"/apps/web")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%v)", resp.StatusCode, body)
	}

	// The row is marked for deletion immediately so admission and
	// budget math stop counting it before the worker finishes.
	app, err := s.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if !app.Deleting {
		t.Error("app not marked deleting right after DELETE")
	}

	// A teardown job is queued for the worker.
	var jobType, jobState string
	if err := s.DB().QueryRow(
		`SELECT type, state FROM jobs WHERE app_id = ? AND type = 'app_delete'`,
		app.ID).Scan(&jobType, &jobState); err != nil {
		t.Fatalf("queued teardown job: %v", err)
	}
	if jobState != "queued" {
		t.Errorf("job state = %q, want queued", jobState)
	}
}

func TestDeleteAppIsIdempotentWhileDeleting(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	createApp(t, srv, deleteTestConfig).Body.Close()

	resp, _ := doDelete(t, http.MethodDelete, srv.URL+"/apps/web")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first delete status = %d, want 202", resp.StatusCode)
	}
	// A client retrying a lost response must not see a spurious
	// conflict; teardown converges.
	resp, _ = doDelete(t, http.MethodDelete, srv.URL+"/apps/web")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("retry delete status = %d, want 202", resp.StatusCode)
	}
	app, err := s.GetAppByName(context.Background(), "web")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if !app.Deleting {
		t.Error("app no longer marked deleting after retry")
	}
}

func TestDeleteAppNotFound(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, body := doDelete(t, http.MethodDelete, srv.URL+"/apps/ghost")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "E_NOT_FOUND" {
		t.Errorf("code = %q, want E_NOT_FOUND", code)
	}
}

func TestDeleteDomainDropsRowAndQueuesSync(t *testing.T) {
	srv, s := testServer(t, t.TempDir())
	createApp(t, srv, deleteTestConfig).Body.Close()
	// Attach a hostname so there is a row to drop.
	resp, err := http.Post(srv.URL+"/apps/web/domains", "application/json",
		strings.NewReader(`{"hostname":"web.example.com","skip_dns_check":true}`))
	if err != nil {
		t.Fatalf("attach domain: %v", err)
	}
	resp.Body.Close()

	resp, body := doDelete(t, http.MethodDelete,
		srv.URL+"/apps/web/domains/web.example.com")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%v)", resp.StatusCode, body)
	}

	// The row is gone immediately.
	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM domains WHERE hostname = ?`,
		"web.example.com").Scan(&n); err != nil {
		t.Fatalf("count domain: %v", err)
	}
	if n != 0 {
		t.Errorf("domain rows after delete = %d, want 0", n)
	}

	// A route replay is queued so the proxy stops routing the host.
	var jobType string
	if err := s.DB().QueryRow(
		`SELECT type FROM jobs WHERE type = 'domain_sync'`).
		Scan(&jobType); err != nil {
		t.Fatalf("queued domain_sync job: %v", err)
	}
	if jobType != "domain_sync" {
		t.Errorf("job type = %q, want domain_sync", jobType)
	}
}

func TestDeleteDomainNotFoundHost(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	createApp(t, srv, deleteTestConfig).Body.Close()
	resp, _ := doDelete(t, http.MethodDelete,
		srv.URL+"/apps/web/domains/never.example.com")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteDomainNotFoundApp(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, _ := doDelete(t, http.MethodDelete,
		srv.URL+"/apps/ghost/domains/web.example.com")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
