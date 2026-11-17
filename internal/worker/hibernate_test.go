package worker

// Hibernation tests: the sleep sweep, the wake relay's admission and
// start path, boot reconstruction, and the flap warning. Everything
// runs against the scripted fake Docker; the real-daemon proof lives in
// the end-to-end run.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// hibConfig is a healthy config with hibernation on.
func hibConfig(name string, memMB, idleS, wakeS int) string {
	return fmt.Sprintf(`{
	"name": %q,
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	%s,
	"resources": {"memory_mb": %d, "cpu_millis": 100, "pids_limit": 64},
	"hibernate": {"enabled": true, "idle_timeout_s": %d, "wake_timeout_s": %d}
}`, name, execHealthFast, memMB, idleS, wakeS)
}

// hibFixture is one deployed app behind a relay, with an upstream
// HTTP server standing in for the app container's loopback port.
type hibFixture struct {
	t        *testing.T
	ctx      context.Context
	s        *store.Store
	fk       *fakeDocker
	app      *store.App
	depID    string
	relay    *Relay
	hostname string
	upPort   int
}

// newHibernateFixture stores the app, its finished deployment, an
// active domain, and a fake container for it.
func newHibernateFixture(t *testing.T, cfgJSON string) *hibFixture {
	t.Helper()
	ctx := context.Background()
	s := testStore(t)
	cfg, err := appconfig.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	t.Cleanup(up.Close)
	upURL, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	upPort, err := strconv.Atoi(upURL.Port())
	if err != nil {
		t.Fatalf("upstream port: %v", err)
	}

	app, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: cfg.Name, ConfigJSON: cfgJSON, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	f := &hibFixture{
		t: t, ctx: ctx, s: s, fk: newFakeDocker(),
		app: app, hostname: cfg.Name + ".example.com", upPort: upPort,
	}
	f.deploy(cfgJSON)
	fk := f.fk
	f.relay = NewRelay(s, fk, slog.New(slog.DiscardHandler))
	// A plausible small host: the automatic awake budget from this
	// total is comfortably above the grants these tests use, so a
	// wake is only ever refused when a test asks for it.
	f.relay.MemTotalMB = func() (int, error) { return 4096, nil }
	return f
}

// deploy records a finished deployment whose container listens on the
// fixture's upstream port, and points the app at it.
func (f *hibFixture) deploy(cfgJSON string) {
	f.t.Helper()
	job, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType, PayloadJSON: "{}",
		MaxAttempts: 5, CreatedBy: "test",
	})
	if err != nil {
		f.t.Fatalf("create job: %v", err)
	}
	dep, err := f.s.CreateDeployment(f.ctx, store.CreateDeploymentInput{
		ID: store.NewID(), AppID: f.app.ID, JobID: job.ID, Kind: "deploy",
		ImageRef: "traefik/whoami:v1.8", ConfigSnapshotJSON: cfgJSON,
		ConfigVersion: f.app.ConfigVersion,
	})
	if err != nil {
		f.t.Fatalf("create deployment: %v", err)
	}
	// The job must not read as in flight: the wake path refuses to
	// touch an app a deploy or teardown owns.
	if _, err := f.s.Claim(f.ctx, "test", time.Now().UTC(), time.Minute); err != nil {
		f.t.Fatalf("claim job: %v", err)
	}
	if err := f.s.FinishJob(f.ctx, job.ID, "test", store.JobSucceeded, "",
		time.Now().UTC()); err != nil {
		f.t.Fatalf("finish job: %v", err)
	}
	f.depID = dep.ID
	// A freshly deployed app is up: tests that need a stopped
	// container say so themselves.
	f.addContainer("running")
	f.pointAt(f.upPort, "c-"+f.app.Name)
	dom, _, err := f.s.AddDomain(f.ctx, f.app.ID, f.hostname)
	if err != nil {
		f.t.Fatalf("add domain: %v", err)
	}
	if err := f.s.SetDomainStatus(f.ctx, dom.ID, store.DomainActive); err != nil {
		f.t.Fatalf("activate domain: %v", err)
	}
}

