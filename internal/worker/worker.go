// Package worker executes jobs claimed from the SQLite queue. It is the only
// component allowed to touch Docker and the Caddy admin socket. It must
// never listen on a TCP port.
//
// The durable loop (claim → run handler → finish/requeue with a lease
// heartbeat and a reaper for jobs orphaned by crashes) lives in
// worker.go; the deploy pipeline lives in deploy.go.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/tinyship/tinyship/internal/caddy"
	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/secrypto"
	"github.com/tinyship/tinyship/internal/store"
)

// DefaultDBPath is where the job queue lives on an installed host.
// Overridable for development and tests.
const DefaultDBPath = "/var/lib/tinyship/tinyship.db"

// DefaultRunDir holds the worker's log socket on an installed host.
// TINYSHIP_RUN_DIR overrides it.
const DefaultRunDir = "/run/tinyship"

// DefaultSecretsKey is the worker's private key for sealed values on
// an installed host. TINYSHIP_SECRETS_KEY overrides it (the API seals
// with the matching secrets.pub in the same directory).
const DefaultSecretsKey = "/etc/tinyship/keys/secrets.key"

// Handler executes one claimed job. Returning nil marks the job succeeded;
// returning a *RetryableError requeues with backoff; anything else fails the
// job immediately. Handlers must be safe to re-run: the worker
// may retry after a crash or lease loss.
type Handler func(ctx context.Context, job *store.Job) error

// RetryableError marks a failure as worth another attempt (network blips,
// registry 5xx). Validation/policy errors must NOT use it.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable reports whether err should be requeued with backoff.
func Retryable(err error) bool {
	var r *RetryableError
	return errors.As(err, &r)
}

// Config controls the worker loop. Zero values use production defaults.
type Config struct {
	DBPath        string
	WorkerID      string        // lease owner identity; default hostname-pid
	ClaimInterval time.Duration // idle poll period; default 1s
	ReapInterval  time.Duration // default store.ReapEvery
	StatsInterval time.Duration // resource sampling period; default StatsInterval
	Lease         time.Duration // claim lease duration; default store.LeaseDuration
	BackoffBase   time.Duration // retry backoff base; default store.BackoffBase
	Handlers      map[string]Handler
	Logger        *slog.Logger
	// RunDir, when non-empty, makes the worker serve container logs on
	// <RunDir>/worker.sock; Docker must then be set —
	// the log socket is the only read path into the daemon. Empty means
	// no log socket (unit tests).
	RunDir string
	Docker Docker
	// Caddy, when set, makes the worker regenerate the proxy config
	// from the database at startup (crash recovery for route state).
	Caddy Caddy
}

// Run starts the worker and blocks until ctx is cancelled. argv is accepted
// so subcommand flags can be added without touching cmd/tinyship.
func Run(ctx context.Context, argv []string) int {
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "tinyship worker: unexpected arguments %v\n", argv)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := Config{
		DBPath:        envOr("TINYSHIP_DB", DefaultDBPath),
		ClaimInterval: time.Second,
		ReapInterval:  store.ReapEvery,
		Logger:        logger,
	}
	if host, err := os.Hostname(); err == nil {
		cfg.WorkerID = fmt.Sprintf("%s-%d", host, os.Getpid())
	} else {
		cfg.WorkerID = fmt.Sprintf("pid-%d", os.Getpid())
	}

	s, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		logger.Error("worker: cannot open store", "err", err, "db", cfg.DBPath)
		return 1
	}
	defer s.Close()

	// The worker is the only component that talks to Docker, and
	// it refuses to run against an unsupported engine
	// (Docker Engine ≥24.0 / API ≥1.43 — enforced by docker.New/CheckEngine).
	dw, err := docker.New(logger)
	if err != nil {
		logger.Error("worker: docker unavailable", "err", err)
		return 1
	}
	defer dw.Close()
	if err := dw.CheckEngine(ctx); err != nil {
		logger.Error("worker: docker engine check failed", "err", err)
		return 1
	}

	// Route engine: the deployer talks to the Caddy admin socket, and
	// boot regenerates the config from the database so routes always
	// reflect the last succeeded deployments.
	cc := caddy.NewClient(envOr("TINYSHIP_CADDY_ADMIN", caddy.DefaultSocket))
	dplr := NewDeployer(s, dw, logger, cfg.WorkerID)
	dplr.Caddy = cc

	// Sealed secrets need the private key. Absent is fine: public
	// deploys run without it and secret-bearing ones fail at validate
	// with a clear message. Present but unreadable or malformed is not
	// fine: the key is the security boundary, so a broken one stops
	// the worker instead of quietly degrading every deploy.
	keyPath := envOr("TINYSHIP_SECRETS_KEY", DefaultSecretsKey)
	key, err := secrypto.LoadPrivateKey(keyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Info("worker: no private key; deploys with sealed values will fail",
			"path", keyPath)
	case err != nil:
		logger.Error("worker: secret key unusable", "path", keyPath, "err", err)
		return 1
	default:
		dplr.SecretKey = key
		logger.Info("worker: private key loaded", "path", keyPath)
	}

	cfg.Handlers = map[string]Handler{
		DeployJobType:     dplr.Handler(),
		AppDeleteJobType:  dplr.AppDeleteHandler(),
		DomainSyncJobType: dplr.DomainSyncHandler(),
	}
	cfg.Caddy = cc
	// Log socket: /run/tinyship/worker.sock by default;
	// TINYSHIP_RUN_DIR moves the whole run dir for development and tests.
	cfg.RunDir = envOr("TINYSHIP_RUN_DIR", DefaultRunDir)
	cfg.Docker = dw

	if err := RunStore(ctx, s, cfg); err != nil {
		logger.Error("worker: stopped with error", "err", err)
		return 1
	}
	logger.Info("worker: shut down cleanly")
	return 0
}

