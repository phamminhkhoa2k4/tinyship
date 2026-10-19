package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/upgrade"
)

// upgradeSandbox builds an install root the way a host would look: a
// migrated database under the data directory and an installed binary
// that answers version --json. The candidate build and its checksum
// live outside the root. Returns the root, the candidate path and the
// checksum.
func upgradeSandbox(t *testing.T, installedVer string, installedSchema int, candVer string, candSchema int) (root, cand, sum string) {
	t.Helper()
	root = t.TempDir()
	db := filepath.Join(root, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("migrate sandbox db: %v", err)
	}
	s.Close()
	binDir := filepath.Join(root, "usr/local/bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeBuild(t, filepath.Join(binDir, "tinyship"), installedVer, installedSchema)
	cand = filepath.Join(t.TempDir(), "candidate")
	writeFakeBuild(t, cand, candVer, candSchema)
	sum, err = upgrade.FileSHA256(cand)
	if err != nil {
		t.Fatal(err)
	}
	return root, cand, sum
}

func writeFakeBuild(t *testing.T, path, ver string, schema int) {
	t.Helper()
	script := "#!/bin/sh\necho '{\"version\":\"" + ver + "\",\"commit\":\"test\",\"schema_version\":" +
		itoa(schema) + ",\"migrations\":[\"0001_init\",\"0002_settings\",\"0003_backup\"]}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestUpgradeSandboxRoundtrip(t *testing.T) {
	root, cand, sum := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))

	code, stdout, stderr := run(t, "upgrade", "--from", cand, "--sha256", sum, "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	bin := filepath.Join(root, "usr/local/bin/tinyship")
	// The candidate is now the installed binary and the old one is the
	// rollback point.
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "0.2.0") {
		t.Error("installed binary was not replaced by the candidate")
	}
	if _, err := os.Stat(bin + upgrade.PrevSuffix); err != nil {
		t.Errorf("rollback binary missing: %v", err)
	}
	// The pre-upgrade backup exists under the backups directory.
	matches, _ := filepath.Glob(filepath.Join(root, "var/lib/tinyship/backups/pre-upgrade-*"))
	if len(matches) != 1 {
		t.Errorf("pre-upgrade backups = %v, want exactly one", matches)
	}
	if !strings.Contains(stdout, "0.2.0") || !strings.Contains(stdout, "Steps:") {
		t.Errorf("report should show the target and the steps:\n%s", stdout)
	}
	// The staged file is consumed and no sandbox run touches services.
	if _, err := os.Stat(bin + upgrade.NextSuffix); err == nil {
		t.Error(".next should not survive the swap")
	}
	if strings.Contains(stdout, "health check passed") {
		t.Error("a sandbox must not run the health check")
	}
}

func TestUpgradeCheckChangesNothing(t *testing.T) {
	root, cand, sum := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))

	code, stdout, stderr := run(t, "upgrade", "--from", cand, "--sha256", sum, "--check", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	bin := filepath.Join(root, "usr/local/bin/tinyship")
	data, _ := os.ReadFile(bin)
	if !strings.Contains(string(data), "0.1.0") {
		t.Error("check must not replace the binary")
	}
	if _, err := os.Stat(bin + upgrade.PrevSuffix); err == nil {
		t.Error("check must not create a rollback binary")
	}
	if matches, _ := filepath.Glob(filepath.Join(root, "var/lib/tinyship/backups/*")); len(matches) != 0 {
		t.Errorf("check must not take a backup; found %v", matches)
	}
	for _, want := range []string{"0.1.0", "0.2.0", "0003_backup"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("check report missing %q:\n%s", want, stdout)
		}
	}
}

func TestUpgradeChecksumMismatchRefused(t *testing.T) {
	root, cand, _ := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))
	wrong := strings.Repeat("ab", 32)

	code, _, stderr := run(t, "upgrade", "--from", cand, "--sha256", wrong, "--install-root", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "checksum") {
		t.Errorf("stderr = %q, want a checksum complaint", stderr)
	}
	data, _ := os.ReadFile(filepath.Join(root, "usr/local/bin/tinyship"))
	if !strings.Contains(string(data), "0.1.0") {
		t.Error("the installed binary must be untouched")
	}
}

