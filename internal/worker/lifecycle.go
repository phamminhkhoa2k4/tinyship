package worker

// App teardown and domain sync handlers. app_delete stops and removes
// every container an app owns, purges its rows, and regenerates proxy
// routes so its hostnames stop routing. domain_sync only regenerates
// routes: the API already changed the rows, the worker replays them
// into the proxy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// AppDeleteJobType is the jobs.type value for app teardown.
const AppDeleteJobType = "app_delete"

// DomainSyncJobType is the jobs.type value for route regeneration
// after a domain change.
const DomainSyncJobType = "domain_sync"

// appDeletePayload is the strict job payload for app_delete.
type appDeletePayload struct {
	AppID string `json:"app_id"`
}

// domainSyncPayload is the strict job payload for domain_sync.
type domainSyncPayload struct {
	AppID string `json:"app_id"`
}

// AppDeleteHandler returns the handler for one app teardown.
func (p *Deployer) AppDeleteHandler() Handler {
	return func(ctx context.Context, job *store.Job) error {
		return p.runAppDelete(ctx, job)
	}
}

// DomainSyncHandler returns the handler for route regeneration.
func (p *Deployer) DomainSyncHandler() Handler {
	return func(ctx context.Context, job *store.Job) error {
		return p.runDomainSync(ctx, job)
	}
}

func decodeAppDeletePayload(raw string) (*appDeletePayload, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var pl appDeletePayload
	if err := dec.Decode(&pl); err != nil {
		return nil, fmt.Errorf("app_delete payload: %w", err)
	}
	if pl.AppID == "" {
		return nil, errors.New("app_delete payload: missing app_id")
	}
	return &pl, nil
}

func decodeDomainSyncPayload(raw string) (*domainSyncPayload, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var pl domainSyncPayload
	if err := dec.Decode(&pl); err != nil {
		return nil, fmt.Errorf("domain_sync payload: %w", err)
	}
	return &pl, nil
}

// runAppDelete tears one app down. Order matters: containers first
// (so nothing keeps serving traffic), then rows (so route generation
// no longer sees the app), then the proxy (so routes actually drop).
// Every step is idempotent: a retry after a crash re-inspects and
// re-removes what is already gone.
func (p *Deployer) runAppDelete(ctx context.Context, job *store.Job) error {
	pl, err := decodeAppDeletePayload(job.PayloadJSON)
	if err != nil {
		return &pipelineError{code: "E_VALIDATION", err: err}
	}
	log := p.Logger.With("job", job.ID, "app_id", pl.AppID)

	app, err := p.Store.GetAppByID(ctx, pl.AppID)
	if errors.Is(err, store.ErrNotFound) {
		// A previous attempt already purged the rows; still sweep
		// containers so a crash between steps converges.
		log.Info("app delete: rows already gone, finishing sweep")
		app = nil
	} else if err != nil {
		return &RetryableError{err}
	}

	removed, err := p.removeAppContainers(ctx, pl.AppID, app, log)
	if err != nil {
		return err
	}

	if app != nil {
		if err := p.Store.PurgeApp(ctx, pl.AppID); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			return &RetryableError{err}
		}
		_ = p.Store.AppendAudit(ctx, p.Actor, "app.delete", "app", pl.AppID,
			"ok", map[string]any{"containers_removed": removed})
	}

	if p.Caddy != nil {
		if err := RegenerateRoutes(ctx, p.Store, p.Caddy, log); err != nil {
			return &RetryableError{err}
		}
	}

	log.Info("app delete complete", "containers_removed", removed)
	return nil
}

// removeAppContainers stops and removes every managed container whose
// app label matches. It ignores containers that vanish mid-flight:
// teardown must converge, not race the daemon.
func (p *Deployer) removeAppContainers(ctx context.Context, appID string, app *store.App, log *slog.Logger) (int, error) {
	containers, err := p.Docker.ListManagedContainers(ctx)
	if err != nil {
		return 0, &RetryableError{err}
	}
	grace := time.Duration(appconfig.DefaultStopGraceS) * time.Second
	if app != nil {
		if cfg, cerr := appconfig.Parse([]byte(app.ConfigJSON)); cerr == nil && cfg.StopGraceS > 0 {
			grace = time.Duration(cfg.StopGraceS) * time.Second
		}
	}
	removed := 0
	for _, c := range containers {
		if c.Labels[policy.LabelApp] != appID {
			continue
		}
		if c.State == "running" {
			if err := p.Docker.StopContainer(ctx, c.ID, grace); err != nil &&
				!strings.Contains(err.Error(), "No such container") {
				return removed, &RetryableError{err}
			}
		}
		if err := p.Docker.RemoveContainer(ctx, c.ID, true); err != nil {
			if strings.Contains(err.Error(), "No such container") {
				continue
			}
			return removed, &RetryableError{err}
		}
		removed++
		log.Info("app delete: container removed", "container", c.ID)
	}
	return removed, nil
}

// runDomainSync regenerates proxy routes from the database. The API
// already committed the row change; this replay makes the proxy match
// it without waiting for the app's next deploy.
func (p *Deployer) runDomainSync(ctx context.Context, job *store.Job) error {
	if _, err := decodeDomainSyncPayload(job.PayloadJSON); err != nil {
		return &pipelineError{code: "E_VALIDATION", err: err}
	}
	log := p.Logger.With("job", job.ID)

	if p.Caddy == nil {
		return &pipelineError{code: "E_CADDY_APPLY", err: fmt.Errorf(
			"domain sync needs a route engine; none is configured")}
	}
	if err := RegenerateRoutes(ctx, p.Store, p.Caddy, log); err != nil {
		return &RetryableError{err}
	}
	log.Info("domain sync complete")
	return nil
}
