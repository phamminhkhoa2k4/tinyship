package cli

// Notification channels: the operator registers sinks, sends a test
// event, and reads recent delivery results. Everything goes through the
// API, which stores the sink list in the database settings; the worker's
// dispatcher does the actual sending.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/notify"
)

func dispatchNotify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usageNotify(stderr)
		return 2
	}
	switch args[0] {
	case "list", "ls", "status":
		return cmdNotifyList(ctx, args[1:], stdout, stderr)
	case "set":
		return cmdNotifySet(ctx, args[1:], stdout, stderr)
	case "add":
		return cmdNotifyAdd(ctx, args[1:], stdout, stderr)
	case "remove", "rm":
		return cmdNotifyRemove(ctx, args[1:], stdout, stderr)
	case "test":
		return cmdNotifyTest(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usageNotify(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship notify: unknown subcommand %q\n", args[0])
		usageNotify(stderr)
		return 2
	}
}

func usageNotify(w io.Writer) {
	fmt.Fprint(w, `usage:
  tinyship notify list [--json]
  tinyship notify set --url <url> [--secret <hex>]
  tinyship notify add telegram --token <bot-token> --chat-id <id>
  tinyship notify add discord --url <webhook-url>
  tinyship notify add slack --url <webhook-url>
  tinyship notify add email --host <smtp> [--port 587] --username <user>
                        --password-file <path> --from <addr> --to <addr[,addr]>
  tinyship notify remove <kind>
  tinyship notify test [kind] [--json]

Manage notification channels: webhook (signed with HMAC-SHA256),
telegram, discord, slack, and email. 'tinyship notify list' shows the
configured sinks and the most recent delivery attempts.
`)
}

// notifyConfig mirrors GET /notify.
type notifyConfig struct {
	Sinks []notify.Sink `json:"sinks"`
	Kinds []string      `json:"kinds"`
}

// notifyEvent mirrors one row of GET /notify/events.
type notifyEvent struct {
	ID          int64    `json:"id"`
	Type        string   `json:"type"`
	App         string   `json:"app"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	DupCount    int      `json:"dup_count"`
	Status      string   `json:"status"`
	Attempts    int      `json:"attempts"`
	SentTo      []string `json:"sent_to"`
	NextAttempt string   `json:"next_attempt"`
	CreatedAt   string   `json:"created_at"`
	DeliveredAt string   `json:"delivered_at"`
	LastError   string   `json:"last_error"`
}

type notifyEvents struct {
	Events []notifyEvent `json:"events"`
}

func cmdNotifyList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("notify list", stderr, `usage: tinyship notify list [--json]

Show the configured notification sinks (including the webhook signing
secret) and the most recent queued events with their delivery status.
`)
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship notify list: unexpected argument %q\n", pos[0])
		return 2
	}

	c := newAPIClient()
	var cfg notifyConfig
	if err := c.doJSON(ctx, http.MethodGet, "/notify", nil, &cfg); err != nil {
		fmt.Fprintf(stderr, "tinyship notify list: %v\n", err)
		return 1
	}
	var events notifyEvents
	if err := c.doJSON(ctx, http.MethodGet, "/notify/events?n=10", nil, &events); err != nil {
		fmt.Fprintf(stderr, "tinyship notify list: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(map[string]any{
			"sinks": cfg.Sinks, "events": events.Events,
		})
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	if len(cfg.Sinks) == 0 {
		fmt.Fprintln(stdout, "No notification sinks configured.")
		fmt.Fprintln(stdout, "Add one: tinyship notify set --url <receiver-url>")
	} else {
		fmt.Fprintln(stdout, "Sinks:")
		for _, sink := range cfg.Sinks {
			fmt.Fprintf(stdout, "  %-9s %s\n", sink.Kind, describeSink(sink))
		}
	}
	if len(events.Events) == 0 {
		fmt.Fprintln(stdout, "No events yet.")
		return 0
	}
	fmt.Fprintln(stdout, "Recent events:")
	for _, ev := range events.Events {
		line := fmt.Sprintf("  #%d  %-16s %-9s", ev.ID, ev.Type, ev.Status)
		if len(ev.SentTo) > 0 {
			line += "  sent to " + strings.Join(ev.SentTo, ",")
		}
		if ev.Status == "failed" && ev.LastError != "" {
			line += "  " + ev.LastError
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// describeSink renders one sink for text output. Credentials that the
// operator never needs to read back (bot token, password) are masked;
// the webhook signing secret is printed in full because the receiver
// needs it to verify signatures.
func describeSink(sink notify.Sink) string {
	switch sink.Kind {
	case "webhook":
		out := sink.URL
		if sink.Secret != "" {
			out += "  secret: " + sink.Secret
		}
		return out
	case "telegram":
		return "chat " + sink.ChatID + "  token " + mask(sink.BotToken)
	case "discord", "slack":
		return sink.URL
	case "email":
		return fmt.Sprintf("%s:%d  %s → %s  user %s pass %s",
			sink.Host, sink.Port, sink.From, sink.To, sink.Username, mask(sink.Password))
	}
	return ""
}

func mask(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 4 {
		return "****"
	}
	return "****" + v[len(v)-4:]
}

func cmdNotifySet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("notify set", stderr, `usage: tinyship notify set --url <url> [--secret <hex>]

Register (or replace) the webhook sink. The payload it receives is
signed with X-TinyShip-Signature: an HMAC-SHA256 of
"<timestamp>.<body>" keyed by the secret. Pass --secret to choose the
key; otherwise a fresh random secret is generated and shown by
'tinyship notify list'.
`)
	url := fs.String("url", "", "webhook receiver URL (http or https)")
	secret := fs.String("secret", "", "HMAC signing secret (default: generated)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship notify set: unexpected argument %q\n", pos[0])
		return 2
	}
	if *url == "" {
		fmt.Fprintln(stderr, "tinyship notify set: --url is required")
		return 2
	}
	sink := notify.Sink{Kind: "webhook", URL: *url, Secret: *secret}
	return postSink(ctx, sink, stdout, stderr, "Webhook set")
}

func cmdNotifyAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tinyship notify add: kind is required (telegram, discord, slack, email)")
		usageNotify(stderr)
		return 2
	}
	kind := args[0]
	rest := args[1:]
	switch kind {
	case "webhook":
		fmt.Fprintln(stderr, "tinyship notify add: use `tinyship notify set --url` for the webhook sink")
		return 2
	case "telegram", "discord", "slack", "email":
	default:
		fmt.Fprintf(stderr, "tinyship notify add: unknown kind %q (want telegram, discord, slack, email)\n", kind)
		return 2
	}

	name := "notify add " + kind
	fs := flagSet(name, stderr, "usage: tinyship notify add "+kind+" [flags]\n")
	var token, chatID, url, host, username, password, passwordFile, from, to *string
	var port *int
	switch kind {
	case "telegram":
		token = fs.String("token", "", "bot token from BotFather")
		chatID = fs.String("chat-id", "", "chat or channel id")
	case "discord", "slack":
		url = fs.String("url", "", "webhook URL")
	case "email":
		host = fs.String("host", "", "SMTP host")
		username = fs.String("username", "", "SMTP username")
		password = fs.String("password", "", "SMTP password (prefer --password-file)")
		passwordFile = fs.String("password-file", "", "read the SMTP password from this file")
		from = fs.String("from", "", "sender address")
		to = fs.String("to", "", "recipient address(es), comma-separated")
		port = fs.Int("port", 587, "SMTP port (STARTTLS, 587 only)")
	}
	pos, code := parseFlags(fs, rest)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship notify add: unexpected argument %q\n", pos[0])
		return 2
	}

	sink := notify.Sink{Kind: kind}
	switch kind {
	case "telegram":
		sink.BotToken, sink.ChatID = *token, *chatID
	case "discord", "slack":
		sink.URL = *url
	case "email":
		pass := *password
		if *passwordFile != "" {
			raw, err := os.ReadFile(*passwordFile)
			if err != nil {
				fmt.Fprintf(stderr, "tinyship notify add: read --password-file: %v\n", err)
				return 1
			}
			pass = strings.TrimRight(string(raw), "\r\n")
		}
		sink.Host, sink.Port = *host, *port
		sink.Username, sink.Password = *username, pass
		sink.From, sink.To = *from, *to
	}
	if err := sink.Validate(); err != nil {
		fmt.Fprintf(stderr, "tinyship notify add: %v\n", err)
		return 2
	}
	return postSink(ctx, sink, stdout, stderr, capitalize(kind)+" sink added")
}

func cmdNotifyRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("notify remove", stderr, `usage: tinyship notify remove <kind>

Remove every configured sink of the given kind.
`)
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship notify remove: exactly one kind is required")
		return 2
	}
	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodDelete, "/notify/sinks/"+pos[0], nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship notify remove: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(stderr, "tinyship notify remove: %v\n", apiError(resp))
		return 1
	}
	fmt.Fprintf(stdout, "Removed %s sink\n", pos[0])
	return 0
}

// cmdNotifyTest queues a test event and waits for the dispatcher to
// report the outcome, so one command answers "does my channel work".
func cmdNotifyTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("notify test", stderr, `usage: tinyship notify test [kind] [--json]

Queue a test notification and wait for the worker to deliver it. With a
kind argument only that sink is targeted, so each channel can be
verified separately.
`)
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 1 {
		fmt.Fprintf(stderr, "tinyship notify test: unexpected argument %q\n", pos[1])
		return 2
	}
	target := ""
	if len(pos) == 1 {
		target = pos[0]
	}

	c := newAPIClient()
	var cfg notifyConfig
	if err := c.doJSON(ctx, http.MethodGet, "/notify", nil, &cfg); err != nil {
		fmt.Fprintf(stderr, "tinyship notify test: %v\n", err)
		return 1
	}
	if len(cfg.Sinks) == 0 {
		fmt.Fprintln(stderr, "tinyship notify test: no notification sinks configured")
		fmt.Fprintln(stderr, "add one first: tinyship notify set --url <receiver-url>")
		return 1
	}
	if target != "" {
		found := false
		for _, s := range cfg.Sinks {
			if s.Kind == target {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(stderr, "tinyship notify test: no %q sink configured\n", target)
			return 1
		}
	}

	raw, _ := json.Marshal(map[string]any{"target_kind": target})
	var queued struct {
		ID int64 `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/notify/test", raw, &queued); err != nil {
		fmt.Fprintf(stderr, "tinyship notify test: %v\n", err)
		return 1
	}

	// The dispatcher ticks on its own period; poll until it reports the
	// row delivered or failed, then hand the verdict back.
	deadline := time.Now().Add(25 * time.Second)
	for {
		var events notifyEvents
		if err := c.doJSON(ctx, http.MethodGet, "/notify/events?n=50", nil, &events); err != nil {
			fmt.Fprintf(stderr, "tinyship notify test: %v\n", err)
			return 1
		}
		for _, ev := range events.Events {
			if ev.ID != queued.ID {
				continue
			}
			switch ev.Status {
			case "delivered":
				if *jsonOut {
					raw, _ := json.Marshal(ev)
					fmt.Fprintln(stdout, string(raw))
					return 0
				}
				fmt.Fprintf(stdout, "Test notification delivered to: %s\n",
					strings.Join(ev.SentTo, ", "))
				return 0
			case "failed":
				if *jsonOut {
					raw, _ := json.Marshal(ev)
					fmt.Fprintln(stdout, string(raw))
				} else {
					fmt.Fprintf(stderr, "tinyship notify test: delivery failed: %s\n",
						ev.LastError)
				}
				return 1
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "tinyship notify test: still pending after 25s — is the worker running?")
			return 1
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// postSink validates nothing client-side beyond the struct the API
// validates anyway; it reports the stored sink (including an
// auto-generated webhook secret) so the operator can copy it.
func postSink(ctx context.Context, sink notify.Sink, stdout, stderr io.Writer, headline string) int {
	raw, err := json.Marshal(sink)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship notify: %v\n", err)
		return 1
	}
	c := newAPIClient()
	var stored struct {
		Sink notify.Sink `json:"sink"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/notify/sinks", raw, &stored); err != nil {
		fmt.Fprintf(stderr, "tinyship notify: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, headline)
	if stored.Sink.Kind == "webhook" && stored.Sink.Secret != "" {
		fmt.Fprintf(stdout, "Signing secret: %s\n", stored.Sink.Secret)
		fmt.Fprintln(stdout, "Verify with: X-TinyShip-Signature: t=<unix>,v1=<hex hmac-sha256 of \"t.body\">")
	}
	return 0
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
