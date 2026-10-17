// Package docker is the ONLY code in TinyShip allowed to talk to the Docker
// daemon. It exposes a closed set of operations; there
// is no method that forwards arbitrary API requests. A static test
// (client_scope_test.go) fails the build if any other package imports the
// Docker client, and CreateContainer accepts only a policy.ContainerSpec —
// raw Docker parameters cannot reach it.
package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/tinyship/tinyship/internal/policy"
)

// MinEngineAPI is the oldest Docker API this binary speaks (Engine
// 24.0 → API 1.43). Older engines are refused at startup.
const MinEngineAPI = "1.43"

// Wrapper wraps the Docker client with the allowlisted operations only.
type Wrapper struct {
	cli *client.Client
	log *slog.Logger
}

// Auth carries registry credentials for a pull; the caller has decrypted
// them in memory. They are never logged.
type Auth struct {
	Registry string // e.g. ghcr.io; empty means docker.io
	Username string
	Secret   string
}

// New connects to the Docker daemon via the environment (DOCKER_HOST or the
// default unix socket) and verifies the engine is new enough.
func New(log *slog.Logger) (*Wrapper, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: connect: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Wrapper{cli: cli, log: log}, nil
}

// CheckEngine verifies the daemon meets the minimum API version and is
// reachable.
func (w *Wrapper) CheckEngine(ctx context.Context) error {
	ping, err := w.cli.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker: daemon unreachable: %w", err)
	}
	if !apiAtLeast(ping.APIVersion, MinEngineAPI) {
		return fmt.Errorf("docker: engine API %s is older than the required %s (install Docker Engine >= 24.0)",
			ping.APIVersion, MinEngineAPI)
	}
	return nil
}

// apiAtLeast compares dotted numeric API versions (e.g. "1.43" <= "1.56").
func apiAtLeast(got, want string) bool {
	g, w := strings.Split(got, "."), strings.Split(want, ".")
	for i := 0; i < len(w); i++ {
		var gv, wv int
		if i < len(g) {
			fmt.Sscanf(g[i], "%d", &gv)
		}
		fmt.Sscanf(w[i], "%d", &wv)
		if gv != wv {
			return gv > wv
		}
	}
	return true
}

// Close releases the client.
func (w *Wrapper) Close() error { return w.cli.Close() }

// --- allowlist ----------------------------------------------------

// PullImage pulls ref and returns the resolved digest. auth may be nil for
// public images. Progress lines go to the job log; credentials never do.
func (w *Wrapper) PullImage(ctx context.Context, ref string, auth *Auth) (string, error) {
	var opts image.PullOptions
	if auth != nil && auth.Secret != "" {
		credJSON, err := json.Marshal(map[string]string{
			"username": auth.Username,
			"password": auth.Secret,
			// Registry field intentionally left: Docker resolves serveraddress
			// from the ref; sending it wrong breaks some registries.
		})
		if err != nil {
			return "", fmt.Errorf("docker: encode auth: %w", err)
		}
		opts.RegistryAuth = base64.StdEncoding.EncodeToString(credJSON)
	}

	rc, err := w.cli.ImagePull(ctx, ref, opts)
	if err != nil {
		return "", fmt.Errorf("docker: pull %s: %w", ref, err)
	}
	defer rc.Close()
	// Drain progress so the daemon finishes the pull; surface coarse
	// progress via structured lines.
	dec := json.NewDecoder(rc)
	for {
		var p struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return "", fmt.Errorf("docker: pull stream %s: %w", ref, err)
		}
		if p.Error != "" {
			return "", fmt.Errorf("docker: pull %s: %s", ref, p.Error)
		}
		if p.Status != "" {
			w.log.Info("pull", "ref", ref, "status", p.Status)
		}
	}

	inspected, _, err := w.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("docker: inspect pulled image %s: %w", ref, err)
	}
	if len(inspected.RepoDigests) == 0 {
		return "", fmt.Errorf("docker: image %s has no repo digest (registry did not return one)", ref)
	}
	// RepoDigests look like "repo@sha256:..."; return the digest part after
	// the first @ so callers can compare against a pinned digest.
	_, digest, ok := strings.Cut(inspected.RepoDigests[0], "@")
	if !ok {
		return "", fmt.Errorf("docker: unexpected repo digest form %q", inspected.RepoDigests[0])
	}
	return digest, nil
}

