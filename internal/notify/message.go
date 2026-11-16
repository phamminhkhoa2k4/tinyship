package notify

// Message rendering: one compact, human-readable text per event, plus
// a real suggested command the operator can copy. Every kind (webhook
// body field, Telegram/Discord/Slack text, email body) renders from
// the same template so a receiver sees the same wording everywhere.

import (
	"fmt"
	"strings"
)

// Event is the delivery-side view of one queue row.
type Event struct {
	ID       int64
	Type     string
	AppName  string
	Title    string
	Detail   map[string]any
	DupCount int
}

// SuggestedCommand returns the copy-pasteable command that helps most
// for this event, or "" when none applies. Only commands that exist in
// the CLI are suggested.
func (e Event) SuggestedCommand() string {
	switch e.Type {
	case "deploy_failed", "rollback":
		if e.AppName != "" {
			return "tinyship logs " + e.AppName
		}
		return "tinyship deployments"
	case "oom":
		if e.AppName != "" {
			return "tinyship stats"
		}
		return "tinyship stats"
	case "disk_low", "backup_failed":
		return "tinyship budget"
	case "cert_expiring", "hibernation_abnormal", "hibernation_wake":
		return "tinyship doctor"
	case "test":
		return ""
	}
	return ""
}

// Render produces the notification text: title, a few detail lines,
// the suggested command, and a repeat marker when the queue collapsed
// duplicates. Kept short — this lands in a phone notification.
func (e Event) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "TinyShip · %s", e.Title)
	if e.AppName != "" {
		fmt.Fprintf(&b, " (%s)", e.AppName)
	}
	b.WriteByte('\n')
	for _, line := range detailLines(e.Detail) {
		fmt.Fprintf(&b, "· %s\n", line)
	}
	if cmd := e.SuggestedCommand(); cmd != "" {
		fmt.Fprintf(&b, "→ %s\n", cmd)
	}
	if e.DupCount > 1 {
		fmt.Fprintf(&b, "↻ repeated ×%d", e.DupCount)
	}
	return strings.TrimRight(b.String(), "\n")
}

// detailLines picks a stable, small subset of detail keys to show —
// everything else stays available in the webhook JSON payload.
func detailLines(detail map[string]any) []string {
	if len(detail) == 0 {
		return nil
	}
	order := []string{
		"step", "code", "error", "free_mb", "floor_mb",
		"oom_count", "hostname", "expires_in_days", "reason",
	}
	var lines []string
	seen := map[string]bool{}
	for _, k := range order {
		v, ok := detail[k]
		if !ok {
			continue
		}
		seen[k] = true
		lines = append(lines, fmt.Sprintf("%s: %v", k, v))
	}
	// Any remaining keys, sorted implicitly by map iteration is not
	// deterministic — append them in key order for stable output.
	var rest []string
	for k := range detail {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	for _, k := range rest {
		lines = append(lines, fmt.Sprintf("%s: %v", k, detail[k]))
	}
	return lines
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
