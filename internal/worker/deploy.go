package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/health"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// DeployJobType is the jobs.type value for deployments.
const DeployJobType = "deploy"

// HealthConsecutivePasses is how many consecutive probe passes mark a
// container healthy.
const HealthConsecutivePasses = 2

// Docker is the subset of the allowlisted wrapper the deploy
// pipeline needs. The production implementation is *docker.Wrapper; tests
// inject a fake. Nothing outside internal/docker may talk to the daemon
// (enforced by the static client-scope test).
type Docker interface {
	PullImage(ctx context.Context, ref string, auth *docker.Auth) (string, error)
	EnsureNetwork(ctx context.Context) error
	EnsureVolume(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, spec *policy.ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	StopContainer(ctx context.Context, id string, grace time.Duration) error
	RemoveContainer(ctx context.Context, id string, force bool) error
	InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error)
	ContainerLogs(ctx context.Context, id string, tail int, since time.Duration, follow bool) (io.ReadCloser, error)
	ExecProbe(ctx context.Context, id string, command []string, timeout time.Duration) (int, error)
}

// Deployer runs the deploy pipeline for claimed deploy jobs.
type Deployer struct {
	Store             *store.Store
	Docker            Docker
	Logger            *slog.Logger
	Actor             string // audit actor (worker id)
	HealthConsecutive int    // default HealthConsecutivePasses
}

// NewDeployer wires the pipeline over a store and an allowlisted Docker
// implementation.
func NewDeployer(s *store.Store, d Docker, log *slog.Logger, actor string) *Deployer {
	if log == nil {
		log = slog.Default()
	}
	return &Deployer{
		Store:             s,
		Docker:            d,
		Logger:            log,
		Actor:             actor,
		HealthConsecutive: HealthConsecutivePasses,
	}
}

// Handler returns the Handler for DeployJobType jobs.
func (p *Deployer) Handler() Handler {
	return p.run
}

// pipelineError carries a error code (E_VALIDATION, E_IMAGE_NOT_FOUND,
// E_HEALTH_FAILED, ...) without a step; step() attaches the step name.
type pipelineError struct {
	code string
	err  error
}

func (e *pipelineError) Error() string {
	if e.err == nil {
		return e.code
	}
	return fmt.Sprintf("%s: %v", e.code, e.err)
}
func (e *pipelineError) Unwrap() error { return e.err }

// stepError is a pipelineError plus the failing pipeline step and whether
// the worker should retry.
type stepError struct {
	step      string
	code      string
	retryable bool
	err       error
}

func (e *stepError) Error() string {
	return fmt.Sprintf("%s (step %s): %v", e.code, e.step, e.err)
}
func (e *stepError) Unwrap() error { return e.err }

func asStepError(err error) *stepError {
	var se *stepError
	if errors.As(err, &se) {
		return se
	}
	return &stepError{step: "unknown", code: "E_DEPLOY_FAILED", err: err}
}

// codeOf extracts an E_* prefix from well-formed store errors (e.g.
// E_SECRETS_UNSUPPORTED, E_NO_FREE_PORT), falling back to fallback.
func codeOf(err error, fallback string) string {
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 && strings.HasPrefix(msg[:i], "E_") {
		return msg[:i]
	}
	return fallback
}

// deployPayload is the strict job payload.
type deployPayload struct {
	DeploymentID string `json:"deployment_id"`
}

func decodeDeployPayload(raw string) (*deployPayload, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var pl deployPayload
	if err := dec.Decode(&pl); err != nil {
		return nil, fmt.Errorf("deploy payload: %w", err)
	}
	if pl.DeploymentID == "" {
		return nil, errors.New("deploy payload: missing deployment_id")
	}
	return &pl, nil
}

// deployRun is the per-job state carried across steps.
type deployRun struct {
	dep *store.Deployment
	app *store.App

	cfg            *appconfig.Config
	env            []string
	imageRef       string
	digest         string
	hostPort       int
	strategy       string
	domains        []string
	containerID    string // the new container (created or reused this run)
	containerName  string
	oldContainerID string
	oldGrace       time.Duration
	oldStopped     bool
	skipped        bool // this step recorded its own skip row

	failedStep string
}