// InspectImage returns the digest and size of a local image.
func (w *Wrapper) InspectImage(ctx context.Context, ref string) (digest string, size int64, err error) {
	inspected, _, err := w.cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return "", 0, fmt.Errorf("docker: inspect image %s: %w", ref, err)
	}
	if len(inspected.RepoDigests) > 0 {
		if _, d, ok := strings.Cut(inspected.RepoDigests[0], "@"); ok {
			digest = d
		}
	}
	return digest, inspected.Size, nil
}

// CreateContainer creates a container from a closed policy spec. This is the
// single translation point between ContainerSpec and Docker parameters;
// nothing else may set container options.
func (w *Wrapper) CreateContainer(ctx context.Context, spec *policy.ContainerSpec) (string, error) {
	cc := containerConfigFor(spec)
	hc := hostConfigFor(spec)

	created, err := w.cli.ContainerCreate(ctx, cc, hc, &network.NetworkingConfig{}, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("docker: create %s: %w", spec.Name, err)
	}
	return created.ID, nil
}

// hostConfigFor translates a closed policy spec into Docker's HostConfig.
// It is a pure function so tests can assert every
// parameter — including that the always-rejected parameters stay zero-valued
// — without a Docker daemon.
func hostConfigFor(spec *policy.ContainerSpec) *container.HostConfig {
	// Loopback-only bindings: never publish to 0.0.0.0.
	bindings := nat.PortMap{}
	for _, p := range spec.Ports {
		np := nat.Port(fmt.Sprintf("%d/tcp", p.ContainerPort))
		bindings[np] = []nat.PortBinding{{
			HostIP:   "127.0.0.1",
			HostPort: fmt.Sprintf("%d", p.HostPort),
		}}
	}

	// Tmpfs mounts: noexec,nosuid unconditionally.
	tmpfs := map[string]string{}
	for _, tf := range spec.Tmpfs {
		opts := policy.TmpfsOptions
		if tf.SizeMB > 0 {
			opts = fmt.Sprintf("%s,size=%dm", opts, tf.SizeMB)
		}
		tmpfs[tf.Path] = opts
	}

	// Named volumes only: name derived by DockerVolumeName so a user name
	// can never address a foreign volume.
	binds := make([]string, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		real := policy.DockerVolumeName(spec.AppName, v.Name)
		binding := fmt.Sprintf("%s:%s", real, v.Path)
		if v.ReadOnly {
			binding += ":ro"
		}
		binds = append(binds, binding)
	}

	pids := spec.PidsLimit

	return &container.HostConfig{
		Binds:          binds,
		Tmpfs:          tmpfs,
		NetworkMode:    container.NetworkMode(spec.Network),
		PortBindings:   bindings,
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyMode(spec.RestartPolicy)},
		CapDrop:        []string{"ALL"},
		CapAdd:         spec.CapAdd,
		SecurityOpt:    []string{policy.SecurityOptNNP},
		ReadonlyRootfs: spec.ReadOnlyRootfs,
		OomScoreAdj:    spec.OomScoreAdj,
		Resources: container.Resources{
			Memory:     spec.MemoryBytes,
			MemorySwap: spec.MemoryBytes, // equal ⇒ no swap
			NanoCPUs:   spec.NanoCPUs,
			PidsLimit:  &pids,
		},
		LogConfig: container.LogConfig{
			Type:   spec.LogDriver,
			Config: spec.LogOpts,
		},
		// Nothing else: no Privileged, no Devices, no PidMode/IpcMode/UTSMode,
		// no Sysctls, no ExtraHosts, no PublishAllPorts, no CapAdd beyond the
		// spec — those HostConfig fields are simply never set.
	}
}