// addContainer registers the app's fake container in the given state.
func (f *hibFixture) addContainer(state string) {
	f.t.Helper()
	if _, err := f.s.DB().Exec(
		`UPDATE deployments SET container_id = ? WHERE id = ?`,
		"c-"+f.app.Name, f.depID); err != nil {
		f.t.Fatalf("set container id: %v", err)
	}
	f.fk.addContainer("c-"+f.app.Name, "ts-"+f.app.Name, state, map[string]string{
		policy.LabelManaged: policy.ManagedValue,
		policy.LabelApp:     f.app.ID,
	})
}

// pointAt aims the deployment at a host port (the fake container's
// loopback listener) and the app at the deployment.
func (f *hibFixture) pointAt(port int, containerID string) {
	f.t.Helper()
	if _, err := f.s.DB().Exec(
		`UPDATE deployments SET host_port = ?, container_id = ? WHERE id = ?`,
		port, containerID, f.depID); err != nil {
		f.t.Fatalf("set deployment port: %v", err)
	}
	if _, err := f.s.DB().Exec(
		`UPDATE apps SET current_deployment_id = ? WHERE id = ?`,
		f.depID, f.app.ID); err != nil {
		f.t.Fatalf("set current deployment: %v", err)
	}
}

// setContainerState moves the fake container between running and
// stopped, the way a deploy or a sweep would.
func (f *hibFixture) setContainerState(state string) {
	f.t.Helper()
	f.fk.mu.Lock()
	defer f.fk.mu.Unlock()
	c := f.fk.containers["c-"+f.app.Name]
	if c == nil {
		f.t.Fatalf("fixture container is missing")
	}
	c.state = state
}

// setState moves the hibernation machine and reloads the fixture app.
func (f *hibFixture) setState(state string) {
	f.t.Helper()
	if err := f.s.SetHibernateState(f.ctx, f.app.ID, state); err != nil {
		f.t.Fatalf("set hibernate state: %v", err)
	}
	f.reload()
}

func (f *hibFixture) reload() {
	f.t.Helper()
	app, err := f.s.GetAppByID(f.ctx, f.app.ID)
	if err != nil {
		f.t.Fatalf("reload app: %v", err)
	}
	f.app = app
}

// backdateIdle pushes the app's idle clock into the past.
func (f *hibFixture) backdateIdle(d time.Duration) {
	f.t.Helper()
	ra := f.relay.app(f.app.ID)
	ra.lastActivity.Store(time.Now().Add(-d).UnixMilli())
}

// request runs one request through the relay as the proxy would.
func (f *hibFixture) request() *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+f.hostname+"/", nil)
	req.Host = f.hostname
	rec := httptest.NewRecorder()
	f.relay.ServeHTTP(rec, req)
	return rec
}

// errorCode reads the uniform error envelope out of a response.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// --- sleep sweep --------------------------------------------------------------

func TestHibernateSweepStopsAnIdleApp(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.backdateIdle(2 * time.Hour)

	hibernateSweep(f.ctx, f.relay, slog.New(slog.DiscardHandler))

	if got := f.fk.stateOf("c-web"); got != "exited" {
		t.Errorf("container state = %q, want exited", got)
	}
	if len(f.fk.stopped) != 1 {
		t.Errorf("stops = %v, want one", f.fk.stopped)
	}
	f.reload()
	if f.app.HibernateState != store.HibernateSleeping {
		t.Errorf("state = %q, want sleeping", f.app.HibernateState)
	}
	st, err := f.s.GetContainerState(f.ctx, f.app.ID)
	if err != nil {
		t.Fatalf("get counters: %v", err)
	}
	if st.HibernateSleepCount != 1 {
		t.Errorf("sleep count = %d, want 1", st.HibernateSleepCount)
	}
}

func TestHibernateSweepWaitsForTheIdleWindow(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	// The relay just saw a request: the window has barely started.
	f.backdateIdle(5 * time.Second)

	hibernateSweep(f.ctx, f.relay, slog.New(slog.DiscardHandler))

	if len(f.fk.stopped) != 0 {
		t.Errorf("stops = %v, want none before the window", f.fk.stopped)
	}
	f.reload()
	if f.app.HibernateState != "" {
		t.Errorf("state = %q, want untouched", f.app.HibernateState)
	}
}

