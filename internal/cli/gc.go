package cli

// GC command: remove images outside each app's retention window. The
// worker applies the policy and never removes an image a container still
// uses or an app whose deployment is in flight.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
)

// gcImageView mirrors the worker's per-image result.
type gcImageView struct {
	App       string `json:"app"`
	Ref       string `json:"ref"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

// gcSkipView mirrors the worker's reason for leaving a candidate alone.
type gcSkipView struct {
	App    string `json:"app"`
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// gcView mirrors the worker's collection result.
type gcView struct {
	DryRun     bool          `json:"dry_run"`
	Removed    []gcImageView `json:"removed"`
	FreedBytes int64         `json:"freed_bytes"`
	Skipped    []gcSkipView  `json:"skipped"`
}

func cmdGC(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("gc", stderr, `usage: tinyship gc [--app name] [--dry-run] [--json]

Remove local images that no longer fall inside an app's retention
window (keep_releases, default 3). The newest successful releases and
every image a container still uses are always kept; an app with a
deploy in flight is left for the next run. The worker also runs the
same policy on a daily schedule.

--dry-run lists the candidates and their sizes without removing them.
`)
	app := fs.String("app", "", "limit the run to one app")
	dryRun := fs.Bool("dry-run", false, "list candidates and sizes without removing")
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship gc: unexpected argument %q\n", pos[0])
		return 2
	}

	body, _ := json.Marshal(map[string]any{"app": *app, "dry_run": *dryRun})
	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodPost, "/gc", body)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship gc: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship gc: %v\n", apiError(resp))
		return 1
	}
	var view gcView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		fmt.Fprintf(stderr, "tinyship gc: decode: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(view)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}

	if len(view.Removed) == 0 {
		if view.DryRun {
			fmt.Fprintln(stdout, "Nothing to remove: every image is inside a retention window or still in use.")
		} else {
			fmt.Fprintln(stdout, "Nothing to remove.")
		}
	} else {
		action := "Removed"
		if view.DryRun {
			action = "Would remove"
		}
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "APP\tIMAGE\tSIZE")
		for _, img := range view.Removed {
			fmt.Fprintf(w, "%s\t%s\t%s\n", img.App, img.Ref, humanBytes(img.SizeBytes))
		}
		if err := w.Flush(); err != nil {
			fmt.Fprintf(stderr, "tinyship gc: %v\n", err)
			return 1
		}
		if view.DryRun {
			fmt.Fprintf(stdout, "%s %d image(s), freeing %s (dry run; nothing was deleted).\n",
				action, len(view.Removed), humanBytes(view.FreedBytes))
		} else {
			fmt.Fprintf(stdout, "%s %d image(s), freed %s.\n",
				action, len(view.Removed), humanBytes(view.FreedBytes))
		}
	}
	for _, skip := range view.Skipped {
		target := skip.App
		if skip.Ref != "" {
			target += " " + skip.Ref
		}
		fmt.Fprintf(stdout, "Skipped %s: %s\n", target, skip.Reason)
	}
	return 0
}

// humanBytes renders a byte count with one decimal for MiB and GiB.
func humanBytes(n int64) string {
	const (
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
