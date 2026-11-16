package notify

// Channel configuration and payload rendering. Sinks live in the
// database settings (operator surface: CLI + API), never in config.yaml
// — that file only records effective configuration. The payload a
// receiver sees carries no secret: the HMAC secret signs the body from
// outside the body.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// SettingKey is the database settings key holding the sink list as a
// JSON array of Sink.
const SettingKey = "notify_sinks"

// Kinds lists every supported channel; CLI validation and docs derive
// from this list.
var Kinds = []string{"webhook", "telegram", "discord", "slack", "email"}

// Sink is one notification channel. Which fields are required depends
// on Kind: webhook needs URL; telegram needs BotToken and ChatID;
// discord and slack need URL (their webhook endpoints); email needs
// Host, Username, Password, From and To.
type Sink struct {
	Kind     string `json:"kind"`
	URL      string `json:"url,omitempty"`
	Secret   string `json:"secret,omitempty"` // webhook HMAC signing secret
	BotToken string `json:"bot_token,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	Host     string `json:"host,omitempty"` // email SMTP host
	Port     int    `json:"port,omitempty"` // email SMTP port (STARTTLS)
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"` // comma-separated recipients
}

// Validate checks the sink's required fields for its kind, filling the
// default SMTP port on an email sink (hence the pointer receiver: the
// caller keeps the default).
func (s *Sink) Validate() error {
	switch s.Kind {
	case "webhook":
		if err := validHTTPURL(s.URL); err != nil {
			return err
		}
	case "telegram":
		if s.BotToken == "" {
			return errors.New("telegram sink requires bot_token")
		}
		if s.ChatID == "" {
			return errors.New("telegram sink requires chat_id")
		}
	case "discord", "slack":
		if err := validHTTPURL(s.URL); err != nil {
			return err
		}
	case "email":
		if s.Host == "" {
			return errors.New("email sink requires host")
		}
		if s.Port == 0 {
			s.Port = 587
		}
		if s.Port != 587 {
			return errors.New("email sink supports STARTTLS on port 587 only")
		}
		if s.Username == "" || s.Password == "" {
			return errors.New("email sink requires username and password")
		}
		if s.From == "" || s.To == "" {
			return errors.New("email sink requires from and to")
		}
	default:
		return fmt.Errorf("unknown notification kind %q (want %s)",
			s.Kind, strings.Join(Kinds, ", "))
	}
	return nil
}

func validHTTPURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("url must be http or https: %q", raw)
	}
	return nil
}

// ParseSinks decodes and validates the stored sink list.
func ParseSinks(raw string) ([]Sink, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var sinks []Sink
	if err := json.Unmarshal([]byte(raw), &sinks); err != nil {
		return nil, fmt.Errorf("notify: parse sinks: %w", err)
	}
	for _, s := range sinks {
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("notify: %s sink: %w", s.Kind, err)
		}
	}
	return sinks, nil
}

// EncodeSinks serializes a sink list for storage.
func EncodeSinks(sinks []Sink) (string, error) {
	b, err := json.Marshal(sinks)
	if err != nil {
		return "", fmt.Errorf("notify: encode sinks: %w", err)
	}
	return string(b), nil
}

// NewSecret returns a fresh 32-byte hex HMAC secret. Auto-generated
// when the operator does not supply one, so a webhook is signable out
// of the box.
func NewSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("notify: generate secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Signature is the value of the X-TinyShip-Signature header:
// t=<unix seconds>,v1=<hex hmac-sha256(secret, t+"."+body)> — Stripe
// style, so a receiver can reject replays by checking t against a
// tolerance window before verifying the MAC.
func Signature(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts.Unix())
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// Verify checks a signature header against a body within tolerance
// seconds of now. Used by tests and available to receiver authors as
// the reference check.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var (
		tsStr string
		want  string
	)
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			tsStr = v
		case "v1":
			want = v
		}
	}
	if tsStr == "" || want == "" {
		return errors.New("notify: signature missing t or v1")
	}
	var ts int64
	if _, err := fmt.Sscanf(tsStr, "%d", &ts); err != nil {
		return fmt.Errorf("notify: signature timestamp: %w", err)
	}
	when := time.Unix(ts, 0)
	if d := now.Sub(when); d > tolerance || d < -tolerance {
		return fmt.Errorf("notify: signature timestamp outside tolerance (%s)", d)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(want)) {
		return errors.New("notify: signature mismatch")
	}
	return nil
}
