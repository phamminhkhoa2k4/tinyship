// Package policy builds the Docker container specification from a validated
// app config. This package is pure data: it imports no Docker
// client. The wrapper (internal/docker) is the only code translating a
// ContainerSpec into Docker API parameters.
//
// The Spec type deliberately has NO fields for the always-rejected
// parameters (Privileged, host networking, PidMode, IpcMode, Devices,
// Sysctls, ExtraHosts, host bind mounts, PublishAllPorts): a forbidden
// parameter cannot even be expressed here, so it cannot reach Docker
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tinyship/tinyship/internal/appconfig"
)

// Fixed topology values. The network name and label keys are
// constants so reconcile/gc can always identify managed objects.
const (
	NetworkName     = "tinyship"
	LabelManaged    = "tinyship.managed"
	LabelApp        = "tinyship.app"
	LabelDeployment = "tinyship.deployment"
	LabelConfigHash = "tinyship.config_hash"
	ManagedValue    = "true"
	VolumePrefix    = "ts-"
	ContainerPrefix = "ts-"
	RestartPolicy   = "unless-stopped"
	SecurityOptNNP  = "no-new-privileges:true"
	TmpfsOptions    = "rw,noexec,nosuid"
	LogDriver       = "json-file"
	LogMaxSize      = "10m"
	LogMaxFiles     = "3"
	OomScoreAdj     = 500 // app containers die before system services
	MinHostPort     = 20000
	MaxHostPort     = 29999 // loopback-only host port range
)

// PortBinding is a loopback-only host port mapping. The HostIP is always
// 127.0.0.1: app ports are never published to the Internet.
type PortBinding struct {
	HostPort      int
	ContainerPort int
}

// VolumeMount references a TinyShip-managed named volume. The real Docker
// volume name is derived (ts-<app>-<name>) so a user-supplied name can never
// touch a foreign volume.
type VolumeMount struct {
	Name     string // user-visible name (validated by appconfig)
	Path     string // absolute container path
	ReadOnly bool
}

// TmpfsMount is an in-memory mount for read-only-rootfs apps.
type TmpfsMount struct {
	Path   string
	SizeMB int // 0 = engine default capped by tmpfs SizeMB rule (≤256)
}

// ContainerSpec is the complete, closed set of parameters a container may be
// created with. Anything not representable here is rejected by construction.
type ContainerSpec struct {
	Name           string
	AppName        string // short app name; used to derive volume names
	Image          string // <image>@<digest> — never a tag
	Labels         map[string]string
	Network        string // always NetworkName
	Ports          []PortBinding
	RestartPolicy  string // always RestartPolicy
	CapDropALL     bool   // always true; CapAdd is the only relaxation
	CapAdd         []string
	NoNewPrivs     bool  // always true
	MemoryBytes    int64 // Memory == MemorySwap: no swap
	NanoCPUs       int64
	PidsLimit      int64
	OomScoreAdj    int
	ReadOnlyRootfs bool
	Tmpfs          []TmpfsMount
	Volumes        []VolumeMount
	Command        []string // argv → Config.Cmd; empty = image default
	User           string   // uid[:gid] → Config.User; empty = image default
	Env            []string // KEY=VALUE; secrets arrive decrypted at creation time only
	StopTimeoutS   int
	LogDriver      string
	LogOpts        map[string]string
}

// Instance identifies the deployment this spec belongs to.
type Instance struct {
	AppID        string
	AppName      string // short name for volume/container naming
	DeploymentID string
	Seq          int // deployment sequence within the app
	Digest       string
	ConfigHash   string // sha256 of the config snapshot; recorded as a label
	HostPort     int
	Env          []string
}

