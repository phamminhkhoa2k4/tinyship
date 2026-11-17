package worker

// The hibernation wake relay: routes of hibernation-enabled apps dial
// 127.0.0.1:<policy.RelayPort> permanently (the mechanism chosen by
// the wake-mechanism spike) and land here. An awake app is proxied
// straight through; the first request to a sleeping app runs the wake —
// two-tier budget admission, container start, a steady health probe
// bounded by the app's configured wake budget — and is only then handed
// to the container. A request that outlives its budget gets 503 with
// Retry-After instead of hanging, and a refused wake says which tier
// refused it. Excluded app types (long-lived WebSocket/SSE, workers
// without an HTTP listener, internal cron) never reach this path: the
// relay only sees ordinary HTTP requests through the proxy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/health"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// Hibernation timing policy: the flap window counts sleep cycles so a
// rapidly restarting app is called out once per burst, and the retry
// hints on 503s stay short — the operator or client retries, not us.
const (
	hibernateFlapWindow    = 15 * time.Minute
	hibernateFlapCycles    = 4
	wakeRetryAfterS        = 30
	jobInFlightRetryAfterS = 5
	// wakeProbeEvery pings between health attempts during a wake; the
	// probe itself is bounded by the config's timeout_s.
	wakeProbeEvery = 500 * time.Millisecond
)

// admissionMu serializes the awake-budget check with the 'waking'
// state write across every waker AND the deploy pipeline's admission
// step: two simultaneous wake-ups (or a wake and a deploy) must never
// both pass a sum that counted the other as still sleeping.
var admissionMu sync.Mutex

// wakeError is a refused or failed wake, mapped straight onto the HTTP
// answer: status, code, message, and the Retry-After hint.
type wakeError struct {
	status      int
	code        string
	message     string
	retryAfterS int
}

func (e *wakeError) Error() string { return e.code + ": " + e.message }

// notify reports whether this refusal deserves an operator
// notification. A deploy that owns the app, an app the operator
// stopped, and a store hiccup are all expected answers to a request
// that arrived at the wrong moment — the client already got a
// Retry-After. Everything else (no container, start failure, wake
// timeout, budget refusal) is a condition the operator must act on.
func (e *wakeError) notify() bool {
	switch e.code {
	case "E_JOB_IN_FLIGHT", "E_APP_STOPPED", "E_INTERNAL":
		return false
	}
	return true
}

// relayApp is the relay's per-app runtime: an in-flight connection
// count (the sleep guard) and the moment of the last proxied request
// (the idle clock, deliberately in-memory so a restart waits one full
// idle window before sleeping anything).
type relayApp struct {
	// mu is held across a wake and across the sleep check→stop→write
	// sequence, so state transitions for one app never interleave.
	// Proxies never hold it: conns is what keeps the sweep away while
	// requests are in flight.
	mu           sync.Mutex
	conns        atomic.Int64
	lastActivity atomic.Int64 // unix ms; zero until first touch
}

// Relay serves wake-or-proxy traffic for hibernating apps.
type Relay struct {
	Store  *store.Store
	Docker Docker
	Logger *slog.Logger
	// Addr overrides the bind address (tests use 127.0.0.1:0); empty
	// means the fixed loopback port the proxy routes dial.
	Addr string
	// MemTotalMB reads the host's total RAM for the ceiling tier;
	// nil means the package default (tests inject).
	MemTotalMB func() (int, error)

	mu   sync.Mutex
	apps map[string]*relayApp
}

// NewRelay wires a relay over the store and the Docker wrapper.
func NewRelay(s *store.Store, d Docker, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.Default()
	}
	return &Relay{Store: s, Docker: d, Logger: log, apps: map[string]*relayApp{}}
}

// memTotalMB reads the host's total RAM, honouring an injected reader.
func (r *Relay) memTotalMB() (int, error) {
	if r.MemTotalMB != nil {
		return r.MemTotalMB()
	}
	return MemTotalMB()
}

// app returns the registry entry for an app, starting its idle clock
// now: an app the relay has never seen has been "active" since this
// moment, so the sweep waits a full idle window before its first
// judgment.
func (r *Relay) app(id string) *relayApp {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ra, ok := r.apps[id]; ok {
		return ra
	}
	ra := &relayApp{}
	ra.lastActivity.Store(time.Now().UnixMilli())
	r.apps[id] = ra
	return ra
}

// touch restarts an app's idle clock, creating the entry when the
// relay has never seen the app. The deploy finalize path uses it: the
// proxy dials the new container directly until the next route-sync
// tick moves the route back onto the relay, and the clock must start
// from that moment so the sweep waits a full idle window (at least
// 60s) — always longer than one sync period (15s) — before it may stop
// a container the route still points at.
func (r *Relay) touch(id string) {
	ra := r.app(id)
	ra.lastActivity.Store(time.Now().UnixMilli())
}

