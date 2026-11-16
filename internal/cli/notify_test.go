package cli

// Notification command tests over a stub API: the flags shape the
// requests, the output reports what the API stored or delivered.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNotifySetPostsWebhookAndPrintsSecret(t *testing.T) {
	var posted map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/notify/sinks" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
			return
		}
		json.NewDecoder(r.Body).Decode(&posted)
		writeJSON(w, http.StatusOK, map[string]any{"sink": map[string]any{
			"kind": "webhook", "url": posted["url"], "secret": "generated-64-hex",
		}})
	}))

	code, stdout, stderr := run(t, "notify", "set", "--url", "https://receiver.test/hook")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if posted["kind"] != "webhook" || posted["url"] != "https://receiver.test/hook" {
		t.Errorf("posted = %v", posted)
	}
	if sent, ok := posted["secret"].(string); ok && sent != "" {
		t.Errorf("posted secret = %v, want absent so the API generates one", posted["secret"])
	}
	if !strings.Contains(stdout, "generated-64-hex") {
		t.Errorf("stdout should print the signing secret:\n%s", stdout)
	}
	if !strings.Contains(stdout, "X-TinyShip-Signature") {
		t.Errorf("stdout should mention the signature header:\n%s", stdout)
	}
}

func TestNotifySetExplicitSecretIsSent(t *testing.T) {
	var posted map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&posted)
		writeJSON(w, http.StatusOK, map[string]any{"sink": posted})
	}))
	code, _, stderr := run(t, "notify", "set", "--url", "https://x.test/h",
		"--secret", "chosen")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if posted["secret"] != "chosen" {
		t.Errorf("posted secret = %v, want chosen", posted["secret"])
	}
}

func TestNotifySetRequiresURL(t *testing.T) {
	calls := 0
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeErr(w, http.StatusInternalServerError, "E_INTERNAL", "should not be called")
	}))
	code, _, stderr := run(t, "notify", "set")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--url is required") {
		t.Errorf("stderr = %q", stderr)
	}
	if calls != 0 {
		t.Errorf("API called %d times, want 0", calls)
	}
}

