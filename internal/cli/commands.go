package cli

// User-facing commands:
//
//	tinyship app create|list|show
//	tinyship deploy [--wait]
//	tinyship logs [--tail --since --follow]
//	tinyship deployments
//	tinyship status
//
// Exit codes: 0 success, 1 runtime/deploy failure, 2 usage error.
// Every command accepts --help and, where it prints data,
// --json for machine-readable output.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
)

// flagSet builds a flag set that prints usage to stderr and maps -h to the
// standard ErrHelp sentinel.
func flagSet(name string, stderr io.Writer, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses fs over args with flags allowed before or after
// positional arguments, so `tinyship deploy web --wait` reads the same as
// `tinyship deploy --wait web`. Positional arguments come back in order; the
// second result is an exit code: -1 = keep going, 0 = handled (--help),
// 2 = usage error.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, int) {
	var positional []string
	// A literal "--" ends flag parsing: everything after it is positional.
	var tail []string
	for i, a := range args {
		if a == "--" {
			tail, args = args[i+1:], args[:i]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return positional, 0
			}
			return positional, 2
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return append(positional, tail...), -1
		}
		// flag parsing stopped at a positional argument; record it and
		// keep parsing for flags that follow.
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// --- app create -----------------------------------------------------------------

// volumeList accumulates repeatable --volume name:path flags.
type volumeList []appconfig.Volume

func (v *volumeList) String() string { return fmt.Sprint([]appconfig.Volume(*v)) }
func (v *volumeList) Set(raw string) error {
	name, path, ok := strings.Cut(raw, ":")
	if !ok || name == "" || path == "" {
		return fmt.Errorf("want name:path, got %q", raw)
	}
	*v = append(*v, appconfig.Volume{Name: name, Path: path})
	return nil
}

func cmdAppCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("app create", stderr, `usage: tinyship app create [flags]

Declare an app. Flags build the config object; use -f to supply the
whole object as JSON (strict schema; unknown fields are rejected).

Required (unless -f): --name, --image, --port.
`)
	var (
		name        = fs.String("name", "", "app name: ^[a-z][a-z0-9-]{1,38}[a-z0-9]$, immutable")
		image       = fs.String("image", "", "OCI image without tag/digest, e.g. ghcr.io/acme/app")
		tag         = fs.String("tag", "", "image tag (default latest, warns W_MUTABLE_TAG)")
		port        = fs.Int("port", 0, "port the app listens on inside the container")
		memoryMB    = fs.Int("memory-mb", 0, "memory limit MiB (default 256)")
		cpuMillis   = fs.Int("cpu-millis", 0, "CPU quota in milli-cores (default 500)")
		pidsLimit   = fs.Int("pids-limit", 0, "max processes (default 256)")
		strategy    = fs.String("strategy", "", "auto|start-first|stop-first (default auto)")
		stopGrace   = fs.Int("stop-grace", 0, "SIGTERM grace seconds (default 30)")
		healthType  = fs.String("health-type", "", "health probe: http|tcp|exec (default tcp on --port)")
		healthPath  = fs.String("health-path", "", "http probe path (default /)")
		healthCmd   = fs.String("health-exec-cmd", "", "comma-separated exec probe command (runs in container)")
		healthRetr  = fs.Int("health-retries", 0, "accepted consecutive probe failures (default 3)")
		healthIntr  = fs.Int("health-interval", 0, "seconds between probes (default 10)")
		healthTO    = fs.Int("health-timeout", 0, "seconds per probe (default 5)")
		healthStart = fs.Int("health-start-period", -1, "seconds before probes count (default 30; 0 = start now)")
		file        = fs.String("f", "", "read the config object from a JSON file instead of flags")
		jsonOut     = fs.Bool("json", false, "print the API response verbatim as JSON")
	)
	var volumes volumeList
	fs.Var(&volumes, "volume", "named volume name:path, repeatable (host paths are never bind-mounted)")

	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship app create: unexpected argument %q\n", pos[0])
		return 2
	}

	var body []byte
	if *file != "" {
		raw, err := readFile(*file)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship app create: %v\n", err)
			return 1
		}
		body = raw
	} else {
		if *name == "" || *image == "" || *port == 0 {
			fmt.Fprintln(stderr, "tinyship app create: --name, --image and --port are required (or use -f)")
			return 2
		}
		cfg := appconfig.Config{
			Name:       *name,
			Image:      *image,
			Tag:        *tag,
			Port:       *port,
			Strategy:   *strategy,
			StopGraceS: *stopGrace,
			Volumes:    volumes,
		}
		if *memoryMB > 0 || *cpuMillis > 0 || *pidsLimit > 0 {
			cfg.Resources = appconfig.Resources{
				MemoryMB: *memoryMB, CPUMillis: *cpuMillis, PidsLimit: *pidsLimit,
			}
		}
		if *healthType != "" || *healthPath != "" || *healthCmd != "" ||
			*healthRetr > 0 || *healthIntr > 0 || *healthTO > 0 || *healthStart >= 0 {
			h := appconfig.Health{
				Type: *healthType, Path: *healthPath, IntervalS: *healthIntr,
				TimeoutS: *healthTO, Retries: *healthRetr,
			}
			if *healthCmd != "" {
				h.Command = strings.Split(*healthCmd, ",")
			}
			if *healthStart >= 0 {
				start := *healthStart
				h.StartPeriodS = &start
			}
			cfg.Health = &h
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship app create: build config: %v\n", err)
			return 1
		}
		body = raw
	}

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodPost, "/apps", body)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship app create: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		fmt.Fprintf(stderr, "tinyship app create: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(raw))
		return 0
	}
	var created struct {
		Name     string   `json:"name"`
		ID       string   `json:"id"`
		Image    string   `json:"image"`
		Tag      string   `json:"tag"`
		Port     int      `json:"port"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		fmt.Fprintf(stderr, "tinyship app create: decode response: %v\n", err)
		return 1
	}
	ref := created.Image
	if created.Tag != "" {
		ref += ":" + created.Tag
	}
	fmt.Fprintf(stdout, "Created app %s (id %s)\n  image: %s\n  port:  %d\n",
		created.Name, created.ID, ref, created.Port)
	for _, warn := range created.Warnings {
		fmt.Fprintf(stdout, "  warning: %s\n", warn)
	}
	fmt.Fprintf(stdout, "Deploy it with: tinyship deploy %s --wait\n", created.Name)
	return 0
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, appconfig.MaxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > appconfig.MaxConfigBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte config limit", path, appconfig.MaxConfigBytes)
	}
	return raw, nil
}

// --- app list / show --------------------------------------------------------------

func cmdAppList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("app list", stderr, "usage: tinyship app list [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship app list: unexpected argument %q\n", pos[0])
		return 2
	}

	var out struct {
		Apps []struct {
			Name                string  `json:"name"`
			ID                  string  `json:"id"`
			Image               string  `json:"image"`
			Tag                 string  `json:"tag"`
			Port                int     `json:"port"`
			DesiredState        string  `json:"desired_state"`
			CurrentDeploymentID *string `json:"current_deployment_id"`
		} `json:"apps"`
	}
	c := newAPIClient()
	if err := c.doJSON(ctx, http.MethodGet, "/apps", nil, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship app list: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	if len(out.Apps) == 0 {
		fmt.Fprintln(stdout, "No apps yet. Create one with: tinyship app create --name <name> --image <image> --port <port>")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tIMAGE\tPORT\tDESIRED\tCURRENT DEPLOYMENT")
	for _, a := range out.Apps {
		ref := a.Image
		if a.Tag != "" {
			ref += ":" + a.Tag
		}
		current := "-"
		if a.CurrentDeploymentID != nil {
			current = shortID(*a.CurrentDeploymentID)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", a.Name, ref, a.Port, a.DesiredState, current)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "tinyship app list: %v\n", err)
		return 1
	}
	return 0
}

func cmdAppShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("app show", stderr, "usage: tinyship app show <name> [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship app show: exactly one app name is required")
		return 2
	}
	name := pos[0]

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+name, nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship app show: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship app show: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(raw))
		return 0
	}

	var body struct {
		App struct {
			Name                string          `json:"name"`
			ID                  string          `json:"id"`
			DesiredState        string          `json:"desired_state"`
			Config              json.RawMessage `json:"config"`
			CurrentDeploymentID *string         `json:"current_deployment_id"`
		} `json:"app"`
		CurrentDeployment *deploymentView `json:"current_deployment"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		fmt.Fprintf(stderr, "tinyship app show: decode: %v\n", err)
		return 1
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, body.App.Config, "  ", "  "); err == nil {
		fmt.Fprintf(stdout, "App %s (id %s, desired: %s)\n  config:\n%s\n",
			body.App.Name, body.App.ID, body.App.DesiredState, indented.String())
	}
	if d := body.CurrentDeployment; d != nil {
		fmt.Fprintf(stdout, "  current deployment: %s (%s)\n", shortID(d.ID), d.State)
		if d.ImageDigest != nil {
			fmt.Fprintf(stdout, "    digest: %s\n", *d.ImageDigest)
		}
		if d.HostPort != nil {
			fmt.Fprintf(stdout, "    host port: 127.0.0.1:%d\n", *d.HostPort)
		}
	} else {
		fmt.Fprintln(stdout, "  current deployment: none (deploy it with `tinyship deploy`)")
	}
	return 0
}