// idleFor reports how long the app has seen no relay traffic.
func (ra *relayApp) idleFor(now time.Time) time.Duration {
	last := ra.lastActivity.Load()
	if last == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(last))
}

// Start binds the relay port synchronously — the caller must learn
// immediately when the wake path does not exist — and serves until ctx
// is done.
func (r *Relay) Start(ctx context.Context) error {
	addr := r.Addr
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", policy.RelayPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("relay: listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: r}
	r.Logger.Info("worker: wake relay listening", "addr", ln.Addr().String())
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.Logger.Error("worker: wake relay stopped", "err", err)
		}
	}()
	return nil
}

// hostnameOf strips the port a client may have put in the Host header.
func hostnameOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// routeFor resolves a Host header to its route row. The row carries
// the app and the current deployment's port — the same join the proxy
// config was built from, so the relay and Caddy can never disagree
// about where an app lives.
func (r *Relay) routeFor(ctx context.Context, hostname string) (*store.RouteRow, error) {
	rows, err := r.Store.ListRouteRows(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Hostname == hostname {
			return &rows[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// ServeHTTP proxies an awake app or wakes a sleeping one first.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	host := hostnameOf(req.Host)
	row, err := r.routeFor(ctx, host)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			relayError(w, http.StatusNotFound, "E_ROUTE_NOT_FOUND",
				fmt.Sprintf("no route for host %q", host), 0)
			return
		}
		relayError(w, http.StatusServiceUnavailable, "E_INTERNAL",
			"relay cannot read routes; retry shortly", jobInFlightRetryAfterS)
		return
	}

	ra := r.app(row.AppID)
	ra.mu.Lock()
	app, err := r.Store.GetAppByID(ctx, row.AppID)
	if err != nil {
		ra.mu.Unlock()
		relayError(w, http.StatusServiceUnavailable, "E_INTERNAL",
			"relay cannot read the app; retry shortly", jobInFlightRetryAfterS)
		return
	}
	if app.Deleting || app.DesiredState != "running" {
		// Deliberately stopped or being deleted: waking it would
		// contradict the operator's intent (the same refusal a direct
		// dial to a stopped container would produce, but explicit).
		ra.mu.Unlock()
		relayError(w, http.StatusBadGateway, "E_APP_STOPPED",
			fmt.Sprintf("app %q is stopped", app.Name), 0)
		return
	}
	cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
	if err != nil {
		ra.mu.Unlock()
		relayError(w, http.StatusBadGateway, "E_APP_CONFIG",
			fmt.Sprintf("app %q has an unreadable config", app.Name), 0)
		return
	}
	ra.lastActivity.Store(time.Now().UnixMilli())
	switch app.HibernateState {
	case store.HibernateSleeping, store.HibernateWaking:
		if wakeErr := r.wakeLocked(ctx, ra, app, cfg); wakeErr != nil {
			ra.mu.Unlock()
			if wakeErr.notify() {
				r.enqueueWakeFailure(ctx, app, wakeErr)
			}
			relayError(w, wakeErr.status, wakeErr.code, wakeErr.message, wakeErr.retryAfterS)
			return
		}
	}
	ra.conns.Add(1)
	ra.mu.Unlock()
	defer ra.conns.Add(-1)

	r.proxy(w, req, row.HostPort)
}

// wakeLocked brings the app up and returns nil only when it is ready
// to serve. The caller holds ra.mu, which serializes this wake against
// sleeps, other requests for the same app, and (via admissionMu) every
// other wake and deploy admission on the host.
func (r *Relay) wakeLocked(ctx context.Context, ra *relayApp, app *store.App, cfg *appconfig.Config) *wakeError {
	log := r.Logger.With("app", app.Name)
	start := time.Now()

	if active, err := r.Store.AppHasActiveJob(ctx, app.ID); err != nil {
		return &wakeError{http.StatusServiceUnavailable, "E_INTERNAL",
			"cannot check app jobs; retry shortly", jobInFlightRetryAfterS}
	} else if active {
		// A deploy, rollback or teardown owns the container right now;
		// it will bring the app up (or take it away) on its own.
		return &wakeError{http.StatusServiceUnavailable, "E_JOB_IN_FLIGHT",
			fmt.Sprintf("a job is running for %q; retry shortly", app.Name),
			jobInFlightRetryAfterS}
	}

	if wakeErr := r.admitWake(ctx, app, cfg); wakeErr != nil {
		return wakeErr
	}

	containerID, hostPort, err := appContainer(ctx, r.Store, app)
	if err != nil {
		r.abortWake(ctx, app, "", "no container to start: "+err.Error())
		return &wakeError{http.StatusServiceUnavailable, "E_WAKE_NO_CONTAINER",
			fmt.Sprintf("app %q has no container to start; deploy it again", app.Name),
			wakeRetryAfterS}
	}

	wakeCtx, cancel := context.WithTimeout(ctx, cfg.HibernateWakeTimeout())
	defer cancel()

	info, err := r.Docker.InspectContainer(wakeCtx, containerID)
	if err != nil {
		r.abortWake(ctx, app, "", "inspect failed: "+err.Error())
		return &wakeError{http.StatusServiceUnavailable, "E_WAKE_START_FAILED",
			fmt.Sprintf("cannot inspect the container of %q: %v", app.Name, err),
			wakeRetryAfterS}
	}
	if info.State != "running" {
		if err := r.Docker.StartContainer(wakeCtx, containerID); err != nil {
			r.abortWake(ctx, app, containerID, "start failed: "+err.Error())
			return &wakeError{http.StatusServiceUnavailable, "E_WAKE_START_FAILED",
				fmt.Sprintf("cannot start %q: %v", app.Name, err), wakeRetryAfterS}
		}
	}

	res := r.waitWake(wakeCtx, containerID, hostPort, cfg)
	if !res.Healthy {
		r.abortWake(ctx, app, containerID, "health: "+res.Reason)
		return &wakeError{http.StatusServiceUnavailable, "E_WAKE_TIMEOUT",
			fmt.Sprintf("%q did not become ready within the %s wake budget: %s",
				app.Name, cfg.HibernateWakeTimeout(), res.Reason), wakeRetryAfterS}
	}

	if err := r.Store.SetHibernateState(ctx, app.ID, store.HibernateAwake); err != nil {
		// The app IS up; a failed write leaves 'waking', which the next
		// request and the boot reconstruction both resolve the same way.
		log.Warn("relay: cannot record awake state", "err", err)
	}
	if err := r.Store.RecordHibernateWake(ctx, app.ID, time.Since(start), time.Now().UTC()); err != nil {
		log.Warn("relay: cannot record wake counters", "err", err)
	}
	log.Info("relay: app woken", "wake_ms", time.Since(start).Milliseconds(),
		"container", containerID)
	return nil
}

// admitWake runs both budget tiers and records 'waking' before the
// lock releases, so the next admission counts this app as awake.
// There is no --force here: a wake that would push either tier over is
// refused with the numbers in the message.
func (r *Relay) admitWake(ctx context.Context, app *store.App, cfg *appconfig.Config) *wakeError {
	admissionMu.Lock()
	defer admissionMu.Unlock()

	running, err := r.Store.ListRunningApps(ctx)
	if err != nil {
		return &wakeError{http.StatusServiceUnavailable, "E_INTERNAL",
			"cannot read the host budget; retry shortly", jobInFlightRetryAfterS}
	}
	settings, err := r.Store.GetBudgetSettings(ctx)
	if err != nil {
		return &wakeError{http.StatusServiceUnavailable, "E_INTERNAL",
			"cannot read the host budget; retry shortly", jobInFlightRetryAfterS}
	}
	memTotal, err := r.memTotalMB()
	if err != nil {
		return &wakeError{http.StatusServiceUnavailable, "E_INTERNAL",
			"cannot read host memory; retry shortly", jobInFlightRetryAfterS}
	}
	budgetMB := settings.BudgetMB
	if budgetMB == 0 {
		budgetMB = AutoBudgetMB(memTotal)
	}
	ceilingMB := CeilingMB(memTotal)

	committed, awake := budgetSums(running)
	// The waker is sleeping: it is inside committed (its grant never
	// went away) but not awake yet, so tier one adds its request.
	need := cfg.Resources.MemoryMB
	overAwake := awake + need - budgetMB
	overCeiling := committed - ceilingMB
	if overAwake > 0 || overCeiling > 0 {
		tier := "awake RAM budget"
		over := overAwake
		if overCeiling > overAwake {
			tier = "committed ceiling"
			over = overCeiling
		}
		return &wakeError{http.StatusServiceUnavailable, "E_WAKE_BUDGET", fmt.Sprintf(
			"waking %q would exceed the host %s: %d MiB awake + %d MiB requested "+
				"(budget %d MiB) and %d MiB committed (ceiling %d MiB); over by %d MiB — "+
				"raise the budget, lower resources.memory_mb, or stop another app",
			app.Name, tier, awake, need, budgetMB, committed, ceilingMB, over),
			wakeRetryAfterS}
	}
	if err := r.Store.SetHibernateState(ctx, app.ID, store.HibernateWaking); err != nil {
		return &wakeError{http.StatusServiceUnavailable, "E_INTERNAL",
			"cannot record the wake; retry shortly", jobInFlightRetryAfterS}
	}
	return nil
}

// abortWake returns the machine to 'sleeping' after a failed wake and
// records the reason. The stop is best effort: a container that never
// started has nothing to stop, and a stop that fails leaves state the
// boot reconstruction re-aligns.
func (r *Relay) abortWake(ctx context.Context, app *store.App, containerID, reason string) {
	if containerID != "" {
		if info, err := r.Docker.InspectContainer(ctx, containerID); err == nil &&
			info.State == "running" {
			grace := time.Duration(appconfig.DefaultStopGraceS) * time.Second
			if cfg, cerr := appconfig.Parse([]byte(app.ConfigJSON)); cerr == nil && cfg.StopGraceS > 0 {
				grace = time.Duration(cfg.StopGraceS) * time.Second
			}
			if err := r.Docker.StopContainer(ctx, containerID, grace); err != nil {
				r.Logger.Warn("relay: failed to stop container after wake failure",
					"app", app.Name, "container", containerID, "err", err)
			}
		}
	}
	if err := r.Store.SetHibernateState(ctx, app.ID, store.HibernateSleeping); err != nil {
		r.Logger.Warn("relay: cannot return to sleeping state",
			"app", app.Name, "err", err)
	}
	r.Logger.Warn("relay: wake failed", "app", app.Name, "reason", reason)
}

// waitWake probes until the app answers or the wake budget closes. The
// config's start period and retry pacing are deploy-time policy; a
// wake probes steadily instead — the held request's deadline is the
// bound and one passing probe is enough to hand it over. A container
// that exits fails immediately instead of waiting out the budget.
func (r *Relay) waitWake(ctx context.Context, containerID string, hostPort int, cfg *appconfig.Config) health.Result {
	target := fmt.Sprintf("127.0.0.1:%d", hostPort)
	var execFn health.ExecFunc
	if cfg.Health != nil && cfg.Health.Type == "exec" {
		execFn = func(c context.Context, command []string, timeout time.Duration) (int, error) {
			return r.Docker.ExecProbe(c, containerID, command, timeout)
		}
	}
	want := HealthConsecutivePasses
	if want < 1 {
		want = 1
	}
	passes, last := 0, health.Result{}
	for {
		if info, err := r.Docker.InspectContainer(ctx, containerID); err == nil &&
			(info.State == "exited" || info.State == "dead") {
			return health.Result{Healthy: false, Reason: fmt.Sprintf(
				"container exited during wake (code %d)", info.ExitCode)}
		}
		if cfg.Health != nil {
			pctx, cancel := context.WithTimeout(ctx, probeSlice(cfg.Health))
			last = health.Probe(pctx, target, cfg.Health, execFn)
			cancel()
			if last.Healthy {
				passes++
				if passes >= want {
					return last
				}
				continue
			}
		}
		passes = 0
		select {
		case <-ctx.Done():
			reason := "wake budget closed"
			if last.Reason != "" {
				reason += "; last: " + last.Reason
			}
			return health.Result{Healthy: false, Reason: reason}
		case <-time.After(wakeProbeEvery):
		}
	}
}

// probeSlice bounds one wake probe: the config's timeout, never more
// than the slice the loop leaves for another attempt.
func probeSlice(h *appconfig.Health) time.Duration {
	d := time.Duration(h.TimeoutS) * time.Second
	if d <= 0 {
		d = 5 * time.Second
	}
	return d
}

// proxy forwards the held request to the app's loopback port. The
// original Host header is preserved (NewSingleHostReverseProxy does
// not rewrite it), so vhost-sensitive apps still see the public name.
func (r *Relay) proxy(w http.ResponseWriter, req *http.Request, hostPort int) {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", hostPort)}
	p := httputil.NewSingleHostReverseProxy(target)
	p.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		r.Logger.Warn("relay: upstream proxy failed", "host", hostnameOf(req.Host), "err", err)
		relayError(w, http.StatusBadGateway, "E_UPSTREAM",
			"the app is not reachable on its loopback port", 0)
	}
	p.ServeHTTP(w, req)
}

// enqueueWakeFailure queues the wake-failure notification; dedup groups
// a burst into one row (the message shows the repeat count).
func (r *Relay) enqueueWakeFailure(ctx context.Context, app *store.App, wakeErr *wakeError) {
	if _, err := r.Store.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
		Type:    "hibernation_wake",
		AppName: app.Name,
		Title:   "App wake failed",
		Detail: map[string]any{
			"reason": wakeErr.message,
			"code":   wakeErr.code,
		},
		DedupKey: "hibernation_wake:" + app.ID,
	}); err != nil {
		r.Logger.Warn("relay: enqueue wake failure notification failed",
			"app", app.Name, "err", err)
	}
}

// relayError writes the API's uniform error envelope with an optional
// Retry-After hint.
func relayError(w http.ResponseWriter, status int, code, message string, retryAfterS int) {
	if retryAfterS > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterS))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