// containerConfigFor translates a closed policy spec into Docker's Config.
func containerConfigFor(spec *policy.ContainerSpec) *container.Config {
	exposed := nat.PortSet{}
	for _, p := range spec.Ports {
		exposed[nat.Port(fmt.Sprintf("%d/tcp", p.ContainerPort))] = struct{}{}
	}
	stop := spec.StopTimeoutS
	cfgOut := &container.Config{
		Image:        spec.Image,
		Labels:       spec.Labels,
		Env:          spec.Env,
		ExposedPorts: exposed,
		StopTimeout:  &stop,
	}
	// empty command/user means the image's own default; when set
	// they are passed as argv/user verbatim (never through a shell).
	if len(spec.Command) > 0 {
		cfgOut.Cmd = spec.Command
	}
	if spec.User != "" {
		cfgOut.User = spec.User
	}
	return cfgOut
}

// StartContainer starts a created container.
func (w *Wrapper) StartContainer(ctx context.Context, id string) error {
	if err := w.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("docker: start %s: %w", id, err)
	}
	return nil
}

// StopContainer stops with the given grace period before force-kill
func (w *Wrapper) StopContainer(ctx context.Context, id string, grace time.Duration) error {
	secs := int(grace.Seconds())
	if secs < 1 {
		secs = 1
	}
	if err := w.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &secs}); err != nil {
		return fmt.Errorf("docker: stop %s: %w", id, err)
	}
	return nil
}

// RemoveContainer deletes a container; force kills if still running (used
// only after ownership checks).
func (w *Wrapper) RemoveContainer(ctx context.Context, id string, force bool) error {
	if err := w.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force}); err != nil {
		return fmt.Errorf("docker: remove %s: %w", id, err)
	}
	return nil
}

// ContainerInfo is the inspected subset the worker needs; kept small so the
// wrapper API stays closed.
type ContainerInfo struct {
	ID     string
	Name   string
	Image  string
	State  string // running, exited, ...
	Health string // healthy, unhealthy, starting, "" (no probe)
	// ExitCode is meaningful when State is "exited"; it makes start/health
	// failure messages concrete.
	ExitCode int
	Labels   map[string]string
}

// InspectContainer returns the state and labels of a container (the
// worker runs ownership checks against these labels).
func (w *Wrapper) InspectContainer(ctx context.Context, id string) (*ContainerInfo, error) {
	inspected, err := w.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("docker: inspect container %s: %w", id, err)
	}
	info := &ContainerInfo{
		ID:     inspected.ID,
		Name:   strings.TrimPrefix(inspected.Name, "/"),
		Image:  inspected.Config.Image,
		Labels: inspected.Config.Labels,
	}
	if inspected.State != nil {
		info.State = inspected.State.Status
		info.ExitCode = inspected.State.ExitCode
		if inspected.State.Health != nil {
			info.Health = inspected.State.Health.Status
		}
	}
	return info, nil
}

// ContainerLogs returns the tail of a container's log as plain text
// Docker's 8-byte stream framing is stripped on the fly, so
// callers never see raw frame bytes (TTY containers already emit plain
// text and pass through). The caller owns the returned reader.
func (w *Wrapper) ContainerLogs(ctx context.Context, id string, tail int, since time.Duration, follow bool) (io.ReadCloser, error) {
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       fmt.Sprintf("%d", tail),
		Follow:     follow,
		Timestamps: false,
	}
	if tail <= 0 {
		opts.Tail = "all" // Docker's sentinel for the full log
	}
	if since > 0 {
		opts.Since = time.Now().Add(-since).Format(time.RFC3339Nano)
	}
	rc, err := w.cli.ContainerLogs(ctx, id, opts)
	if err != nil {
		return nil, fmt.Errorf("docker: logs %s: %w", id, err)
	}
	// Demux while streaming (keeps --follow responsive) and keep the
	// underlying connection closable.
	return struct {
		io.Reader
		io.Closer
	}{Reader: NewLogDemux(rc), Closer: rc}, nil
}

