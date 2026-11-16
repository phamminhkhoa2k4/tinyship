package cli

// Right-size command: read the suggestion view, optionally apply it.
// Apply never takes the number from the client — the server recomputes
// the same suggestion, checks it against the budget, updates the config
// and queues a deploy through the normal pipeline.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// rightsizeView mirrors the API's suggestion response.
type rightsizeView struct {
	App          string   `json:"app"`
	WindowDays   int      `json:"window_days"`
	Samples      int      `json:"samples"`
	ObservedFrom string   `json:"observed_from,omitempty"`
	ObservedTo   string   `json:"observed_to,omitempty"`
	PeakMB       int      `json:"peak_mb"`
	CurrentMB    int      `json:"current_mb"`
	SuggestedMB  int      `json:"suggested_mb"`
	Verdict      string   `json:"verdict"`
	Confidence   string   `json:"confidence"`
	Reasons      []string `json:"reasons"`
	BudgetMB     int      `json:"budget_mb"`
	FreeMB       int      `json:"free_mb"`
}

func cmdRightsize(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("rightsize", stderr, `usage: tinyship rightsize <name> [--apply] [--wait] [--json]

Suggest a right-sized memory limit for an app from its observed peak
over the last 7 days: peak plus 25 percent, rounded up to 16 MiB,
never below the peak itself. The suggestion shows the data and the
reasons behind it; when less than 7 days were observed it is marked
low confidence.

--apply updates the app's limit and queues a deploy through the normal
pipeline (admission control still applies). Nothing is ever applied
automatically.
`)
	apply := fs.Bool("apply", false, "apply the suggestion and queue a deploy")
	wait := fs.Bool("wait", false, "with --apply, follow the deploy until it succeeds or fails")
	waitTimeout := fs.Duration("wait-timeout", 10*time.Minute, "how long --wait may take before giving up")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship rightsize: exactly one app name is required")
		return 2
	}
	name := pos[0]

	c := newAPIClient()
	if !*apply {
		resp, err := c.do(ctx, http.MethodGet, "/apps/"+name+"/rightsize", nil)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship rightsize: %v\n", err)
			return 1
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(stderr, "tinyship rightsize: %v\n", apiError(resp))
			return 1
		}
		raw, _ := io.ReadAll(resp.Body)
		if *jsonOut {
			fmt.Fprint(stdout, string(raw))
			return 0
		}
		var view rightsizeView
		if err := json.Unmarshal(raw, &view); err != nil {
			fmt.Fprintf(stderr, "tinyship rightsize: decode: %v\n", err)
			return 1
		}
		printRightsize(stdout, &view)
		return 0
	}

	resp, err := c.do(ctx, http.MethodPost, "/apps/"+name+"/rightsize", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship rightsize: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		fmt.Fprintf(stderr, "tinyship rightsize: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(raw))
		return 0
	}
	var applied struct {
		DeploymentID string `json:"deployment_id"`
		Seq          int    `json:"seq"`
		State        string `json:"state"`
		OldMemoryMB  int    `json:"old_memory_mb"`
		NewMemoryMB  int    `json:"new_memory_mb"`
	}
	if err := json.Unmarshal(raw, &applied); err != nil {
		fmt.Fprintf(stderr, "tinyship rightsize: decode: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Applied to %s: memory %d → %d MiB; queued deploy (seq %d, %s)\n",
		name, applied.OldMemoryMB, applied.NewMemoryMB, applied.Seq, applied.State)
	if !*wait {
		fmt.Fprintf(stdout, "Follow it with: tinyship deployments %s\n", name)
		return 0
	}
	return followDeployment(ctx, c, "rightsize", name, applied.DeploymentID,
		*waitTimeout, stdout, stderr, *jsonOut)
}

// printRightsize renders the suggestion view.
func printRightsize(w io.Writer, v *rightsizeView) {
	fmt.Fprintf(w, "Right-size for %s (window: last %d days)\n", v.App, v.WindowDays)
	if v.Verdict == "no_data" {
		for _, reason := range v.Reasons {
			fmt.Fprintf(w, "  %s\n", reason)
		}
		fmt.Fprintf(w, "  current limit: %d MiB\n", v.CurrentMB)
		return
	}
	fmt.Fprintf(w, "  observed:  %d MiB peak, %d samples, %s → %s (confidence: %s)\n",
		v.PeakMB, v.Samples, v.ObservedFrom[:10], v.ObservedTo[:10], v.Confidence)
	fmt.Fprintf(w, "  current:   %d MiB\n", v.CurrentMB)
	fmt.Fprintf(w, "  suggested: %d MiB\n", v.SuggestedMB)
	switch v.Verdict {
	case "lower":
		fmt.Fprintf(w, "  verdict:   lower the limit by %d MiB\n", v.CurrentMB-v.SuggestedMB)
	case "raise":
		fmt.Fprintf(w, "  verdict:   raise the limit by %d MiB (peak exceeds it)\n",
			v.SuggestedMB-v.CurrentMB)
	default:
		fmt.Fprintln(w, "  verdict:   already right-sized")
	}
	fmt.Fprintf(w, "  budget:    %d MiB free of %d MiB\n", v.FreeMB, v.BudgetMB)
	for _, reason := range v.Reasons {
		fmt.Fprintf(w, "  reason:    %s\n", reason)
	}
	fmt.Fprintf(w, "To apply it: tinyship rightsize %s --apply\n", v.App)
}
