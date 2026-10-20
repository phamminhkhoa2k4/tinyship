package worker

// Deploy pipeline tests. A scripted fake
// Docker stands in for the allowlisted wrapper so every failure kind
// AC3 demands — pull error, immediate exit, health timeout — runs without a
// daemon; the real-daemon proof lives in the end-to-end run.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/secrypto"
	"github.com/tinyship/tinyship/internal/store"
)

// --- fake Docker ------------------------------------------------------------

type fakeContainer struct {
	id     string
	name   string
	labels map[string]string
	state  string
	exit   int
	oom    bool
}

type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	byName     map[string]string
	nextID     int

	pulls     int
	creates   int
	networks  int
	volumes   []string
	removed   []string
	started   []string
	stopped   []string
	stopGrace time.Duration

	lastPullRef string
	// lastPullAuth is the credential the pipeline handed the daemon;
	// tests assert it (and that no token ever reaches logs).
	lastPullAuth *docker.Auth

	logTail   int // last ContainerLogs arguments, for assertions
	logSince  time.Duration
	logFollow bool

	statMemBytes uint64
	statMemLimit uint64
	statCPUPct   float64
	statErr      error

	images    map[string]int64 // image ref -> size in bytes
	removedIm []string

	pullErr    error
	digest     string
	createErr  error
	startErr   error
	removeErr  error
	afterStart func(c *fakeContainer) // simulate a crashing entrypoint
	logs       string
	execCode   int
	execErr    error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers: map[string]*fakeContainer{},
		byName:     map[string]string{},
		digest:     "sha256:" + strings.Repeat("ab", 32),
		logs:       "boot line\nready to serve\n",
		images:     map[string]int64{},
	}
}

func (f *fakeDocker) addContainer(id, name, state string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = &fakeContainer{id: id, name: name, state: state, labels: labels}
	f.byName[name] = id
}

func (f *fakeDocker) lookup(key string) *fakeContainer {
	if c, ok := f.containers[key]; ok {
		return c
	}
	if id, ok := f.byName[key]; ok {
		return f.containers[id]
	}
	return nil
}

func (f *fakeDocker) PullImage(_ context.Context, ref string, auth *docker.Auth) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	f.lastPullRef = ref
	f.lastPullAuth = auth
	if f.pullErr != nil {
		return "", f.pullErr
	}
	return f.digest, nil
}

func (f *fakeDocker) EnsureNetwork(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks++
	return nil
}

func (f *fakeDocker) EnsureVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes = append(f.volumes, name)
	return nil
}

func (f *fakeDocker) CreateContainer(_ context.Context, spec *policy.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.lookup(spec.Name) != nil {
		// Real Docker refuses duplicate names; the pipeline pre-checks, so
		// reaching here means the idempotency logic regressed.
		return "", fmt.Errorf("conflict: container name %s already in use", spec.Name)
	}
	f.nextID++
	id := fmt.Sprintf("cid-%03d", f.nextID)
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	f.containers[id] = &fakeContainer{id: id, name: spec.Name, state: "created", labels: labels}
	f.byName[spec.Name] = id
	f.creates++
	return id, nil
}

func (f *fakeDocker) StartContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	c := f.lookup(id)
	if c == nil {
		return fmt.Errorf("Error response from daemon: No such container: %s", id)
	}
	c.state = "running"
	f.started = append(f.started, id)
	if f.afterStart != nil {
		f.afterStart(c)
	}
	return nil
}

func (f *fakeDocker) StopContainer(_ context.Context, id string, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.lookup(id)
	if c == nil {
		return fmt.Errorf("Error response from daemon: No such container: %s", id)
	}
	c.state = "exited"
	f.stopped = append(f.stopped, id)
	f.stopGrace = grace
	return nil
}

func (f *fakeDocker) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	c := f.lookup(id)
	if c == nil {
		return fmt.Errorf("Error response from daemon: No such container: %s", id)
	}
	delete(f.containers, c.id)
	if f.byName[c.name] == c.id {
		delete(f.byName, c.name)
	}
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeDocker) InspectContainer(_ context.Context, id string) (*docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.lookup(id)
	if c == nil {
		return nil, fmt.Errorf("Error response from daemon: No such container: %s", id)
	}
	labels := map[string]string{}
	for k, v := range c.labels {
		labels[k] = v
	}
	return &docker.ContainerInfo{ID: c.id, Name: c.name, State: c.state, ExitCode: c.exit,
		OOMKilled: c.oom, Labels: labels}, nil
}

