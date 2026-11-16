package notify

// Delivery adapters: one Send per channel kind. All of them share a
// caller-provided HTTP client with a hard timeout so a dead endpoint
// cannot wedge the dispatcher; email uses net/smtp with mandatory
// STARTTLS (port 587 only, enforced at config time).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// MaxBodyBytes bounds how much of a response we read when reporting a
// non-2xx back into last_error.
const MaxBodyBytes = 4 << 10

// Payload is the webhook JSON body. It is what the HMAC signs; it
// never contains the secret itself or any sink credentials.
type Payload struct {
	ID       int64          `json:"id"`
	Type     string         `json:"type"`
	App      string         `json:"app,omitempty"`
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Detail   map[string]any `json:"detail,omitempty"`
	DupCount int            `json:"dup_count,omitempty"`
	At       time.Time      `json:"at"`
}

// Send delivers ev to one sink and returns an error when the channel
// rejected it (or is unreachable) — the dispatcher then retries.
func Send(ctx context.Context, client *http.Client, sink Sink, ev Event, secret string) error {
	msg := ev.Render()
	switch sink.Kind {
	case "webhook":
		return sendWebhook(ctx, client, sink, ev, msg, secret)
	case "telegram":
		return sendTelegram(ctx, client, sink, msg)
	case "discord":
		return sendDiscord(ctx, client, sink, msg)
	case "slack":
		return sendSlack(ctx, client, sink, msg)
	case "email":
		return sendEmail(sink, ev, msg)
	default:
		return fmt.Errorf("notify: unknown sink kind %q", sink.Kind)
	}
}

// body builds the signed webhook payload for an event.
func body(ev Event, msg string) ([]byte, error) {
	return json.Marshal(Payload{
		ID:       ev.ID,
		Type:     ev.Type,
		App:      ev.AppName,
		Title:    ev.Title,
		Message:  msg,
		Detail:   ev.Detail,
		DupCount: ev.DupCount,
		At:       time.Now().UTC(),
	})
}

func sendWebhook(ctx context.Context, client *http.Client, sink Sink, ev Event, msg, secret string) error {
	raw, err := body(ev, msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sink.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tinyship/notify")
	if secret != "" {
		req.Header.Set("X-TinyShip-Signature", Signature(secret, time.Now(), raw))
	}
	return do(client, req)
}

func sendTelegram(ctx context.Context, client *http.Client, sink Sink, msg string) error {
	raw, err := json.Marshal(map[string]any{
		"chat_id": sink.ChatID,
		"text":    msg,
	})
	if err != nil {
		return err
	}
	// The bot token is part of the URL path — never log this URL.
	endpoint := "https://api.telegram.org/bot" + sink.BotToken + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram: status %d: %s", resp.StatusCode,
			strings.TrimSpace(string(respBody)))
	}
	// Telegram reports logical errors in an ok:false body with 200.
	var tr struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &tr); err == nil && !tr.OK {
		return fmt.Errorf("telegram: %s", tr.Description)
	}
	return nil
}

func sendDiscord(ctx context.Context, client *http.Client, sink Sink, msg string) error {
	return postJSON(ctx, client, sink.URL, map[string]any{"content": msg}, "discord")
}

func sendSlack(ctx context.Context, client *http.Client, sink Sink, msg string) error {
	return postJSON(ctx, client, sink.URL, map[string]any{"text": msg}, "slack")
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, payload any, kind string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := do(client, req); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	return nil
}

// do executes a request and turns non-2xx into an error carrying a
// bounded response snippet for last_error.
func do(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBodyBytes))
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	return fmt.Errorf("status %d: %s", resp.StatusCode,
		strings.TrimSpace(string(snippet)))
}

// tlsConfig pins the SMTP server name for certificate verification.
func tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// sendEmail delivers through STARTTLS submission. The connection is
// refused in clear when the server does not offer STARTTLS: a password
// must never travel in plaintext.
func sendEmail(sink Sink, ev Event, msg string) error {
	addr := fmt.Sprintf("%s:%d", sink.Host, sink.Port)
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("email: dial: %w", err)
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("email: server does not offer STARTTLS")
	}
	if err := c.StartTLS(tlsConfig(host)); err != nil {
		return fmt.Errorf("email: starttls: %w", err)
	}
	auth := smtp.PlainAuth("", sink.Username, sink.Password, host)
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("email: auth: %w", err)
	}
	if err := c.Mail(sink.From); err != nil {
		return fmt.Errorf("email: from: %w", err)
	}
	for _, rcpt := range strings.Split(sink.To, ",") {
		rcpt = strings.TrimSpace(rcpt)
		if rcpt == "" {
			continue
		}
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("email: rcpt %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("email: data: %w", err)
	}
	subject := ev.Title
	if ev.AppName != "" {
		subject += " (" + ev.AppName + ")"
	}
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: TinyShip · %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		sink.From, sink.To, subject, msg)
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: body: %w", err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("email: quit: %w", err)
	}
	return nil
}
