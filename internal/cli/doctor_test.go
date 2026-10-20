package cli

// Host-diagnosis tests: every verdict is driven through the injection
// points so a test host needs no real docker, proxy or services
// underneath it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

// doctorRoot builds an install-root whose files exist with the modes
// the install table expects, including a migrated database.
func doctorRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	db, config, keys, token, _ := backupPaths(root)
	for _, dir := range []string{
		filepath.Dir(db), filepath.Dir(config), keys, filepath.Dir(token),
		filepath.Join(root, "var/lib/tinyship", "backups"),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// store.Open creates and migrates the schema the check will read.
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}
	s.Close()
	for path, mode := range map[string]os.FileMode{
		config:                             0o640,
		filepath.Join(keys, "secrets.key"): 0o400,
		filepath.Join(keys, "secrets.pub"): 0o640,
		token:                              0o640,
		db:                                 0o660,
	} {
		if path != db {
			if err := os.WriteFile(path, []byte("x"), mode); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %s: %v", path, err)
		}
	}
	return root
}

// doctorHealthy stubs the host's moving parts into a passing state.
// Tests override individual pieces on top of it.
func doctorHealthy(t *testing.T) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("socket file: %v", err)
	}
	t.Setenv("TINYSHIP_CADDY_ADMIN", sock)

	stubDockerAPI(t, "1.56", nil)
	stubDoctor(t, &doctorLookPath, func(string) (string, error) { return "/usr/bin/found", nil })
	stubDoctor(t, &doctorCaddyAdmin, func(context.Context, string) error { return nil })
	stubDoctor(t, &doctorStatfs, func(string) (uint64, error) { return 20000, nil })
	stubSystemctl(t, "active", nil)
}

// stubDockerAPI answers the docker version probe.
func stubDockerAPI(t *testing.T, out string, err error) {
	t.Helper()
	prev := doctorRunDocker
	doctorRunDocker = func(context.Context, ...string) ([]byte, error) {
		return []byte(out), err
	}
	t.Cleanup(func() { doctorRunDocker = prev })
}

// stubSystemctl answers both is-active probes with one state.
func stubSystemctl(t *testing.T, state string, err error) {
	t.Helper()
	prev := doctorSystemctl
	doctorSystemctl = func(context.Context, ...string) ([]byte, error) {
		return []byte(state), err
	}
	t.Cleanup(func() { doctorSystemctl = prev })
}

// stubDoctor swaps a single injection point; T and F share a signature
// only in spirit, so each call site passes a matching closure.
func stubDoctor[T any](t *testing.T, target *T, replacement T) {
	t.Helper()
	prev := *target
	*target = replacement
	t.Cleanup(func() { *target = prev })
}