// run is one claimed deploy job.
func (p *Deployer) run(ctx context.Context, job *store.Job) error {
	pl, err := decodeDeployPayload(job.PayloadJSON)
	if err != nil {
		return &pipelineError{code: "E_VALIDATION", err: err}
	}
	dep, err := p.Store.GetDeployment(ctx, pl.DeploymentID)
	if errors.Is(err, store.ErrNotFound) {
		return &pipelineError{code: "E_VALIDATION",
			err: fmt.Errorf("deployment %q does not exist", pl.DeploymentID)}
	}
	if err != nil {
		return &RetryableError{err}
	}
	if dep.State == store.DepSucceeded {
		return nil // finalize already committed
	}
	app, err := p.Store.GetAppByID(ctx, dep.AppID)
	if errors.Is(err, store.ErrNotFound) {
		return &pipelineError{code: "E_VALIDATION",
			err: fmt.Errorf("app %q for deployment %s does not exist", dep.AppID, dep.ID)}
	}
	if err != nil {
		return &RetryableError{err}
	}
	if err := p.Store.MarkDeploymentRunning(ctx, dep.ID); err != nil {
		return &RetryableError{err}
	}

	d := &deployRun{dep: dep, app: app}
	log := p.Logger.With("job", job.ID, "deployment", dep.ID, "app", app.Name, "seq", dep.Seq)
	log.Info("deploy: starting pipeline", "image", dep.ImageRef)

	err = p.pipeline(ctx, d, log)
	if err == nil {
		log.Info("deploy: succeeded")
		return nil
	}
	if Retryable(err) && job.Attempts < job.MaxAttempts {
		// Transient failure with attempts left: leave the deployment
		// running; the worker requeues with backoff.
		log.Warn("deploy: transient failure, will retry", "err", err,
			"attempt", job.Attempts, "max", job.MaxAttempts)
		return err
	}
	// Permanent failure — or transient failures with attempts exhausted.
	se := asStepError(err)
	log.Error("deploy: failed", "step", se.step, "code", se.code, "err", se.err)
	if failErr := p.Store.FailDeployment(ctx, dep.ID, se.step, se.code, se.Error()); failErr != nil {
		log.Error("deploy: could not record failure", "err", failErr)
	}
	p.recover(ctx, d, log, se)
	return err
}

