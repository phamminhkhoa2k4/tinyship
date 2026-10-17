// Command tinyship is the single binary for the TinyShip API, worker, and
// CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tinyship/tinyship/internal/api"
	"github.com/tinyship/tinyship/internal/cli"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/version"
	"github.com/tinyship/tinyship/internal/worker"
)

func main() {
	// systemd sends SIGTERM on stop; give services a chance to drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	code := run(ctx, os.Args[1:])
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tinyship <api|worker|cli command>")
		return 2
	}

	switch args[0] {
	case "api":
		return api.Run(ctx, args[1:])
	case "worker":
		return worker.Run(ctx, args[1:])
	default:
		// Everything else is a CLI command: app, deploy, logs, version, ...
		version.SchemaVersion = store.SchemaVersion()
		return cli.Run(ctx, args)
	}
}
