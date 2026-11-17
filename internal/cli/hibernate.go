package cli

// `tinyship app hibernate` flips an app between hibernating and
// always-awake through the API toggle. It never redeploys: the flag
// changes only when the sleep sweep may stop the container and which
// upstream the proxy dials, both of which the worker derives from the
// stored config on its next pass.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func cmdAppHibernate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("app hibernate", stderr, `usage: tinyship app hibernate <name> [--on|--off] [--json]

Turn hibernation on or off for one app without a redeploy. With neither
flag the current value is inverted. A hibernating app stops after its
idle window and is woken by the next request; waking counts against the
host's awake RAM budget and is refused with a 503 when it would not fit.`)
	on := fs.Bool("on", false, "enable hibernation")
	off := fs.Bool("off", false, "disable hibernation")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship app hibernate: exactly one app name is required")
		return 2
	}
	if *on && *off {
		fmt.Fprintln(stderr, "tinyship app hibernate: --on and --off are exclusive")
		return 2
	}
	name := pos[0]

	body := map[string]any{}
	switch {
	case *on:
		body["enabled"] = true
	case *off:
		body["enabled"] = false
	}
	raw, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship app hibernate: %v\n", err)
		return 1
	}
	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodPost, "/apps/"+name+"/hibernate", raw)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship app hibernate: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship app hibernate: %v\n", apiError(resp))
		return 1
	}
	payload, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(payload))
		return 0
	}
	var out struct {
		Name           string `json:"name"`
		HibernateState string `json:"hibernate_state"`
		Config         struct {
			Hibernate struct {
				Enabled      *bool `json:"enabled"`
				IdleTimeoutS int   `json:"idle_timeout_s"`
				WakeTimeoutS int   `json:"wake_timeout_s"`
			} `json:"hibernate"`
		} `json:"config"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship app hibernate: decode: %v\n", err)
		return 1
	}
	enabled := out.Config.Hibernate.Enabled != nil && *out.Config.Hibernate.Enabled
	if !enabled {
		fmt.Fprintf(stdout, "hibernation off for %s", out.Name)
		if out.HibernateState == "sleeping" || out.HibernateState == "waking" {
			fmt.Fprintf(stdout,
				"; the container is still asleep and wakes on its next request")
		}
		fmt.Fprintln(stdout)
		return 0
	}
	state := out.HibernateState
	if state == "" {
		state = "awake"
	}
	fmt.Fprintf(stdout,
		"hibernation on for %s (state: %s): stops after %ds idle, wakes within %ds\n",
		out.Name, state, out.Config.Hibernate.IdleTimeoutS,
		out.Config.Hibernate.WakeTimeoutS)
	return 0
}
