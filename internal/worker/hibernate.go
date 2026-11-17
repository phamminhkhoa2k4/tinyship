package worker

// Hibernation state machine: the sleep side (the sweep runs on the
// stats tick), the boot-time reconstruction that re-aligns database
// state with the containers that actually exist, and the budget sums
// both admission tiers share. The wake side lives in relay.go.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// budgetSums totals granted memory across running apps: committed
// counts every row (awake and sleeping — their grants never went
// away), awake counts those not sleeping. A config that no longer
// parses counts at the default limit, the same rule deploy admission
// applies.
func budgetSums(apps []*store.App) (committed, awake int) {
	for _, a := range apps {
		mb := appconfig.DefaultMemoryMB
		if cfg, err := appconfig.Parse([]byte(a.ConfigJSON)); err == nil {
			mb = cfg.Resources.MemoryMB
		}
		committed += mb
		if a.HibernateState != store.HibernateSleeping {
			awake += mb
		}
	}
	return committed, awake
}

// appContainer resolves the container the current deployment created —
// the same container the route row's port belongs to. The deployment
// is the authority: container_state's sample can lag a deploy, and a
// label scan would pick up retirement leftovers.
func appContainer(ctx context.Context, s *store.Store, app *store.App) (containerID string, hostPort int, err error) {
	if app.CurrentDeploymentID == nil {
		return "", 0, fmt.Errorf("app has no current deployment")
	}
	dep, err := s.GetDeployment(ctx, *app.CurrentDeploymentID)
	if err != nil {
		return "", 0, err
	}
	if dep.ContainerID == nil || *dep.ContainerID == "" {
		return "", 0, fmt.Errorf("current deployment has no container")
	}
	if dep.HostPort == nil {
		return "", 0, fmt.Errorf("current deployment has no host port")
	}
	return *dep.ContainerID, *dep.HostPort, nil
}

// managedByApp indexes managed containers by app id, keeping a running
// container over an exited one (the same precedence the sampler uses).
func managedByApp(infos []docker.ContainerInfo) map[string]docker.ContainerInfo {
	byApp := make(map[string]docker.ContainerInfo, len(infos))
	for _, info := range infos {
		appID := info.Labels[policy.LabelApp]
		if appID == "" {
			continue
		}
		if current, ok := byApp[appID]; ok && current.State == "running" {
			continue
		}
		byApp[appID] = info
	}
	return byApp
}

// reconstructHibernate re-aligns the sleep/wake state with reality
// after a crash, before routes, reconciliation, and the relay start.
// Every rule covers a crash between the two writes each transition
// makes (container first or database first), so state is derived, not
// remembered:
//
//	waking + running container   → awake   (the wake finished, the write did not)
//	waking + stopped/absent      → sleeping (the wake died; leave nothing running)
//	sleeping + running container → sleeping after a stop (external start or a
//	                               stop that never landed; the database wins)
//	awake   + stopped container  → sleeping (a sweep stopped it, then crashed)
//	awake   + absent container   → awake    (deploy drift: leave it visible)
func reconstructHibernate(ctx context.Context, s *store.Store, d Docker, log *slog.Logger) {
	if d == nil {
		return
	}
	apps, err := s.ListApps(ctx)
	if err != nil {
		log.Warn("worker: hibernation reconstruction cannot list apps", "err", err)
		return
	}
	infos, err := d.ListManagedContainers(ctx)
	if err != nil {
		log.Warn("worker: hibernation reconstruction cannot list containers", "err", err)
		return
	}
	byApp := managedByApp(infos)

	for _, app := range apps {
		state := app.HibernateState
		if state == store.HibernateOff || state == store.HibernateAwake {
			if state == store.HibernateAwake {
				if info, ok := byApp[app.ID]; ok && !runningState(info.State) {
					// The sweep stopped the container and died before
					// writing 'sleeping'. A container that does not
					// exist at all is deploy drift, not a sleep: leave
					// 'awake' so reconcile keeps reporting it.
					if info.State == "exited" || info.State == "dead" {
						setState(ctx, s, app, store.HibernateSleeping, log, "crashed mid-sleep")
					}
				}
			}
			continue
		}
		info, exists := byApp[app.ID]
		switch state {
		case store.HibernateWaking:
			if exists && runningState(info.State) {
				setState(ctx, s, app, store.HibernateAwake, log, "crashed after wake finished")
				continue
			}
			stopIfRunning(ctx, d, app, info, exists, log)
			setState(ctx, s, app, store.HibernateSleeping, log, "crashed during wake")
		case store.HibernateSleeping:
			if exists && runningState(info.State) {
				stopIfRunning(ctx, d, app, info, exists, log)
				log.Info("worker: stopped a running container of a sleeping app",
					"app", app.Name, "container", info.ID)
			}
		}
	}
}

// runningState reports whether a container state means the app is up
// (restarting counts: Docker is still trying, and stopping it mid-
// restart would fight the daemon).
func runningState(state string) bool {
	return state == "running" || state == "restarting"
}

func stopIfRunning(ctx context.Context, d Docker, app *store.App, info docker.ContainerInfo, exists bool, log *slog.Logger) {
	if !exists || !runningState(info.State) {
		return
	}
	grace := time.Duration(appconfig.DefaultStopGraceS) * time.Second
	if cfg, err := appconfig.Parse([]byte(app.ConfigJSON)); err == nil && cfg.StopGraceS > 0 {
		grace = time.Duration(cfg.StopGraceS) * time.Second
	}
	if err := d.StopContainer(ctx, info.ID, grace); err != nil {
		log.Warn("worker: reconstruction could not stop container",
			"app", app.Name, "container", info.ID, "err", err)
	}
}