func TestDoctorAllPassExitsZero(t *testing.T) {
	doctorHealthy(t)
	stubReconcile(t, map[string]bool{}, nil)
	root := doctorRoot(t)
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps":
			writeJSON(w, http.StatusOK, map[string]any{"apps": []any{}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "doctor", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"PASS docker", "PASS caddy", "PASS disk", "PASS dns", "0 fail"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = run(t, "doctor", "--json", "--install-root", root)
	if code != 0 {
		t.Fatalf("--json exit = %d, want 0; stdout = %q", code, stdout)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, stdout)
	}
	if len(report.Checks) == 0 {
		t.Fatalf("report has no checks: %s", stdout)
	}
	if report.Summary.Fail != 0 || report.Summary.Warn != 0 || report.Summary.Pass != len(report.Checks) {
		t.Errorf("summary = %+v over %d checks, want all pass", report.Summary, len(report.Checks))
	}
	for _, chk := range report.Checks {
		if chk.Name == "" || chk.Detail == "" {
			t.Errorf("check with empty name or detail: %+v", chk)
		}
	}
}

func TestDoctorWarnsBetweenDeployFloorAndInstallerMinimum(t *testing.T) {
	doctorHealthy(t)
	stubDoctor(t, &doctorStatfs, func(string) (uint64, error) { return 4000, nil })
	stubReconcile(t, map[string]bool{}, nil)
	root := doctorRoot(t)
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps":
			writeJSON(w, http.StatusOK, map[string]any{"apps": []any{}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "doctor", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warn is not fail); stderr = %q\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"WARN disk", "below the installer minimum 5120 MiB", "above the deploy floor 3072 MiB",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestDoctorFailsBelowConfiguredDeployFloor(t *testing.T) {
	doctorHealthy(t)
	stubDoctor(t, &doctorStatfs, func(string) (uint64, error) { return 3500, nil })
	stubReconcile(t, map[string]bool{}, nil)
	root := doctorRoot(t)
	db, _, _, _, _ := backupPaths(root)
	s, err := store.OpenPlain(db)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := s.SetMinFreeDiskMB(context.Background(), 4000); err != nil {
		t.Fatalf("set disk floor: %v", err)
	}
	s.Close()
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps":
			writeJSON(w, http.StatusOK, map[string]any{"apps": []any{}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, _ := run(t, "doctor", "--install-root", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 below the deploy floor\n%s", code, stdout)
	}
	for _, want := range []string{
		"FAIL disk", "below the deploy floor 4000 MiB", "admission will refuse a deploy",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestCheckDiskDoesNotCreateMissingDatabase(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/lib/tinyship"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, _, _, _, _ := backupPaths(root)
	stubDoctor(t, &doctorStatfs, func(string) (uint64, error) { return 20000, nil })

	checks := checkDisk(context.Background(), root)
	if len(checks) != 1 || checks[0].Status != doctorPass {
		t.Fatalf("checks = %+v, want one passing disk check", checks)
	}
	if _, err := os.Lstat(db); !os.IsNotExist(err) {
		t.Errorf("doctor created a database at %s (err = %v); it must stay read-only", db, err)
	}
}

func TestDoctorFailReturnsNonZeroWithHints(t *testing.T) {
	doctorHealthy(t)
	// The engine is gone and both units are down: three failures.
	stubDockerAPI(t, "", errors.New("Cannot connect to the Docker daemon"))
	stubSystemctl(t, "inactive", errors.New("inactive"))
	stubReconcile(t, map[string]bool{}, nil)
	root := doctorRoot(t)
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps":
			writeJSON(w, http.StatusOK, map[string]any{"apps": []any{}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "doctor", "--install-root", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (a failing check must be non-zero); stderr = %q\n%s",
			code, stderr, stdout)
	}
	for _, want := range []string{
		"FAIL docker",
		"fix: install Docker Engine",
		"FAIL tinyship-api.service",
		"fix: systemctl start tinyship-api.service",
		"3 fail",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = run(t, "doctor", "--json", "--install-root", root)
	if code != 1 {
		t.Fatalf("--json exit = %d, want 1", code)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, stdout)
	}
	if report.Summary.Fail != 3 {
		t.Errorf("summary.fail = %d, want 3: %s", report.Summary.Fail, stdout)
	}
	// Every non-pass check carries a remediation hint.
	for _, chk := range report.Checks {
		if chk.Status != doctorPass && chk.Hint == "" {
			t.Errorf("%s (%s) has no fix hint", chk.Name, chk.Status)
		}
	}
}

func TestDoctorWarnsOnUnresolvedDNSWithoutFailing(t *testing.T) {
	doctorHealthy(t)
	// The declared domain does not resolve: warn, but keep exit 0 —
	// DNS may still be propagating.
	stubDoctor(t, &doctorLookupHost, func(string) ([]string, error) {
		return nil, errors.New("no such host")
	})
	stubReconcile(t, map[string]bool{"app-1": true}, nil)
	root := doctorRoot(t)
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps":
			writeJSON(w, http.StatusOK, map[string]any{"apps": []map[string]any{
				{"id": "app-1", "name": "web", "image": "nginx", "tag": "v1", "desired_state": "running"},
			}})
		case "/apps/web/domains":
			writeJSON(w, http.StatusOK, map[string]any{"domains": []map[string]any{
				{"id": "d1", "hostname": "web.example.test", "status": "active"},
			}})
		default:
			writeErr(w, http.StatusNotFound, "E_NOT_FOUND", "x")
		}
	}))

	code, stdout, stderr := run(t, "doctor", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warn is not fail); stderr = %q\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"WARN dns web.example.test",
		"does not resolve",
		"fix: point an A/AAAA record",
		"0 fail",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestDoctorRejectsArguments(t *testing.T) {
	code, _, stderr := run(t, "doctor", "unexpected")
	if code != 2 {
		t.Errorf("exit = %d, want 2 for a stray argument; stderr = %q", code, stderr)
	}
}
