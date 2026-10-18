package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// swapStdin points the secret input at a fixed string for one test.
func swapStdin(t *testing.T, input string) {
	t.Helper()
	prev := stdin
	stdin = strings.NewReader(input)
	t.Cleanup(func() { stdin = prev })
}

func TestEnvListRendersValuesAndHidesSecrets(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apps/web/env" {
			t.Errorf("request = %s %s, want GET /apps/web/env", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"env": []map[string]any{
				{"key": "MODE", "kind": "plain", "value": "prod"},
				{"key": "TOKEN", "kind": "secret"},
			},
		})
	}))

	code, stdout, stderr := run(t, "env", "list", "web")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	for _, want := range []string{"MODE", "prod", "TOKEN", "(secret, not shown)", "next deploy"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestEnvListEmptyAndMissingApp(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/ghost/env" {
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", `no such app "ghost"`)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"env": []any{}})
	}))
	code, stdout, _ := run(t, "env", "list", "web")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "No variables") {
		t.Errorf("stdout = %q, want empty-state hint", stdout)
	}
	code, _, stderr := run(t, "env", "list", "ghost")
	if code != 1 || !strings.Contains(stderr, "E_NOT_FOUND") {
		t.Errorf("missing app: code=%d stderr=%q", code, stderr)
	}
}

func TestEnvSetPlain(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, map[string]any{
			"key": "MODE", "kind": "plain", "message": "takes effect on the next deploy",
		})
	}))

	code, stdout, stderr := run(t, "env", "set", "web", "MODE", "prod")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodPut || gotPath != "/apps/web/env/MODE" {
		t.Errorf("request = %s %s, want PUT /apps/web/env/MODE", gotMethod, gotPath)
	}
	if gotBody["value"] != "prod" || gotBody["kind"] != "plain" {
		t.Errorf("body = %v, want plain prod", gotBody)
	}
	if !strings.Contains(stdout, "next deploy") {
		t.Errorf("stdout = %q, want next-deploy notice", stdout)
	}
}

func TestEnvSetSecretFromStdin(t *testing.T) {
	swapStdin(t, "hunter2\n")
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, map[string]any{"key": "TOKEN", "kind": "secret"})
	}))

	code, stdout, stderr := run(t, "env", "set", "web", "TOKEN", "--secret")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotBody["kind"] != "secret" {
		t.Errorf("kind = %v, want secret", gotBody["kind"])
	}
	// Stdin trailing newline is trimmed, nothing more.
	if gotBody["value"] != "hunter2" {
		t.Errorf("value = %q, want hunter2", gotBody["value"])
	}
	if !strings.Contains(stdout, "encrypted") {
		t.Errorf("stdout = %q, want 'encrypted'", stdout)
	}
}

func TestEnvSetSecretFromFile(t *testing.T) {
	path := writeTemp(t, "file-secret\n")
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, map[string]any{"key": "TOKEN", "kind": "secret"})
	}))

	code, _, stderr := run(t, "env", "set", "web", "TOKEN", "--secret-file", path)
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotBody["value"] != "file-secret" || gotBody["kind"] != "secret" {
		t.Errorf("body = %v, want secret file-secret", gotBody)
	}
}

func TestEnvSetUsageErrors(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("usage errors must not reach the API")
	}))
	cases := [][]string{
		{"env", "set", "web"},      // no key
		{"env", "set", "web", "K"}, // no value, no --secret
		{"env", "set", "web", "K", "v", "--secret", "--secret-file", "x"}, // both inputs
	}
	for _, args := range cases {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
	}
}

func TestEnvUnset(t *testing.T) {
	var gotMethod, gotPath string
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{"key": "MODE", "deleted": true})
	}))
	code, stdout, stderr := run(t, "env", "unset", "web", "MODE")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodDelete || gotPath != "/apps/web/env/MODE" {
		t.Errorf("request = %s %s, want DELETE", gotMethod, gotPath)
	}
	if !strings.Contains(stdout, "next deploy") {
		t.Errorf("stdout = %q, want next-deploy notice", stdout)
	}
}

func TestEnvDispatchUnknownSubcommand(t *testing.T) {
	code, _, stderr := run(t, "env", "frobnicate")
	if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestCredentialSetReadsTokenFromStdin(t *testing.T) {
	swapStdin(t, "tok_abc\n")
	var gotPath string
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, map[string]any{
			"credential": map[string]any{
				"name": "ghcr-linh", "registry": "ghcr.io", "username": "linh",
			},
		})
	}))

	code, stdout, stderr := run(t, "credential", "set", "ghcr-linh",
		"--registry", "ghcr.io", "--username", "linh")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotPath != "/credentials" {
		t.Errorf("path = %q, want /credentials", gotPath)
	}
	if gotBody["token"] != "tok_abc" {
		t.Errorf("token = %q, want tok_abc (stdin, trimmed)", gotBody["token"])
	}
	if strings.Contains(stdout, "tok_abc") {
		t.Errorf("stdout leaks the token: %s", stdout)
	}
	if !strings.Contains(stdout, "sealed") {
		t.Errorf("stdout = %q, want sealed confirmation", stdout)
	}
}

func TestCredentialSetReadsTokenFromFile(t *testing.T) {
	path := writeTemp(t, "tok-file")
	var gotBody map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, map[string]any{
			"credential": map[string]any{"name": "n", "registry": "r", "username": "u"},
		})
	}))
	code, _, stderr := run(t, "credential", "set", "n",
		"--registry", "r", "--username", "u", "--token-file", path)
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotBody["token"] != "tok-file" {
		t.Errorf("token = %q, want tok-file", gotBody["token"])
	}
}

func TestCredentialSetUsageErrors(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("usage errors must not reach the API")
	}))
	// The last case reaches the stdin read; pin it empty so the test
	// never touches the real process stdin.
	swapStdin(t, "")
	cases := [][]string{
		{"credential", "set"},                                            // no name
		{"credential", "set", "n", "--registry", "r"},                    // no username
		{"credential", "set", "n", "--username", "u"},                    // no registry
		{"credential", "set", "n", "--registry", "r", "--username", "u"}, // empty token
	}
	for _, args := range cases {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
	}
}

func TestCredentialListRendersTableWithoutTokens(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"credentials": []map[string]any{
				{"name": "ghcr-linh", "registry": "ghcr.io", "username": "linh"},
			},
		})
	}))
	code, stdout, stderr := run(t, "credential", "list")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	for _, want := range []string{"ghcr-linh", "ghcr.io", "linh"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestCredentialDelete(t *testing.T) {
	var gotMethod, gotPath string
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{"name": "old", "deleted": true})
	}))
	code, stdout, stderr := run(t, "credential", "delete", "old")
	if code != 0 {
		t.Fatalf("exit = %d stderr = %q", code, stderr)
	}
	if gotMethod != http.MethodDelete || gotPath != "/credentials/old" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(stdout, "deleted") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestEnvSetJSONOutput(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"key": "K", "kind": "plain"})
	}))
	code, stdout, _ := run(t, "env", "set", "web", "K", "v", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
}

// writeTemp writes a small file for file-based input tests.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "secret")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := io.WriteString(f, content); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return f.Name()
}