func (f *fakeDocker) Stats(context.Context, string) (*docker.ContainerStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statErr != nil {
		return nil, f.statErr
	}
	return &docker.ContainerStats{
		MemUsageBytes: f.statMemBytes,
		MemLimitBytes: f.statMemLimit,
		CPUPercent:    f.statCPUPct,
	}, nil
}

func (f *fakeDocker) InspectImage(_ context.Context, ref string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	size, ok := f.images[ref]
	if !ok {
		return "", 0, fmt.Errorf("Error response from daemon: No such image: %s", ref)
	}
	return "", size, nil
}

func (f *fakeDocker) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[ref]; !ok {
		return fmt.Errorf("Error response from daemon: No such image: %s", ref)
	}
	delete(f.images, ref)
	f.removedIm = append(f.removedIm, ref)
	return nil
}

func (f *fakeDocker) ContainerLogs(_ context.Context, _ string, tail int, since time.Duration, follow bool) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logTail, f.logSince, f.logFollow = tail, since, follow
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func (f *fakeDocker) ExecProbe(_ context.Context, _ string, _ []string, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execCode, f.execErr
}

func (f *fakeDocker) ListManagedContainers(context.Context) ([]docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.ContainerInfo
	for _, c := range f.containers {
		if c.labels[policy.LabelManaged] != policy.ManagedValue {
			continue
		}
		labels := map[string]string{}
		for k, v := range c.labels {
			labels[k] = v
		}
		out = append(out, docker.ContainerInfo{
			ID: c.id, Name: c.name, State: c.state, ExitCode: c.exit, Labels: labels,
		})
	}
	return out, nil
}

func (f *fakeDocker) containerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

func (f *fakeDocker) stateOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.lookup(id); c != nil {
		return c.state
	}
	return "gone"
}

// --- fixture ----------------------------------------------------------------

const execHealthFast = `"health": {"type": "exec", "command": ["/bin/true"],
	"interval_s": 2, "timeout_s": 1, "retries": 2, "start_period_s": 0}`

