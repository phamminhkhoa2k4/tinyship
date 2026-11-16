package notify

// Adapter tests over a canned transport: no network, but every wire
// shape is asserted — headers, signed body, and the platform-specific
// failure conventions (Telegram reports errors in a 200 body).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cannedClient(t *testing.T, status int, body string,
	got ...func(*http.Request, []byte)) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		if len(got) > 0 && got[0] != nil {
			got[0](req, raw)
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
}

func testEvent() Event {
	return Event{
		ID: 7, Type: "deploy_failed", AppName: "web", Title: "Deploy failed",
		Detail: map[string]any{"step": "pull", "code": "E_PULL"},
	}
}

func TestWebhookSendSignsBodyAndCarriesNoSecret(t *testing.T) {
	const secret = "top-secret-key"
	var hdr string
	var body []byte
	client := cannedClient(t, http.StatusNoContent, "", func(req *http.Request, raw []byte) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if u := req.URL.String(); u != "https://receiver.test/hook" {
			t.Errorf("url = %s", u)
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		hdr = req.Header.Get("X-TinyShip-Signature")
		body = raw
	})
	sink := Sink{Kind: "webhook", URL: "https://receiver.test/hook", Secret: secret}
	if err := Send(context.Background(), client, sink, testEvent(), secret); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if hdr == "" {
		t.Fatal("signature header missing")
	}
	if err := Verify(secret, hdr, body, time.Now(), time.Minute); err != nil {
		t.Errorf("receiver-side verify: %v", err)
	}
	// The signed body is what the receiver parses; the secret must
	// never travel inside it.
	if strings.Contains(string(body), secret) {
		t.Errorf("body leaks the signing secret: %s", body)
	}
	if !strings.Contains(string(body), `"type":"deploy_failed"`) ||
		!strings.Contains(string(body), "Deploy failed") {
		t.Errorf("body missing event fields: %s", body)
	}
	if err := Verify("other", hdr, body, time.Now(), time.Minute); err == nil {
		t.Error("verify with wrong secret: want error")
	}
}

func TestWebhookSendUnsignedWhenNoSecret(t *testing.T) {
	var hdr string
	client := cannedClient(t, http.StatusOK, "{}", func(req *http.Request, _ []byte) {
		hdr = req.Header.Get("X-TinyShip-Signature")
	})
	sink := Sink{Kind: "webhook", URL: "https://receiver.test/hook"}
	if err := Send(context.Background(), client, sink, testEvent(), ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if hdr != "" {
		t.Errorf("signature header = %q, want none without a secret", hdr)
	}
}

func TestWebhookNon2xxIsErrorWithSnippet(t *testing.T) {
	client := cannedClient(t, http.StatusInternalServerError, "boom went the server")
	sink := Sink{Kind: "webhook", URL: "https://receiver.test/hook", Secret: "s"}
	err := Send(context.Background(), client, sink, testEvent(), "s")
	if err == nil {
		t.Fatal("Send 500: want error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q should carry status and snippet", err)
	}
}

func TestTelegramOkFalseBodyIsAnError(t *testing.T) {
	client := cannedClient(t, http.StatusOK,
		`{"ok":false,"description":"chat not found"}`)
	sink := Sink{Kind: "telegram", BotToken: "1:abc", ChatID: "-100"}
	err := Send(context.Background(), client, sink, testEvent(), "")
	if err == nil {
		t.Fatal("Send ok:false: want error")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error %q should carry Telegram's description", err)
	}
}

func TestTelegramOkBodySucceeds(t *testing.T) {
	var reqBody []byte
	client := cannedClient(t, http.StatusOK, `{"ok":true,"result":{}}`,
		func(_ *http.Request, raw []byte) { reqBody = raw })
	sink := Sink{Kind: "telegram", BotToken: "1:abc", ChatID: "-100"}
	if err := Send(context.Background(), client, sink, testEvent(), ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(string(reqBody), `"chat_id":"-100"`) ||
		!strings.Contains(string(reqBody), "Deploy failed") {
		t.Errorf("telegram body = %s", reqBody)
	}
}

func TestDiscordAndSlackPayloadShapes(t *testing.T) {
	cases := []struct {
		kind, wantKey string
	}{
		{"discord", `"content"`},
		{"slack", `"text"`},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			var raw []byte
			client := cannedClient(t, http.StatusOK, "", func(_ *http.Request, b []byte) {
				raw = b
			})
			sink := Sink{Kind: tc.kind, URL: "https://hooks.test/x"}
			if err := Send(context.Background(), client, sink, testEvent(), ""); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if !strings.Contains(string(raw), tc.wantKey) ||
				!strings.Contains(string(raw), "Deploy failed") {
				t.Errorf("%s body = %s, want %s with the message", tc.kind, raw, tc.wantKey)
			}
		})
	}
}

func TestRenderIsStableAndSuggestsRealCommands(t *testing.T) {
	ev := Event{
		ID: 3, Type: "deploy_failed", AppName: "web", Title: "Deploy failed",
		Detail:   map[string]any{"code": "E_PULL", "step": "pull"},
		DupCount: 2,
	}
	msg := ev.Render()
	for _, want := range []string{
		"TinyShip · Deploy failed", "(web)", "step: pull", "code: E_PULL",
		"tinyship logs web", "repeated ×2",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("render missing %q:\n%s", want, msg)
		}
	}
	// Detail lines keep a deterministic order across runs.
	if i, j := strings.Index(msg, "step: pull"), strings.Index(msg, "code: E_PULL"); i > j {
		t.Errorf("detail order not stable:\n%s", msg)
	}

	// Unknown or empty app never suggests a broken command.
	bare := Event{Type: "oom", Title: "killed"}
	if cmd := bare.SuggestedCommand(); cmd != "tinyship stats" {
		t.Errorf("oom suggestion = %q", cmd)
	}
	test := Event{Type: "test"}
	if cmd := test.SuggestedCommand(); cmd != "" {
		t.Errorf("test suggestion = %q, want none", cmd)
	}
}
