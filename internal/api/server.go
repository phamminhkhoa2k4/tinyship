// Package api serves the local control API over 127.0.0.1:7878 and
// /run/tinyship/api.sock. The API reads/writes the SQLite store
// and proxies log reads to the worker's read-only socket; it never
// touches Docker or the Caddy admin socket.
//
// Routes:
//
//	GET /healthz
//	GET /stats live app resource view via the worker
//	POST /apps create an app from a strict config object
//	GET /apps list apps
//	GET /apps/{name} show app + current deployment
//	POST /apps/{name}/deploy queue a deploy job (202)
//	GET /apps/{name}/deployments deployment history
//	GET /deployments/{id} one deployment with its timeline
//	DELETE /apps/{name} queue app teardown (202)
//	DELETE /apps/{name}/domains/{hostname} detach a hostname (202)
//	GET /apps/{name}/logs stream container logs via the worker
//	POST /apps/{name}/domains attach a hostname (DNS-checked)
//	GET /apps/{name}/domains list an app's domains
//	GET /apps/{name}/env list variables (secrets as names only)
//	PUT /apps/{name}/env/{key} set or replace a variable
//	DELETE /apps/{name}/env/{key} remove a variable
//	POST /credentials create or rotate a registry login (sealed)
//	GET /credentials list registry logins (no tokens)
//	DELETE /credentials/{name} remove an unreferenced login
//
// Every route except GET /healthz requires an admin bearer token
// (see auth.go); the CLI reads its token from a system file.
// The loopback bind plus filesystem permissions on the Unix socket
// complement, not replace, that authentication.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/version"
	"github.com/tinyship/tinyship/internal/worker"
)

// DefaultTCPAddr is the loopback-only listen address. The API and
// dashboard must not be Internet-reachable by default; serving on a
// non-loopback address requires the explicit --allow-remote opt-in
const DefaultTCPAddr = "127.0.0.1:7878"

// DefaultRunDir mirrors the worker's run dir; TINYSHIP_RUN_DIR
// moves it for development and tests.
const DefaultRunDir = "/run/tinyship"

// APISockName is the API's Unix socket inside the run dir.
const APISockName = "api.sock"

// maxConfigBody bounds request bodies before parsing; the config itself is
// capped at appconfig.MaxConfigBytes.
const maxConfigBody = appconfig.MaxConfigBytes + 4096

// isLoopbackAddr reports whether addr targets only the loopback
// interface. A wildcard host (":7878", "0.0.0.0", "[::]") is not
// loopback: it would expose the API to the network.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Run starts the API server and blocks until ctx is cancelled or the server
// fails. argv carries the subcommand flags (--listen, --allow-remote).
func Run(ctx context.Context, argv []string) int {
	fs := flag.NewFlagSet("tinyship api", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", DefaultTCPAddr,
		"TCP address to serve on (loopback only unless --allow-remote is given)")
	allowRemote := fs.Bool("allow-remote", false,
		"permit serving on a non-loopback address")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "tinyship api: unexpected arguments %v\n", fs.Args())
		return 2
	}
	if !isLoopbackAddr(*listen) && !*allowRemote {
		fmt.Fprintf(os.Stderr, "tinyship api: refusing to listen on %s: it is not a loopback address; pass --allow-remote if you intend to expose the API and dashboard to the network\n", *listen)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	dbPath := envOr("TINYSHIP_DB", worker.DefaultDBPath)
	runDir := envOr("TINYSHIP_RUN_DIR", DefaultRunDir)

	s, err := store.Open(ctx, dbPath)
	if err != nil {
		logger.Error("api: cannot open store", "err", err, "db", dbPath)
		return 1
	}
	defer s.Close()

	handler := newMuxWith(s, runDir, logger, func(ctx context.Context, hostname string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	}, s, *listen)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lnTCP, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("api: cannot listen", "addr", *listen, "err", err)
		return 1
	}
	var lnUnix net.Listener
	if sock, err := listenUnix(runDir); err != nil {
		// A missing run dir only breaks the socket transport; the TCP
		// listener still serves the CLI and dashboards.
		logger.Warn("api: unix socket unavailable, continuing on TCP only",
			"err", err)
	} else {
		lnUnix = sock
	}

	srv := &http.Server{Handler: handler}
	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(lnTCP) }()
	if lnUnix != nil {
		go func() { errs <- srv.Serve(lnUnix) }()
	}
	logger.Info("api: listening", "tcp", *listen, "unix", unixPath(lnUnix))
	if !isLoopbackAddr(*listen) {
		logger.Warn("api: serving on a non-loopback address; the API and dashboard are reachable from the network, keep the admin token safe",
			"addr", *listen)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
		if lnUnix != nil {
			_ = os.Remove(lnUnix.Addr().String())
		}
		logger.Info("api: shut down cleanly")
		return 0
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("api: server failed", "err", err)
			return 1
		}
		return 0
	}
}

