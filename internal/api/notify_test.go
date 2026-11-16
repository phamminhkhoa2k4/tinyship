package api

// Notification routes: sink registration (with the auto-generated
// webhook secret), removal, test queuing, and event inspection.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/notify"
)

func TestNotifyConfigEmptyByDefault(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, err := http.Get(srv.URL + "/notify")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	sinks, ok := body["sinks"].([]any)
	if !ok || len(sinks) != 0 {
		t.Errorf("sinks = %v, want empty list", body["sinks"])
	}
	kinds, _ := body["kinds"].([]any)
	if len(kinds) != len(notify.Kinds) {
		t.Errorf("kinds = %v, want %d entries", body["kinds"], len(notify.Kinds))
	}
}

func TestAddNotifySinkGeneratesWebhookSecret(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, err := http.Post(srv.URL+"/notify/sinks", "application/json",
		strings.NewReader(`{"kind":"webhook","url":"https://receiver.test/hook"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := decodeBody(t, resp)
	sink, _ := body["sink"].(map[string]any)
	if sink == nil {
		t.Fatalf("sink = %v", body)
	}
	secret, _ := sink["secret"].(string)
	if len(secret) != 64 {
		t.Errorf("generated secret length = %d, want 64 hex chars", len(secret))
	}

	// The stored config now serves it back — that is how the operator
	// copies it to the receiver.
	got, err := http.Get(srv.URL + "/notify")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer got.Body.Close()
	cfg := decodeBody(t, got)
	sinks := cfg["sinks"].([]any)
	if len(sinks) != 1 {
		t.Fatalf("sinks = %v, want one", sinks)
	}
	stored := sinks[0].(map[string]any)
	if stored["secret"] != secret {
		t.Errorf("stored secret = %v, want %s", stored["secret"], secret)
	}

	// Replacing the same kind must not duplicate it.
	resp2, err := http.Post(srv.URL+"/notify/sinks", "application/json",
		strings.NewReader(`{"kind":"webhook","url":"https://other.test/hook","secret":"chosen"}`))
	if err != nil {
		t.Fatalf("post replace: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("replace status = %d", resp2.StatusCode)
	}
	got2, err := http.Get(srv.URL + "/notify")
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}
	defer got2.Body.Close()
	if n := len(decodeBody(t, got2)["sinks"].([]any)); n != 1 {
		t.Errorf("sinks after replace = %d, want 1", n)
	}
}

func TestAddNotifySinkExplicitSecretIsKept(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	resp, err := http.Post(srv.URL+"/notify/sinks", "application/json",
		strings.NewReader(`{"kind":"webhook","url":"https://receiver.test/h","secret":"mine"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	sink := decodeBody(t, resp)["sink"].(map[string]any)
	if sink["secret"] != "mine" {
		t.Errorf("secret = %v, want mine kept", sink["secret"])
	}
}

func TestAddNotifySinkValidation(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	cases := []struct {
		name, payload string
	}{
		{"webhook no url", `{"kind":"webhook"}`},
		{"unknown kind", `{"kind":"pagerduty"}`},
		{"email wrong port", `{"kind":"email","host":"smtp.test","port":25,` +
			`"username":"u","password":"p","from":"a@b.c","to":"d@e.f"}`},
		{"not json", `nonsense`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/notify/sinks", "application/json",
				strings.NewReader(tc.payload))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if code := decodeBody(t, resp)["error"].(map[string]any)["code"]; code != "E_VALIDATION" {
				t.Errorf("code = %v", code)
			}
		})
	}
}

func TestRemoveNotifySink(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	put := func(payload string) *http.Response {
		resp, err := http.Post(srv.URL+"/notify/sinks", "application/json",
			strings.NewReader(payload))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp
	}
	put(`{"kind":"telegram","bot_token":"1:a","chat_id":"42"}`)
	put(`{"kind":"slack","url":"https://hooks.slack.test/x"}`)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/notify/sinks/telegram", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	got, err := http.Get(srv.URL + "/notify")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer got.Body.Close()
	sinks := decodeBody(t, got)["sinks"].([]any)
	if len(sinks) != 1 || sinks[0].(map[string]any)["kind"] != "slack" {
		t.Errorf("sinks = %v, want only slack", sinks)
	}

	// Removing what is not there is a 404, not a silent success.
	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/notify/sinks/telegram", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("delete again: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", resp2.StatusCode)
	}
}

func TestNotifyTestQueuesEventAndFiltersTarget(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())

	resp, err := http.Post(srv.URL+"/notify/test", "application/json",
		strings.NewReader(`{"target_kind":"webhook"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	id := int64(decodeBody(t, resp)["id"].(float64))
	if id == 0 {
		t.Fatal("id = 0")
	}

	// Unknown target is refused up front.
	bad, err := http.Post(srv.URL+"/notify/test", "application/json",
		strings.NewReader(`{"target_kind":"carrier-pigeon"}`))
	if err != nil {
		t.Fatalf("post bad: %v", err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("bad target status = %d, want 400", bad.StatusCode)
	}

	// The queued event shows up in the feed with its filter intact.
	got, err := http.Get(srv.URL + "/notify/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer got.Body.Close()
	events := decodeBody(t, got)["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want the test event", events)
	}
	ev := events[0].(map[string]any)
	if ev["type"] != "test" || ev["status"] != "pending" {
		t.Errorf("event = %v, want pending test", ev)
	}
	detail := ev["detail"].(string)
	var parsed struct {
		TargetKind string `json:"target_kind"`
	}
	if err := json.Unmarshal([]byte(detail), &parsed); err != nil {
		t.Fatalf("detail not json: %v", err)
	}
	if parsed.TargetKind != "webhook" {
		t.Errorf("target_kind = %q", parsed.TargetKind)
	}
}

func TestNotifyEventsPaginationBound(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	for _, n := range []string{"0", "201", "abc"} {
		resp, err := http.Get(srv.URL + "/notify/events?n=" + n)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("n=%s status = %d, want 400", n, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