const happyConfig = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	` + execHealthFast + `,
	"resources": {"memory_mb": 64, "cpu_millis": 100, "pids_limit": 64}
}`

type deployFixture struct {
	t      *testing.T
	ctx    context.Context
	s      *store.Store
	fk     *fakeDocker
	app    *store.App
	depID  string
	job    *store.Job
	depRec *store.Deployment
	dplr   *Deployer
}

func newDeployFixture(t *testing.T, cfgJSON string) *deployFixture {
	t.Helper()
	return newDeployFixtureIn(t, testStore(t), cfgJSON)
}

// newDeployFixtureIn adds one app+deployment on an existing store, so a
// scenario can pin several apps against one shared budget.
func newDeployFixtureIn(t *testing.T, s *store.Store, cfgJSON string) *deployFixture {
	t.Helper()
	ctx := context.Background()
	cfg, err := appconfig.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	app, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: cfg.Name, ConfigJSON: cfgJSON, Actor: "cli",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	depID := store.NewID()
	job, err := s.CreateJob(ctx, store.CreateJobInput{
		AppID: &app.ID, Type: DeployJobType,
		PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, depID),
		MaxAttempts: 5, CreatedBy: "cli",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	depRec, err := s.CreateDeployment(ctx, store.CreateDeploymentInput{
		ID: depID, AppID: app.ID, JobID: job.ID, Kind: "deploy",
		ImageRef:           cfg.Image + ":v1.8",
		ConfigSnapshotJSON: cfgJSON,
		ConfigVersion:      app.ConfigVersion,
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	fk := newFakeDocker()
	dplr := NewDeployer(s, fk, slog.New(slog.DiscardHandler), "test-worker")
	pinHostFacts(dplr)
	return &deployFixture{
		t: t, ctx: ctx, s: s, fk: fk, app: app,
		depID: depID, job: job, depRec: depRec,
		dplr: dplr,
	}
}

// pinHostFacts gives a deployer a generous fake host so admission never
// depends on the machine running the tests. Scenario tests re-pin the
// fields with tight values.
func pinHostFacts(p *Deployer) {
	p.MemTotalMB = func() (int, error) { return 8192, nil }
	p.DiskFreeMB = func(string) (int, error) { return 100 * 1024, nil }
	p.NumCPUs = func() int { return 4 }
}

// run executes the handler with the fixture's attempt counters. The
// consecutive-passes setting is 1 here purely for pacing; the value 2
// is asserted by TestNewDeployerDefaultsToTwoConsecutivePasses and the
// convergence logic itself is covered in internal/health.
func (f *deployFixture) run(attempt, maxAttempts int) error {
	f.t.Helper()
	f.dplr.HealthConsecutive = 1
	j := *f.job
	j.Attempts = attempt
	j.MaxAttempts = maxAttempts
	return f.dplr.Handler()(f.ctx, &j)
}

func (f *deployFixture) deployment() *store.Deployment {
	f.t.Helper()
	d, err := f.s.GetDeployment(f.ctx, f.depID)
	if err != nil {
		f.t.Fatalf("get deployment: %v", err)
	}
	return d
}

// timeline returns "step:status" for every non-start event.
func (f *deployFixture) timeline() []string {
	f.t.Helper()
	events, err := f.s.ListDeploymentEvents(f.ctx, f.depID)
	if err != nil {
		f.t.Fatalf("list events: %v", err)
	}
	var out []string
	for _, ev := range events {
		if ev.Status == store.EvStart {
			continue
		}
		out = append(out, ev.Step+":"+ev.Status)
	}
	return out
}

func assertStep(t *testing.T, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("timeline = %v\nwant %v", got, want)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("timeline[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
}

// --- happy path ----------------------------------------------------

func TestDeployHappyPathFirstDeploy(t *testing.T) {
	f := newDeployFixture(t, happyConfig)

	// env comes from the app_env store, not the deploy request.
	// (Detail asserted in TestDeployEnvComesFromStore.)

	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	d := f.deployment()
	if d.State != store.DepSucceeded || d.FinishedAt == nil {
		t.Fatalf("state = %q, want succeeded", d.State)
	}
	if d.ImageDigest == nil || *d.ImageDigest != f.fk.digest {
		t.Errorf("digest = %v, want pulled digest", d.ImageDigest)
	}
	if d.HostPort == nil || *d.HostPort < 20000 || *d.HostPort > 29999 {
		t.Errorf("host port = %v, want one from 20000-29999", d.HostPort)
	}
	if d.StrategyEffective == nil || *d.StrategyEffective != "start-first" {
		t.Errorf("strategy = %v, want start-first (auto, no writable volumes)", d.StrategyEffective)
	}
	if d.ContainerID == nil || d.ContainerName == nil {
		t.Fatalf("container not recorded: %+v", d)
	}

	app, _ := f.s.GetAppByID(f.ctx, f.app.ID)
	if app.CurrentDeploymentID == nil || *app.CurrentDeploymentID != f.depID {
		t.Errorf("current_deployment_id = %v, want %s", app.CurrentDeploymentID, f.depID)
	}

	// managed labels on the created container.
	f.fk.mu.Lock()
	c := f.fk.lookup(*d.ContainerName)
	f.fk.mu.Unlock()
	if c == nil {
		t.Fatalf("container %s missing from fake daemon", *d.ContainerName)
	}
	if c.labels[policy.LabelManaged] != policy.ManagedValue {
		t.Errorf("managed label = %q", c.labels[policy.LabelManaged])
	}
	if c.labels[policy.LabelApp] != f.app.ID {
		t.Errorf("app label = %q, want %s", c.labels[policy.LabelApp], f.app.ID)
	}
	if c.labels[policy.LabelDeployment] != f.depID {
		t.Errorf("deployment label = %q", c.labels[policy.LabelDeployment])
	}

	// timeline with explicit skips.
	assertStep(t, []string{
		"validate:ok", "admission:ok", "pull:ok", "stop_old:skip",
		"create:ok", "start:ok", "health:ok",
		"route_switch:skip", "final_check:skip", "retire_old:skip",
		"finalize:ok",
	}, f.timeline())

	if f.fk.networks == 0 {
		t.Error("network was not ensured")
	}
	if f.fk.creates != 1 {
		t.Errorf("creates = %d, want 1", f.fk.creates)
	}
}

func TestDeployEnvComesFromStore(t *testing.T) {
	// Re-run the deploy against a fake that captures the full spec.
	f := newDeployFixture(t, happyConfig)
	var captured []string
	recorder := &specCapture{inner: f.fk, onCreate: func(env []string) { captured = env }}
	f.dplr = NewDeployer(f.s, recorder, slog.New(slog.DiscardHandler), "test-worker")
	pinHostFacts(f.dplr)
	f.dplr.HealthConsecutive = 1

	now := time.Now().UTC().UnixMilli()
	if _, err := f.s.DB().Exec(
		`INSERT INTO app_env (app_id, key, kind, value_plain, updated_at) VALUES (?, 'MODE', 'plain', 'prod', ?)`,
		f.app.ID, now); err != nil {
		t.Fatalf("insert env: %v", err)
	}
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(captured) != 1 || captured[0] != "MODE=prod" {
		t.Errorf("container env = %v, want [MODE=prod] from app_env", captured)
	}
}

// specCapture wraps the fake and records env at create time.
type specCapture struct {
	inner        *fakeDocker
	onCreate     func(env []string)
	onCreateSpec func(spec *policy.ContainerSpec)
}

func (s *specCapture) PullImage(ctx context.Context, ref string, a *docker.Auth) (string, error) {
	return s.inner.PullImage(ctx, ref, a)
}
func (s *specCapture) EnsureNetwork(ctx context.Context) error { return s.inner.EnsureNetwork(ctx) }
func (s *specCapture) EnsureVolume(ctx context.Context, n string) error {
	return s.inner.EnsureVolume(ctx, n)
}
func (s *specCapture) CreateContainer(ctx context.Context, spec *policy.ContainerSpec) (string, error) {
	if s.onCreate != nil {
		s.onCreate(append([]string{}, spec.Env...))
	}
	if s.onCreateSpec != nil {
		s.onCreateSpec(spec)
	}
	return s.inner.CreateContainer(ctx, spec)
}
func (s *specCapture) StartContainer(ctx context.Context, id string) error {
	return s.inner.StartContainer(ctx, id)
}
func (s *specCapture) StopContainer(ctx context.Context, id string, g time.Duration) error {
	return s.inner.StopContainer(ctx, id, g)
}
func (s *specCapture) RemoveContainer(ctx context.Context, id string, force bool) error {
	return s.inner.RemoveContainer(ctx, id, force)
}
func (s *specCapture) InspectContainer(ctx context.Context, id string) (*docker.ContainerInfo, error) {
	return s.inner.InspectContainer(ctx, id)
}
func (s *specCapture) Stats(ctx context.Context, id string) (*docker.ContainerStats, error) {
	return s.inner.Stats(ctx, id)
}
func (s *specCapture) InspectImage(ctx context.Context, ref string) (string, int64, error) {
	return s.inner.InspectImage(ctx, ref)
}
func (s *specCapture) RemoveImage(ctx context.Context, ref string) error {
	return s.inner.RemoveImage(ctx, ref)
}
func (s *specCapture) ContainerLogs(ctx context.Context, id string, tail int, since time.Duration, follow bool) (io.ReadCloser, error) {
	return s.inner.ContainerLogs(ctx, id, tail, since, follow)
}
func (s *specCapture) ExecProbe(ctx context.Context, id string, cmd []string, timeout time.Duration) (int, error) {
	return s.inner.ExecProbe(ctx, id, cmd, timeout)
}
func (s *specCapture) ListManagedContainers(ctx context.Context) ([]docker.ContainerInfo, error) {
	return s.inner.ListManagedContainers(ctx)
}

// --- payload strictness ----------------------------------------------

func TestDeployRejectsBadPayloads(t *testing.T) {
	for name, payload := range map[string]string{
		"missing id":      `{}`,
		"unknown field":   `{"deployment_id":"x","sudo":true}`,
		"unknown dep ref": fmt.Sprintf(`{"deployment_id":%q}`, store.NewID()),
		"trailing":        `{"deployment_id":"x"} 1`,
		// A hostile payload trying to smuggle a forbidden container
		// parameter: the strict decoder refuses unknown fields, so each
		// one fails validation before any side effect.
		"privileged mode":     `{"deployment_id":"x","privileged":true}`,
		"host networking":     `{"deployment_id":"x","network_mode":"host"}`,
		"host pid namespace":  `{"deployment_id":"x","pid_mode":"host"}`,
		"host ipc namespace":  `{"deployment_id":"x","ipc_mode":"host"}`,
		"host bind mount":     `{"deployment_id":"x","binds":["/var/run/docker.sock:/var/run/docker.sock"]}`,
		"docker socket mount": `{"deployment_id":"x","socket_path":"/var/run/docker.sock"}`,
		"dangerous caps":      `{"deployment_id":"x","cap_add":["SYS_ADMIN"]}`,
		"host devices":        `{"deployment_id":"x","devices":["/dev/sda"]}`,
		"arbitrary sysctls":   `{"deployment_id":"x","sysctls":{"net.ipv4.ip_forward":"1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeployFixture(t, happyConfig)
			j := *f.job
			j.PayloadJSON = payload
			err := f.dplr.Handler()(f.ctx, &j)
			if err == nil {
				t.Fatal("bad payload accepted")
			}
			if Retryable(err) {
				t.Errorf("payload errors must be permanent, got retryable: %v", err)
			}
			if !strings.Contains(err.Error(), "E_VALIDATION") {
				t.Errorf("err = %v, want E_VALIDATION", err)
			}
			if d := f.deployment(); d.State != store.DepPending {
				t.Errorf("deployment state = %q, want untouched pending", d.State)
			}
			if f.fk.pulls != 0 {
				t.Error("a bad payload must cause no side effects")
			}
		})
	}
}

