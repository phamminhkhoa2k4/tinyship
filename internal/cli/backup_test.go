package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/backup"
	"github.com/tinyship/tinyship/internal/store"
)

// backupSandbox builds a sandbox install root with a real migrated
// database plus the config/key/token files, and points the command's
// environment at it. Returns the root and the DB path.
func backupSandbox(t *testing.T) (root, db string) {
	t.Helper()
	root = t.TempDir()
	db = filepath.Join(root, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("migrate sandbox db: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"etc/tinyship/config.yaml":      "host: {}\n",
		"etc/tinyship/keys/secrets.key": "PRIVATE\n",
		"etc/tinyship/keys/secrets.pub": "PUBLIC\n",
		"etc/tinyship/token":            "tok-secret\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TINYSHIP_DB", db)
	t.Setenv("TINYSHIP_TOKEN_FILE", filepath.Join(root, "etc/tinyship/token"))
	return root, db
}

func TestBackupSandboxRoundtrip(t *testing.T) {
	root, _ := backupSandbox(t)
	out := filepath.Join(t.TempDir(), "b.tsbak")

	code, stdout, stderr := run(t, "backup", "--install-root", root, "-o", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// The archive must open and carry every member with the applied
	// schema version of the sandbox DB.
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	arc, err := backup.Read(f, "")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if arc.Manifest.SchemaVersion != 2 {
		t.Errorf("schema_version = %d, want 2 (the migrated sandbox schema)", arc.Manifest.SchemaVersion)
	}
	want := []string{
		"var/lib/tinyship/tinyship.db",
		"etc/tinyship/config.yaml",
		"etc/tinyship/keys/secrets.key",
		"etc/tinyship/keys/secrets.pub",
		"etc/tinyship/token",
	}
	for _, name := range want {
		if _, ok := arc.Files[name]; !ok {
			t.Errorf("member %s missing; have %v", name, memberNames(arc.Manifest))
		}
	}
	if string(arc.Files["etc/tinyship/keys/secrets.key"]) != "PRIVATE\n" {
		t.Errorf("secret key content wrong: %q", arc.Files["etc/tinyship/keys/secrets.key"])
	}
	// The snapshot must itself be a working TinyShip database.
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, arc.Files["var/lib/tinyship/tinyship.db"], 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := store.Open(context.Background(), snap)
	if err != nil {
		t.Fatalf("open extracted snapshot: %v", err)
	}
	s2.Close()

	// Volume exclusion and the unencrypted warning are standing output.
	if !strings.Contains(stdout, "volume data is NOT in this file") {
		t.Errorf("stdout must state the volume exclusion; got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "NOT encrypted") {
		t.Errorf("stderr must warn about the unencrypted file; got:\n%s", stderr)
	}
}

func TestBackupOutputModeIsOwnerOnly(t *testing.T) {
	root, _ := backupSandbox(t)
	out := filepath.Join(t.TempDir(), "b.tsbak")
	if code, _, stderr := run(t, "backup", "--install-root", root, "-o", out); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600 (the file holds the secret key)", perm)
	}
}

func TestBackupEncryptedStdin(t *testing.T) {
	root, _ := backupSandbox(t)
	out := filepath.Join(t.TempDir(), "b.tsbak")
	prev := stdin
	stdin = strings.NewReader("hunter2\n")
	t.Cleanup(func() { stdin = prev })

	code, stdout, stderr := run(t, "backup", "--install-root", root, "--encrypt", "-o", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(stderr, "NOT encrypted") {
		t.Error("encrypted backup must not print the unencrypted warning")
	}
	if !strings.Contains(stdout, "Encrypted") {
		t.Errorf("stdout should say encrypted; got:\n%s", stdout)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := backup.Read(f, "hunter2"); err != nil {
		t.Fatalf("decrypt with the passphrase: %v", err)
	}
	// The token plaintext must not survive in the ciphertext.
	raw, _ := os.ReadFile(out)
	if bytes.Contains(raw, []byte("tok-secret")) {
		t.Error("token plaintext leaked into the encrypted archive")
	}
}

func TestBackupPassphraseFile(t *testing.T) {
	root, _ := backupSandbox(t)
	out := filepath.Join(t.TempDir(), "b.tsbak")
	pf := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pf, []byte("file-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "backup", "--install-root", root, "--passphrase-file", pf, "-o", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := backup.Read(f, "file-pass"); err != nil {
		t.Fatalf("decrypt with the file passphrase: %v", err)
	}
}

func TestBackupEncryptAndPassphraseFileMutuallyExclusive(t *testing.T) {
	root, _ := backupSandbox(t)
	code, _, stderr := run(t, "backup", "--install-root", root, "--encrypt",
		"--passphrase-file", "/nonexistent")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr = %q, want mutual-exclusion error", stderr)
	}
}

func TestBackupEmptyPassphraseRefused(t *testing.T) {
	root, _ := backupSandbox(t)
	prev := stdin
	stdin = strings.NewReader("\n")
	t.Cleanup(func() { stdin = prev })
	code, _, stderr := run(t, "backup", "--install-root", root, "--encrypt")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "empty") {
		t.Errorf("stderr = %q, want an empty-passphrase complaint", stderr)
	}
}

func TestBackupJSONSummary(t *testing.T) {
	root, _ := backupSandbox(t)
	out := filepath.Join(t.TempDir(), "b.tsbak")
	code, stdout, _ := run(t, "backup", "--install-root", root, "--json", "-o", out)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, `"path":"`+out) ||
		!strings.Contains(stdout, `"encrypted":false`) ||
		!strings.Contains(stdout, `"schema_version":2`) {
		t.Errorf("json summary wrong:\n%s", stdout)
	}
}

func TestBackupRefusesOverwrite(t *testing.T) {
	root, _ := backupSandbox(t)
	out := filepath.Join(t.TempDir(), "b.tsbak")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "backup", "--install-root", root, "-o", out)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want an already-exists complaint", stderr)
	}
	raw, _ := os.ReadFile(out)
	if string(raw) != "old" {
		t.Error("existing file was modified")
	}
}