func TestHibernateSweepNeverSleepsAnAppWithAnOpenConnection(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.backdateIdle(2 * time.Hour)
	ra := f.relay.app(f.app.ID)
	ra.conns.Store(1)

	hibernateSweep(f.ctx, f.relay, slog.New(slog.DiscardHandler))

	if len(f.fk.stopped) != 0 {
		t.Errorf("stops = %v, want none while a request is in flight", f.fk.stopped)
	}
}

func TestHibernateSweepSkipsAnAppWithAJobInFlight(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.backdateIdle(2 * time.Hour)
	if _, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType, PayloadJSON: "{}",
		MaxAttempts: 5, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}

	hibernateSweep(f.ctx, f.relay, slog.New(slog.DiscardHandler))

	if len(f.fk.stopped) != 0 {
		t.Errorf("stops = %v, want none while a deploy is queued", f.fk.stopped)
	}
	f.reload()
	if f.app.HibernateState != "" {
		t.Errorf("state = %q, want untouched", f.app.HibernateState)
	}
}

func TestHibernateSweepIgnoresAppsWithoutHibernation(t *testing.T) {
	f := newHibernateFixture(t, memConfig("web", 256))
	f.backdateIdle(2 * time.Hour)

	hibernateSweep(f.ctx, f.relay, slog.New(slog.DiscardHandler))

	if len(f.fk.stopped) != 0 {
		t.Errorf("stops = %v, want none for an app without hibernation", f.fk.stopped)
	}
}

func TestHibernateSweepDoesNothingWithoutARelay(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.backdateIdle(2 * time.Hour)

	// No relay means no wake path: sleeping would strand the app.
	hibernateSweep(f.ctx, nil, slog.New(slog.DiscardHandler))

	if len(f.fk.stopped) != 0 {
		t.Errorf("stops = %v, want none without a wake path", f.fk.stopped)
	}
}

func TestHibernateSweepRetiresTheMachineWhenHibernationIsTurnedOff(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	// The app was asleep, hibernation was turned off, and the next
	// request already brought it back up.
	f.setState(store.HibernateAwake)
	cfg, err := appconfig.Parse([]byte(f.app.ConfigJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	off := false
	cfg.Hibernate.Enabled = &off
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := f.s.UpdateAppConfig(f.ctx, f.app.ID, string(raw)); err != nil {
		t.Fatalf("update config: %v", err)
	}
	f.reload()

	hibernateSweep(f.ctx, f.relay, slog.New(slog.DiscardHandler))

	f.reload()
	if f.app.HibernateState != "" {
		t.Errorf("state = %q, want the machine retired to empty", f.app.HibernateState)
	}
	if len(f.fk.stopped) != 0 {
		t.Errorf("stops = %v, want none", f.fk.stopped)
	}
}

func TestHibernateSweepWarnsOncePerFlapBurst(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	log := slog.New(slog.DiscardHandler)

	// Four sleep/wake cycles inside the window: the fourth must warn.
	for i := 0; i < hibernateFlapCycles; i++ {
		f.setState(store.HibernateAwake)
		f.backdateIdle(2 * time.Hour)
		hibernateSweep(f.ctx, f.relay, log)
	}
	events, err := f.s.ListNotifyEvents(f.ctx, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var abnormal int
	for _, ev := range events {
		if ev.Type == "hibernation_abnormal" {
			abnormal++
		}
	}
	if abnormal != 1 {
		t.Errorf("hibernation_abnormal events = %d, want exactly 1 per burst", abnormal)
	}
}

// --- wake relay ---------------------------------------------------------------

func TestRelayWakesASleepingAppAndHandsOverTheRequest(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	// The sweep stopped the container before it went to sleep.
	f.setContainerState("exited")
	f.setState(store.HibernateSleeping)

	rec := f.request()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %q, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream-ok") {
		t.Errorf("body = %q, want the upstream answer", rec.Body.String())
	}
	if len(f.fk.started) != 1 || f.fk.started[0] != "c-web" {
		t.Errorf("started = %v, want the app container once", f.fk.started)
	}
	f.reload()
	if f.app.HibernateState != store.HibernateAwake {
		t.Errorf("state = %q, want awake", f.app.HibernateState)
	}
	st, err := f.s.GetContainerState(f.ctx, f.app.ID)
	if err != nil {
		t.Fatalf("counters: %v", err)
	}
	if st.HibernateWakeCount != 1 || st.HibernateWakeMaxMs <= 0 {
		t.Errorf("wake counters = %d/%dms, want one timed wake",
			st.HibernateWakeCount, st.HibernateWakeMaxMs)
	}
}

func TestRelayProxiesAnAwakeAppWithoutStartingAnything(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.setState(store.HibernateAwake)
	f.fk.mu.Lock()
	f.fk.containers["c-web"].state = "running"
	f.fk.mu.Unlock()

	rec := f.request()

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream-ok") {
		t.Fatalf("status = %d body %q, want the upstream answer", rec.Code, rec.Body.String())
	}
	if len(f.fk.started) != 0 {
		t.Errorf("started = %v, want no start for an awake app", f.fk.started)
	}
}

func TestRelayRefusesAWakeThatWouldPassTheAwakeBudget(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 512, 60, 30))
	f.setState(store.HibernateSleeping)
	if err := f.s.SetBudgetMB(f.ctx, 64); err != nil {
		t.Fatalf("set budget: %v", err)
	}

	rec := f.request()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != "E_WAKE_BUDGET" {
		t.Errorf("code = %q, want E_WAKE_BUDGET", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a refused wake must carry a Retry-After hint")
	}
	if len(f.fk.started) != 0 {
		t.Errorf("started = %v, want no start for a refused wake", f.fk.started)
	}
	f.reload()
	if f.app.HibernateState != store.HibernateSleeping {
		t.Errorf("state = %q, want still sleeping", f.app.HibernateState)
	}
	// The refusal is the operator's problem to fix, so it is worth
	// one (deduplicated) notification — and no other kind.
	events, err := f.s.ListNotifyEvents(f.ctx, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var wakes int
	for _, ev := range events {
		switch ev.Type {
		case "hibernation_wake":
			wakes++
		default:
			t.Errorf("unexpected notification %q for a budget refusal", ev.Type)
		}
	}
	if wakes != 1 {
		t.Errorf("hibernation_wake events = %d, want 1", wakes)
	}
}

func TestRelayRefusesAWakeWhileADeployOwnsTheApp(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.setState(store.HibernateSleeping)
	if _, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType, PayloadJSON: "{}",
		MaxAttempts: 5, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}

	rec := f.request()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != "E_JOB_IN_FLIGHT" {
		t.Errorf("code = %q, want E_JOB_IN_FLIGHT", code)
	}
	if len(f.fk.started) != 0 {
		t.Errorf("started = %v, want no start while a job runs", f.fk.started)
	}
}

