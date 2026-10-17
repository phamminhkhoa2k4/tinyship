package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/tinyship/tinyship/internal/policy"
)

func specFixture() *policy.ContainerSpec {
	return &policy.ContainerSpec{
		Name:          "ts-web-3",
		AppName:       "web",
		Image:         "nginx@sha256:" + strings.Repeat("ab", 32),
		Network:       policy.NetworkName,
		Ports:         []policy.PortBinding{{HostPort: 20042, ContainerPort: 8080}},
		RestartPolicy: policy.RestartPolicy,
		CapDropALL:    true,
		NoNewPrivs:    true,
		MemoryBytes:   256 * 1024 * 1024,
		NanoCPUs:      500 * 1_000_000,
		PidsLimit:     256,
		OomScoreAdj:   500,
		StopTimeoutS:  30,
		Labels:        map[string]string{policy.LabelManaged: policy.ManagedValue, policy.LabelApp: "app-1"},
		LogDriver:     policy.LogDriver,
		LogOpts:       map[string]string{"max-size": "10m", "max-file": "3"},
		Tmpfs:         []policy.TmpfsMount{{Path: "/run", SizeMB: 64}},
		Volumes:       []policy.VolumeMount{{Name: "data", Path: "/var/lib/app", ReadOnly: true}},
	}
}

