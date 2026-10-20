package worker

// Image garbage collection: each app keeps the images of its newest
// successful releases (default three, configurable per app) and the
// images any managed container still references. Everything else that an
// app's deployment history mentions is a removal candidate. The collector
// runs on a schedule and on demand; a dry run reports candidates with
// their sizes without touching the daemon.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// DefaultGCInterval is how often the worker runs the image collector.
const DefaultGCInterval = 24 * time.Hour

// ErrGCAppNotFound reports an on-demand run for a name that does not exist.
var ErrGCAppNotFound = errors.New("no such app")

// GCStore is the store slice the collector needs.
type GCStore interface {
	ListApps(ctx context.Context) ([]*store.App, error)
	ListDeployments(ctx context.Context, appID string) ([]*store.Deployment, error)
	AppHasActiveJob(ctx context.Context, appID string) (bool, error)
	AppendAudit(ctx context.Context, actor, action, targetType, targetID, result string, detail any) error
}

// GCRunner applies the image retention policy.
type GCRunner struct {
	Store  GCStore
	Docker Docker
	Actor  string
	Logger *slog.Logger
}

// NewGCRunner wires the collector over a store and the Docker wrapper.
func NewGCRunner(s GCStore, d Docker, actor string, log *slog.Logger) *GCRunner {
	if log == nil {
		log = slog.Default()
	}
	return &GCRunner{Store: s, Docker: d, Actor: actor, Logger: log}
}

// GCOptions narrows one run.
type GCOptions struct {
	App    string // app name; empty means every app
	DryRun bool
}

// GCImage is one image the run removed (or would remove).
type GCImage struct {
	App       string `json:"app"`
	Ref       string `json:"ref"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

// GCSkip records why a candidate was left alone.
type GCSkip struct {
	App    string `json:"app"`
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// GCResult is the outcome of one run.
type GCResult struct {
	DryRun     bool      `json:"dry_run"`
	Removed    []GCImage `json:"removed"`
	FreedBytes int64     `json:"freed_bytes"`
	Skipped    []GCSkip  `json:"skipped,omitempty"`
}

// Run applies the retention policy once.
func (g *GCRunner) Run(ctx context.Context, opts GCOptions) (*GCResult, error) {
	apps, err := g.Store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	if opts.App != "" {
		var selected []*store.App
		for _, app := range apps {
			if app.Name == opts.App {
				selected = append(selected, app)
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrGCAppNotFound, opts.App)
		}
		apps = selected
	}

	// The keep set is global: two apps may run the same image, and a
	// digest kept for either one must survive.
	keep, err := g.keepSet(ctx, apps)
	if err != nil {
		return nil, err
	}

	res := &GCResult{DryRun: opts.DryRun}
	seen := map[string]bool{}
	for _, app := range apps {
		if app.Deleting {
			res.Skipped = append(res.Skipped, GCSkip{App: app.Name, Reason: "app is being deleted"})
			continue
		}
		active, err := g.Store.AppHasActiveJob(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		if active {
			// A deploy may be pulling or swapping the exact images the
			// collector would touch; leave the app for the next run.
			res.Skipped = append(res.Skipped, GCSkip{App: app.Name, Reason: "deployment in flight"})
			continue
		}
		deps, err := g.Store.ListDeployments(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			if dep.ImageDigest == nil || *dep.ImageDigest == "" {
				continue
			}
			if keep[*dep.ImageDigest] || seen[*dep.ImageDigest] {
				continue
			}
			seen[*dep.ImageDigest] = true
			g.collect(ctx, app.Name, dep.ImageRef, *dep.ImageDigest, opts.DryRun, res)
		}
	}

	if err := g.Store.AppendAudit(ctx, g.Actor, "image.gc", "host", "", "ok", map[string]any{
		"dry_run":     res.DryRun,
		"app":         opts.App,
		"removed":     len(res.Removed),
		"freed_bytes": res.FreedBytes,
		"skipped":     len(res.Skipped),
	}); err != nil {
		g.Logger.Warn("worker: cannot audit image gc", "err", err)
	}
	return res, nil
}

// keepSet returns every digest the collector must not touch: the newest
// releases per app plus images referenced by any managed container.
func (g *GCRunner) keepSet(ctx context.Context, apps []*store.App) (map[string]bool, error) {
	keep := map[string]bool{}
	for _, app := range apps {
		keepN := appconfig.DefaultKeepReleases
		if cfg, err := appconfig.Parse([]byte(app.ConfigJSON)); err == nil && cfg.KeepReleases > 0 {
			keepN = cfg.KeepReleases
		}
		deps, err := g.Store.ListDeployments(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		kept := 0
		for _, dep := range deps {
			// Superseded releases were successful too; they are the
			// history the retention window counts.
			if dep.State != store.DepSucceeded && dep.State != store.DepSuperseded {
				continue
			}
			if dep.ImageDigest == nil || *dep.ImageDigest == "" {
				continue
			}
			if kept >= keepN {
				break
			}
			keep[*dep.ImageDigest] = true
			kept++
		}
		if app.CurrentDeploymentID != nil {
			for _, dep := range deps {
				if dep.ID == *app.CurrentDeploymentID && dep.ImageDigest != nil {
					keep[*dep.ImageDigest] = true
				}
			}
		}
	}

	infos, err := g.Docker.ListManagedContainers(ctx)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.Labels[policy.LabelApp] == "" {
			continue
		}
		// A stopped container still blocks image removal, so every
		// managed container's image is kept, not only running ones.
		if digest, _, err := g.Docker.InspectImage(ctx, info.Image); err == nil && digest != "" {
			keep[digest] = true
		}
	}
	return keep, nil
}

// collect inspects and (unless dry-running) removes one candidate. An
// image that is already gone is a skip, not a failure: another app or an
// earlier run may have removed it.
func (g *GCRunner) collect(ctx context.Context, appName, imageRef, digest string, dryRun bool, res *GCResult) {
	ref := repoOf(imageRef) + "@" + digest
	_, size, err := g.Docker.InspectImage(ctx, ref)
	if err != nil {
		res.Skipped = append(res.Skipped, GCSkip{App: appName, Ref: ref, Reason: "image not present locally"})
		return
	}
	if !dryRun {
		if err := g.Docker.RemoveImage(ctx, ref); err != nil {
			res.Skipped = append(res.Skipped, GCSkip{App: appName, Ref: ref, Reason: err.Error()})
			return
		}
	}
	res.Removed = append(res.Removed, GCImage{
		App: appName, Ref: ref, Digest: digest, SizeBytes: size,
	})
	res.FreedBytes += size
}

// repoOf strips a tag or digest from an image reference, keeping registry
// ports intact (the colon after the last slash is a tag separator).
func repoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}
