package cli

// Environment commands: list, set, and unset an app's variables.
// Secrets are never accepted as command-line arguments (they would
// land in shell history and the process table): --secret reads stdin,
// --secret-file reads a file.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
)

// stdin is the secret input stream; tests swap it for a buffer.
var stdin io.Reader = os.Stdin

// dispatchEnv handles the `tinyship env <subcommand>` group.
func dispatchEnv(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tinyship env: subcommand required: list, set, or unset")
		usageEnv(stderr)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return cmdEnvList(ctx, args[1:], stdout, stderr)
	case "set":
		return cmdEnvSet(ctx, args[1:], stdout, stderr)
	case "unset", "rm", "remove":
		return cmdEnvUnset(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usageEnv(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship env: unknown subcommand %q\n", args[0])
		usageEnv(stderr)
		return 2
	}
}

func usageEnv(w io.Writer) {
	fmt.Fprint(w, `usage:
  tinyship env list <app> [--json]
  tinyship env set <app> <key> <value> [--json]              plain variable
  tinyship env set <app> <key> --secret [--json]             secret from stdin
  tinyship env set <app> <key> --secret-file <path> [--json] secret from a file
  tinyship env unset <app> <key> [--json]

Secrets are stored encrypted and are never shown again, only named.
Every change takes effect on the next deploy of that app.
Run 'tinyship env set --help' for all flags.
`)
}

// envVarView is the API's env DTO as the CLI renders it.
type envVarView struct {
	Key   string  `json:"key"`
	Kind  string  `json:"kind"`
	Value *string `json:"value,omitempty"`
}

func cmdEnvList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("env list", stderr, "usage: tinyship env list <app> [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship env list: an app name is required")
		return 2
	}

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+pos[0]+"/env", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship env list: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship env list: %v\n", apiError(resp))
		return 1
	}
	var body struct {
		Env []envVarView `json:"env"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintf(stderr, "tinyship env list: decode: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(body)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	if len(body.Env) == 0 {
		fmt.Fprintf(stdout, "No variables set for %q. Set one with `tinyship env set %s <key> <value>`.\n",
			pos[0], pos[0])
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tKIND\tVALUE")
	for _, e := range body.Env {
		val := ""
		switch {
		case e.Kind == "secret":
			val = "(secret, not shown)"
		case e.Value != nil:
			val = *e.Value
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Key, e.Kind, val)
	}
	tw.Flush()
	fmt.Fprintln(stdout, "Changes take effect on the next deploy.")
	return 0
}

func cmdEnvSet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("env set", stderr, `usage: tinyship env set <app> <key> <value> [flags]

Set one variable. With --secret or --secret-file the value is stored
encrypted and never shown again; secrets are read from stdin or a
file so they stay out of shell history. The change applies at the
next deploy.
`)
	secret := fs.Bool("secret", false, "read the secret value from stdin")
	secretFile := fs.String("secret-file", "", "read the secret value from this file")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) < 2 || len(pos) > 3 {
		fmt.Fprintln(stderr, "tinyship env set: an app name and a key are required")
		return 2
	}
	if *secret && *secretFile != "" {
		fmt.Fprintln(stderr, "tinyship env set: use either --secret or --secret-file, not both")
		return 2
	}

	payload := map[string]any{"kind": "plain"}
	switch {
	case *secretFile != "":
		raw, err := os.ReadFile(*secretFile)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship env set: read secret file: %v\n", err)
			return 1
		}
		payload["value"] = strings.TrimRight(string(raw), "\r\n")
		payload["kind"] = "secret"
	case *secret:
		raw, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship env set: read stdin: %v\n", err)
			return 1
		}
		payload["value"] = strings.TrimRight(string(raw), "\r\n")
		payload["kind"] = "secret"
	case len(pos) == 3:
		payload["value"] = pos[2]
	default:
		fmt.Fprintln(stderr, "tinyship env set: a value is required (or use --secret to read it from stdin)")
		return 2
	}

	body, _ := json.Marshal(payload)
	c := newAPIClient()
	path := "/apps/" + pos[0] + "/env/" + pos[1]
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodPut, path, body, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship env set: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	kind, _ := out["kind"].(string)
	if kind == "secret" {
		fmt.Fprintf(stdout, "Secret %s stored for %s (encrypted; shown by name only).\n",
			out["key"], pos[0])
	} else {
		fmt.Fprintf(stdout, "Variable %s set for %s.\n", out["key"], pos[0])
	}
	fmt.Fprintln(stdout, "It takes effect on the next deploy.")
	return 0
}

func cmdEnvUnset(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("env unset", stderr, "usage: tinyship env unset <app> <key> [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "tinyship env unset: an app name and a key are required")
		return 2
	}

	c := newAPIClient()
	path := "/apps/" + pos[0] + "/env/" + pos[1]
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship env unset: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	fmt.Fprintf(stdout, "Variable %s removed from %s.\n", pos[1], pos[0])
	fmt.Fprintln(stdout, "It takes effect on the next deploy.")
	return 0
}
