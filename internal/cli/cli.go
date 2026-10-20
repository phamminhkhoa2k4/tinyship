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

	"github.com/tinyship/tinyship/internal/store"
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
	case "env":
		return dispatchEnv(ctx, args[1:], stdout, stderr)
	case "credential", "credentials":
		return dispatchCredential(ctx, args[1:], stdout, stderr)
	case "token":
		return dispatchToken(ctx, args[1:], stdout, stderr)
	case "uninstall":
		return cmdUninstall(ctx, args[1:], stdout, stderr)
	case "backup":
		return cmdBackup(ctx, args[1:], stdout, stderr)
	case "restore":
		return cmdRestore(ctx, args[1:], stdout, stderr)
	case "upgrade":
		return cmdUpgrade(ctx, args[1:], stdout, stderr)
	case "deploy":
		return cmdDeploy(ctx, args[1:], stdout, stderr)
	case "rollback":
		return cmdRollback(ctx, args[1:], stdout, stderr)
	case "logs":
		return cmdLogs(ctx, args[1:], stdout, stderr)
	case "deployments":
		return cmdDeployments(ctx, args[1:], stdout, stderr)
	case "status":
		return cmdStatus(ctx, args[1:], stdout, stderr)
	case "stats":
		return cmdStats(ctx, args[1:], stdout, stderr)
	case "gc":
		return cmdGC(ctx, args[1:], stdout, stderr)
	case "doctor":
		return cmdDoctor(ctx, args[1:], stdout, stderr)
	case "budget":
		return cmdBudget(ctx, args[1:], stdout, stderr)
	case "keygen":
		return cmdKeygen(ctx, args[1:], stdout, stderr)
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
		fmt.Fprintln(stderr, "tinyship app: subcommand required: create, list, show, or delete")
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
	case "delete", "rm":
		return cmdAppDelete(ctx, args[1:], stdout, stderr)
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
  app           Create, list, show, and delete apps
  domain        Attach, list, and remove hostnames per app
  env           Set, list, and unset app variables (secrets supported)
  credential    Store registry logins for private images
  token         Issue, list, revoke, and rotate admin tokens
  deploy        Queue a deploy of an app's current config
  rollback      Roll an app back to its most recent good release
  logs          Read the current container's logs
  deployments   Show an app's deployment history
  status        Show API health and apps
  stats         Show live RAM, CPU, and OOM counts per app
  gc            Remove images outside each app's retention window
  doctor        Diagnose the host: services, docker, files, DNS, drift
  budget        Show or set the host RAM budget
  keygen        Generate the secret encryption key pair
  uninstall     Remove the control plane (root; --purge for everything)
  backup        Write one backup file: DB snapshot, config, keys, token (root)
  restore       Restore a backup file and report which apps need a deploy (root)
  upgrade       Replace the binary with a checksummed build, with rollback (root)
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
  tinyship app delete <name> --yes

App names and images are immutable; redeploying applies config
changes. Deleting an app stops and removes its container and routes;
named volumes keep their data. Run 'tinyship app create --help' for all flags.
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
	// The migration list rides along so an upgrade running against a
	// freshly downloaded build can report exactly which migrations it
	// will apply without guessing from schema numbers alone.
	out := struct {
		Version       string   `json:"version"`
		Commit        string   `json:"commit"`
		SchemaVersion int      `json:"schema_version"`
		Migrations    []string `json:"migrations,omitempty"`
	}{version.Version, version.Commit, version.SchemaVersion, cliMigrationNames()}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "tinyship version: %v\n", err)
		return 1
	}
	return 0
}

// cliMigrationNames renders the embedded migrations as "NNNN_name",
// the same shape the upgrade report uses for pending migrations.
func cliMigrationNames() []string {
	migrations, err := store.Migrations()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(migrations))
	for _, m := range migrations {
		names = append(names, fmt.Sprintf("%04d_%s", m.Version, m.Name))
	}
	return names
}