func TestRelayNeverWakesAnAppTheOperatorStopped(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.setState(store.HibernateSleeping)
	if _, err := f.s.DB().Exec(
		`UPDATE apps SET desired_state = 'stopped' WHERE id = ?`, f.app.ID); err != nil {
		t.Fatalf("stop app: %v", err)
	}
	f.reload()

	rec := f.request()

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := errorCode(t, rec); code != "E_APP_STOPPED" {
		t.Errorf("code = %q, want E_APP_STOPPED", code)
	}
	if len(f.fk.started) != 0 {
		t.Errorf("started = %v, want no start for a stopped app", f.fk.started)
	}
}

func TestRelayRefusesAWakeWithNoContainerToStart(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.setState(store.HibernateSleeping)
	if _, err := f.s.DB().Exec(
		`UPDATE deployments SET container_id = NULL WHERE id = ?`, f.depID); err != nil {
		t.Fatalf("clear container: %v", err)
	}

	rec := f.request()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != "E_WAKE_NO_CONTAINER" {
		t.Errorf("code = %q, want E_WAKE_NO_CONTAINER", code)
	}
	f.reload()
	if f.app.HibernateState != store.HibernateSleeping {
		t.Errorf("state = %q, want back to sleeping", f.app.HibernateState)
	}
}

func TestRelayFailedWakeReturnsToSleepingAndNotifies(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.setContainerState("exited")
	f.setState(store.HibernateSleeping)
	f.fk.startErr = fmt.Errorf("daemon said no")

	rec := f.request()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != "E_WAKE_START_FAILED" {
		t.Errorf("code = %q, want E_WAKE_START_FAILED", code)
	}
	f.reload()
	if f.app.HibernateState != store.HibernateSleeping {
		t.Errorf("state = %q, want back to sleeping", f.app.HibernateState)
	}
	events, err := f.s.ListNotifyEvents(f.ctx, 50)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var wakeEvents int
	for _, ev := range events {
		if ev.Type == "hibernation_wake" {
			wakeEvents++
		}
	}
	if wakeEvents != 1 {
		t.Errorf("hibernation_wake events = %d, want 1", wakeEvents)
	}
}