// RunStore is the worker loop over an already-open store (used by tests and
// by Run). It returns when ctx is cancelled.
func RunStore(ctx context.Context, s *store.Store, cfg Config) error {
	if cfg.ClaimInterval <= 0 {
		cfg.ClaimInterval = time.Second
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = store.ReapEvery
	}
	if cfg.StatsInterval <= 0 {
		cfg.StatsInterval = StatsInterval
	}
	if cfg.Lease <= 0 {
		cfg.Lease = store.LeaseDuration
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = store.BackoffBase
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	logger.Info("worker: starting",
		"id", cfg.WorkerID, "db", cfg.DBPath, "handlers", len(cfg.Handlers))

	// Converge proxy routes to the database before claiming jobs: a
	// crash during a previous route switch must not leave traffic on a
	// half-switched config. Failure is logged and non-fatal: route
	// updates during deploys will surface it again with full context.
	if cfg.Caddy != nil {
		if err := RegenerateRoutes(ctx, s, cfg.Caddy, logger); err != nil {
			logger.Error("worker: boot route regeneration failed", "err", err)
		}
	}

	// Report-only drift check before claiming jobs: containers that
	// should be running but are not, and containers running with no
	// database row behind them. Nothing is remediated here.
	bootReconcile(ctx, s, cfg.Docker, logger)

	// Resource sampling backs both the CLI stats view and the durable
	// OOM history. Sampling needs a daemon; without one the routes that
	// depend on Docker are already unavailable.
	var sampler *StatsSampler
	if cfg.Docker != nil {
		sampler = NewStatsSampler(s, cfg.Docker, logger)
		if _, err := sampler.Sample(ctx); err != nil {
			logger.Warn("worker: initial stats sample failed", "err", err)
		}
	}

	// The read-only log socket: the API reaches container logs only
	// through here, and only after the ownership checks.
	if cfg.RunDir != "" {
		if cfg.Docker == nil {
			return errors.New("worker: RunDir is set but no Docker wrapper was provided")
		}
		ls, err := NewLogServer(s, cfg.Docker, cfg.RunDir, cfg.WorkerID, logger)
		if err != nil {
			return fmt.Errorf("worker: log server: %w", err)
		}
		ls.Stats = sampler
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ls.Serve(ctx); err != nil {
				logger.Error("worker: log server stopped", "err", err)
			}
		}()
		defer func() {
			ls.Close()
			wg.Wait()
		}()
	}

	reap := time.NewTicker(cfg.ReapInterval)
	defer reap.Stop()
	claim := time.NewTicker(cfg.ClaimInterval)
	defer claim.Stop()
	var stats <-chan time.Time
	if sampler != nil {
		tick := time.NewTicker(cfg.StatsInterval)
		defer tick.Stop()
		stats = tick.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-stats:
			if _, err := sampler.Sample(ctx); err != nil {
				// A missed sample only delays the next observation.
				logger.Warn("worker: stats sample failed", "err", err)
			}
		case <-reap.C:
			requeued, failed, err := s.ReapExpired(ctx, time.Now().UTC())
			if err != nil {
				// A failed reap is not fatal; the next tick retries. Losing a
				// reaper tick delays recovery but cannot corrupt state.
				logger.Error("worker: reap failed", "err", err)
				continue
			}
			if len(requeued) > 0 || len(failed) > 0 {
				logger.Warn("worker: reaped expired leases",
					"requeued", len(requeued), "failed", len(failed))
			}
		case <-claim.C:
			for {
				job, err := s.Claim(ctx, cfg.WorkerID, time.Now().UTC(), cfg.Lease)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return nil
					}
					logger.Error("worker: claim failed", "err", err)
					break
				}
				if job == nil {
					break // queue empty; wait for the next tick
				}
				// Execute inline: SQLite gives one writer, and the per-app
				// lock means parallel execution would serialize anyway.
				cfg.execute(ctx, s, logger, job)
			}
		}
	}
}

