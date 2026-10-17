package appconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Name:  "web",
		Image: "nginx",
		Tag:   "1.27",
		Port:  8080,
	}
}

// --- Parse: strict decoding -----------------------------------

func TestParseRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{"name":"web","image":"nginx","port":80,"privaliged":true}`)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), "privaliged") {
		t.Fatalf("unknown field = %v, want error naming the field", err)
	}
	// The field path must be identifiable.
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error should say the field is unknown: %v", err)
	}
}

func TestParseRejectsTrailingDataAndOversize(t *testing.T) {
	if _, err := Parse([]byte(`{"name":"web"} {"name":"web"}`)); err == nil ||
		!strings.Contains(err.Error(), "trailing") {
		t.Errorf("trailing data = %v, want rejection", err)
	}
	big := append([]byte(`{"name":"`), make([]byte, MaxConfigBytes+1)...)
	big = append(big, `"}`...)
	if _, err := Parse(big); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversize = %v, want size-limit rejection", err)
	}
}

func TestParseRoundTripsValidConfig(t *testing.T) {
	raw, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// --- Validate: field rules ---------------------------------

func TestValidateNameRules(t *testing.T) {
	for _, name := range []string{"ab", "UPPER", "has_underscore", "-starts", "ends-", strings.Repeat("a", 41)} {
		c := validConfig()
		c.Name = name
		if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "name:") {
			t.Errorf("name %q = %v, want name: error", name, err)
		}
	}
	for _, name := range []string{"abc", "web", "my-app-1", strings.Repeat("a", 40)} {
		c := validConfig()
		c.Name = name
		if _, err := Validate(c); err != nil {
			t.Errorf("valid name %q rejected: %v", name, err)
		}
	}
}

func TestValidateImageTagDigestRules(t *testing.T) {
	c := validConfig()
	c.Image = "nginx:1.27" // tag embedded in image
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "tag field") {
		t.Errorf("embedded tag = %v", err)
	}

	c = validConfig()
	c.Image = "nginx@sha256:" + strings.Repeat("a", 64)
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "digest field") {
		t.Errorf("embedded digest = %v", err)
	}

	c = validConfig()
	c.Image = ""
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "image: required") {
		t.Errorf("missing image = %v", err)
	}

	c = validConfig()
	c.Tag = "1.27"
	c.Digest = "sha256:" + strings.Repeat("a", 64)
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("tag+digest = %v, want mutual exclusion error", err)
	}

	c = validConfig()
	c.Digest = "sha256:short"
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "digest:") {
		t.Errorf("bad digest = %v", err)
	}
}

func TestValidatePortStrategyResources(t *testing.T) {
	c := validConfig()
	c.Port = 0
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "port:") {
		t.Errorf("port 0 = %v", err)
	}

	c = validConfig()
	c.Strategy = "blue-green"
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "strategy:") {
		t.Errorf("bad strategy = %v", err)
	}

	// Defaults applied in place: 256MB / 500m / 256 pids / 30s.
	c = validConfig()
	if _, err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Resources.MemoryMB != DefaultMemoryMB || c.Resources.CPUMillis != DefaultCPUMillis ||
		c.Resources.PidsLimit != DefaultPidsLimit || c.StopGraceS != DefaultStopGraceS {
		t.Errorf("defaults not applied: %+v grace=%d", c.Resources, c.StopGraceS)
	}

	// Min bounds enforced.
	c = validConfig()
	c.Resources.MemoryMB = MinMemoryMB - 1
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "memory_mb") {
		t.Errorf("memory below min = %v", err)
	}
	c = validConfig()
	c.Resources.CPUMillis = MinCPUMillis - 1
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "cpu_millis") {
		t.Errorf("cpu below min = %v", err)
	}
	c = validConfig()
	c.Resources.PidsLimit = MaxPidsLimit + 1
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "pids_limit") {
		t.Errorf("pids above max = %v", err)
	}
}

func TestValidateCapAddOnlyAllowlisted(t *testing.T) {
	c := validConfig()
	c.CapAdd = []string{"NET_ADMIN"}
	_, err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "E_POLICY_FORBIDDEN") &&
		!strings.Contains(err.Error(), "forbidden") {
		t.Errorf("NET_ADMIN = %v, want forbidden", err)
	}

	c = validConfig()
	c.CapAdd = []string{AllowedCapAdd}
	if _, err := Validate(c); err != nil {
		t.Errorf("NET_BIND_SERVICE rejected: %v", err)
	}
}

func TestValidateMountsRejectHostEscapes(t *testing.T) {
	for _, path := range []string{"relative/path", "/has/../escape", "/", "/proc/self", "/sys", "/dev"} {
		c := validConfig()
		c.Tmpfs = []Tmpfs{{Path: path}}
		if _, err := Validate(c); err == nil {
			t.Errorf("tmpfs path %q accepted, must be rejected", path)
		}
	}
	// Named volumes only; the name regex is what keeps DockerVolumeName sane.
	c := validConfig()
	c.Volumes = []Volume{{Name: "../../etc", Path: "/data"}}
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "volumes[0].name") {
		t.Errorf("hostile volume name = %v", err)
	}
	// Duplicates rejected.
	c = validConfig()
	c.Volumes = []Volume{{Name: "data", Path: "/a"}, {Name: "data", Path: "/b"}}
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("duplicate volume = %v", err)
	}
}