// --- pull failures ---------------------------------

func TestDeployImageNotFoundFailsImmediately(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.fk.pullErr = errors.New("manifest unknown: manifest unknown")

	err := f.run(1, 5)
	if err == nil {
		t.Fatal("expected failure")
	}
	if Retryable(err) {
		t.Errorf("image-not-found must not retry, got retryable: %v", err)
	}
	d := f.deployment()
	if d.State != store.DepFailed || d.FailureStep == nil || *d.FailureStep != "pull" {
		t.Errorf("deployment = %q step %v, want failed/pull", d.State, d.FailureStep)
	}
	if d.FailureCode == nil || *d.FailureCode != "E_IMAGE_NOT_FOUND" {
		t.Errorf("code = %v, want E_IMAGE_NOT_FOUND", d.FailureCode)
	}
	if f.fk.creates != 0 {
		t.Error("no container may exist after a pull failure (no side effects)")
	}
}

func TestDeployPullTransientRetriesThenFailsWhenExhausted(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.fk.pullErr = errors.New("read tcp 1.2.3.4:443: connection reset by peer")

	// With attempts left: retryable, deployment stays running for the
	// worker to requeue with backoff.
	err := f.run(1, 5)
	if err == nil || !Retryable(err) {
		t.Fatalf("transient pull = %v, want retryable", err)
	}
	if d := f.deployment(); d.State != store.DepRunning {
		t.Errorf("deployment = %q, want running while retrying", d.State)
	}
	// The failed attempt is still on the timeline.
	if got := f.timeline(); !containsStr(got, "pull:fail") {
		t.Errorf("timeline = %v, want pull:fail", got)
	}

	// Last attempt: the same transient error now fails the deployment.
	err = f.run(5, 5)
	if err == nil {
		t.Fatal("expected failure when attempts exhausted")
	}
	d := f.deployment()
	if d.State != store.DepFailed || d.FailureStep == nil || *d.FailureStep != "pull" {
		t.Errorf("deployment = %q step %v, want failed/pull", d.State, d.FailureStep)
	}
}

