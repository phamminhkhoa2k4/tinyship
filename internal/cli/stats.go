package cli

// Stats command: render the worker's resource sample per app. The values
// are read-only; a failed sample leaves the last stored observation in
// place, so the view may show an older timestamp.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"
)

// appStatsView mirrors the worker's per-app resource sample.
type appStatsView struct {
	AppID        string    `json:"app_id"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Health       string    `json:"health,omitempty"`
	MemUsageMB   int       `json:"mem_usage_mb"`
	MemLimitMB   int       `json:"mem_limit_mb"`
	CPUPercent   float64   `json:"cpu_percent"`
	NearLimit    bool      `json:"near_limit"`
	OOMKillCount int       `json:"oom_kill_count"`
	SampledAt    time.Time `json:"sampled_at"`
}

func cmdStats(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("stats", stderr, `usage: tinyship stats [app] [--json]

Show each app's last resource sample: memory against its configured
limit, CPU relative to one core, whether memory use is at or above 80%
of the limit, and how many times the kernel killed the container for
exceeding memory.

Without an app name, every app is listed.
`)
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 1 {
		fmt.Fprintf(stderr, "tinyship stats: unexpected argument %q\n", pos[1])
		return 2
	}
	filter := ""
	if len(pos) == 1 {
		filter = pos[0]
	}

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodGet, "/stats", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship stats: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship stats: %v\n", apiError(resp))
		return 1
	}
	var view struct {
		Apps []appStatsView `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		fmt.Fprintf(stderr, "tinyship stats: decode: %v\n", err)
		return 1
	}
	if filter != "" {
		kept := view.Apps[:0]
		for _, a := range view.Apps {
			if a.Name == filter {
				kept = append(kept, a)
			}
		}
		view.Apps = kept
		if len(view.Apps) == 0 {
			fmt.Fprintf(stderr, "tinyship stats: no such app %q\n", filter)
			return 1
		}
	}
	if *jsonOut {
		raw, _ := json.Marshal(view)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	if len(view.Apps) == 0 {
		fmt.Fprintln(stdout, "No apps yet. Create one with: tinyship app create")
		return 0
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tRAM/LIMIT\tCPU\tNEAR LIMIT\tOOM\tSAMPLED")
	for _, a := range view.Apps {
		limit := "-"
		if a.MemLimitMB > 0 {
			limit = fmt.Sprintf("%d/%d MiB", a.MemUsageMB, a.MemLimitMB)
		} else {
			limit = fmt.Sprintf("%d MiB", a.MemUsageMB)
		}
		near := "no"
		if a.NearLimit {
			near = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%s\t%d\t%s\n",
			a.Name, a.State, limit, a.CPUPercent, near, a.OOMKillCount,
			a.SampledAt.Local().Format("15:04:05"))
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "tinyship stats: %v\n", err)
		return 1
	}
	return 0
}
