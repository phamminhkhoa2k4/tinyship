package notify

// Channel config and signature tests: the signing/verification pair is
// pinned to an externally computed vector, sink validation enforces the
// per-kind requirements, and the stored list round-trips.

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// TestSignatureKnownVector pins the wire format against an HMAC-SHA256
// computed outside this package, so a refactor cannot silently change
// what receivers verify.
func TestSignatureKnownVector(t *testing.T) {
	body := []byte(`{"type":"test","title":"hi"}`)
	ts := time.Unix(1700000000, 0)
	got := Signature("test-secret", ts, body)
	want := "t=1700000000,v1=e9686f91f3e215150a207c509446710fdb8f641a38c4ae5ba4b2359b3eaf2520"
	if got != want {
		t.Errorf("Signature = %q\nwant %q", got, want)
	}
}

func TestVerifyAcceptsFreshSignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	now := time.Now()
	sig := Signature("k", now, body)
	if err := Verify("k", sig, body, now, 5*time.Minute); err != nil {
		t.Errorf("Verify fresh: %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	body := []byte(`{"a":1}`)
	now := time.Now()
	sig := Signature("right", now, body)
	if err := Verify("wrong", sig, body, now, 5*time.Minute); err == nil {
		t.Error("Verify wrong secret: want error")
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	now := time.Now()
	sig := Signature("k", now, []byte(`{"a":1}`))
	if err := Verify("k", sig, []byte(`{"a":2}`), now, 5*time.Minute); err == nil {
		t.Error("Verify tampered body: want error")
	}
}

func TestVerifyRejectsReplayOutsideTolerance(t *testing.T) {
	body := []byte(`{}`)
	old := time.Now().Add(-time.Hour)
	sig := Signature("k", old, body)
	if err := Verify("k", sig, body, time.Now(), 5*time.Minute); err == nil {
		t.Error("Verify replay: want error outside tolerance")
	}
	// Still accepted inside the tolerance window.
	if err := Verify("k", sig, body, old.Add(4*time.Minute), 5*time.Minute); err != nil {
		t.Errorf("Verify inside tolerance: %v", err)
	}
}

func TestVerifyRejectsMalformedHeader(t *testing.T) {
	now := time.Now()
	for _, hdr := range []string{"", "garbage", "t=abc", "v1=deadbeef", "t=1700000000"} {
		if err := Verify("k", hdr, []byte("{}"), now, time.Minute); err == nil {
			t.Errorf("Verify(%q): want error", hdr)
		}
	}
}

func TestNewSecretIsRandom32ByteHex(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	b, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("secret length = %d, want 64 hex chars", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("secret not hex: %v", err)
	}
	if a == b {
		t.Error("two generated secrets are identical")
	}
}

func TestSinkValidatePerKind(t *testing.T) {
	cases := []struct {
		name    string
		sink    Sink
		wantErr string // "" means valid
	}{
		{"webhook ok", Sink{Kind: "webhook", URL: "https://x.test/hook"}, ""},
		{"webhook no url", Sink{Kind: "webhook"}, "url"},
		{"webhook bad scheme", Sink{Kind: "webhook", URL: "ftp://x"}, "http"},
		{"telegram ok", Sink{Kind: "telegram", BotToken: "123:abc", ChatID: "-100"}, ""},
		{"telegram no token", Sink{Kind: "telegram", ChatID: "1"}, "bot_token"},
		{"telegram no chat", Sink{Kind: "telegram", BotToken: "123:abc"}, "chat_id"},
		{"discord ok", Sink{Kind: "discord", URL: "https://discord.test/api/webhook/1"}, ""},
		{"slack ok", Sink{Kind: "slack", URL: "https://hooks.slack.test/services/T"}, ""},
		{"email ok", Sink{Kind: "email", Host: "smtp.test", Port: 587,
			Username: "u", Password: "p", From: "a@b.c", To: "d@e.f"}, ""},
		{"email wrong port", Sink{Kind: "email", Host: "smtp.test", Port: 25,
			Username: "u", Password: "p", From: "a@b.c", To: "d@e.f"}, "587"},
		{"email no host", Sink{Kind: "email", Port: 587, Username: "u",
			Password: "p", From: "a@b.c", To: "d@e.f"}, "host"},
		{"email no creds", Sink{Kind: "email", Host: "smtp.test", Port: 587,
			From: "a@b.c", To: "d@e.f"}, "username"},
		{"unknown kind", Sink{Kind: "pager"}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := tc.sink
			err := sink.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestEmailSinkDefaultsPortToSTARTTLS(t *testing.T) {
	sink := Sink{Kind: "email", Host: "smtp.test",
		Username: "u", Password: "p", From: "a@b.c", To: "d@e.f"}
	if err := sink.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if sink.Port != 587 {
		t.Errorf("port = %d, want the 587 default kept on the caller's sink", sink.Port)
	}
}

func TestParseSinksRoundTrip(t *testing.T) {
	in := []Sink{
		{Kind: "webhook", URL: "https://x.test/hook", Secret: "s3cret"},
		{Kind: "telegram", BotToken: "1:abc", ChatID: "42"},
	}
	encoded, err := EncodeSinks(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := ParseSinks(encoded)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestParseSinksEmptyAndInvalid(t *testing.T) {
	if sinks, err := ParseSinks(""); err != nil || sinks != nil {
		t.Errorf("empty = %v/%v, want nil/nil", sinks, err)
	}
	if _, err := ParseSinks("{not json"); err == nil {
		t.Error("malformed json: want error")
	}
	if _, err := ParseSinks(`[{"kind":"webhook"}]`); err == nil {
		t.Error("sink failing validation: want error")
	}
}