// --- deploy --------------------------------------------------------------------------

// deploymentView is the deployment shape the CLI renders (subset of the
// API's deployment DTO).
type deploymentView struct {
	ID             string     `json:"id"`
	Seq            int        `json:"seq"`
	State          string     `json:"state"`
	ImageRef       string     `json:"image_ref"`
	ImageDigest    *string    `json:"image_digest"`
	HostPort       *int       `json:"host_port"`
	FailureStep    *string    `json:"failure_step"`
	FailureCode    *string    `json:"failure_code"`
	FailureMessage *string    `json:"failure_message"`
	CreatedAt      time.Time  `json:"created_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	Events         []struct {
		Step   string          `json:"step"`
		Status string          `json:"status"`
		TS     time.Time       `json:"ts"`
		Detail json.RawMessage `json:"detail"`
	} `json:"events"`
}

func cmdDeploy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("deploy", stderr, `usage: tinyship deploy <name> [--wait]

Queue a deploy of the app's current config. The worker pulls the
image, pins its digest, creates the container under the documented policy,
and probes it from the host until healthy twice in a row.
`)
	wait := fs.Bool("wait", false, "follow the deployment until it succeeds or fails")
	waitTimeout := fs.Duration("wait-timeout", 10*time.Minute, "how long --wait may take before giving up")
	jsonOut := fs.Bool("json", false, "print the queue response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship deploy: exactly one app name is required")
		return 2
	}
	name := pos[0]

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodPost, "/apps/"+name+"/deploy", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship deploy: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		fmt.Fprintf(stderr, "tinyship deploy: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	var queued struct {
		DeploymentID string `json:"deployment_id"`
		Seq          int    `json:"seq"`
		State        string `json:"state"`
	}
	if err := json.Unmarshal(raw, &queued); err != nil {
		fmt.Fprintf(stderr, "tinyship deploy: decode: %v\n", err)
		return 1
	}
	if !*wait {
		if *jsonOut {
			fmt.Fprint(stdout, string(raw))
			return 0
		}
		fmt.Fprintf(stdout, "Queued deploy of %s: deployment %s (seq %d, %s)\n",
			name, queued.DeploymentID, queued.Seq, queued.State)
		fmt.Fprintf(stdout, "Follow it with: tinyship deploy %s --wait\n", name)
		return 0
	}

	// Poll until the deployment reaches a terminal state (a
	// failed deploy exits non-zero).
	deadline := time.Now().Add(*waitTimeout)
	for {
		var dep deploymentView
		if err := c.doJSON(ctx, http.MethodGet, "/deployments/"+queued.DeploymentID, nil, &dep); err != nil {
			fmt.Fprintf(stderr, "tinyship deploy: %v\n", err)
			return 1
		}
		if dep.State == "succeeded" || dep.State == "failed" {
			if *jsonOut {
				raw, _ := json.Marshal(dep)
				fmt.Fprintln(stdout, string(raw))
			} else {
				printTimeline(stdout, name, &dep)
			}
			if dep.State == "failed" {
				return 1
			}
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "tinyship deploy: still %s after %s; check `tinyship deployments %s`\n",
				dep.State, *waitTimeout, name)
			return 1
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "tinyship deploy: interrupted; deployment %s continues on the server\n",
				queued.DeploymentID)
			return 130
		case <-time.After(1 * time.Second):
		}
	}
}

func printTimeline(w io.Writer, name string, dep *deploymentView) {
	fmt.Fprintf(w, "Deploy of %s (seq %d): %s\n", name, dep.Seq, strings.ToUpper(dep.State))
	if dep.ImageDigest != nil {
		fmt.Fprintf(w, "  image:  %s\n", dep.ImageRef)
		fmt.Fprintf(w, "  digest: %s\n", *dep.ImageDigest)
	}
	if dep.HostPort != nil {
		fmt.Fprintf(w, "  port:   127.0.0.1:%d\n", *dep.HostPort)
	}
	if len(dep.Events) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  STEP\tSTATUS\tDETAIL")
		for _, ev := range dep.Events {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", ev.Step, ev.Status, eventDetail(ev.Detail))
		}
		tw.Flush()
	}
	if dep.State == "failed" {
		step, code, msg := "?", "E_DEPLOY_FAILED", ""
		if dep.FailureStep != nil {
			step = *dep.FailureStep
		}
		if dep.FailureCode != nil {
			code = *dep.FailureCode
		}
		if dep.FailureMessage != nil {
			msg = *dep.FailureMessage
		}
		fmt.Fprintf(w, "Failed at step %s (%s)\n%s\n", step, code, indentLines(msg, "  "))
		fmt.Fprintf(w, "The previous version (if any) is still serving. Inspect with: tinyship deployments %s\n", name)
	}
}

// eventDetail renders a timeline event's detail payload compactly.
func eventDetail(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	// Skip purely informational bookkeeping fields.
	delete(m, "note")
	parts := make([]string, 0, len(m))
	for k, v := range m {
		if s := fmt.Sprint(v); s != "" && s != "0" && s != "false" {
			parts = append(parts, k+"="+s)
		}
	}
	return strings.Join(parts, " ")
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// --- logs ---------------------------------------------------------------------------------

func cmdLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("logs", stderr, `usage: tinyship logs <name> [--tail N] [--since 15m] [--follow]

Read the current container's logs through the worker's read-only socket;
the worker enforces container ownership before streaming.
`)
	tail := fs.Int("tail", 100, "lines from the end; 0 = full log")
	since := fs.String("since", "", "only lines newer than this Go duration, e.g. 15m or 1h")
	follow := fs.Bool("follow", false, "keep streaming new lines (Ctrl-C to stop)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship logs: exactly one app name is required")
		return 2
	}
	name := pos[0]

	q := fmt.Sprintf("tail=%d", *tail)
	if *since != "" {
		if _, err := time.ParseDuration(*since); err != nil {
			fmt.Fprintf(stderr, "tinyship logs: --since %q must be a Go duration like 15m: %v\n", *since, err)
			return 2
		}
		q += "&since=" + *since
	}
	if *follow {
		q += "&follow=1"
	}

	c := newAPIClient()
	req, err := c.newRequest(ctx, http.MethodGet, "/apps/"+name+"/logs?"+q, nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship logs: %v\n", err)
		return 1
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship logs: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship logs: %v\n", apiError(resp))
		return 1
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		// A Ctrl-C during follow resets the pipe; that is not a failure.
		if ctx.Err() != nil {
			return 130
		}
		fmt.Fprintf(stderr, "tinyship logs: stream ended: %v\n", err)
		return 1
	}
	return 0
}

// --- deployments ------------------------------------------------------------------------------

func cmdDeployments(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("deployments", stderr, "usage: tinyship deployments <name> [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship deployments: exactly one app name is required")
		return 2
	}
	name := pos[0]

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+name+"/deployments", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship deployments: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship deployments: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(raw))
		return 0
	}
	var out struct {
		Deployments []deploymentView `json:"deployments"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship deployments: decode: %v\n", err)
		return 1
	}
	if len(out.Deployments) == 0 {
		fmt.Fprintf(stdout, "No deployments for %s yet.\n", name)
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\tDEPLOYMENT\tSTATE\tIMAGE\tCREATED")
	for _, d := range out.Deployments {
		created := d.CreatedAt.Local().Format("2006-01-02 15:04:05")
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", d.Seq, shortID(d.ID), d.State, d.ImageRef, created)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "tinyship deployments: %v\n", err)
		return 1
	}
	return 0
}