func TestNotifyListRendersSinksAndEvents(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/notify":
			writeJSON(w, http.StatusOK, map[string]any{
				"sinks": []map[string]any{{
					"kind": "webhook", "url": "https://receiver.test/hook",
					"secret": "abc123",
				}, {
					"kind": "telegram", "bot_token": "123456:AAAA", "chat_id": "-100",
				}},
				"kinds": []string{"webhook", "telegram"},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/notify/events"):
			writeJSON(w, http.StatusOK, map[string]any{"events": []map[string]any{{
				"id": 4, "type": "deploy_failed", "status": "delivered",
				"sent_to": []string{"webhook"},
			}, {
				"id": 3, "type": "disk_low", "status": "failed",
				"last_error": "status 500: boom",
			}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "notify", "list")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{
		"Sinks:", "webhook", "https://receiver.test/hook", "secret: abc123",
		"chat -100", "token ****AAAA", // the bot token itself stays masked
		"Recent events:", "#4", "deploy_failed", "sent to webhook",
		"#3", "failed", "status 500: boom",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "123456:AAAA") {
		t.Errorf("stdout leaks the bot token:\n%s", stdout)
	}
}

func TestNotifyListEmptySuggestsSetup(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/notify":
			writeJSON(w, http.StatusOK, map[string]any{"sinks": []any{}, "kinds": []string{}})
		case "/notify/events":
			writeJSON(w, http.StatusOK, map[string]any{"events": []any{}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))
	code, stdout, stderr := run(t, "notify", "list")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "No notification sinks configured.") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

func TestNotifyTestReportsDelivery(t *testing.T) {
	var polls atomic.Int32
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/notify":
			writeJSON(w, http.StatusOK, map[string]any{
				"sinks": []map[string]any{{
					"kind": "webhook", "url": "https://receiver.test/hook", "secret": "s",
				}},
				"kinds": []string{"webhook"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/notify/test":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["target_kind"] != "" {
				t.Errorf("target_kind = %v", body["target_kind"])
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"id": 9})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/notify/events"):
			status, lastErr := "pending", ""
			if polls.Add(1) >= 2 {
				status = "delivered"
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": []map[string]any{{
				"id": 9, "type": "test", "status": status,
				"sent_to": []string{"webhook"}, "last_error": lastErr,
			}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "notify", "test")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "Test notification delivered to: webhook") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestNotifyTestPassesTargetKind(t *testing.T) {
	var target any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/notify":
			writeJSON(w, http.StatusOK, map[string]any{
				"sinks": []map[string]any{
					{"kind": "webhook", "url": "https://x.test/h"},
					{"kind": "telegram", "bot_token": "1:a", "chat_id": "2"},
				},
				"kinds": []string{"webhook", "telegram"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/notify/test":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			target = body["target_kind"]
			writeJSON(w, http.StatusAccepted, map[string]any{"id": 1})
		case strings.HasPrefix(r.URL.Path, "/notify/events"):
			writeJSON(w, http.StatusOK, map[string]any{"events": []map[string]any{{
				"id": 1, "type": "test", "status": "delivered",
				"sent_to": []string{"telegram"},
			}}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "notify", "test", "telegram")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if target != "telegram" {
		t.Errorf("target_kind posted = %v", target)
	}
	if !strings.Contains(stdout, "telegram") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestNotifyTestWithoutSinksFailsFast(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sinks": []any{}, "kinds": []any{}})
	}))
	code, _, stderr := run(t, "notify", "test")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no notification sinks configured") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNotifyTestFailureReportsCause(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/notify":
			writeJSON(w, http.StatusOK, map[string]any{
				"sinks": []map[string]any{{"kind": "webhook", "url": "https://x.test/h"}},
				"kinds": []string{"webhook"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/notify/test":
			writeJSON(w, http.StatusAccepted, map[string]any{"id": 5})
		case strings.HasPrefix(r.URL.Path, "/notify/events"):
			writeJSON(w, http.StatusOK, map[string]any{"events": []map[string]any{{
				"id": 5, "type": "test", "status": "failed",
				"last_error": "webhook: status 500",
			}}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))
	code, _, stderr := run(t, "notify", "test")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "webhook: status 500") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNotifyRemoveDeletesByKind(t *testing.T) {
	var method, path string
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{"removed": "slack"})
	}))
	code, stdout, stderr := run(t, "notify", "remove", "slack")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if method != http.MethodDelete || path != "/notify/sinks/slack" {
		t.Errorf("request = %s %s", method, path)
	}
	if !strings.Contains(stdout, "Removed slack sink") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestNotifyAddTelegramRequiresFlags(t *testing.T) {
	code, _, stderr := run(t, "notify", "add", "telegram")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "bot_token") {
		t.Errorf("stderr = %q, want the missing field named", stderr)
	}
}

func TestNotifyAddTelegramPostsSink(t *testing.T) {
	var posted map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&posted)
		writeJSON(w, http.StatusOK, map[string]any{"sink": posted})
	}))
	code, stdout, stderr := run(t, "notify", "add", "telegram",
		"--token", "123:abc", "--chat-id", "-100500")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if posted["kind"] != "telegram" || posted["bot_token"] != "123:abc" ||
		posted["chat_id"] != "-100500" {
		t.Errorf("posted = %v", posted)
	}
	if !strings.Contains(stdout, "Telegram sink added") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestNotifyAddEmailReadsPasswordFile(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pw, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var posted map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&posted)
		writeJSON(w, http.StatusOK, map[string]any{"sink": posted})
	}))
	code, _, stderr := run(t, "notify", "add", "email",
		"--host", "smtp.test", "--username", "u", "--password-file", pw,
		"--from", "ship@tiny.test", "--to", "ops@tiny.test")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if posted["kind"] != "email" || posted["host"] != "smtp.test" {
		t.Errorf("posted = %v", posted)
	}
	if posted["password"] != "hunter2" {
		t.Errorf("password = %v, want the trimmed file content", posted["password"])
	}
	if posted["port"] != float64(587) {
		t.Errorf("port = %v, want the 587 default", posted["port"])
	}
}

func TestNotifyAddWebhookIsRedirectedToSet(t *testing.T) {
	code, _, stderr := run(t, "notify", "add", "webhook", "--url", "https://x.test/h")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "notify set") {
		t.Errorf("stderr = %q, want the redirect to notify set", stderr)
	}
}
