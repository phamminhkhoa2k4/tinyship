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
	"github.com/tinyship/tinyship/internal/secrypto"
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
	// ListManagedContainers enumerates every container carrying the
	// managed label; app teardown finds its containers by app label
	// even when database rows are already gone.
	ListManagedContainers(ctx context.Context) ([]docker.ContainerInfo, error)
}

// Deployer runs the deploy pipeline for claimed deploy jobs.
type Deployer struct {
	Store             *store.Store
	Docker            Docker
	Logger            *slog.Logger
	Actor             string // audit actor (worker id)
	HealthConsecutive int    // default HealthConsecutivePasses
	// SecretKey opens sealed env values and registry tokens. Nil means
	// no key material: deploys that need a secret fail at validate
	// with a clear error instead of starting a container with a
	// half-built environment.
	SecretKey []byte
	// Caddy applies generated proxy config; nil means no route engine
	// is configured and an app with active domains fails loudly.
	Caddy Caddy
	// Probe performs the final check; nil means DefaultProbe.
	Probe ProbeFunc
	// Host facts the admission guard reads; nil means the real readers
	// (/proc/meminfo, statfs of the Docker data path, runtime CPUs).
	// Tests pin them to a scenario instead of the real host.
	MemTotalMB func() (int, error)
	DiskFreeMB func(path string) (int, error)
	NumCPUs    func() int
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
// E_NO_FREE_PORT), falling back to fallback.
func codeOf(err error, fallback string) string {
	msg := err.Error()
	if i := strings.Index(msg, ":"); i > 0 && strings.HasPrefix(msg[:i], "E_") {
		return msg[:i]
	}
	return fallback
}

// deployPayload is the strict job payload. Force admits a deploy past
// the RAM budget (never past the disk floor or the security policy).
type deployPayload struct {
	DeploymentID string `json:"deployment_id"`
	Force        bool   `json:"force,omitempty"`
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
	auth           *docker.Auth // decrypted registry login for this run; never logged
	imageRef       string
	digest         string
	hostPort       int
	strategy       string
	strategyAuto   bool // strategy was derived, not chosen: admission may lower it
	oldMemMB       int  // granted memory of the deployment being replaced (0 = none)
	domains        []string
	force          bool   // admitted past the RAM budget by explicit request
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

	d := &deployRun{dep: dep, app: app, force: pl.Force}
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

// pipeline executes the steps in order. route_switch applies the
// generated proxy config when the app has active domains and records
// skip otherwise; final_check probes those domains through the proxy.
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

// openSecret decrypts a sealed value with the worker's private key.
// A missing or wrong key fails the run permanently: retrying cannot
// conjure key material, and starting a container without the secret
// would be worse than not starting it.
func (p *Deployer) openSecret(sealed []byte, what string) ([]byte, error) {
	if len(p.SecretKey) == 0 {
		return nil, &pipelineError{code: "E_SECRETS_KEY", err: fmt.Errorf(
			"%s is sealed but the worker has no private key configured "+
				"(generate one with `tinyship keygen`)", what)}
	}
	plain, err := secrypto.Open(p.SecretKey, sealed)
	if err != nil {
		return nil, &pipelineError{code: "E_SECRETS_KEY", err: fmt.Errorf(
			"%s: %w", what, err)}
	}
	return plain, nil
}

// stepValidate re-reads and revalidates everything from the database:
// app config, env store, image reference, effective strategy, and
// the previous container to replace. A rollback revalidates the target
// deployment's config snapshot instead of the app's current config, but
// still takes env vars live: rolling back the image must not roll back
// the secrets and settings the operator has changed since.
func (p *Deployer) stepValidate(ctx context.Context, d *deployRun) (any, error) {
	configJSON := d.app.ConfigJSON
	if d.dep.Kind == "rollback" {
		configJSON = d.dep.ConfigSnapshotJSON
	}
	cfg, err := appconfig.Parse([]byte(configJSON))
	if err != nil {
		return nil, &pipelineError{code: "E_VALIDATION",
			err: fmt.Errorf("stored config for app %s: %w", d.app.Name, err)}
	}
	if _, err := appconfig.Validate(cfg); err != nil {
		return nil, &pipelineError{code: "E_VALIDATION", err: err}
	}
	d.cfg = cfg

	// Credential existence is a config problem; the token itself is
	// opened here and lives only in memory for this run.
	d.auth = nil
	if cfg.RegistryCredential != "" {
		cred, err := p.Store.GetCredential(ctx, cfg.RegistryCredential)
		if err != nil {
			return nil, &pipelineError{code: "E_VALIDATION", err: fmt.Errorf(
				"registry_credential %q: %w (create it with `tinyship credential set`)",
				cfg.RegistryCredential, err)}
		}
		token, err := p.openSecret(cred.SecretSealed, "registry credential "+cred.Name)
		if err != nil {
			return nil, err
		}
		d.auth = &docker.Auth{
			Registry: cred.Registry,
			Username: cred.Username,
			Secret:   string(token),
		}
	}

	vars, err := p.Store.ListAppEnv(ctx, d.app.ID)
	if err != nil {
		return nil, &RetryableError{err}
	}
	env := make([]string, 0, len(vars))
	for _, v := range vars {
		if v.Kind == store.EnvPlain {
			env = append(env, v.Key+"="+v.ValuePlain)
			continue
		}
		plain, err := p.openSecret(v.ValueSealed, "env "+v.Key)
		if err != nil {
			return nil, err
		}
		env = append(env, v.Key+"="+string(plain))
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
	// writable, otherwise start-first. An auto start-first may be
	// lowered to stop-first by admission when the parallel peak does
	// not fit the budget.
	d.strategyAuto = false
	switch cfg.Strategy {
	case "start-first", "stop-first":
		d.strategy = cfg.Strategy
	default: // auto
		d.strategyAuto = true
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
			if prevCfg, err := appconfig.Parse([]byte(prev.ConfigSnapshotJSON)); err == nil {
				if prevCfg.StopGraceS > 0 {
					d.oldGrace = time.Duration(prevCfg.StopGraceS) * time.Second
				}
				d.oldMemMB = prevCfg.Resources.MemoryMB
			}
		}
	}

	return map[string]any{
		"image": d.imageRef, "strategy": d.strategy,
		"memory_mb": cfg.Resources.MemoryMB, "warnings_env": len(env),
	}, nil
}

// stepAdmission enforces the host guardrails before anything is pulled
// or created: the RAM budget with the parallel-peak rule, the free-disk
// floor, and the CPU overcommit warning. It also allocates the loopback
// host port in the same step, so a deploy that is refused never holds a
// port.
//
// The budget is the granted limits, not live usage. When the strategy
// was derived (auto), a start-first peak that does not fit is lowered
// to stop-first rather than refused; an explicitly chosen start-first
// is never silently changed.
func (p *Deployer) stepAdmission(ctx context.Context, d *deployRun) (any, error) {
	budget, err := p.Store.GetBudgetSettings(ctx)
	if err != nil {
		return nil, &RetryableError{err}
	}
	memTotal, err := p.memTotalMB()
	if err != nil {
		return nil, &RetryableError{err}
	}
	budgetMB := budget.BudgetMB
	budgetSource := "configured"
	if budgetMB == 0 {
		budgetMB = AutoBudgetMB(memTotal)
		budgetSource = "auto"
	}

	// Committed memory: every other running app's granted limit. A
	// corrupt stored config must not silently vanish from the sum, so
	// it counts at the default limit.
	running, err := p.Store.ListRunningApps(ctx)
	if err != nil {
		return nil, &RetryableError{err}
	}
	committed, committedCPU := 0, 0
	for _, a := range running {
		if a.ID == d.app.ID {
			continue
		}
		mb, cpu := appconfig.DefaultMemoryMB, appconfig.DefaultCPUMillis
		if otherCfg, err := appconfig.Parse([]byte(a.ConfigJSON)); err == nil {
			mb, cpu = otherCfg.Resources.MemoryMB, otherCfg.Resources.CPUMillis
		}
		committed += mb
		committedCPU += cpu
	}

	newMB := d.cfg.Resources.MemoryMB
	// Peak accounting: start-first runs old and new together, so the
	// budget must fit both. The old limit is the snapshot of the
	// deployment being replaced; no old container, no peak.
	needed := newMB
	if d.strategy == "start-first" && d.oldContainerID != "" {
		needed = d.oldMemMB + newMB
	}
	if d.strategyAuto && committed+needed > budgetMB && committed+newMB <= budgetMB {
		d.strategy = "stop-first"
		needed = newMB
		if err := p.Store.SetDeploymentStrategy(ctx, d.dep.ID, d.strategy); err != nil {
			return nil, &RetryableError{err}
		}
	}

	warnings := []string{}
	over := committed + needed - budgetMB
	if over > 0 {
		if !d.force {
			return nil, &pipelineError{code: "E_BUDGET_EXCEEDED", err: fmt.Errorf(
				"host RAM budget exceeded: %d MiB committed + %d MiB needed = %d MiB, "+
					"budget %d MiB (%s), over by %d MiB; lower resources.memory_mb, "+
					"stop or delete another app, or redeploy with --force",
				committed, needed, committed+needed, budgetMB, budgetSource, over)}
		}
		// Force admits past the RAM budget only; the disk floor and
		// the security policy still apply below and in later steps.
		warnings = append(warnings, "W_BUDGET_FORCED")
		if err := p.Store.AppendAudit(ctx, p.Actor, "admission.force", "deployment",
			d.dep.ID, "forced", map[string]any{
				"app": d.app.Name, "budget_mb": budgetMB, "budget_source": budgetSource,
				"committed_mb": committed, "requested_mb": needed, "over_by_mb": over,
			}); err != nil {
			return nil, &RetryableError{err}
		}
	}

	// Disk floor: refuse to fill the disk that holds Docker's data.
	// This is never bypassed by force.
	free, err := p.diskFreeMB(DockerDataPath)
	if err != nil {
		return nil, &RetryableError{err}
	}
	if free < budget.MinFreeDiskMB {
		return nil, &pipelineError{code: "E_DISK_LOW", err: fmt.Errorf(
			"only %d MiB free on the Docker data partition, floor is %d MiB; "+
				"free disk space (remove old images or containers) and retry",
			free, budget.MinFreeDiskMB)}
	}

	// CPU overcommit is a warning, never a rejection.
	totalCPU := committedCPU + d.cfg.Resources.CPUMillis
	if float64(totalCPU) > float64(p.numCPUs())*1000*budget.CPUOvercommit {
		warnings = append(warnings, "W_CPU_OVERCOMMIT")
	}

	port, err := p.Store.AllocateHostPort(ctx, d.dep.ID)
	if err != nil {
		if strings.HasPrefix(err.Error(), "E_") {
			return nil, &pipelineError{code: codeOf(err, "E_ADMISSION"), err: err}
		}
		return nil, &RetryableError{err} // transient DB trouble
	}
	d.hostPort = port
	return map[string]any{
		"host_port": port, "memory_mb": newMB, "strategy": d.strategy,
		"budget_mb": budgetMB, "budget_source": budgetSource,
		"committed_mb": committed, "needed_mb": needed,
		"disk_free_mb": free, "warnings": warnings,
	}, nil
}

// memTotalMB, diskFreeMB, numCPUs read the host facts admission checks
// against; the real readers back the fields of the same name.
func (p *Deployer) memTotalMB() (int, error) {
	if p.MemTotalMB != nil {
		return p.MemTotalMB()
	}
	return readMemTotalMB()
}

func (p *Deployer) diskFreeMB(path string) (int, error) {
	if p.DiskFreeMB != nil {
		return p.DiskFreeMB(path)
	}
	return statfsFreeMB(path)
}

func (p *Deployer) numCPUs() int {
	if p.NumCPUs != nil {
		return p.NumCPUs()
	}
	return numCPU()
}

// stepPull pulls the image and pins the digest. Network and 5xx
// errors retry; "not found"/auth failures fail the job immediately
func (p *Deployer) stepPull(ctx context.Context, d *deployRun, log *slog.Logger) (any, error) {
	digest, err := p.Docker.PullImage(ctx, d.imageRef, d.auth)
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