// --- start/health failures ---------------------------------

func TestDeployContainerExitImmediatelyFails(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	f.fk.afterStart = func(c *fakeContainer) {
		c.state = "exited"
		c.exit = 139
	}

	err := f.run(1, 5)
	if err == nil {
		t.Fatal("expected failure")
	}
	if Retryable(err) {
		t.Errorf("immediate exit must not retry, got: %v", err)
	}
	if !strings.Contains(err.Error(), "E_START_FAILED") ||
		!strings.Contains(err.Error(), "139") {
		t.Errorf("err = %v, want E_START_FAILED with exit code", err)
	}
	d := f.deployment()
	if d.State != store.DepFailed || *d.FailureStep != "start" {
		t.Errorf("deployment = %q/%v, want failed/start", d.State, d.FailureStep)
	}
	// The new container must be cleaned up.
	if f.fk.stateOf(*d.ContainerID) != "gone" {
		t.Errorf("container %s not cleaned up after failure", *d.ContainerID)
	}
	if got := f.timeline(); !containsStr(got, "start:fail") {
		t.Errorf("timeline = %v, want start:fail", got)
	}
}

const failingHealthConfig = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	"health": {"type": "exec", "command": ["/bin/true"],
		"interval_s": 2, "timeout_s": 1, "retries": 1, "start_period_s": 0}
}`

func TestDeployHealthFailureAttachesLogTailAndCleansUp(t *testing.T) {
	f := newDeployFixture(t, failingHealthConfig)
	f.fk.execCode = 1 // health probe keeps failing

	err := f.run(1, 5)
	if err == nil {
		t.Fatal("expected health failure")
	}
	if Retryable(err) {
		t.Errorf("health failure must not retry at job level, got: %v", err)
	}
	if !strings.Contains(err.Error(), "E_HEALTH_FAILED") {
		t.Errorf("err = %v, want E_HEALTH_FAILED", err)
	}
	// the last 50 log lines ride along.
	if !strings.Contains(err.Error(), "last 50 container log lines") ||
		!strings.Contains(err.Error(), "ready to serve") {
		t.Errorf("err = %v, want embedded log tail", err)
	}
	d := f.deployment()
	if d.State != store.DepFailed || *d.FailureStep != "health" || *d.FailureCode != "E_HEALTH_FAILED" {
		t.Errorf("deployment = %q step %v code %v", d.State, d.FailureStep, d.FailureCode)
	}
	if !strings.Contains(*d.FailureMessage, "ready to serve") {
		t.Errorf("failure_message lacks log tail: %.100s", *d.FailureMessage)
	}
	if f.fk.stateOf(*d.ContainerID) != "gone" {
		t.Error("unhealthy new container must be removed")
	}
}

const writableVolumeConfig = `{
	"name": "web",
	"image": "traefik/whoami",
	"tag": "v1.8",
	"port": 8080,
	"volumes": [{"name": "data", "path": "/data"}],
	"health": {"type": "exec", "command": ["/bin/true"],
		"interval_s": 2, "timeout_s": 1, "retries": 1, "start_period_s": 0},
	"stop_grace_s": 5
}`

func TestDeployStopFirstRestoresOldContainerOnFailure(t *testing.T) {
	f := newDeployFixture(t, writableVolumeConfig)

	// Previous successful deployment with a running container.
	prevDepID := store.NewID()
	prevJob, err := f.s.CreateJob(f.ctx, store.CreateJobInput{
		AppID: &f.app.ID, Type: DeployJobType,
		PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, prevDepID),
		CreatedBy:   "cli",
	})
	if err != nil {
		t.Fatalf("prev job: %v", err)
	}
	prev, err := f.s.CreateDeployment(f.ctx, store.CreateDeploymentInput{
		ID: prevDepID, AppID: f.app.ID, JobID: prevJob.ID, Kind: "deploy",
		ImageRef:           "traefik/whoami:v1.7",
		ConfigSnapshotJSON: `{"name":"web","image":"traefik/whoami","tag":"v1.7","port":8080,"stop_grace_s":7}`,
		ConfigVersion:      1,
	})
	if err != nil {
		t.Fatalf("prev deployment: %v", err)
	}
	if err := f.s.MarkDeploymentRunning(f.ctx, prev.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SucceedDeployment(f.ctx, f.app.ID, prev.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetDeploymentContainer(f.ctx, prev.ID, "old-1", "ts-web-0"); err != nil {
		t.Fatal(err)
	}
	f.fk.addContainer("old-1", "ts-web-0", "running", map[string]string{
		policy.LabelManaged: policy.ManagedValue, policy.LabelApp: f.app.ID,
	})

	// New version: writable volume ⇒ auto resolves to stop-first;
	// health then fails, and the pipeline must restore the old container.
	f.fk.execCode = 1
	err = f.run(1, 5)
	if err == nil {
		t.Fatal("expected health failure")
	}

	d := f.deployment()
	if d.State != store.DepFailed || *d.FailureStep != "health" {
		t.Fatalf("deployment = %q/%v, want failed/health", d.State, d.FailureStep)
	}
	if d.StrategyEffective == nil || *d.StrategyEffective != "stop-first" {
		t.Errorf("strategy = %v, want stop-first (writable volume)", d.StrategyEffective)
	}
	// old container was stopped (stop_old) then restarted.
	if f.fk.stateOf("old-1") != "running" {
		t.Errorf("old container state = %q, want running again", f.fk.stateOf("old-1"))
	}
	if !containsStr(f.fk.stopped, "old-1") {
		t.Errorf("stopped = %v, want old container stopped first", f.fk.stopped)
	}
	if f.fk.stopGrace != 7*time.Second {
		t.Errorf("stop grace = %v, want 7s from the previous config snapshot", f.fk.stopGrace)
	}
	// The new container is gone.
	if f.fk.containerCount() != 1 {
		t.Errorf("containers = %d, want only the restored old one", f.fk.containerCount())
	}
	// Timeline shows stop_old ran (start-first would skip it).
	if got := f.timeline(); !containsStr(got, "stop_old:ok") {
		t.Errorf("timeline = %v, want stop_old:ok", got)
	}
}

// --- idempotent re-run -------------------------------

func TestDeployReusesContainerWithMatchingConfigHash(t *testing.T) {
	f := newDeployFixture(t, happyConfig)

	// A container from a crashed previous attempt: right deployment label and
	// config hash, correct name (ts-<app>-<seq>), not yet started.
	cfg, err := appconfig.Parse([]byte(happyConfig))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appconfig.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	hash := policy.ConfigHash(cfg)
	name := policy.ContainerPrefix + f.app.Name + "-1"
	f.fk.addContainer("pre-1", name, "created", map[string]string{
		policy.LabelManaged:    policy.ManagedValue,
		policy.LabelApp:        f.app.ID,
		policy.LabelDeployment: f.depID,
		policy.LabelConfigHash: hash,
	})

	if err := f.run(2, 5); err != nil {
		t.Fatalf("deploy re-run: %v", err)
	}
	if f.fk.creates != 0 {
		t.Errorf("creates = %d, want 0 (container with matching hash is reused)", f.fk.creates)
	}
	d := f.deployment()
	if d.State != store.DepSucceeded {
		t.Errorf("state = %q, want succeeded", d.State)
	}
	if d.ContainerID == nil || *d.ContainerID != "pre-1" {
		t.Errorf("container_id = %v, want pre-1", d.ContainerID)
	}
	if f.fk.stateOf("pre-1") != "running" {
		t.Errorf("reused container state = %q, want running", f.fk.stateOf("pre-1"))
	}
	if got := f.timeline(); !containsStr(got, "create:ok") {
		t.Errorf("timeline = %v, want create:ok", got)
	}
}

// --- registry credential and sealed env ---------------------------------

func TestDeployMissingRegistryCredentialFailsValidate(t *testing.T) {
	f := newDeployFixture(t, `{
	"name": "web", "image": "ghcr.io/acme/app", "tag": "v1", "port": 8080,
	"registry_credential": "acme-registry",
	`+execHealthFast+`
}`)
	err := f.run(1, 5)
	if err == nil {
		t.Fatal("expected failure")
	}
	if Retryable(err) || !strings.Contains(err.Error(), "E_VALIDATION") {
		t.Errorf("err = %v, want permanent E_VALIDATION", err)
	}
	if f.fk.pulls != 0 {
		t.Error("pull must not run when the credential does not exist")
	}
	d := f.deployment()
	if d.State != store.DepFailed || *d.FailureStep != "validate" || *d.FailureCode != "E_VALIDATION" {
		t.Errorf("deployment = %q/%v/%v", d.State, d.FailureStep, d.FailureCode)
	}
}

// giveCredential stores a sealed credential and points the deployer's
// key material at the pair that sealed it.
func giveCredential(t *testing.T, f *deployFixture, name, registry, username, token string) {
	t.Helper()
	priv, pub, err := secrypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	sealed, err := secrypto.Seal(pub, []byte(token))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := f.s.UpsertCredential(f.ctx, name, registry, username, sealed); err != nil {
		t.Fatalf("upsert credential: %v", err)
	}
	f.dplr.SecretKey = priv
}

func TestDeployPullUsesRegistryCredential(t *testing.T) {
	f := newDeployFixture(t, `{
	"name": "web", "image": "ghcr.io/acme/app", "tag": "v1", "port": 8080,
	"registry_credential": "acme-registry",
	`+execHealthFast+`
}`)
	giveCredential(t, f, "acme-registry", "ghcr.io", "linh", "tok_secret_value")

	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	auth := f.fk.lastPullAuth
	if auth == nil {
		t.Fatal("pull received no credentials")
	}
	if auth.Username != "linh" || auth.Secret != "tok_secret_value" {
		t.Errorf("auth = %+v, want linh with the decrypted token", auth)
	}
	if auth.Registry != "ghcr.io" {
		t.Errorf("auth.Registry = %q, want ghcr.io", auth.Registry)
	}
}

func TestDeployWrongRegistryTokenFailsWithoutRetry(t *testing.T) {
	f := newDeployFixture(t, `{
	"name": "web", "image": "ghcr.io/acme/app", "tag": "v1", "port": 8080,
	"registry_credential": "acme-registry",
	`+execHealthFast+`
}`)
	giveCredential(t, f, "acme-registry", "ghcr.io", "linh", "expired-token")
	f.fk.pullErr = errors.New("unauthorized: authentication required")

	err := f.run(5, 5)
	if err == nil {
		t.Fatal("expected failure")
	}
	if Retryable(err) || !strings.Contains(err.Error(), "E_REGISTRY_AUTH") {
		t.Errorf("err = %v, want permanent E_REGISTRY_AUTH", err)
	}
	d := f.deployment()
	if d.State != store.DepFailed || *d.FailureStep != "pull" || *d.FailureCode != "E_REGISTRY_AUTH" {
		t.Errorf("deployment = %q/%v/%v", d.State, d.FailureStep, d.FailureCode)
	}
}

func TestDeploySealedEnvWithoutKeyFailsValidate(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	now := time.Now().UTC().UnixMilli()
	if _, err := f.s.DB().Exec(
		`INSERT INTO app_env (app_id, key, kind, value_sealed, updated_at) VALUES (?, 'TOKEN', 'secret', ?, ?)`,
		f.app.ID, []byte("sealed"), now); err != nil {
		t.Fatal(err)
	}
	err := f.run(1, 5)
	if err == nil || !strings.Contains(err.Error(), "E_SECRETS_KEY") {
		t.Fatalf("err = %v, want E_SECRETS_KEY", err)
	}
	if d := f.deployment(); d.State != store.DepFailed || *d.FailureStep != "validate" {
		t.Errorf("deployment = %q/%v", d.State, d.FailureStep)
	}
}

func TestDeploySealedEnvDecryptsIntoContainer(t *testing.T) {
	f := newDeployFixture(t, happyConfig)
	priv, pub, err := secrypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	sealed, err := secrypto.Seal(pub, []byte("decrypted-token"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	f.dplr.SecretKey = priv
	now := time.Now().UTC().UnixMilli()
	if _, err := f.s.DB().Exec(
		`INSERT INTO app_env (app_id, key, kind, value_sealed, updated_at) VALUES (?, 'TOKEN', 'secret', ?, ?)`,
		f.app.ID, sealed, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB().Exec(
		`INSERT INTO app_env (app_id, key, kind, value_plain, updated_at) VALUES (?, 'MODE', 'plain', 'prod', ?)`,
		f.app.ID, now); err != nil {
		t.Fatal(err)
	}

	var captured []string
	f.dplr.Docker = &specCapture{inner: f.fk, onCreate: func(env []string) {
		captured = append(captured, env...)
	}}
	if err := f.run(1, 5); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	want := map[string]string{"TOKEN": "decrypted-token", "MODE": "prod"}
	for _, kv := range captured {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || want[k] == "" {
			continue
		}
		if v != want[k] {
			t.Errorf("env %s = %q, want %q", k, v, want[k])
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("container env missing %v (got %v)", want, captured)
	}
}

// --- defaults and helpers ---------------------------------------------------

func TestNewDeployerDefaultsToTwoConsecutivePasses(t *testing.T) {
	p := NewDeployer(testStore(t), newFakeDocker(), nil, "w")
	if p.HealthConsecutive != HealthConsecutivePasses || HealthConsecutivePasses != 2 {
		t.Errorf("HealthConsecutive = %d, want HealthConsecutivePasses = 2",
			p.HealthConsecutive)
	}
}

// TestLogTailDemuxesFramedOutput: the health-failure log tail must show
// plain text even when the daemon returns Docker's multiplexed framing
// (the wrapper demuxes; this pins the belt-and-braces DemuxBytes too).
func TestLogTailDemuxesFramedOutput(t *testing.T) {
	fk := newFakeDocker()
	fk.logs = string([]byte{1, 0, 0, 0, 0, 0, 0, 6, 'h', 'e', 'l', 'l', 'o', '\n'})
	d := NewDeployer(nil, fk, slog.New(slog.DiscardHandler), "test")
	got := d.logTail(context.Background(), "cid-x", 10)
	if !strings.Contains(got, "hello\n") {
		t.Errorf("logTail = %q, want demuxed plain text", got)
	}
	if strings.Contains(got, "\x01") {
		t.Errorf("logTail leaked frame bytes: %q", got)
	}
}

// TestRunStoreServesLogSocket proves the wiring: a worker with a RunDir
// exposes <RunDir>/worker.sock while running and removes it on shutdown.
func TestRunStoreServesLogSocket(t *testing.T) {
	s := testStore(t)
	runDir := t.TempDir()
	stop := runWorker(t, s, Config{
		RunDir: runDir,
		Docker: newFakeDocker(),
	})
	sock := runDir + "/" + LogSockName
	client := unixClient(sock)
	deadline := time.Now().Add(3 * time.Second)
	var ok bool
	for !ok && time.Now().Before(deadline) {
		if resp, err := client.Get("http://unix/healthz"); err == nil {
			resp.Body.Close()
			ok = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("worker never opened its log socket")
	}
	stop()
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

// TestRunStoreRejectsRunDirWithoutDocker: the log socket is the only read
// path into the daemon, so a worker without a wrapper must refuse to run
// rather than serve nothing.
func TestRunStoreRejectsRunDirWithoutDocker(t *testing.T) {
	s := testStore(t)
	err := RunStore(context.Background(), s, Config{RunDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no Docker wrapper") {
		t.Errorf("RunStore = %v, want refusal mentioning the missing wrapper", err)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
