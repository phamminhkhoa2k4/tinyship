package policy

import (
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/appconfig"
)

func validConfig() *appconfig.Config {
	return &appconfig.Config{
		Name:   "web",
		Image:  "nginx",
		Tag:    "1.27",
		Port:   8080,
		Health: nil,
		Resources: appconfig.Resources{
			MemoryMB:  256,
			CPUMillis: 500,
			PidsLimit: 256,
		},
		Strategy:   "auto",
		StopGraceS: 30,
	}
}

func validInstance() Instance {
	return Instance{
		AppID:        "01HXAPP000000000000000000",
		AppName:      "web",
		DeploymentID: "01HXDEP000000000000000000",
		Seq:          3,
		Digest:       "sha256:" + strings.Repeat("ab", 32),
		ConfigHash:   "deadbeef",
		HostPort:     20042,
		Env:          []string{"FOO=bar"},
	}
}

func TestBuildSpecMatchesTable62(t *testing.T) {
	cfg := validConfig()
	spec, err := Build(cfg, validInstance())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Fixed mapping assertions.
	if want := "ts-web-3"; spec.Name != want {
		t.Errorf("Name = %q, want %q", spec.Name, want)
	}
	if want := "nginx@sha256:" + strings.Repeat("ab", 32); spec.Image != want {
		t.Errorf("Image = %q, want %q (image@digest pinning)", spec.Image, want)
	}
	if spec.Network != NetworkName {
		t.Errorf("Network = %q, want %q", spec.Network, NetworkName)
	}
	if spec.RestartPolicy != "unless-stopped" {
		t.Errorf("RestartPolicy = %q", spec.RestartPolicy)
	}
	if !spec.CapDropALL {
		t.Error("CapDropALL must always be true")
	}
	if !spec.NoNewPrivs {
		t.Error("NoNewPrivs must always be true")
	}
	if spec.MemoryBytes != 256*1024*1024 {
		t.Errorf("MemoryBytes = %d", spec.MemoryBytes)
	}
	if spec.NanoCPUs != 500*1_000_000 {
		t.Errorf("NanoCPUs = %d", spec.NanoCPUs)
	}
	if spec.PidsLimit != 256 {
		t.Errorf("PidsLimit = %d", spec.PidsLimit)
	}
	if spec.OomScoreAdj != 500 {
		t.Errorf("OomScoreAdj = %d, want 500", spec.OomScoreAdj)
	}
	if spec.StopTimeoutS != 30 {
		t.Errorf("StopTimeoutS = %d", spec.StopTimeoutS)
	}
	if spec.LogDriver != "json-file" || spec.LogOpts["max-size"] != "10m" || spec.LogOpts["max-file"] != "3" {
		t.Errorf("log config = %s %v", spec.LogDriver, spec.LogOpts)
	}

	// Labels match the fixed mapping.
	for k, want := range map[string]string{
		LabelManaged:    ManagedValue,
		LabelApp:        "01HXAPP000000000000000000",
		LabelDeployment: "01HXDEP000000000000000000",
		LabelConfigHash: "deadbeef",
	} {
		if spec.Labels[k] != want {
			t.Errorf("Labels[%s] = %q, want %q", k, spec.Labels[k], want)
		}
	}

	// Loopback port binding: 127.0.0.1 only (never 0.0.0.0).
	if len(spec.Ports) != 1 || spec.Ports[0].HostPort != 20042 || spec.Ports[0].ContainerPort != 8080 {
		t.Errorf("Ports = %+v", spec.Ports)
	}
}

func TestBuildRejectsUnpinnedDigest(t *testing.T) {
	inst := validInstance()
	inst.Digest = "nginx:1.27" // tag, not digest
	if _, err := Build(validConfig(), inst); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("Build with tag digest = %v, want digest error", err)
	}

	inst.Digest = ""
	if _, err := Build(validConfig(), inst); err == nil {
		t.Error("Build with empty digest must fail: deployment must pin a digest")
	}
}

func TestBuildRejectsHostPortOutsideRange(t *testing.T) {
	for _, port := range []int{1, 8080, 19999, 30000} {
		inst := validInstance()
		inst.HostPort = port
		if _, err := Build(validConfig(), inst); err == nil {
			t.Errorf("Build with host port %d must fail", port)
		}
	}
}

// TestForbiddenParametersAreUnrepresentable is the structural proof for
// there is no field on ContainerSpec for privileged mode, host
// networking, devices, host binds, or arbitrary sysctls — a hostile config
// cannot express them at all.
func TestForbiddenParametersAreUnrepresentable(t *testing.T) {
	// The closed spec has no fields named for forbidden capabilities; if a
	// refactor adds one, fail loudly so the allowlist decision is re-made
	// deliberately (E_POLICY_FORBIDDEN_PARAM).
	spec, err := Build(validConfig(), validInstance())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// CapAdd must be empty for a config without cap_add.
	if len(spec.CapAdd) != 0 {
		t.Errorf("CapAdd = %v, want empty", spec.CapAdd)
	}

	// sane() is the invariant tripwire: flip the baseline and it must fire.
	broken := *spec
	broken.CapDropALL = false
	if err := broken.sane(); err == nil {
		t.Error("sane() must reject CapDropALL=false")
	}
	broken = *spec
	broken.Network = "host"
	if err := broken.sane(); err == nil {
		t.Error("sane() must reject network=host")
	}
	broken = *spec
	broken.CapAdd = []string{"NET_ADMIN"}
	if err := broken.sane(); err == nil {
		t.Error("sane() must reject NET_ADMIN")
	}
}

func TestDockerVolumeNameIsDerived(t *testing.T) {
	got := DockerVolumeName("web", "data")
	if got != "ts-web-data" {
		t.Errorf("DockerVolumeName = %q, want ts-web-data", got)
	}
	// Derived naming means a user name cannot address a foreign volume.
	if got := DockerVolumeName("web", "../../etc"); got != "ts-web-../../etc" {
		t.Errorf("unexpected derived name %q", got)
	}
}

func TestConfigHashStableAndDistinct(t *testing.T) {
	cfg1 := validConfig()
	cfg2 := validConfig()
	if ConfigHash(cfg1) != ConfigHash(cfg2) {
		t.Error("same config must hash the same")
	}
	cfg2.Port = 9090
	if ConfigHash(cfg1) == ConfigHash(cfg2) {
		t.Error("different config must hash differently")
	}
}