// execute runs one job with a lease heartbeat and records the outcome.
// The heartbeat stops when the handler returns; if the handler
// overruns the lease and the reaper takes the job back, FinishJob reports
// ErrLeaseLost and the result is dropped — the retry re-runs the step
// (handlers must be idempotent).
func (cfg Config) execute(ctx context.Context, s *store.Store, logger *slog.Logger, job *store.Job) {
	logger = logger.With("job", job.ID, "type", job.Type, "attempt", job.Attempts,
		"app", deref(job.AppID))

	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go func() {
		t := time.NewTicker(store.HeartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case now := <-t.C:
				ok, err := s.Heartbeat(beatCtx, job.ID, cfg.WorkerID, now, cfg.Lease)
				if err != nil {
					logger.Warn("worker: heartbeat error", "err", err)
					continue
				}
				if !ok {
					logger.Warn("worker: lease lost, abandoning job")
					stopBeat()
					return
				}
			}
		}
	}()

	// Cooperative cancellation at handler-defined safe points.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if req, err := s.CancelRequested(runCtx, job.ID); err == nil && req {
					logger.Info("worker: cancel requested, stopping at safe point")
					cancel()
					return
				}
			}
		}
	}()

	start := time.Now()
	handler := cfg.Handlers[job.Type]
	var err error
	if handler == nil {
		// No handler is a permanent failure: retrying cannot conjure one
		err = fmt.Errorf("E_NO_HANDLER: no handler registered for job type %q", job.Type)
	} else {
		err = handler(runCtx, job)
	}
	dur := time.Since(start)

	finishCtx, finishCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer finishCancel()

	switch {
	case err == nil:
		if werr := s.FinishJob(finishCtx, job.ID, cfg.WorkerID, store.JobSucceeded, "", time.Now().UTC()); werr != nil {
			logger.Error("worker: finish failed", "err", werr)
			return
		}
		logger.Info("worker: job succeeded", "duration", dur)
	case errors.Is(err, context.Canceled) && runCtx.Err() != nil && ctx.Err() == nil:
		// The handler stopped because the user cancelled (not because the
		// whole worker is shutting down): running → cancelled.
		if werr := s.FinishJob(finishCtx, job.ID, cfg.WorkerID, store.JobCancelled, "cancelled by user", time.Now().UTC()); werr != nil {
			logger.Error("worker: cancel finish failed", "err", werr)
			return
		}
		logger.Info("worker: job cancelled at safe point", "duration", dur)
	case Retryable(err) && job.Attempts < job.MaxAttempts:
		if werr := s.RequeueJob(finishCtx, job.ID, cfg.WorkerID, err.Error(), job.Attempts, time.Now().UTC(), cfg.BackoffBase); werr != nil {
			logger.Error("worker: requeue failed", "err", werr)
			return
		}
		logger.Warn("worker: job failed (retrying)", "err", err, "duration", dur)
	default:
		if werr := s.FinishJob(finishCtx, job.ID, cfg.WorkerID, store.JobFailed, err.Error(), time.Now().UTC()); werr != nil {
			logger.Error("worker: mark failed", "err", werr)
			return
		}
		logger.Warn("worker: job failed", "err", err, "duration", dur)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
