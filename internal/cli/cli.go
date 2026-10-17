// Package cli implements the user-facing commands. The API server and
// worker are separate subcommands handled in cmd/tinyship.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/tinyship/tinyship/internal/version"
)

// Run executes a CLI command and returns the process exit code:
// 0 = success, non-zero = failure, 2 = usage error.
func Run(ctx context.Context, args []string) int {
	return RunWithIO(ctx, args, os.Stdout, os.Stderr)
}

// RunWithIO is Run with injectable writers, used by tests.
func RunWithIO(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		return cmdVersion(args[1:], stdout, stderr)
	case "app":
		return dispatchApp(ctx, args[1:], stdout, stderr)
	case "domain":
		return dispatchDomain(ctx, args[1:], stdout, stderr)
	case "deploy":
		return cmdDeploy(ctx, args[1:], stdout, stderr)
	case "logs":
		return cmdLogs(ctx, args[1:], stdout, stderr)
	case "deployments":
		return cmdDeployments(ctx, args[1:], stdout, stderr)
	case "status":
		return cmdStatus(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

// dispatchApp handles the `tinyship app <subcommand>` group (
// dispatch surfaces valid subcommands rather than one opaque error).
func dispatchApp(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tinyship app: subcommand required: create, list, or show")
		usageApp(stderr)
		return 2
	}
	switch args[0] {
	case "create":
		return cmdAppCreate(ctx, args[1:], stdout, stderr)
	case "list", "ls":
		return cmdAppList(ctx, args[1:], stdout, stderr)
	case "show", "get":
		return cmdAppShow(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usageApp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship app: unknown subcommand %q\n", args[0])
		usageApp(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `tinyship — image-first deployment manager for a small VPS

Usage:
  tinyship <command> [flags]

Commands:
  app           Create, list, and show apps
  domain        Attach hostnames and list them per app
  deploy        Queue a deploy of an app's current config
  logs          Read the current container's logs
  deployments   Show an app's deployment history
  status        Show API health and apps
  version       Print binary, commit, and DB schema versions
  help          Show this help

Service subcommands (run by systemd, not by users):
  api           Run the API server
  worker        Run the job worker

The CLI talks to the API at http://127.0.0.1:7878 (override: TINYSHIP_API).
Run 'tinyship <command> --help' for command flags.
`)
}

func usageApp(w io.Writer) {
	fmt.Fprint(w, `usage:
  tinyship app create --name <name> --image <image> --port <port> [flags]
  tinyship app create -f config.json
  tinyship app list [--json]
  tinyship app show <name> [--json]

App names and images are immutable; redeploying applies config
changes. Run 'tinyship app create --help' for all flags.
`)
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		switch a {
		case "--json":
			return cmdVersionJSON(stdout, stderr)
		case "--help", "-h":
			fmt.Fprintln(stdout, "usage: tinyship version [--json]")
			return 0
		default:
			fmt.Fprintf(stderr, "tinyship version: unknown flag %q\n", a)
			return 2
		}
	}

	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Version\t%s\n", version.Version)
	fmt.Fprintf(w, "Commit\t%s\n", version.Commit)
	fmt.Fprintf(w, "DB schema\t%d\n", version.SchemaVersion)
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "tinyship version: %v\n", err)
		return 1
	}
	return 0
}

func cmdVersionJSON(stdout io.Writer, stderr io.Writer) int {
	out := struct {
		Version       string `json:"version"`
		Commit        string `json:"commit"`
		SchemaVersion int    `json:"schema_version"`
	}{version.Version, version.Commit, version.SchemaVersion}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "tinyship version: %v\n", err)
		return 1
	}
	return 0
}