func TestRelayWakeTimesOutWhenTheAppNeverAnswers(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 5))
	f.setContainerState("exited")
	f.setState(store.HibernateSleeping)
	f.fk.execCode = 1 // the health check never passes

	rec := f.request()

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errorCode(t, rec); code != "E_WAKE_TIMEOUT" {
		t.Errorf("code = %q, want E_WAKE_TIMEOUT", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a timed-out wake must carry a Retry-After hint")
	}
	f.reload()
	if f.app.HibernateState != store.HibernateSleeping {
		t.Errorf("state = %q, want back to sleeping", f.app.HibernateState)
	}
	// The half-started container is stopped again: a wake that timed
	// out must not leave RAM held.
	if got := f.fk.stateOf("c-web"); got != "exited" {
		t.Errorf("container state = %q, want exited after the failed wake", got)
	}
}

func TestRelayAnswersAnUnknownHostWithoutTouchingAnything(t *testing.T) {
	f := newHibernateFixture(t, hibConfig("web", 256, 60, 30))
	f.setState(store.HibernateSleeping)

	req := httptest.NewRequest(http.MethodGet, "http://ghost.example.com/", nil)
	req.Host = "ghost.example.com"
	rec := httptest.NewRecorder()
	f.relay.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(f.fk.started) != 0 {
		t.Errorf("started = %v, want nothing started", f.fk.started)
	}
}

// --- boot reconstruction ------------------------------------------------------

func TestReconstructHibernateRealignsAfterACrash(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	fk := newFakeDocker()
	log := slog.New(slog.DiscardHandler)

	cases := []struct {
		name      string
		hibState  string
		container string // "" means no container row at all
		wantState string
		wantStop  bool
	}{
		{"wake finished but the write did not", store.HibernateWaking, "running",
			store.HibernateAwake, false},
		{"crash during the wake", store.HibernateWaking, "exited",
			store.HibernateSleeping, false},
		{"container back under a sleeping app", store.HibernateSleeping, "running",
			store.HibernateSleeping, true},
		{"sweep stopped it and died before writing", store.HibernateAwake, "exited",
			store.HibernateSleeping, false},
		{"deploy drift stays visible", store.HibernateAwake, "",
			store.HibernateAwake, false},
		{"hibernation off is left alone", store.HibernateOff, "running",
			store.HibernateOff, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hibConfig(fmt.Sprintf("app%d", i), 64, 60, 30)
			app, err := s.CreateApp(ctx, store.CreateAppInput{
				Name: fmt.Sprintf("app%d", i), ConfigJSON: cfg, Actor: "test",
			})
			if err != nil {
				t.Fatalf("create app: %v", err)
			}
			if err := s.SetHibernateState(ctx, app.ID, tc.hibState); err != nil {
				t.Fatalf("set state: %v", err)
			}
			if tc.container != "" {
				fk.addContainer("c-"+tc.name, "ts-app"+fmt.Sprint(i), tc.container,
					map[string]string{
						policy.LabelManaged: policy.ManagedValue,
						policy.LabelApp:     app.ID,
					})
			}
		})
	}

	reconstructHibernate(ctx, s, fk, log)

	for i, tc := range cases {
		app, err := s.GetAppByName(ctx, fmt.Sprintf("app%d", i))
		if err != nil {
			t.Fatalf("reload app%d: %v", i, err)
		}
		if app.HibernateState != tc.wantState {
			t.Errorf("%s: state = %q, want %q", tc.name, app.HibernateState, tc.wantState)
		}
	}
	if tc := cases[2]; len(fk.stopped) != 1 || fk.stopped[0] != "c-"+tc.name {
		t.Errorf("stops = %v, want the sleeping app's running container", fk.stopped)
	}
}

// --- deploy ↔ idle clock ------------------------------------------------------

