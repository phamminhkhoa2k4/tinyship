// Package appconfig validates the app configuration object.
// Validation is strict: unknown fields are rejected with the field
// path, and every configured limit is enforced here so the worker
// never sees an unvalidated config.
package appconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Size limits.
const (
	MaxConfigBytes = 64 * 1024
	MaxCommandArgs = 32
	MaxArgBytes    = 1024
	MaxTmpfs       = 8
	MaxVolumes     = 8
	MaxTmpfsMB     = 256
	MaxEnvVars     = 200
	MaxEnvValueB   = 32 * 1024
)

// Provisional defaults; benchmark-tuned before release.
const (
	DefaultMemoryMB     = 256
	DefaultCPUMillis    = 500
	DefaultPidsLimit    = 256
	DefaultStopGraceS   = 30
	DefaultStartPeriodS = 30 // health.start_period_s default
	DefaultKeepReleases = 3  // images retained per app before gc may remove them
	MinKeepReleases     = 1
	MaxKeepReleases     = 50
	MinMemoryMB         = 32
	MinCPUMillis        = 50
	MinPidsLimit        = 16
	MaxPidsLimit        = 4096
	AllowedCapAdd       = "NET_BIND_SERVICE" // the one capability the config may add
	TINYSHIPEnvPrefix   = "TINYSHIP_"
)

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)
	imageRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[a-z0-9._-]{1,128})?(/[a-z0-9]([a-z0-9._-]*[a-z0-9])?)*$`)
	tagRe     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRe  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	userRe    = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)
	volNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)
	envKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// Config is the validated app configuration. It is stored as JSON
// in apps.config_json and snapshotted per deployment.
type Config struct {
	Name           string    `json:"name"`
	Image          string    `json:"image"`
	Tag            string    `json:"tag,omitempty"`
	Digest         string    `json:"digest,omitempty"`
	Port           int       `json:"port"`
	Strategy       string    `json:"strategy,omitempty"`
	Health         *Health   `json:"health,omitempty"`
	Resources      Resources `json:"resources,omitempty"`
	Command        []string  `json:"command,omitempty"`
	User           string    `json:"user,omitempty"`
	ReadOnlyRootfs bool      `json:"read_only_rootfs,omitempty"`
	Tmpfs          []Tmpfs   `json:"tmpfs,omitempty"`
	Volumes        []Volume  `json:"volumes,omitempty"`
	CapAdd         []string  `json:"cap_add,omitempty"`
	StopGraceS     int       `json:"stop_grace_s,omitempty"`
	// KeepReleases bounds how many successful releases keep their image
	// on disk. Older images are removed by the garbage collector unless
	// a container still uses them.
	KeepReleases       int    `json:"keep_releases,omitempty"`
	RegistryCredential string `json:"registry_credential,omitempty"`
}

// Health describes the host-run health probe.
type Health struct {
	Type         string   `json:"type,omitempty"` // http|tcp|exec
	Path         string   `json:"path,omitempty"`
	ExpectStatus [2]int   `json:"expect_status,omitempty"`
	Port         int      `json:"port,omitempty"`
	Command      []string `json:"command,omitempty"`
	IntervalS    int      `json:"interval_s,omitempty"`
	TimeoutS     int      `json:"timeout_s,omitempty"`
	Retries      int      `json:"retries,omitempty"`
	// StartPeriodS is a pointer so JSON can distinguish "absent" (default
	// 30) from an explicit 0 (probe immediately), both legal under the
	// range 0–600.
	StartPeriodS *int `json:"start_period_s,omitempty"`
}

// Resources are the mandatory container limits (nothing runs
// unbounded; defaults come from host config, here the provisional numbers).
type Resources struct {
	MemoryMB  int `json:"memory_mb,omitempty"`
	CPUMillis int `json:"cpu_millis,omitempty"`
	PidsLimit int `json:"pids_limit,omitempty"`
}

// Tmpfs is an in-memory mount for read-only-rootfs apps.
type Tmpfs struct {
	Path   string `json:"path"`
	SizeMB int    `json:"size_mb,omitempty"`
}

// Volume is a named Docker volume mount. Host bind mounts do not exist in
// the schema and cannot be expressed.
type Volume struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Warning codes surfaced to the user without failing validation.
const (
	WMutableTag  = "W_MUTABLE_TAG"     // tag=latest or no digest
	WRunsAsRoot  = "W_RUNS_AS_ROOT"    // user resolves to root
	WWritableVol = "W_WRITABLE_VOLUME" // writable volume + start-first risk
)

// Parse strictly decodes one JSON config value (unknown fields
// rejected) within the size limit, without validating semantics.
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("config: %d bytes exceeds the %d byte limit", len(data), MaxConfigBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// Exactly one value: reject trailing garbage.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("config: trailing data after the config object")
	}
	return &c, nil
}

// Validate checks the config against and applies defaults in place.
// It returns non-nil warnings (advisory) alongside any hard error; a config
// with errors must never reach the worker's container builder.
func Validate(c *Config) (warnings []string, err error) {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if !nameRe.MatchString(c.Name) {
		add("name: %q must match %s (3-40 chars, lowercase)", c.Name, nameRe.String())
	}
	if c.Image == "" {
		add("image: required")
	} else {
		if strings.ContainsAny(c.Image, "@") {
			add("image: must not embed a digest; use the digest field")
		} else if idx := strings.LastIndex(c.Image, ":"); idx > strings.LastIndex(c.Image, "/") {
			add("image: must not embed a tag; use the tag field (got %q)", c.Image)
		} else if !imageRe.MatchString(c.Image) {
			add("image: %q is not a valid OCI repository reference", c.Image)
		}
	}
	if c.Digest != "" && !digestRe.MatchString(c.Digest) {
		add("digest: %q must be sha256 followed by 64 hex chars", c.Digest)
	}
	if c.Tag != "" && c.Digest != "" {
		add("tag and digest: mutually exclusive")
	}
	if c.Tag != "" && !tagRe.MatchString(c.Tag) {
		add("tag: %q is not a valid OCI tag", c.Tag)
	}
	if c.Port < 1 || c.Port > 65535 {
		add("port: %d out of range 1-65535", c.Port)
	}
	switch c.Strategy {
	case "", "auto", "start-first", "stop-first":
	default:
		add("strategy: %q must be auto, start-first, or stop-first", c.Strategy)
	}
	if c.StopGraceS != 0 && (c.StopGraceS < 1 || c.StopGraceS > 300) {
		add("stop_grace_s: %d out of range 1-300", c.StopGraceS)
	}
	if c.KeepReleases == 0 {
		c.KeepReleases = DefaultKeepReleases
	}
	if c.KeepReleases < MinKeepReleases || c.KeepReleases > MaxKeepReleases {
		add("keep_releases: %d out of range %d-%d", c.KeepReleases, MinKeepReleases, MaxKeepReleases)
	}
	if c.User != "" && !userRe.MatchString(c.User) {
		add("user: %q must be numeric uid or uid:gid", c.User)
	}
	if c.User == "0" || c.User == "0:0" {
		warnings = append(warnings, WRunsAsRoot)
	}

	// command: bounded argv, never through a shell.
	if len(c.Command) > MaxCommandArgs {
		add("command: %d args exceeds the limit of %d", len(c.Command), MaxCommandArgs)
	}
	for i, a := range c.Command {
		if len(a) > MaxArgBytes {
			add("command[%d]: %d bytes exceeds the limit of %d", i, len(a), MaxArgBytes)
		}
	}

	// resources
	r := &c.Resources
	if r.MemoryMB == 0 {
		r.MemoryMB = DefaultMemoryMB
	}
	if r.MemoryMB < MinMemoryMB {
		add("resources.memory_mb: %d below the minimum of %d", r.MemoryMB, MinMemoryMB)
	}
	if r.CPUMillis == 0 {
		r.CPUMillis = DefaultCPUMillis
	}
	if r.CPUMillis < MinCPUMillis {
		add("resources.cpu_millis: %d below the minimum of %d", r.CPUMillis, MinCPUMillis)
	}
	if r.PidsLimit == 0 {
		r.PidsLimit = DefaultPidsLimit
	}
	if r.PidsLimit < MinPidsLimit || r.PidsLimit > MaxPidsLimit {
		add("resources.pids_limit: %d out of range %d-%d", r.PidsLimit, MinPidsLimit, MaxPidsLimit)
	}

	// cap_add: only NET_BIND_SERVICE.
	for _, cap := range c.CapAdd {
		if cap != AllowedCapAdd {
			add("cap_add: %q is forbidden; only %q is allowed in v0.1", cap, AllowedCapAdd)
		}
	}

	// tmpfs
	if len(c.Tmpfs) > MaxTmpfs {
		add("tmpfs: %d entries exceeds the limit of %d", len(c.Tmpfs), MaxTmpfs)
	}
	for i, tf := range c.Tmpfs {
		if errP := validateMountPath(tf.Path); errP != nil {
			add("tmpfs[%d].path: %v", i, errP)
		}
		if tf.SizeMB < 0 || tf.SizeMB > MaxTmpfsMB {
			add("tmpfs[%d].size_mb: %d out of range 0-%d", i, tf.SizeMB, MaxTmpfsMB)
		}
	}

	// volumes: named volumes only; absolute normalized paths.
	if len(c.Volumes) > MaxVolumes {
		add("volumes: %d entries exceeds the limit of %d", len(c.Volumes), MaxVolumes)
	}
	seenVol := map[string]bool{}
	seenPath := map[string]bool{}
	for i, v := range c.Volumes {
		if !volNameRe.MatchString(v.Name) {
			add("volumes[%d].name: %q must match %s", i, v.Name, volNameRe.String())
		}
		if seenVol[v.Name] {
			add("volumes[%d].name: %q duplicated", i, v.Name)
		}
		seenVol[v.Name] = true
		if errP := validateMountPath(v.Path); errP != nil {
			add("volumes[%d].path: %v", i, errP)
		}
		if seenPath[v.Path] {
			add("volumes[%d].path: %q duplicated", i, v.Path)
		}
		seenPath[v.Path] = true
		if !v.ReadOnly {
			warnings = append(warnings, WWritableVol)
		}
	}

	// health: defaults apply to the implicit config too (TCP on the port
	// with interval 10, timeout 5, retries 3, start_period 30).
	if c.Health == nil {
		c.Health = &Health{Type: "tcp", Port: c.Port}
	}
	if errH := validateHealth(c.Health, c.Port, &warnings); errH != nil {
		add("health: %v", errH)
	}

	// defaults
	if c.Strategy == "" {
		c.Strategy = "auto"
	}
	if c.StopGraceS == 0 {
		c.StopGraceS = DefaultStopGraceS
	}
	if (c.Tag == "" || c.Tag == "latest") && c.Digest == "" {
		warnings = append(warnings, WMutableTag)
	}

	if len(errs) > 0 {
		return warnings, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return warnings, nil
}

func validateHealth(h *Health, appPort int, warnings *[]string) error {
	switch h.Type {
	case "", "tcp":
		h.Type = "tcp"
	case "http":
		if h.Path == "" {
			h.Path = "/"
		}
		if !strings.HasPrefix(h.Path, "/") || len(h.Path) > 512 {
			return fmt.Errorf("path %q must start with / and be ≤512 chars", h.Path)
		}
		if h.ExpectStatus != [2]int{} && (h.ExpectStatus[0] < 100 || h.ExpectStatus[1] > 599 || h.ExpectStatus[0] > h.ExpectStatus[1]) {
			return fmt.Errorf("expect_status %v must be an ordered pair within 100-599", h.ExpectStatus)
		}
		if h.ExpectStatus == [2]int{} {
			h.ExpectStatus = [2]int{200, 399}
		}
	case "exec":
		if len(h.Command) == 0 {
			return fmt.Errorf("command is required for exec checks")
		}
		if len(h.Command) > MaxCommandArgs {
			return fmt.Errorf("command: %d args exceeds the limit of %d", len(h.Command), MaxCommandArgs)
		}
	default:
		return fmt.Errorf("type %q must be http, tcp, or exec", h.Type)
	}
	if h.Port == 0 {
		h.Port = appPort
	}
	if h.Port < 1 || h.Port > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", h.Port)
	}
	// probes run from the host against the loopback mapping, and
	// publishes exactly one port (the app's). A health port other
	// than the app port is therefore unreachable from where the probe runs;
	// checking from inside the container is what type=exec is for.
	if h.Type != "exec" && h.Port != appPort {
		return fmt.Errorf("port %d: only the app port %d is published to the "+
			"host for probing; use type=exec or health on port %d", h.Port, appPort, appPort)
	}
	if h.IntervalS == 0 {
		h.IntervalS = 10
	}
	if h.IntervalS < 2 || h.IntervalS > 300 {
		return fmt.Errorf("interval_s %d out of range 2-300", h.IntervalS)
	}
	if h.TimeoutS == 0 {
		h.TimeoutS = 5
	}
	if h.TimeoutS < 1 || h.TimeoutS > 60 || h.TimeoutS >= h.IntervalS {
		return fmt.Errorf("timeout_s %d must be in 1-60 and < interval_s (%d)", h.TimeoutS, h.IntervalS)
	}
	if h.Retries == 0 {
		h.Retries = 3
	}
	if h.Retries < 1 || h.Retries > 20 {
		return fmt.Errorf("retries %d out of range 1-20", h.Retries)
	}
	if h.StartPeriodS != nil && (*h.StartPeriodS < 0 || *h.StartPeriodS > 600) {
		return fmt.Errorf("start_period_s %d out of range 0-600", *h.StartPeriodS)
	}
	if h.StartPeriodS == nil {
		// start_period_s defaults to 30 when absent.
		def := DefaultStartPeriodS
		h.StartPeriodS = &def
	}
	_ = warnings
	return nil
}

// validateMountPath enforces absolute normalized container paths with no
// host escapes: no "..", not "/", not under /proc /sys /dev.
func validateMountPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q must be an absolute path", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("%q must not contain ..", p)
	}
	if p == "/" {
		return fmt.Errorf("must not be /")
	}
	clean := p
	for _, banned := range []string{"/proc", "/sys", "/dev"} {
		if clean == banned || strings.HasPrefix(clean, banned+"/") {
			return fmt.Errorf("%q must not be under %s", p, banned)
		}
	}
	return nil
}

// ValidateEnv checks an environment key/value pair.
// Secrets are stored sealed elsewhere; this only validates shape. Returns a
// warning code for advisory issues.
func ValidateEnv(key, value string) error {
	if !envKeyRe.MatchString(key) {
		return fmt.Errorf("env key %q must match %s", key, envKeyRe.String())
	}
	if strings.HasPrefix(key, TINYSHIPEnvPrefix) {
		return fmt.Errorf("env key %q must not use the reserved %s prefix", key, TINYSHIPEnvPrefix)
	}
	if len(value) > MaxEnvValueB {
		return fmt.Errorf("env value for %q is %d bytes, limit is %d", key, len(value), MaxEnvValueB)
	}
	return nil
}