func unixPath(ln net.Listener) string {
	if ln == nil {
		return ""
	}
	return ln.Addr().String()
}

// listenUnix binds <runDir>/api.sock, replacing a stale file from a dead
// process (the same single-instance rule as the worker socket).
func listenUnix(runDir string) (net.Listener, error) {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return nil, err
	}
	sock := filepath.Join(runDir, APISockName)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- server plumbing --------------------------------------------------------

// server holds everything route handlers need.
type server struct {
	store  *store.Store
	logger *slog.Logger
	// runDir locates the worker's read-only log socket.
	runDir string
	// tcpAddr is the TCP listen address, reported by /healthz so the
	// CLI can warn when it is not loopback.
	tcpAddr string
	// workerClient speaks HTTP over that socket; tests inject their own.
	workerClient *http.Client
	// resolve performs DNS lookups for the domain attach check; tests
	// inject their own.
	resolve Resolver
	// pubKey is the sealing public key, loaded once on first secret
	// write and kept for the process lifetime.
	pubKey     []byte
	pubKeyPath string
	// auth, when set, enables bearer-token authentication on every route
	// except the process-health check. nil disables it (tests, dev paths).
	auth tokenVerifier
	// limiter throttles bad-token attempts when auth is enabled.
	limiter *authLimiter
}

// newMux builds the route table. Exported behavior for tests via httptest.
// It keeps authentication disabled so handler unit tests do not each need a
// token; the production run path uses newMuxWith with the store as auther.
func newMux(s *store.Store, runDir string, logger *slog.Logger) http.Handler {
	return newMuxWith(s, runDir, logger, func(ctx context.Context, hostname string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", hostname)
	}, nil, DefaultTCPAddr)
}