// TestDeployFinalizeRestartsTheIdleClock: after a deploy the proxy
// dials the new container directly until route-sync rewrites the route
// to the relay, and the relay never saw the traffic in between. The
// finalize step must restart the idle clock so the sweep waits out the
// whole idle window — always longer than one sync period — instead of
// stopping a container the route still points at.
func TestDeployFinalizeRestartsTheIdleClock(t *testing.T) {
	f := newDeployFixtureIn(t, testStore(t), hibConfig("web", 128, 60, 30))
	relay := NewRelay(f.s, f.fk, slog.New(slog.DiscardHandler))
	relay.MemTotalMB = func() (int, error) { return 4096, nil }
	f.dplr.Relay = relay

	// The relay's clock predates the deploy by hours. Without the
	// finalize touch the sweep could sleep the fresh container the
	// moment the job finishes, while the route still dials it.
	ra := relay.app(f.app.ID)
	ra.lastActivity.Store(time.Now().Add(-2 * time.Hour).UnixMilli())

	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if idle := ra.idleFor(time.Now()); idle > time.Minute {
		t.Errorf("idle clock after deploy = %v, want a fresh clock", idle)
	}

	// The sweep shares the stats tick; with the fresh clock it must
	// stop nothing even though two hours of idleness were recorded
	// before the deploy ran.
	stops := len(f.fk.stopped)
	hibernateSweep(f.ctx, relay, slog.New(slog.DiscardHandler))
	if got := len(f.fk.stopped); got != stops {
		t.Errorf("sweep stopped %d container(s) right after a deploy, want none",
			got-stops)
	}
}

// TestFinalizeLeavesTheClockAloneWithoutHibernation: hosts without a
// wake path, and apps that never hibernate, must not pay for the touch.
func TestFinalizeLeavesTheClockAloneWithoutHibernation(t *testing.T) {
	f := newDeployFixtureIn(t, testStore(t), memConfig("web", 128))
	relay := NewRelay(f.s, f.fk, slog.New(slog.DiscardHandler))
	relay.MemTotalMB = func() (int, error) { return 4096, nil }
	f.dplr.Relay = relay

	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, ok := relay.apps[f.app.ID]; ok {
		t.Error("a deploy of an app without hibernation must not create a relay clock")
	}

	// Same deploy with no relay wired at all (nil): the pipeline must
	// not care.
	f2 := newDeployFixtureIn(t, testStore(t), memConfig("api", 128))
	f2.dplr.Relay = nil
	if err := f2.run(1, 5); err != nil {
		t.Fatalf("deploy without relay: %v", err)
	}
}

// --- deploy ↔ state machine ----------------------------------------------------

// TestDeployBringsASleepingAppBackAwake: a succeeded deploy leaves a
// running container. A stale 'sleeping' would make the sweep skip the
// fresh app forever and boot reconstruction stop the new container on
// the next restart, so finalize must move both in-flight positions back
// to 'awake' in the same transaction.
func TestDeployBringsASleepingAppBackAwake(t *testing.T) {
	for _, from := range []string{store.HibernateSleeping, store.HibernateWaking} {
		t.Run("from="+from, func(t *testing.T) {
			f := newDeployFixtureIn(t, testStore(t), hibConfig("web", 128, 60, 30))
			if err := f.s.SetHibernateState(f.ctx, f.app.ID, from); err != nil {
				t.Fatalf("set state: %v", err)
			}
			if err := f.run(1, 5); err != nil {
				t.Fatalf("deploy: %v", err)
			}
			app, err := f.s.GetAppByID(f.ctx, f.app.ID)
			if err != nil {
				t.Fatalf("read app: %v", err)
			}
			if app.HibernateState != store.HibernateAwake {
				t.Errorf("state after deploy = %q, want awake", app.HibernateState)
			}
		})
	}
}

// TestDeployKeepsAnUntouchedHibernateState: finalize only moves apps
// that actually slept; a machine already retired (or never armed) must
// not be promoted by a deploy.
func TestDeployKeepsAnUntouchedHibernateState(t *testing.T) {
	f := newDeployFixtureIn(t, testStore(t), hibConfig("web", 128, 60, 30))
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	app, err := f.s.GetAppByID(f.ctx, f.app.ID)
	if err != nil {
		t.Fatalf("read app: %v", err)
	}
	if app.HibernateState != store.HibernateOff {
		t.Errorf("state after deploy = %q, want untouched empty", app.HibernateState)
	}
}