func TestValidateCommandUserAndEnvShape(t *testing.T) {
	c := validConfig()
	c.Command = make([]string, MaxCommandArgs+1)
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "command:") {
		t.Errorf("too many args = %v", err)
	}
	c = validConfig()
	c.Command = []string{strings.Repeat("x", MaxArgBytes+1)}
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "command[0]") {
		t.Errorf("oversized arg = %v", err)
	}

	c = validConfig()
	c.User = "root" // must be numeric
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "user:") {
		t.Errorf("non-numeric user = %v", err)
	}

	// Warnings are advisory, not errors.
	c = validConfig()
	c.User = "0"
	warns, err := Validate(c)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !contains(warns, WRunsAsRoot) {
		t.Errorf("warnings = %v, want %s", warns, WRunsAsRoot)
	}
	c = validConfig() // tag latest + no digest
	c.Tag = ""
	warns, err = Validate(c)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !contains(warns, WMutableTag) {
		t.Errorf("warnings = %v, want %s", warns, WMutableTag)
	}
}

func TestValidateEnvRules(t *testing.T) {
	if err := ValidateEnv("1BAD", "x"); err == nil {
		t.Error("invalid env key accepted")
	}
	if err := ValidateEnv("TINYSHIP_SECRET", "x"); err == nil ||
		!strings.Contains(err.Error(), "reserved") {
		t.Errorf("reserved prefix = %v, want reserved-prefix rejection", err)
	}
	if err := ValidateEnv("OK", strings.Repeat("v", MaxEnvValueB+1)); err == nil {
		t.Error("oversized env value accepted")
	}
	if err := ValidateEnv("MODE", "prod"); err != nil {
		t.Errorf("valid env rejected: %v", err)
	}
}

// --- health defaults and rules -----------------------

func TestValidateHealthDefaults(t *testing.T) {
	// Implicit health: TCP on the app port with defaults.
	c := validConfig()
	if _, err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	h := c.Health
	if h.Type != "tcp" || h.Port != c.Port {
		t.Errorf("implicit health = %+v, want tcp on app port", h)
	}
	if h.IntervalS != 10 || h.TimeoutS != 5 || h.Retries != 3 {
		t.Errorf("interval/timeout/retries = %d/%d/%d, want 10/5/3", h.IntervalS, h.TimeoutS, h.Retries)
	}
	if h.StartPeriodS == nil || *h.StartPeriodS != DefaultStartPeriodS {
		t.Errorf("start_period_s = %v, want default %d", h.StartPeriodS, DefaultStartPeriodS)
	}

	// Explicit start_period_s: 0 must survive as 0, not become 30.
	c = validConfig()
	c.Health = &Health{Type: "tcp", StartPeriodS: intPtr(0)}
	if _, err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Health.StartPeriodS == nil || *c.Health.StartPeriodS != 0 {
		t.Errorf("explicit 0 became %v", c.Health.StartPeriodS)
	}
}

func TestValidateHealthHTTPAndExec(t *testing.T) {
	c := validConfig()
	c.Health = &Health{Type: "http"} // path + expect_status defaults
	if _, err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Health.Path != "/" || c.Health.ExpectStatus != [2]int{200, 399} {
		t.Errorf("http defaults = %+v", c.Health)
	}

	c = validConfig()
	c.Health = &Health{Type: "http", Path: "no-slash"}
	if _, err := Validate(c); err == nil {
		t.Error("path without leading / accepted")
	}

	c = validConfig()
	c.Health = &Health{Type: "http", ExpectStatus: [2]int{599, 100}}
	if _, err := Validate(c); err == nil {
		t.Error("reversed expect_status accepted")
	}

	c = validConfig()
	c.Health = &Health{Type: "exec"} // command required
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Errorf("exec without command = %v", err)
	}

	c = validConfig()
	c.Health = &Health{Type: "udp"}
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "type") {
		t.Errorf("unknown health type = %v", err)
	}

	// timeout must be < interval.
	c = validConfig()
	c.Health = &Health{Type: "tcp", IntervalS: 5, TimeoutS: 5}
	if _, err := Validate(c); err == nil || !strings.Contains(err.Error(), "timeout_s") {
		t.Errorf("timeout >= interval = %v", err)
	}
}

// TestValidateHealthPortMustBePublished is the coherence rule: only
// the app port is published loopback-only, so probing any other
// port from the host is impossible — reject loudly at validation time.
func TestValidateHealthPortMustBePublished(t *testing.T) {
	c := validConfig() // port 8080
	c.Health = &Health{Type: "tcp", Port: 9090}
	_, err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "only the app port 8080") {
		t.Fatalf("health.port 9090 on app port 8080 = %v, want publication error", err)
	}

	// exec checks run inside the container: any port is meaningless there.
	c = validConfig()
	c.Health = &Health{Type: "exec", Command: []string{"true"}, Port: 9090}
	if _, err := Validate(c); err != nil {
		t.Errorf("exec health with unrelated port rejected: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func intPtr(v int) *int { return &v }