// pipeline executes the steps in order. route_switch and final_check
// record skip (there is no Caddy integration, so domains never exist) —
// the timeline shows the full pipeline with explicit reasons.
func (p *Deployer) pipeline(ctx context.Context, d *deployRun, log *slog.Logger) error {
	if err := p.step(ctx, d, "validate", func() (any, error) {
		return p.stepValidate(ctx, d)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "admission", func() (any, error) {
		return p.stepAdmission(ctx, d)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "pull", func() (any, error) {
		return p.stepPull(ctx, d, log)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "stop_old", func() (any, error) {
		return p.stepStopOld(ctx, d)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "create", func() (any, error) {
		return p.stepCreate(ctx, d, log)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "start", func() (any, error) {
		return p.stepStart(ctx, d, log)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "health", func() (any, error) {
		return p.stepHealth(ctx, d, log)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "route_switch", func() (any, error) {
		return p.stepRouteSwitch(ctx, d)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "final_check", func() (any, error) {
		return p.stepFinalCheck(ctx, d)
	}); err != nil {
		return err
	}
	if err := p.step(ctx, d, "retire_old", func() (any, error) {
		return p.stepRetireOld(ctx, d, log)
	}); err != nil {
		return err
	}
	return p.step(ctx, d, "finalize", func() (any, error) {
		return p.stepFinalize(ctx, d)
	})
}

// step runs one pipeline step, appending start/ok/fail timeline events.
// The returned error always carries step + code.
func (p *Deployer) step(ctx context.Context, d *deployRun, name string, fn func() (any, error)) error {
	start := time.Now()
	_ = p.Store.AppendDeploymentEvent(ctx, d.dep.ID, name, store.EvStart, nil)

	detail, err := fn()
	if err == nil {
		if !d.skipped {
			_ = p.Store.AppendDeploymentEvent(ctx, d.dep.ID, name, store.EvOK, detail)
		}
		d.skipped = false
		p.Logger.Debug("deploy step ok", "deployment", d.dep.ID, "step", name,
			"duration", time.Since(start))
		return nil
	}
	d.skipped = false

	code := "E_DEPLOY_FAILED"
	var pe *pipelineError
	if errors.As(err, &pe) && pe.code != "" {
		code = pe.code
	}
	retry := Retryable(err)
	d.failedStep = name
	_ = p.Store.AppendDeploymentEvent(ctx, d.dep.ID, name, store.EvFail, map[string]any{
		"code": code, "error": err.Error(), "retryable": retry,
		"duration_ms": time.Since(start).Milliseconds(),
	})
	return &stepError{step: name, code: code, retryable: retry, err: err}
}

// skip records an explicit skip row so the timeline never pretends a step
// ran. It marks the
// run so step() does not double-report the step as ok.
func (p *Deployer) skip(ctx context.Context, d *deployRun, name string, detail any) {
	_ = p.Store.AppendDeploymentEvent(ctx, d.dep.ID, name, store.EvSkip, detail)
	d.skipped = true
}

// --- steps ------------------------------------------------------------------

// stepValidate re-reads and revalidates everything from the database
// app config, env store, image reference, effective strategy, and
// the previous container to replace.
func (p *Deployer) stepValidate(ctx context.Context, d *deployRun) (any, error) {
	cfg, err := appconfig.Parse([]byte(d.app.ConfigJSON))
	if err != nil {
		return nil, &pipelineError{code: "E_VALIDATION",
			err: fmt.Errorf("stored config for app %s: %w", d.app.Name, err)}
	}
	if _, err := appconfig.Validate(cfg); err != nil {
		return nil, &pipelineError{code: "E_VALIDATION", err: err}
	}
	d.cfg = cfg

	if cfg.RegistryCredential != "" {
		return nil, &pipelineError{code: "E_REGISTRY_AUTH",
			err: fmt.Errorf("registry credential %q: private registry authentication "+
				"is not implemented in this release", cfg.RegistryCredential)}
	}
	env, err := p.Store.ListAppEnvPlain(ctx, d.app.ID)
	if err != nil {
		return nil, &pipelineError{code: codeOf(err, "E_VALIDATION"), err: err}
	}
	d.env = env

	switch {
	case cfg.Digest != "":
		d.imageRef = cfg.Image + "@" + cfg.Digest
	case cfg.Tag != "":
		d.imageRef = cfg.Image + ":" + cfg.Tag
	default:
		d.imageRef = cfg.Image // docker resolves :latest
	}

	// Effective strategy: auto means stop-first when any volume is
	// writable, otherwise start-first.
	switch cfg.Strategy {
	case "start-first", "stop-first":
		d.strategy = cfg.Strategy
	default: // auto
		d.strategy = "start-first"
		for _, v := range cfg.Volumes {
			if !v.ReadOnly {
				d.strategy = "stop-first"
				break
			}
		}
	}
	if err := p.Store.SetDeploymentStrategy(ctx, d.dep.ID, d.strategy); err != nil {
		return nil, &RetryableError{err}
	}

	// Previous container (for stop_old/retire_old). First deploy: none.
	prevID := d.dep.PreviousDeploymentID
	if prevID == nil {
		prevID = d.app.CurrentDeploymentID
	}
	d.oldGrace = time.Duration(appconfig.DefaultStopGraceS) * time.Second
	if prevID != nil && *prevID != "" && *prevID != d.dep.ID {
		prev, err := p.Store.GetDeployment(ctx, *prevID)
		if errors.Is(err, store.ErrNotFound) {
			// superseded/pruned record: nothing to retire
		} else if err != nil {
			return nil, &RetryableError{err}
		} else {
			if prev.ContainerID != nil {
				d.oldContainerID = *prev.ContainerID
			}
			if prevCfg, err := appconfig.Parse([]byte(prev.ConfigSnapshotJSON)); err == nil && prevCfg.StopGraceS > 0 {
				d.oldGrace = time.Duration(prevCfg.StopGraceS) * time.Second
			}
		}
	}

	return map[string]any{
		"image": d.imageRef, "strategy": d.strategy,
		"memory_mb": cfg.Resources.MemoryMB, "warnings_env": len(env),
	}, nil
}

// stepAdmission allocates the loopback host port in one transaction. The
// full budget algorithm is not implemented yet; the step still
// records the memory limit it would account for.
func (p *Deployer) stepAdmission(ctx context.Context, d *deployRun) (any, error) {
	port, err := p.Store.AllocateHostPort(ctx, d.dep.ID)
	if err != nil {
		if strings.HasPrefix(err.Error(), "E_") {
			return nil, &pipelineError{code: codeOf(err, "E_ADMISSION"), err: err}
		}
		return nil, &RetryableError{err} // transient DB trouble
	}
	d.hostPort = port
	return map[string]any{
		"host_port": port,
		"memory_mb": d.cfg.Resources.MemoryMB,
		"strategy":  d.strategy,
		"note":      "budget/disk admission is not implemented; this step records the limit only",
	}, nil
}

// stepPull pulls the image and pins the digest. Network and 5xx
// errors retry; "not found"/auth failures fail the job immediately
func (p *Deployer) stepPull(ctx context.Context, d *deployRun, log *slog.Logger) (any, error) {
	digest, err := p.Docker.PullImage(ctx, d.imageRef, nil)
	if err != nil {
		switch lower := strings.ToLower(err.Error()); {
		case strings.Contains(lower, "unauthorized"),
			strings.Contains(lower, "authentication required"),
			strings.Contains(lower, "authorization failed"):
			return nil, &pipelineError{code: "E_REGISTRY_AUTH", err: err}
		case strings.Contains(lower, "manifest unknown"),
			strings.Contains(lower, "not found"),
			strings.Contains(lower, "no such"),
			strings.Contains(lower, "name unknown"),
			strings.Contains(lower, "does not exist"),
			strings.Contains(lower, "denied"):
			return nil, &pipelineError{code: "E_IMAGE_NOT_FOUND", err: err}
		default:
			return nil, &RetryableError{err}
		}
	}
	if d.cfg.Digest != "" && digest != d.cfg.Digest {
		return nil, &pipelineError{code: "E_VALIDATION", err: fmt.Errorf(
			"pinned digest mismatch: config wants %s, registry served %s", d.cfg.Digest, digest)}
	}
	d.digest = digest
	if err := p.Store.SetDeploymentImageDigest(ctx, d.dep.ID, digest); err != nil {
		return nil, &RetryableError{err}
	}
	log.Info("pulled image", "ref", d.imageRef, "digest", digest) // job log progress
	return map[string]any{"ref": d.imageRef, "digest": digest}, nil
}

// stepStopOld stops the current container for stop-first strategies only.
// First deploys and start-first runs record skip.
func (p *Deployer) stepStopOld(ctx context.Context, d *deployRun) (any, error) {
	if d.oldContainerID == "" {
		p.skip(ctx, d, "stop_old", map[string]any{"reason": "first deploy: nothing to stop"})
		return nil, nil
	}
	if d.strategy != "stop-first" {
		p.skip(ctx, d, "stop_old", map[string]any{
			"reason": "start-first: old container keeps running until the new one is healthy"})
		return nil, nil
	}
	info, err := p.Docker.InspectContainer(ctx, d.oldContainerID)
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			p.skip(ctx, d, "stop_old", map[string]any{"reason": "previous container already gone"})
			return nil, nil
		}
		return nil, &RetryableError{err}
	}
	if info.State != "running" {
		return map[string]any{"already_stopped": true}, nil
	}
	if err := p.Docker.StopContainer(ctx, d.oldContainerID, d.oldGrace); err != nil {
		// stop_old: a failed stop fails the deployment and keeps
		// the old container untouched. Transient daemon errors retry first.
		return nil, &RetryableError{err}
	}
	d.oldStopped = true
	return map[string]any{"container": p.short(d.oldContainerID), "grace_s": int(d.oldGrace.Seconds())}, nil
}

// stepCreate builds the closed spec and creates the container.
// A container with this deployment's name and config hash is reused, which
// is what makes a crash re-run idempotent.
func (p *Deployer) stepCreate(ctx context.Context, d *deployRun, log *slog.Logger) (any, error) {
	wantHash := policy.ConfigHash(d.cfg)
	spec, err := policy.Build(d.cfg, policy.Instance{
		AppID:        d.app.ID,
		AppName:      d.app.Name,
		DeploymentID: d.dep.ID,
		Seq:          d.dep.Seq,
		Digest:       d.digest,
		ConfigHash:   wantHash,
		HostPort:     d.hostPort,
		Env:          d.env,
	})
	if err != nil {
		return nil, &pipelineError{code: "E_POLICY_FORBIDDEN_PARAM", err: err}
	}
	if err := p.Docker.EnsureNetwork(ctx); err != nil {
		return nil, &RetryableError{err}
	}
	for _, v := range spec.Volumes {
		if err := p.Docker.EnsureVolume(ctx, policy.DockerVolumeName(d.app.Name, v.Name)); err != nil {
			return nil, &RetryableError{err}
		}
	}

	// Idempotent re-run: reuse our own container when name + hash match.
	if existing, err := p.Docker.InspectContainer(ctx, spec.Name); err == nil {
		if existing.Labels[policy.LabelDeployment] == d.dep.ID &&
			existing.Labels[policy.LabelConfigHash] == wantHash {
			d.containerID, d.containerName = existing.ID, spec.Name
			if err := p.Store.SetDeploymentContainer(ctx, d.dep.ID, existing.ID, spec.Name); err != nil {
				return nil, &RetryableError{err}
			}
			log.Info("deploy: reusing existing container from previous attempt",
				"container", spec.Name)
			return map[string]any{"container": spec.Name, "reused": true}, nil
		}
		// Different config hash: remove and recreate.
		if err := p.Docker.RemoveContainer(ctx, existing.ID, true); err != nil &&
			!strings.Contains(err.Error(), "No such container") {
			return nil, &RetryableError{err}
		}
	}

	id, err := p.Docker.CreateContainer(ctx, spec)
	if err != nil {
		return nil, &RetryableError{err}
	}
	d.containerID, d.containerName = id, spec.Name
	if err := p.Store.SetDeploymentContainer(ctx, d.dep.ID, id, spec.Name); err != nil {
		return nil, &RetryableError{err}
	}
	return map[string]any{"container": spec.Name, "id": p.short(id)}, nil
}

// stepStart starts the container and confirms it is actually running
func (p *Deployer) stepStart(ctx context.Context, d *deployRun, log *slog.Logger) (any, error) {
	info, err := p.Docker.InspectContainer(ctx, d.containerID)
	if err != nil {
		return nil, &RetryableError{err}
	}
	if info.State != "running" {
		if err := p.Docker.StartContainer(ctx, d.containerID); err != nil {
			return nil, &RetryableError{err}
		}
		// Give a crashing entrypoint a moment to actually exit before
		// judging; anything that survives this window must still pass health.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		info, err = p.Docker.InspectContainer(ctx, d.containerID)
		if err != nil {
			return nil, &RetryableError{err}
		}
	}
	if info.State != "running" {
		tail := p.logTail(ctx, d.containerID, 50)
		return nil, &pipelineError{code: "E_START_FAILED", err: fmt.Errorf(
			"container %s is %s (exit code %d)%s", p.short(d.containerID),
			info.State, info.ExitCode, tail)}
	}
	log.Info("deploy: container running", "container", p.short(d.containerID))
	return map[string]any{"container": p.short(d.containerID)}, nil
}

// stepHealth probes from the host until the config's health check
// passes HealthConsecutive times in a row. On failure the
// message carries the last 50 log lines and never retries at
// the job level.
func (p *Deployer) stepHealth(ctx context.Context, d *deployRun, log *slog.Logger) (any, error) {
	target := fmt.Sprintf("127.0.0.1:%d", d.hostPort)
	var execFn health.ExecFunc
	if d.cfg.Health.Type == "exec" {
		execFn = func(c context.Context, command []string, timeout time.Duration) (int, error) {
			return p.Docker.ExecProbe(c, d.containerID, command, timeout)
		}
	}
	want := p.HealthConsecutive
	if want < 1 {
		want = HealthConsecutivePasses
	}
	res := health.WaitStable(ctx, target, d.cfg.Health, execFn, want)
	if !res.Healthy {
		extra := p.logTail(ctx, d.containerID, 50)
		if info, err := p.Docker.InspectContainer(ctx, d.containerID); err == nil &&
			info.State != "running" {
			extra = fmt.Sprintf("\ncontainer state: %s (exit code %d)", info.State, info.ExitCode) + extra
		}
		return nil, &pipelineError{code: "E_HEALTH_FAILED", err: fmt.Errorf(
			"%s (target %s, type %s)%s", res.Reason, target, d.cfg.Health.Type, extra)}
	}
	log.Info("deploy: health check passed", "target", target, "type", d.cfg.Health.Type)
	return map[string]any{"target": target, "type": d.cfg.Health.Type,
		"consecutive_passes": want}, nil
}

// stepRouteSwitch would regenerate the Caddy server config from the DB.
// There is no Caddy integration, so an app with active domains must fail
// loudly instead of silently not routing; an app with none records skip.
func (p *Deployer) stepRouteSwitch(ctx context.Context, d *deployRun) (any, error) {
	domains, err := p.Store.ListActiveDomains(ctx, d.app.ID)
	if err != nil {
		return nil, &RetryableError{err}
	}
	d.domains = domains
	if len(domains) > 0 {
		return nil, &pipelineError{code: "E_CADDY_APPLY", err: fmt.Errorf(
			"app has %d active domain(s) but Caddy integration is not present "+
				"in this release; refusing to report success without routing", len(domains))}
	}
	p.skip(ctx, d, "route_switch", map[string]any{
		"reason": "no active domains; Caddy route generation is not available"})
	return nil, nil
}

// stepFinalCheck probes Caddy over SNI for each active domain.
// Without domains it records skip, exactly as the spec prescribes.
func (p *Deployer) stepFinalCheck(ctx context.Context, d *deployRun) (any, error) {
	if len(d.domains) > 0 {
		return nil, &pipelineError{code: "E_FINAL_CHECK_FAILED", err: fmt.Errorf(
			"%d active domain(s) require the Caddy final check", len(d.domains))}
	}
	p.skip(ctx, d, "final_check", map[string]any{
		"reason": "no active domains to probe"})
	return nil, nil
}

// stepRetireOld stops and removes the previous container. Per
// failures here are warnings only: the deployment still succeeds and
// reconcile/gc (E-later) reclaims orphans.
func (p *Deployer) stepRetireOld(ctx context.Context, d *deployRun, log *slog.Logger) (any, error) {
	if d.oldContainerID == "" {
		p.skip(ctx, d, "retire_old", map[string]any{"reason": "first deploy: nothing to retire"})
		return nil, nil
	}
	if _, err := p.Docker.InspectContainer(ctx, d.oldContainerID); err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return map[string]any{"already_removed": true}, nil
		}
		// Warning only per — but a daemon outage here is suspicious, so
		// report it in the event and keep going.
		p.skip(ctx, d, "retire_old", map[string]any{"warning": err.Error()})
		return nil, nil
	}
	var warnings []string
	if !d.oldStopped {
		if err := p.Docker.StopContainer(ctx, d.oldContainerID, d.oldGrace); err != nil &&
			!strings.Contains(err.Error(), "No such container") {
			warnings = append(warnings, "stop: "+err.Error())
		}
	}
	if err := p.Docker.RemoveContainer(ctx, d.oldContainerID, true); err != nil &&
		!strings.Contains(err.Error(), "No such container") {
		warnings = append(warnings, "remove: "+err.Error())
	}
	if len(warnings) > 0 {
		log.Warn("deploy: retire_old warnings (deployment still succeeds)",
			"container", p.short(d.oldContainerID), "warnings", warnings)
		return map[string]any{"container": p.short(d.oldContainerID),
			"warning": strings.Join(warnings, "; ")}, nil
	}
	return map[string]any{"container": p.short(d.oldContainerID)}, nil
}

// stepFinalize commits the whole outcome in one transaction:
// deployment succeeded, previous deployment superseded,
// apps.current_deployment_id updated. A DB failure here is transient and
// re-runs safely.
func (p *Deployer) stepFinalize(ctx context.Context, d *deployRun) (any, error) {
	if err := p.Store.SucceedDeployment(ctx, d.app.ID, d.dep.ID); err != nil {
		return nil, &RetryableError{err}
	}
	return map[string]any{"current": d.dep.ID}, nil
}

// --- failure recovery -------------------------------------------

// recover runs after a permanent failure: record the failure,
// clean up the new container, and for stop-first restart the previous
// container so the app keeps serving. A failed restart is audited as
// needs_attention.
func (p *Deployer) recover(ctx context.Context, d *deployRun, log *slog.Logger, se *stepError) {
	if d.containerID != "" {
		if err := p.Docker.RemoveContainer(ctx, d.containerID, true); err != nil &&
			!strings.Contains(err.Error(), "No such container") {
			log.Error("deploy: failed to clean up new container", "container",
				p.short(d.containerID), "err", err)
		} else {
			log.Info("deploy: cleaned up new container", "container", p.short(d.containerID))
		}
	}
	if d.oldStopped && d.oldContainerID != "" {
		if err := p.Docker.StartContainer(ctx, d.oldContainerID); err != nil {
			log.Error("deploy: previous container did not restart; app needs attention",
				"container", p.short(d.oldContainerID), "err", err)
			_ = p.Store.AppendAudit(ctx, p.Actor, "deploy.recover", "container",
				d.oldContainerID, "needs_attention", map[string]any{
					"deployment": d.dep.ID, "app": d.app.Name,
					"step": se.step, "code": se.code, "error": err.Error(),
				})
		} else {
			log.Info("deploy: restored previous container",
				"container", p.short(d.oldContainerID))
		}
	}
}

// --- helpers -----------------------------------------------------------------

// logTail reads the last n container log lines for failure messages.
// The Docker wrapper already strips stream framing; DemuxBytes is a
// second belt for fakes that return framed bytes.
func (p *Deployer) logTail(ctx context.Context, containerID string, n int) string {
	if containerID == "" {
		return ""
	}
	rc, err := p.Docker.ContainerLogs(ctx, containerID, n, 0, false)
	if err != nil {
		return fmt.Sprintf("\n(log unavailable: %v)", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 64*1024))
	if err != nil {
		return fmt.Sprintf("\n(log read failed: %v)", err)
	}
	text := string(docker.DemuxBytes(raw))
	if text == "" {
		return "\n(last " + fmt.Sprint(n) + " log lines: empty)"
	}
	return "\nlast " + fmt.Sprint(n) + " container log lines:\n" + text
}

// short truncates ids for messages while staying unambiguous.
func (p *Deployer) short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