// newMuxWith is newMux with an explicit DNS resolver (so domain-attach
// tests script resolution outcomes), an explicit authenticator (nil
// disables auth, as in handler unit tests), and the TCP address the
// server reports as its listen target. Production passes the store.
func newMuxWith(s *store.Store, runDir string, logger *slog.Logger, resolve Resolver, auth tokenVerifier, tcpAddr string) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	srv := &server{
		store:   s,
		logger:  logger,
		runDir:  runDir,
		tcpAddr: tcpAddr,
		resolve: resolve,
		auth:    auth,
		limiter: newAuthLimiter(),
		pubKeyPath: envOr("TINYSHIP_SECRETS_PUB",
			filepath.Join("/etc/tinyship/keys", "secrets.pub")),
		workerClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix",
						filepath.Join(runDir, worker.LogSockName))
				},
			},
			// No global timeout: log follow streams are long-lived; each
			// handler bounds its own reads.
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.handleHealthz)
	mux.HandleFunc("GET /stats", srv.handleStats)
	mux.HandleFunc("GET /budget", srv.handleBudget)
	mux.HandleFunc("POST /budget", srv.handleSetBudget)
	mux.HandleFunc("POST /apps", srv.handleCreateApp)
	mux.HandleFunc("GET /apps", srv.handleListApps)
	mux.HandleFunc("GET /apps/{name}", srv.handleShowApp)
	mux.HandleFunc("POST /apps/{name}/deploy", srv.handleDeploy)
	mux.HandleFunc("POST /apps/{name}/rollback", srv.handleRollback)
	mux.HandleFunc("GET /apps/{name}/deployments", srv.handleListDeployments)
	mux.HandleFunc("GET /apps/{name}/logs", srv.handleLogs)
	mux.HandleFunc("POST /apps/{name}/domains", srv.handleAddDomain)
	mux.HandleFunc("GET /apps/{name}/domains", srv.handleListDomains)
	mux.HandleFunc("GET /apps/{name}/env", srv.handleListEnv)
	mux.HandleFunc("PUT /apps/{name}/env/{key}", srv.handleSetEnv)
	mux.HandleFunc("DELETE /apps/{name}/env/{key}", srv.handleUnsetEnv)
	mux.HandleFunc("POST /credentials", srv.handleCreateCredential)
	mux.HandleFunc("GET /credentials", srv.handleListCredentials)
	mux.HandleFunc("DELETE /credentials/{name}", srv.handleDeleteCredential)
	mux.HandleFunc("GET /deployments/{id}", srv.handleShowDeployment)
	mux.HandleFunc("DELETE /apps/{name}", srv.handleDeleteApp)
	mux.HandleFunc("DELETE /apps/{name}/domains/{hostname}", srv.handleDeleteDomain)
	if auth != nil {
		return srv.authMiddleware(mux)
	}
	return mux
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    version.Version,
		"commit":     version.Commit,
		"time":       time.Now().UTC().Format(time.RFC3339),
		"tcp_listen": s.tcpAddr,
	})
}

// --- JSON helpers ------------------------------------------------------------

// errorBody is the uniform error envelope.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// readConfig parses the strict app config from a bounded request body
func readConfig(r *http.Request) (*appconfig.Config, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxConfigBody))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	return appconfig.Parse(body)
}

// --- DTOs --------------------------------------------------------------------

type appDTO struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	DesiredState        string          `json:"desired_state"`
	Image               string          `json:"image,omitempty"`
	Tag                 string          `json:"tag,omitempty"`
	Port                int             `json:"port,omitempty"`
	Config              json.RawMessage `json:"config,omitempty"`
	CurrentDeploymentID *string         `json:"current_deployment_id,omitempty"`
	Warnings            []string        `json:"warnings,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

func appToDTO(a *store.App, withConfig bool, warnings []string) appDTO {
	d := appDTO{
		ID: a.ID, Name: a.Name, DesiredState: a.DesiredState,
		CurrentDeploymentID: a.CurrentDeploymentID,
		Warnings:            warnings,
		CreatedAt:           a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
	// The stored config is validated at creation; re-parsing here only
	// extracts display fields and can never fail closed on the API.
	var cfg appconfig.Config
	if err := json.Unmarshal([]byte(a.ConfigJSON), &cfg); err == nil {
		d.Image, d.Tag, d.Port = cfg.Image, cfg.Tag, cfg.Port
	}
	if withConfig {
		d.Config = json.RawMessage(a.ConfigJSON)
	}
	return d
}

type deploymentDTO struct {
	ID               string     `json:"id"`
	AppID            string     `json:"app_id"`
	Seq              int        `json:"seq"`
	Kind             string     `json:"kind"`
	State            string     `json:"state"`
	ImageRef         string     `json:"image_ref"`
	ImageDigest      *string    `json:"image_digest,omitempty"`
	Strategy         *string    `json:"strategy,omitempty"`
	HostPort         *int       `json:"host_port,omitempty"`
	ContainerID      *string    `json:"container_id,omitempty"`
	ContainerName    *string    `json:"container_name,omitempty"`
	FailureStep      *string    `json:"failure_step,omitempty"`
	FailureCode      *string    `json:"failure_code,omitempty"`
	FailureMessage   *string    `json:"failure_message,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	PreviousDeployID *string    `json:"previous_deployment_id,omitempty"`
	RollbackOfID     *string    `json:"rollback_of,omitempty"`
	Events           []eventDTO `json:"events,omitempty"`
}