// TestHostConfigTable62 asserts the full parameter mapping.
func TestHostConfigTable62(t *testing.T) {
	hc := hostConfigFor(specFixture())

	if hc.NetworkMode != container.NetworkMode(policy.NetworkName) {
		t.Errorf("NetworkMode = %q, want %q", hc.NetworkMode, policy.NetworkName)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyMode(policy.RestartPolicy) {
		t.Errorf("RestartPolicy = %q", hc.RestartPolicy.Name)
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	if len(hc.SecurityOpt) != 1 || hc.SecurityOpt[0] != policy.SecurityOptNNP {
		t.Errorf("SecurityOpt = %v, want %v", hc.SecurityOpt, []string{policy.SecurityOptNNP})
	}
	if hc.Resources.Memory != 256*1024*1024 {
		t.Errorf("Memory = %d", hc.Resources.Memory)
	}
	if hc.Resources.MemorySwap != hc.Resources.Memory {
		t.Errorf("MemorySwap = %d must equal Memory = %d (no swap)",
			hc.Resources.MemorySwap, hc.Resources.Memory)
	}
	if hc.Resources.NanoCPUs != 500*1_000_000 {
		t.Errorf("NanoCPUs = %d", hc.Resources.NanoCPUs)
	}
	if hc.Resources.PidsLimit == nil || *hc.Resources.PidsLimit != 256 {
		t.Errorf("PidsLimit = %v", hc.Resources.PidsLimit)
	}
	if hc.OomScoreAdj != 500 {
		t.Errorf("OomScoreAdj = %d", hc.OomScoreAdj)
	}
	if hc.LogConfig.Type != "json-file" || hc.LogConfig.Config["max-size"] != "10m" || hc.LogConfig.Config["max-file"] != "3" {
		t.Errorf("LogConfig = %+v", hc.LogConfig)
	}

	// Tmpfs gets noexec,nosuid.
	if len(hc.Tmpfs) != 1 {
		t.Fatalf("Tmpfs = %v", hc.Tmpfs)
	}
	if got := hc.Tmpfs["/run"]; !strings.Contains(got, "noexec") || !strings.Contains(got, "nosuid") || !strings.Contains(got, "size=64m") {
		t.Errorf("Tmpfs[/run] = %q, want noexec,nosuid,size=64m", got)
	}

	// Named volume is derived (ts-web-data), read-only preserved.
	if len(hc.Binds) != 1 || hc.Binds[0] != "ts-web-data:/var/lib/app:ro" {
		t.Errorf("Binds = %v, want [ts-web-data:/var/lib/app:ro]", hc.Binds)
	}

	// Port binding is loopback-only.
	bindings := hc.PortBindings["8080/tcp"]
	if len(bindings) != 1 {
		t.Fatalf("PortBindings = %v", hc.PortBindings)
	}
	if bindings[0].HostIP != "127.0.0.1" {
		t.Errorf("HostIP = %q, want 127.0.0.1 (never published to the Internet)", bindings[0].HostIP)
	}
	if bindings[0].HostPort != "20042" {
		t.Errorf("HostPort = %q", bindings[0].HostPort)
	}
}

// TestHostConfigForbiddenParamsZero is the negative direction of
// AC3: the always-rejected parameters must remain zero-valued even for a
// maximally-privileged-looking spec (there is no way to set them from
// policy, so this guards the translation layer itself).
func TestHostConfigForbiddenParamsZero(t *testing.T) {
	spec := specFixture()
	spec.CapAdd = []string{"NET_BIND_SERVICE"} // the only allowed relaxation

	hc := hostConfigFor(spec)

	if hc.Privileged {
		t.Error("Privileged must never be set")
	}
	if hc.NetworkMode != container.NetworkMode(policy.NetworkName) || hc.NetworkMode == "host" {
		t.Errorf("NetworkMode = %q, host networking is forbidden", hc.NetworkMode)
	}
	if hc.PidMode != "" || hc.IpcMode != "" || hc.UTSMode != "" || hc.UsernsMode != "" {
		t.Errorf("namespace modes must stay empty: Pid=%q Ipc=%q UTS=%q Userns=%q",
			hc.PidMode, hc.IpcMode, hc.UTSMode, hc.UsernsMode)
	}
	if len(hc.Devices) != 0 || len(hc.DeviceCgroupRules) != 0 {
		t.Error("devices/device rules must never be set")
	}
	if len(hc.Sysctls) != 0 {
		t.Error("sysctls must never be set")
	}
	if len(hc.ExtraHosts) != 0 {
		t.Error("extra_hosts must never be set")
	}
	if hc.PublishAllPorts {
		t.Error("PublishAllPorts must never be set (ports are explicit loopback bindings)")
	}
	// CapAdd allowlist: only NET_BIND_SERVICE reaches Docker.
	for _, cap := range hc.CapAdd {
		if cap != "NET_BIND_SERVICE" {
			t.Errorf("CapAdd %q escaped the allowlist", cap)
		}
	}
	// Security opt cannot be downgraded: exactly no-new-privileges, nothing
	// like seccomp=unconfined or apparmor=unconfined.
	for _, opt := range hc.SecurityOpt {
		if strings.Contains(opt, "unconfined") {
			t.Errorf("SecurityOpt %q downgrades hardening", opt)
		}
	}
}

func TestContainerConfigImageIsPinned(t *testing.T) {
	cc := containerConfigFor(specFixture())
	if !strings.HasPrefix(cc.Image, "nginx@sha256:") {
		t.Errorf("Image = %q, want repository@digest (never a tag)", cc.Image)
	}
	if cc.Labels[policy.LabelManaged] != policy.ManagedValue {
		t.Errorf("Labels missing managed marker: %v", cc.Labels)
	}
	if cc.StopTimeout == nil || *cc.StopTimeout != 30 {
		t.Errorf("StopTimeout = %v, want 30", cc.StopTimeout)
	}
}

// --- ownership --------------------------------------------------

func fakeInspector(info *ContainerInfo, err error) Inspector {
	return func(_ context.Context, _ string) (*ContainerInfo, error) { return info, err }
}

func TestAssertOwnedThreeChecks(t *testing.T) {
	const appID = "app-1"
	owned := func(ctx context.Context, id, container string) (bool, error) { return true, nil }

	// Happy path: all three checks pass.
	good := fakeInspector(&ContainerInfo{
		ID:     "c1",
		Labels: map[string]string{policy.LabelManaged: policy.ManagedValue, policy.LabelApp: appID},
	}, nil)
	if err := AssertOwned(context.Background(), good, appID, "c1", owned); err != nil {
		t.Errorf("owned container rejected: %v", err)
	}

	// Check 1: not managed by TinyShip.
	foreign := fakeInspector(&ContainerInfo{ID: "c2", Labels: map[string]string{}}, nil)
	if err := AssertOwned(context.Background(), foreign, appID, "c2", owned); err == nil || !strings.Contains(err.Error(), "E_NOT_MANAGED") {
		t.Errorf("foreign container = %v, want E_NOT_MANAGED", err)
	}

	// Check 2: managed but for a different app.
	otherApp := fakeInspector(&ContainerInfo{
		ID:     "c3",
		Labels: map[string]string{policy.LabelManaged: policy.ManagedValue, policy.LabelApp: "app-2"},
	}, nil)
	if err := AssertOwned(context.Background(), otherApp, appID, "c3", owned); err == nil || !strings.Contains(err.Error(), "app-2") {
		t.Errorf("cross-app container = %v, want refusal naming app-2", err)
	}

	// Check 3: labels fine but no deployment of this app references it.
	notInDB := fakeInspector(&ContainerInfo{
		ID:     "c4",
		Labels: map[string]string{policy.LabelManaged: policy.ManagedValue, policy.LabelApp: appID},
	}, nil)
	neverOwns := func(context.Context, string, string) (bool, error) { return false, nil }
	if err := AssertOwned(context.Background(), notInDB, appID, "c4", neverOwns); err == nil || !strings.Contains(err.Error(), "deployment") {
		t.Errorf("unrecorded container = %v, want deployment lookup refusal", err)
	}

	// No app id in payload: refuse before touching anything.
	if err := AssertOwned(context.Background(), good, "", "c1", owned); err == nil {
		t.Error("empty app id must be refused")
	}
}
