package api

// The hibernation toggle: it flips one app's flag in place, without a
// redeploy, and refuses anything that is not exactly that request.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func hibernatePost(t *testing.T, srv *httptest.Server, app, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/apps/"+app+"/hibernate", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestHibernateToggleFlipsTheFlagWithoutADeploy(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	createApp(t, srv, testConfig)

	resp := hibernatePost(t, srv, "web", `{"enabled": true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable: status = %d body %s", resp.StatusCode, decodeBody(t, resp))
	}
	body := decodeBody(t, resp)
	if body["hibernate_state"] != "" {
		t.Errorf("hibernate_state = %v, want empty before anything sleeps", body["hibernate_state"])
	}
	cfg, _ := body["config"].(map[string]any)
	hib, _ := cfg["hibernate"].(map[string]any)
	if hib["enabled"] != true {
		t.Errorf("config.hibernate = %v, want enabled", hib)
	}

	// Read back through the ordinary show endpoint.
	if hib := showHibernation(t, srv)["enabled"]; hib != true {
		t.Errorf("stored config hibernate = %v, want enabled", hib)
	}

	// An empty body inverts the current value.
	resp = hibernatePost(t, srv, "web", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle: status = %d body %s", resp.StatusCode, decodeBody(t, resp))
	}
	if hib := showHibernation(t, srv)["enabled"]; hib != false {
		t.Errorf("after the toggle hibernate = %v, want disabled", hib)
	}

	// The toggle must never queue a deploy: it changes only when the
	// sweep may sleep the app and which upstream the proxy dials.
	deps := getOK(t, srv, "/apps/web/deployments")
	if list, ok := deps["deployments"].([]any); ok && len(list) != 0 {
		t.Errorf("deployments = %d, want none queued by the toggle", len(list))
	}
}

func TestHibernateToggleRejectsBadBodies(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	createApp(t, srv, testConfig)

	for _, body := range []string{
		`{"enabled": "yes"}`,
		`{"enabled": true, "extra": 1}`,
		`{"nonsense": true}`,
		``,
	} {
		resp := hibernatePost(t, srv, "web", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
		if code := errCode(t, resp); code != "E_VALIDATION" {
			t.Errorf("body %q: code = %s, want E_VALIDATION", body, code)
		}
	}
}

func TestHibernateToggleOnAMissingAppIsA404(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	resp := hibernatePost(t, srv, "ghost", `{"enabled": true}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if code := errCode(t, resp); code != "E_NOT_FOUND" {
		t.Errorf("code = %s, want E_NOT_FOUND", code)
	}
}

// showHibernation reads the stored hibernate block back through the
// ordinary show endpoint, so the test proves what a later request will
// actually parse.
func showHibernation(t *testing.T, srv *httptest.Server) map[string]any {
	t.Helper()
	show := getOK(t, srv, "/apps/web")
	app, _ := show["app"].(map[string]any)
	cfg, _ := app["config"].(map[string]any)
	hib, _ := cfg["hibernate"].(map[string]any)
	if hib == nil {
		t.Fatalf("stored config has no hibernate block: %v", cfg)
	}
	return hib
}

// getOK performs a GET and fails the test on a non-200 answer.
func getOK(t *testing.T, srv *httptest.Server, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d", path, resp.StatusCode)
	}
	return decodeBody(t, resp)
}