func TestBackupMissingDBFailsClearly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TINYSHIP_DB", filepath.Join(root, "nope.db"))
	code, _, stderr := run(t, "backup", "--install-root", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no database") {
		t.Errorf("stderr = %q, want a missing-database error", stderr)
	}
}

func TestBackupRealHostRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the refusal cannot trigger")
	}
	t.Setenv("TINYSHIP_INSTALL_ROOT", "")
	code, _, stderr := run(t, "backup")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "must run as root") {
		t.Errorf("stderr = %q, want a root-required error", stderr)
	}
}

func TestBackupDefaultPathUnderBackupsDir(t *testing.T) {
	root, _ := backupSandbox(t)
	code, stdout, stderr := run(t, "backup", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	wantDir := filepath.Join(root, "var/lib/tinyship/backups")
	if !strings.Contains(stdout, wantDir) {
		t.Errorf("stdout should report a file under %s; got:\n%s", wantDir, stdout)
	}
	// The default file name carries the UTC timestamp prefix.
	if !strings.Contains(stdout, "tinyship-backup-") || !strings.Contains(stdout, ".tsbak") {
		t.Errorf("default file name wrong:\n%s", stdout)
	}
}

func TestBackupMissingOptionalFilesSkipped(t *testing.T) {
	root, db := backupSandbox(t)
	// Remove the token; the backup must succeed and say what it skipped.
	if err := os.Remove(filepath.Join(root, "etc/tinyship/token")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINYSHIP_DB", db)
	out := filepath.Join(t.TempDir(), "b.tsbak")
	code, _, stderr := run(t, "backup", "--install-root", root, "-o", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "not found, skipped") {
		t.Errorf("stderr should list the skipped token file; got:\n%s", stderr)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	arc, err := backup.Read(f, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := arc.Files["etc/tinyship/token"]; ok {
		t.Error("absent token file must not appear in the archive")
	}
}
