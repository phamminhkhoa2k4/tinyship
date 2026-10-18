package cli

// app delete and domain remove tests: argument handling, the --yes
// gate, and the exact API requests the CLI issues. Response rendering
// is asserted on the queued job id the CLI prints back.

import (
	"net/http"
	"strings"
	"testing"
)

func TestAppDeleteRequiresYesConfirmation(t *testing.T) {
	var called bool
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
	}))

	code, stdout, stderr := run(t, "app", "delete", "web")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage refusal)", code)
	}
	if !strings.Contains(stderr, "--yes is required") {
		t.Errorf("stderr missing confirmation hint: %q", stderr)
	}
	if called {
		t.Error("DELETE was sent without --yes; the confirmation gate failed")
	}
	_ = stdout
}

func TestAppDeleteQueuesTeardown(t *testing.T) {
	var gotMethod, gotPath string
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, http.StatusAccepted, map[string]any{
			"app": "web", "job_id": "01JAPPDELETE000000000000000",
			"message": "deletion queued",
		})
	}))

	code, stdout, stderr := run(t, "app", "delete", "web", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodDelete || gotPath != "/apps/web" {
		t.Errorf("request = %s %s, want DELETE /apps/web", gotMethod, gotPath)
	}
	for _, want := range []string{"Deletion of web queued", "01JAPPDELETE000000000000000"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestAppDeleteNotFound(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "E_NOT_FOUND", `no such app "ghost"`)
	}))

	code, _, stderr := run(t, "app", "delete", "ghost", "--yes")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_NOT_FOUND") {
		t.Errorf("stderr missing API error code: %q", stderr)
	}
}

func TestAppDeleteNeedsOneName(t *testing.T) {
	code, _, stderr := run(t, "app", "delete")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if !strings.Contains(stderr, "app name") {
		t.Errorf("stderr missing arity message: %q", stderr)
	}
}

func TestDomainRemoveQueuesRouteSync(t *testing.T) {
	var gotMethod, gotPath string
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, http.StatusAccepted, map[string]any{
			"app": "web", "hostname": "web.example.com",
			"job_id":  "01JDOMSYNC000000000000000000",
			"message": "hostname detached",
		})
	}))

	code, stdout, stderr := run(t, "domain", "remove", "web", "web.example.com")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodDelete || gotPath != "/apps/web/domains/web.example.com" {
		t.Errorf("request = %s %s, want DELETE /apps/web/domains/web.example.com",
			gotMethod, gotPath)
	}
	for _, want := range []string{"web.example.com", "detached", "01JDOMSYNC000000000000000000"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestDomainRemoveMissingArgs(t *testing.T) {
	code, _, stderr := run(t, "domain", "remove", "web")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if !strings.Contains(stderr, "hostname") {
		t.Errorf("stderr missing arity message: %q", stderr)
	}
}

func TestDomainRemoveNotFoundHost(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "does not own hostname")
	}))

	code, _, stderr := run(t, "domain", "remove", "web", "never.example.com")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_NOT_FOUND") {
		t.Errorf("stderr missing API error code: %q", stderr)
	}
}