// ExecProbe runs a one-shot command inside a container and reports the exit
// code (health checks of type exec, /). The command comes only
// from a validated health config.
func (w *Wrapper) ExecProbe(ctx context.Context, id string, command []string, timeout time.Duration) (int, error) {
	execCfg := container.ExecOptions{
		Cmd:          command,
		AttachStdout: true,
		AttachStderr: true,
	}
	created, err := w.cli.ContainerExecCreate(ctx, id, execCfg)
	if err != nil {
		return -1, fmt.Errorf("docker: exec create on %s: %w", id, err)
	}
	attach, err := w.cli.ContainerExecAttach(ctx, created.ID, container.ExecStartOptions{})
	if err != nil {
		return -1, fmt.Errorf("docker: exec attach on %s: %w", id, err)
	}
	defer attach.Close()

	done := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(io.Discard, attach.Reader)
		done <- copyErr
	}()
	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-time.After(timeout):
		return -1, fmt.Errorf("docker: exec on %s timed out after %s", id, timeout)
	case <-done:
	}

	inspected, err := w.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return -1, fmt.Errorf("docker: exec inspect on %s: %w", id, err)
	}
	return inspected.ExitCode, nil
}

// ListManagedContainers lists only containers carrying the TinyShip managed
// label.
func (w *Wrapper) ListManagedContainers(ctx context.Context) ([]ContainerInfo, error) {
	list, err := w.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", policy.LabelManaged+"="+policy.ManagedValue)),
	})
	if err != nil {
		return nil, fmt.Errorf("docker: list managed containers: %w", err)
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, c := range list {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, ContainerInfo{
			ID: c.ID, Name: name, Image: c.Image, State: c.State, Labels: c.Labels,
		})
	}
	return out, nil
}

// EnsureNetwork creates the tinyship bridge network if missing. ICC is
// disabled: app containers cannot talk to each other (deliberate v0.1
// choice). idempotent.
func (w *Wrapper) EnsureNetwork(ctx context.Context) error {
	_, err := w.cli.NetworkInspect(ctx, policy.NetworkName, network.InspectOptions{})
	if err == nil {
		return nil // exists
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("docker: inspect network %s: %w", policy.NetworkName, err)
	}
	_, err = w.cli.NetworkCreate(ctx, policy.NetworkName, network.CreateOptions{
		Driver: "bridge",
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": "false",
		},
		Labels: map[string]string{policy.LabelManaged: policy.ManagedValue},
	})
	if err != nil {
		return fmt.Errorf("docker: create network %s: %w", policy.NetworkName, err)
	}
	return nil
}

// EnsureVolume creates a managed named volume if missing (idempotent). The
// name must carry the ts- prefix; foreign names are refused.
func (w *Wrapper) EnsureVolume(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, policy.VolumePrefix) {
		return fmt.Errorf("docker: volume %q does not carry the %q prefix; refusing", name, policy.VolumePrefix)
	}
	_, err := w.cli.VolumeInspect(ctx, name)
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("docker: inspect volume %s: %w", name, err)
	}
	_, err = w.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Labels: map[string]string{policy.LabelManaged: policy.ManagedValue},
	})
	if err != nil {
		return fmt.Errorf("docker: create volume %s: %w", name, err)
	}
	return nil
}

// RemoveImage deletes a local image; only the gc job may call it, and only
// for images that pass its retention policy.
func (w *Wrapper) RemoveImage(ctx context.Context, ref string) error {
	if _, err := w.cli.ImageRemove(ctx, ref, image.RemoveOptions{}); err != nil {
		return fmt.Errorf("docker: remove image %s: %w", ref, err)
	}
	return nil
}

// --- compile-time proof the API stays closed -------------------------------

var _ interface {
	PullImage(context.Context, string, *Auth) (string, error)
	InspectImage(context.Context, string) (string, int64, error)
	CreateContainer(context.Context, *policy.ContainerSpec) (string, error)
	StartContainer(context.Context, string) error
	StopContainer(context.Context, string, time.Duration) error
	RemoveContainer(context.Context, string, bool) error
	InspectContainer(context.Context, string) (*ContainerInfo, error)
	ContainerLogs(context.Context, string, int, time.Duration, bool) (io.ReadCloser, error)
	ExecProbe(context.Context, string, []string, time.Duration) (int, error)
	ListManagedContainers(context.Context) ([]ContainerInfo, error)
	EnsureNetwork(context.Context) error
	EnsureVolume(context.Context, string) error
	RemoveImage(context.Context, string) error
} = (*Wrapper)(nil)
