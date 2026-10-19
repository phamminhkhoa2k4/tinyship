package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/reconcile"
	"github.com/tinyship/tinyship/internal/store"
)

// bootReconcile compares the database with the containers that are
// actually running and reports the result. It is report-only: no
// container is started, stopped or removed, so an operator decides
// what drift gets remediated and how.
func bootReconcile(ctx context.Context, s *store.Store, d Docker, logger *slog.Logger) {
	if d == nil {
		return
	}
	infos, err := d.ListManagedContainers(ctx)
	if err != nil {
		logger.Warn("worker: boot reconcile could not list containers", "err", err)
		return
	}
	logReconcileReport(ctx, s, runningAppSet(infos), logger)
}

// runningAppSet keeps the app ids of containers that are managed,
// running, and carry an app label; anything else is not evidence of
// an app being up.
func runningAppSet(infos []docker.ContainerInfo) map[string]bool {
	running := make(map[string]bool, len(infos))
	for _, info := range infos {
		if info.State != "running" {
			continue
		}
		if id := info.Labels[policy.LabelApp]; id != "" {
			running[id] = true
		}
	}
	return running
}

// logReconcileReport prints the boot comparison at warn level when
// reality differs from the database's intent and at info level when
// the two agree.
func logReconcileReport(ctx context.Context, s *store.Store, running map[string]bool, logger *slog.Logger) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	apps, err := s.ListApps(ctx)
	if err != nil {
		logger.Warn("worker: boot reconcile could not list apps", "err", err)
		return
	}
	rep := reconcile.Compare(reconcile.FromStore(apps), running)
	attrs := []any{
		"up_to_date", joinOrDash(rep.UpToDate),
		"need_deploy", joinOrDash(rep.NeedDeploy),
		"stopped", joinOrDash(rep.Stopped),
		"unknown_running", joinOrDash(rep.UnknownRunning),
	}
	if rep.Drift() {
		logger.Warn("worker: boot reconcile found drift (report only)", attrs...)
	} else {
		logger.Info("worker: boot reconcile in sync", attrs...)
	}
}

// joinOrDash keeps empty lists readable as a single token in logs.
func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ",")
}
