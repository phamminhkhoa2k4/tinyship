package cli

// Domain command tests: request shape, pending_dns warnings, table and
// JSON rendering, and the usage/error exit codes.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDomainAddSendsHostnameAndRendersActive(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "dom1", "hostname": "blog.example.com", "status": "active",
		})
	}))

	code, stdout, stderr := run(t, "domain", "add", "web", "blog.example.com")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if gotPath != "POST /apps/web/domains" {
		t.Errorf("request = %q, want POST /apps/web/domains", gotPath)
	}
	if gotBody["hostname"] != "blog.example.com" {
		t.Errorf("body hostname = %v", gotBody["hostname"])
	}
	if _, present := gotBody["skip_dns_check"]; present {
		t.Errorf("skip_dns_check must be omitted when false: %v", gotBody)
	}
	if !strings.Contains(stdout, "blog.example.com") || !strings.Contains(stdout, "active") {
		t.Errorf("output = %q", stdout)
	}
}

func TestDomainAddPendingDNSWarnsLoudly(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "dom1", "hostname": "blog.example.com", "status": "pending_dns",
			"warnings": []string{"no A/AAAA record points at this host; domain stays pending_dns and is not routed"},
		})
	}))

	code, stdout, stderr := run(t, "domain", "add", "web", "blog.example.com")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"NOT ROUTED YET", "pending_dns", "warning:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestDomainAddSkipDNSCheckFlag(t *testing.T) {
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "dom1", "hostname": "blog.example.com", "status": "active",
			"warnings": []string{"DNS check skipped: certificates are only issued once records point at this host"},
		})
	}))

	code, _, stderr := run(t, "domain", "add", "web", "blog.example.com", "--skip-dns-check")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if gotBody["skip_dns_check"] != true {
		t.Errorf("skip_dns_check = %v, want true", gotBody["skip_dns_check"])
	}
}

func TestDomainAddJSONOutput(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": "dom1", "hostname": "blog.example.com", "status": "active",
		})
	}))

	code, stdout, stderr := run(t, "domain", "add", "web", "blog.example.com", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Errorf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if decoded["status"] != "active" {
		t.Errorf("status = %v", decoded["status"])
	}
}

func TestDomainAddErrorsAndUsage(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "E_DOMAIN_TAKEN", "hostname belongs to another app")
	}))
	code, _, stderr := run(t, "domain", "add", "web", "blog.example.com")
	if code != 1 || !strings.Contains(stderr, "E_DOMAIN_TAKEN") {
		t.Errorf("exit = %d stderr = %q, want 1 with E_DOMAIN_TAKEN", code, stderr)
	}

	// Usage errors need no API.
	code, _, _ = run(t, "domain", "add", "web")
	if code != 2 {
		t.Errorf("missing hostname: exit = %d, want 2", code)
	}
	code, _, _ = run(t, "domain", "nope")
	if code != 2 {
		t.Errorf("unknown subcommand: exit = %d, want 2", code)
	}
	code, _, _ = run(t, "domain")
	if code != 2 {
		t.Errorf("missing subcommand: exit = %d, want 2", code)
	}
}

func TestDomainListRendersTableAndEmptyHint(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apps/web/domains" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"domains": []map[string]any{
			{"id": "d1", "hostname": "blog.example.com", "status": "active", "is_primary": true},
			{"id": "d2", "hostname": "shop.example.com", "status": "pending_dns"},
		}})
	}))

	code, stdout, stderr := run(t, "domain", "list", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"HOSTNAME", "blog.example.com", "active", "yes",
		"shop.example.com", "pending_dns"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestDomainListEmptyShowsHint(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"domains": []any{}})
	}))

	code, stdout, stderr := run(t, "domain", "list", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "tinyship domain add") {
		t.Errorf("empty output missing hint:\n%s", stdout)
	}
}

func TestDomainListUsageError(t *testing.T) {
	code, _, _ := run(t, "domain", "list")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}