func setState(ctx context.Context, s *store.Store, app *store.App, state string, log *slog.Logger, why string) {
	if err := s.SetHibernateState(ctx, app.ID, state); err != nil {
		log.Warn("worker: reconstruction cannot set state",
			"app", app.Name, "state", state, "err", err)
		return
	}
	log.Info("worker: hibernation state reconstructed",
		"app", app.Name, "state", state, "why", why)
}

// hibernateSweep stops the containers of apps that sat idle through
// their configured window. It runs on the stats tick and only when a
// relay exists: without the wake path an app would sleep with no way
// back up.
func hibernateSweep(ctx context.Context, r *Relay, log *slog.Logger) {
	if r == nil {
		return
	}
	apps, err := r.Store.ListApps(ctx)
	if err != nil {
		log.Warn("worker: hibernate sweep cannot list apps", "err", err)
		return
	}
	now := time.Now().UTC()
	for _, app := range apps {
		cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
		if err != nil {
			continue
		}
		if !cfg.HibernateEnabled() {
			// Hibernation was turned off while the machine was still
			// armed: once the app is awake again, retire it so the
			// routes go back to the container directly. A sleeping
			// app keeps its wake path until the next request brings
			// it up — dropping the route now would strand it.
			if app.HibernateState == store.HibernateAwake {
				if err := r.Store.SetHibernateState(ctx, app.ID, store.HibernateOff); err != nil {
					log.Warn("worker: cannot retire hibernation state",
						"app", app.Name, "err", err)
				}
			}
			continue
		}
		if app.Deleting || app.DesiredState != "running" {
			continue
		}
		if app.HibernateState == store.HibernateSleeping ||
			app.HibernateState == store.HibernateWaking {
			continue
		}
		ra := r.app(app.ID)
		if ra.conns.Load() > 0 {
			continue // open connection: never sleep an app in use
		}
		if ra.idleFor(now) < cfg.HibernateIdleTimeout() {
			continue
		}
		ra.mu.Lock()
		if err := r.sleepLocked(ctx, ra, app, cfg, now); err != nil {
			log.Warn("worker: hibernate sweep failed", "app", app.Name, "err", err)
		}
		ra.mu.Unlock()
	}
}

// sleepLocked stops the app's container and writes 'sleeping', all
// under the app's relay lock: a request that arrives mid-sequence
// waits, then reads 'sleeping' and wakes the app — it never reaches a
// half-stopped container. Everything is re-checked fresh inside the
// lock (state, conns, jobs) because a wake or a deploy may have run
// between the sweep's list and here.
func (r *Relay) sleepLocked(ctx context.Context, ra *relayApp, listed *store.App, cfg *appconfig.Config, now time.Time) error {
	app, err := r.Store.GetAppByID(ctx, listed.ID)
	if err != nil {
		return err
	}
	if !cfg.HibernateEnabled() || app.Deleting || app.DesiredState != "running" {
		return nil
	}
	if app.HibernateState == store.HibernateSleeping ||
		app.HibernateState == store.HibernateWaking {
		return nil
	}
	if ra.conns.Load() > 0 {
		return nil
	}
	if ra.idleFor(now) < cfg.HibernateIdleTimeout() {
		return nil
	}
	if active, err := r.Store.AppHasActiveJob(ctx, app.ID); err != nil {
		return err
	} else if active {
		return nil
	}

	containerID, _, err := appContainer(ctx, r.Store, app)
	if err != nil {
		// Nothing to sleep: an app without a container stays 'awake'
		// so boot reconciliation keeps reporting the missing deploy.
		return nil
	}
	info, err := r.Docker.InspectContainer(ctx, containerID)
	if err != nil {
		if isContainerGone(err) {
			return nil
		}
		return err
	}
	if runningState(info.State) {
		grace := cfg.StopGraceS
		if grace <= 0 {
			grace = appconfig.DefaultStopGraceS
		}
		if err := r.Docker.StopContainer(ctx, containerID,
			time.Duration(grace)*time.Second); err != nil && !isContainerGone(err) {
			return err
		}
	}
	if err := r.Store.SetHibernateState(ctx, app.ID, store.HibernateSleeping); err != nil {
		return err
	}
	flaps, err := r.Store.RecordHibernateSleep(ctx, app.ID, now, hibernateFlapWindow)
	if err != nil {
		return fmt.Errorf("record sleep counters: %w", err)
	}
	r.Logger.Info("worker: app slept", "app", app.Name,
		"idle_s", int(ra.idleFor(now).Seconds()), "container", containerID)

	if flaps == hibernateFlapCycles {
		// Exactly at the threshold: one warning per burst (the window
		// only restarts after a quiet spell, and dedup groups repeats).
		if _, err := r.Store.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
			Type:    "hibernation_abnormal",
			AppName: app.Name,
			Title:   "App hibernating abnormally",
			Detail: map[string]any{
				"reason": fmt.Sprintf("%d sleep/wake cycles within %d minutes",
					flaps, int(hibernateFlapWindow.Minutes())),
				"suggestion": fmt.Sprintf(
					"raise hibernate.idle_timeout_s (currently %d) or disable hibernation for %s",
					cfg.Hibernate.IdleTimeoutS, app.Name),
			},
			DedupKey: "hibernation_abnormal:" + app.ID,
		}); err != nil {
			r.Logger.Warn("worker: enqueue hibernation warning failed",
				"app", app.Name, "err", err)
		}
	}
	return nil
}

// isContainerGone matches the daemon's missing-container error the way
// the teardown path already does.
func isContainerGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "No such container") || strings.Contains(msg, "not found")
}