func TestUpgradeBlockedDowngrade(t *testing.T) {
	// The candidate understands an older schema than what is installed.
	root, cand, sum := upgradeSandbox(t, "0.2.0", 3, "0.1.0", 2)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))

	code, stdout, stderr := run(t, "upgrade", "--from", cand, "--sha256", sum, "--install-root", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "BLOCKED") && !strings.Contains(stderr, "downgrades are not supported") {
		t.Errorf("the block must be reported clearly:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(root, "usr/local/bin/tinyship"))
	if !strings.Contains(string(data), "0.2.0") {
		t.Error("a blocked upgrade must not touch the binary")
	}
}

func TestUpgradeRequiresFrom(t *testing.T) {
	code, _, stderr := run(t, "upgrade")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--from is required") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestUpgradeRequiresChecksum(t *testing.T) {
	root, cand, _ := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	code, _, stderr := run(t, "upgrade", "--from", cand, "--install-root", root)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "checksum required") {
		t.Errorf("stderr = %q, want a missing-checksum complaint", stderr)
	}
}

func TestUpgradeChecksumFileSha256sumFormat(t *testing.T) {
	root, cand, sum := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))
	sumFile := filepath.Join(t.TempDir(), "sums")
	// The sha256sum format: hex, two spaces, name.
	if err := os.WriteFile(sumFile, []byte(sum+"  tinyship\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "upgrade", "--from", cand, "--sha256-file", sumFile, "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
}

func TestUpgradeRealHostRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the refusal cannot trigger")
	}
	t.Setenv("TINYSHIP_INSTALL_ROOT", "")
	code, _, stderr := run(t, "upgrade", "--from", "/tmp/x", "--sha256", strings.Repeat("a", 64))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "must run as root") {
		t.Errorf("stderr = %q, want a root-required error", stderr)
	}
}

func TestUpgradeJSONReport(t *testing.T) {
	root, cand, sum := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))
	code, stdout, _ := run(t, "upgrade", "--from", cand, "--sha256", sum, "--check", "--json", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{`"version":"0.2.0"`, `"schema_version":3`, `"applied_schema":2`, `"pending_migrations"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("json report missing %s:\n%s", want, stdout)
		}
	}
}

func TestUpgradeRerunConvergesAfterKill(t *testing.T) {
	// The upgrade was killed after the swap: the candidate is already
	// installed, .prev and the backup exist. A re-run must skip the
	// swap and converge without clobbering the rollback point.
	root, cand, sum := upgradeSandbox(t, "0.1.0", 2, "0.2.0", 3)
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "var/lib/tinyship/tinyship.db"))
	bin := filepath.Join(root, "usr/local/bin/tinyship")
	old, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, "upgrade", "--from", cand, "--sha256", sum, "--install-root", root); code != 0 {
		t.Fatalf("first run: exit = %d, stderr = %s", code, stderr)
	}
	// Kill aftermath simulated: .prev exists, backup exists, binary is
	// the candidate. Run again.
	code, stdout, stderr := run(t, "upgrade", "--from", cand, "--sha256", sum, "--install-root", root)
	if code != 0 {
		t.Fatalf("re-run: exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "already installed") {
		t.Errorf("re-run should report convergence:\n%s", stdout)
	}
	// .prev still holds the ORIGINAL binary, not the candidate.
	prev, err := os.ReadFile(bin + upgrade.PrevSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if string(prev) != string(old) {
		t.Error("the re-run clobbered the rollback point")
	}
	// Only one backup exists (reused, not recreated).
	if matches, _ := filepath.Glob(filepath.Join(root, "var/lib/tinyship/backups/pre-upgrade-*")); len(matches) != 1 {
		t.Errorf("backups = %v, want the interrupted run's single archive", matches)
	}
}