type eventDTO struct {
	Step   string          `json:"step"`
	Status string          `json:"status"`
	TS     time.Time       `json:"ts"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

func depToDTO(d *store.Deployment) deploymentDTO {
	return deploymentDTO{
		ID: d.ID, AppID: d.AppID, Seq: d.Seq, Kind: d.Kind, State: d.State,
		ImageRef: d.ImageRef, ImageDigest: d.ImageDigest,
		Strategy: d.StrategyEffective, HostPort: d.HostPort,
		ContainerID: d.ContainerID, ContainerName: d.ContainerName,
		FailureStep: d.FailureStep, FailureCode: d.FailureCode,
		FailureMessage: d.FailureMessage,
		CreatedAt:      d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
		PreviousDeployID: d.PreviousDeploymentID,
		RollbackOfID:     d.RollbackOfID,
	}
}

func eventsToDTO(events []*store.DeploymentEvent) []eventDTO {
	out := make([]eventDTO, 0, len(events))
	for _, e := range events {
		out = append(out, eventDTO{
			Step: e.Step, Status: e.Status, TS: e.TS, Detail: e.Detail,
		})
	}
	return out
}

// --- app routes ---------------------------------------------------------------

// handleCreateApp validates the config strictly and creates the app.
// Warnings (W_MUTABLE_TAG, ...) are advisory and returned with the
// created app.
func (s *server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	cfg, err := readConfig(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	warnings, err := appconfig.Validate(cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	// The config must be storable verbatim for later re-validation.
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	app, err := s.store.CreateApp(r.Context(), store.CreateAppInput{
		Name: cfg.Name, ConfigJSON: string(raw), Actor: actorName(r),
	})
	if err != nil {
		if errors.Is(err, store.ErrAppNameExists) {
			writeError(w, http.StatusConflict, "E_VALIDATION",
				fmt.Sprintf("app %q already exists; names are immutable", cfg.Name))
			return
		}
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, appToDTO(app, true, warnings))
}

func (s *server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	out := make([]appDTO, 0, len(apps))
	for _, a := range apps {
		out = append(out, appToDTO(a, false, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *server) handleShowApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	body := map[string]any{"app": appToDTO(app, true, nil)}
	if app.CurrentDeploymentID != nil {
		if dep, err := s.store.GetDeployment(r.Context(), *app.CurrentDeploymentID); err == nil {
			body["current_deployment"] = depToDTO(dep)
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// --- deploy route --------------------------------------------------------------

// handleDeploy queues a deploy job: one pending deployment plus its
// job, referencing each other. The worker revalidates everything.
func (s *server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND",
			fmt.Sprintf("no such app %q; create it with `tinyship app create`", name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	// Fast feedback: the same validation the worker will perform. The
	// worker still revalidates from the DB.
	cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
	if err != nil {
		writeError(w, http.StatusConflict, "E_VALIDATION",
			fmt.Sprintf("stored config for %q is invalid: %v", name, err))
		return
	}
	if _, err := appconfig.Validate(cfg); err != nil {
		writeError(w, http.StatusConflict, "E_VALIDATION", err.Error())
		return
	}
	// Optional overrides; an empty body keeps the defaults.
	body, err := readDeployBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}

	depID := store.NewID()
	payload := fmt.Sprintf(`{"deployment_id":%q}`, depID)
	if body.Force {
		payload = fmt.Sprintf(`{"deployment_id":%q,"force":true}`, depID)
	}
	// One transaction: the worker claims jobs in a poll loop, and a
	// job visible before its deployment row exists fails the pipeline
	// at its first lookup.
	dep, job, err := s.store.CreateDeploymentJob(r.Context(),
		store.CreateDeploymentInput{
			ID:                   depID,
			AppID:                app.ID,
			Kind:                 "deploy",
			ImageRef:             imageRef(cfg),
			ConfigSnapshotJSON:   app.ConfigJSON,
			ConfigVersion:        app.ConfigVersion,
			PreviousDeploymentID: app.CurrentDeploymentID,
		},
		store.CreateJobInput{
			AppID:       &app.ID,
			Type:        worker.DeployJobType,
			PayloadJSON: payload,
			CreatedBy:   "api",
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"deployment_id": dep.ID,
		"job_id":        job.ID,
		"seq":           dep.Seq,
		"state":         dep.State,
		"forced":        body.Force,
	})
}

// imageRef derives the pull reference exactly like the pipeline's validate
// step (digest wins over tag; absent tag means :latest).
func imageRef(cfg *appconfig.Config) string {
	switch {
	case cfg.Digest != "":
		return cfg.Image + "@" + cfg.Digest
	case cfg.Tag != "":
		return cfg.Image + ":" + cfg.Tag
	default:
		return cfg.Image
	}
}

// --- deployment routes -----------------------------------------------------------

func (s *server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	app, err := s.store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	deps, err := s.store.ListDeployments(r.Context(), app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	out := make([]deploymentDTO, 0, len(deps))
	for _, d := range deps {
		out = append(out, depToDTO(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": out})
}

// handleShowDeployment returns one deployment plus its full timeline
func (s *server) handleShowDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.store.GetDeployment(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such deployment")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	events, err := s.store.ListDeploymentEvents(r.Context(), dep.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	dto := depToDTO(dep)
	dto.Events = eventsToDTO(events)
	writeJSON(w, http.StatusOK, dto)
}

// --- stats route -----------------------------------------------------

// handleStats proxies the worker's resource view. The API holds no Docker
// access, so the sample is taken by the worker and copied through.
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.proxyWorker(w, r, http.MethodGet, "http://worker/stats", nil)
}

// proxyWorker forwards one request to the worker socket and copies the
// response back. The socket is the only API→worker channel.
func (s *server) proxyWorker(w http.ResponseWriter, r *http.Request, method, target string, body io.Reader) {
	req, err := http.NewRequestWithContext(r.Context(), method, target, body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	resp, err := s.workerClient.Do(req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "E_WORKER_UNAVAILABLE",
			fmt.Sprintf("worker socket unavailable at %s (is the worker running?): %v",
				filepath.Join(s.runDir, worker.LogSockName), err))
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- logs route ------------------------------------------------------

// handleLogs proxies the read to the worker's Unix socket. The API holds no
// Docker access; the worker performs the ownership checks and
// audits refusals.
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if app.CurrentDeploymentID == nil {
		writeError(w, http.StatusConflict, "E_NO_DEPLOYMENT",
			fmt.Sprintf("app %q has no deployed container yet; run `tinyship deploy %s` and let it succeed", name, name))
		return
	}
	dep, err := s.store.GetDeployment(r.Context(), *app.CurrentDeploymentID)
	if err != nil || dep.ContainerID == nil {
		writeError(w, http.StatusConflict, "E_NO_DEPLOYMENT",
			fmt.Sprintf("app %q has no container to read logs from", name))
		return
	}

	q := r.URL.Query()
	proxy := urlValues(q)
	proxy.Set("app_id", app.ID)
	proxy.Set("container_id", *dep.ContainerID)

	workerURL := "http://worker/logs?" + proxy.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, workerURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	resp, err := s.workerClient.Do(req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "E_WORKER_UNAVAILABLE",
			fmt.Sprintf("worker log socket unavailable at %s (is the worker running?): %v",
				filepath.Join(s.runDir, worker.LogSockName), err))
		return
	}
	defer resp.Body.Close()

	// Copy status/headers/body through; Flush per chunk so --follow works.
	for _, h := range []string{"Content-Type"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return // client hung up (Ctrl-C during follow)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// urlValues copies only the documented log parameters for the worker proxy.
func urlValues(q map[string][]string) url.Values {
	out := url.Values{}
	for _, key := range []string{"tail", "since", "follow"} {
		if v, ok := q[key]; ok && len(v) > 0 {
			out[key] = v
		}
	}
	return out
}