// Build validates the instance against the config and produces the closed
// ContainerSpec. cfg must already pass
// appconfig.Validate; Build re-checks only what it depends on.
func Build(cfg *appconfig.Config, in Instance) (*ContainerSpec, error) {
	if in.AppID == "" || in.AppName == "" || in.DeploymentID == "" {
		return nil, fmt.Errorf("policy: app id, name, and deployment id are required")
	}
	if !strings.HasPrefix(in.Digest, "sha256:") || len(in.Digest) != len("sha256:")+64 {
		return nil, fmt.Errorf("policy: deployment must carry a pinned sha256 digest, got %q", in.Digest)
	}
	if in.HostPort < MinHostPort || in.HostPort > MaxHostPort {
		return nil, fmt.Errorf("policy: host port %d outside the managed range %d-%d", in.HostPort, MinHostPort, MaxHostPort)
	}

	spec := &ContainerSpec{
		Name:           ContainerPrefix + in.AppName + "-" + fmt.Sprintf("%d", in.Seq),
		AppName:        in.AppName,
		Image:          cfg.Image + "@" + in.Digest,
		Network:        NetworkName,
		CapDropALL:     true,
		NoNewPrivs:     true,
		RestartPolicy:  RestartPolicy,
		MemoryBytes:    int64(cfg.Resources.MemoryMB) * 1024 * 1024,
		NanoCPUs:       int64(cfg.Resources.CPUMillis) * 1_000_000,
		PidsLimit:      int64(cfg.Resources.PidsLimit),
		OomScoreAdj:    OomScoreAdj,
		ReadOnlyRootfs: cfg.ReadOnlyRootfs,
		StopTimeoutS:   cfg.StopGraceS,
		Env:            append([]string(nil), in.Env...),
		LogDriver:      LogDriver,
		LogOpts: map[string]string{
			"max-size": LogMaxSize,
			"max-file": LogMaxFiles,
		},
		Labels: map[string]string{
			LabelManaged:    ManagedValue,
			LabelApp:        in.AppID,
			LabelDeployment: in.DeploymentID,
			LabelConfigHash: in.ConfigHash,
		},
		Ports: []PortBinding{{
			HostPort:      in.HostPort,
			ContainerPort: cfg.Port,
		}},
	}

	// CapAdd: validated by appconfig (only NET_BIND_SERVICE), copied as-is.
	spec.CapAdd = append(spec.CapAdd, cfg.CapAdd...)

	// Tmpfs gets noexec,nosuid unconditionally.
	for _, tf := range cfg.Tmpfs {
		spec.Tmpfs = append(spec.Tmpfs, TmpfsMount{Path: tf.Path, SizeMB: tf.SizeMB})
	}

	// Named volumes only; real names derived by the wrapper (ts-<app>-<name>).
	for _, v := range cfg.Volumes {
		spec.Volumes = append(spec.Volumes, VolumeMount{Name: v.Name, Path: v.Path, ReadOnly: v.ReadOnly})
	}

	// Command and user: empty means the image's own default; when
	// present they are copied verbatim — argv never passes through a shell.
	spec.Command = append(spec.Command, cfg.Command...)
	spec.User = cfg.User

	if err := spec.sane(); err != nil {
		return nil, err
	}
	return spec, nil
}

// DockerVolumeName returns the real name of a managed volume for an app.
func DockerVolumeName(appName, userName string) string {
	return VolumePrefix + appName + "-" + userName
}

// sane is defense in depth: assert the invariants Build must never violate,
// catching refactors that weaken the closed spec.
func (s *ContainerSpec) sane() error {
	if s.Network != NetworkName {
		return fmt.Errorf("policy: network must be %q", NetworkName)
	}
	if !s.CapDropALL || !s.NoNewPrivs || s.RestartPolicy != RestartPolicy {
		return fmt.Errorf("policy: baseline hardening (cap_drop/no-new-privs/restart) must stay enabled")
	}
	if s.OomScoreAdj != OomScoreAdj {
		return fmt.Errorf("policy: oom_score_adj must be %d", OomScoreAdj)
	}
	for _, p := range s.Ports {
		if p.ContainerPort < 1 || p.ContainerPort > 65535 {
			return fmt.Errorf("policy: container port %d invalid", p.ContainerPort)
		}
	}
	for _, cap := range s.CapAdd {
		if cap != appconfig.AllowedCapAdd {
			return fmt.Errorf("policy: capability %q is not on the allowlist", cap)
		}
	}
	return nil
}

// ConfigHash computes the canonical hash recorded on containers and
// deployments. Keys are sorted so the hash is stable.
func ConfigHash(cfg *appconfig.Config) string {
	// Marshal via a canonical copy: JSON field order is fixed by the struct,
	// and slices keep their validated order.
	raw, _ := json.Marshal(cfg)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SortedEnvKeys is a helper for deterministic env output in tests.
func SortedEnvKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