// --- status -------------------------------------------------------------------------------------

func cmdStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("status", stderr, "usage: tinyship status [--json]\n")
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship status: unexpected argument %q\n", pos[0])
		return 2
	}

	c := newAPIClient()
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/healthz", nil, &health); err != nil {
		fmt.Fprintf(stderr, "tinyship status: API unreachable at %s (is `tinyship api` running?): %v\n",
			c.base, err)
		return 1
	}
	var apps struct {
		Apps []struct {
			Name                string  `json:"name"`
			Image               string  `json:"image"`
			Tag                 string  `json:"tag"`
			DesiredState        string  `json:"desired_state"`
			CurrentDeploymentID *string `json:"current_deployment_id"`
		} `json:"apps"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/apps", nil, &apps); err != nil {
		fmt.Fprintf(stderr, "tinyship status: %v\n", err)
		return 1
	}
	if *jsonOut {
		out := map[string]any{"api": health, "apps": apps.Apps}
		raw, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	fmt.Fprintf(stdout, "API     %s (version %s, commit %s)\n", strings.ToUpper(health.Status), health.Version, health.Commit)
	fmt.Fprintf(stdout, "Apps    %d\n", len(apps.Apps))
	for _, a := range apps.Apps {
		ref := a.Image
		if a.Tag != "" {
			ref += ":" + a.Tag
		}
		state := "never deployed"
		if a.CurrentDeploymentID != nil {
			state = "deployment " + shortID(*a.CurrentDeploymentID)
		}
		fmt.Fprintf(stdout, "  %-20s %-30s desired=%s, %s\n", a.Name, ref, a.DesiredState, state)
	}
	return 0
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}
