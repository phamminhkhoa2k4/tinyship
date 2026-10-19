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

// restoreBackup writes a backup archive from a source sandbox that
// holds a real migrated database plus config/key/token files. Returns
// the archive path.
func restoreBackup(t *testing.T, srcRoot, passphrase string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "b.tsbak")
	prev := stdin
	args := []string{"backup", "--install-root", srcRoot, "-o", out}
	if passphrase != "" {
		stdin = strings.NewReader(passphrase + "\n")
		args = append(args, "--encrypt")
	}
	code, _, stderr := run(t, args...)
	stdin = prev
	if code != 0 {
		t.Fatalf("backup for restore test: exit = %d, stderr = %s", code, stderr)
	}
	return out
}

// stubReconcile makes the reconcile step deterministic: a fixed set of
// running app IDs (or a docker error).
func stubReconcile(t *testing.T, running map[string]bool, err error) {
	t.Helper()
	prev := runningAppContainers
	runningAppContainers = func(ctx context.Context) (map[string]bool, error) {
		return running, err
	}
	t.Cleanup(func() { runningAppContainers = prev })
}

func TestRestoreFreshRootRoundtrip(t *testing.T) {
	// Source sandbox holds the state; the destination root is empty
	// (the fresh-machine path: no services, no users, nothing on disk).
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(context.Background(), store.CreateAppInput{
		Name: "web", ConfigJSON: `{"image":"nginx:alpine","port":8080}`,
		Actor: "test",
	}); err != nil {
		t.Fatalf("create app in source db: %v", err)
	}
	s.Close()
	for rel, content := range map[string]string{
		"etc/tinyship/config.yaml":      "host: {}\n",
		"etc/tinyship/keys/secrets.key": "PRIVATE\n",
		"etc/tinyship/keys/secrets.pub": "PUBLIC\n",
		"etc/tinyship/token":            "tok\n",
	} {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	arc := restoreBackup(t, src, "")

	dst := t.TempDir()
	stubReconcile(t, map[string]bool{}, nil)
	code, stdout, stderr := run(t, "restore", arc, "--install-root", dst, "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// Every member must land at its destination with the right content.
	db := filepath.Join(dst, "var/lib/tinyship/tinyship.db")
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("restored db missing: %v", err)
	}
	// The data directory must keep its setgid bit: files the services
	// create inside it later need the tinyship-db group without
	// anyone remembering to set it.
	dataDir, err := os.Stat(filepath.Join(dst, "var/lib/tinyship"))
	if err != nil {
		t.Fatal(err)
	}
	if dataDir.Mode()&os.ModeSetgid == 0 {
		t.Errorf("data directory mode = %v, want the setgid bit kept", dataDir.Mode())
	}
	ds, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	apps, err := ds.ListApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ds.Close()
	if len(apps) != 1 || apps[0].Name != "web" {
		t.Errorf("restored db apps = %+v, want [web]", apps)
	}
	key, err := os.ReadFile(filepath.Join(dst, "etc/tinyship/keys/secrets.key"))
	if err != nil || string(key) != "PRIVATE\n" {
		t.Errorf("restored key = %q, %v", key, err)
	}
	tok, err := os.ReadFile(filepath.Join(dst, "etc/tinyship/token"))
	if err != nil || string(tok) != "tok\n" {
		t.Errorf("restored token = %q, %v", tok, err)
	}
	// The fresh machine needs the deploy report.
	if !strings.Contains(stdout, "needs a deploy") || !strings.Contains(stdout, "web") {
		t.Errorf("stdout should report web needs a deploy; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "tinyship deploy") {
		t.Errorf("stdout should suggest the deploy command; got:\n%s", stdout)
	}
}

func TestRestoreEncryptedArchive(t *testing.T) {
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	arc := restoreBackup(t, src, "s3cret-pass")

	dst := t.TempDir()
	// Wrong passphrase: refuse, change nothing.
	prev := stdin
	stdin = strings.NewReader("wrong\n")
	code, _, stderr := run(t, "restore", arc, "--install-root", dst, "--yes")
	stdin = prev
	if code != 1 {
		t.Errorf("wrong passphrase: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "wrong passphrase") {
		t.Errorf("stderr = %q, want a wrong-passphrase error", stderr)
	}
	if _, err := os.Stat(filepath.Join(dst, "var/lib/tinyship/tinyship.db")); err == nil {
		t.Error("a failed decryption must not write any file")
	}

	// Right passphrase from stdin: restore succeeds.
	stdin = strings.NewReader("s3cret-pass\n")
	stubReconcile(t, map[string]bool{}, nil)
	code, _, stderr = run(t, "restore", arc, "--install-root", dst, "--yes")
	stdin = prev
	if code != 0 {
		t.Fatalf("right passphrase: exit = %d, stderr = %s", code, stderr)
	}
}

func TestRestoreRefusesNewerSchema(t *testing.T) {
	// Hand-build an archive whose manifest claims a future schema.
	var buf bytes.Buffer
	_, err := backup.Write(&buf, backup.Options{
		Files:         []backup.File{{Name: "var/lib/tinyship/tinyship.db", Data: []byte("x")}},
		SchemaVersion: 9999,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "future.tsbak")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	code, _, stderr := run(t, "restore", p, "--install-root", dst, "--yes")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "upgrade the binary first") {
		t.Errorf("stderr = %q, want upgrade guidance", stderr)
	}
	if _, err := os.Stat(filepath.Join(dst, "var/lib/tinyship/tinyship.db")); err == nil {
		t.Error("a refused restore must not write any file")
	}
}

func TestRestoreCorruptArchiveChangesNothing(t *testing.T) {
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	arcPath := restoreBackup(t, src, "")
	raw, err := os.ReadFile(arcPath)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the tail (gzip trailer / member content).
	raw[len(raw)-3] ^= 0xff
	corrupt := filepath.Join(t.TempDir(), "corrupt.tsbak")
	if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	// Pre-existing state must survive the refused restore.
	keep := filepath.Join(dst, "etc/tinyship/config.yaml")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("KEEP\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "restore", corrupt, "--install-root", dst, "--yes")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	got, err := os.ReadFile(keep)
	if err != nil || string(got) != "KEEP\n" {
		t.Errorf("pre-existing file changed by a refused restore: %q, %v", got, err)
	}
	if !strings.Contains(stderr, "corrupt") && !strings.Contains(stderr, "checksum") &&
		!strings.Contains(stderr, "archive integrity") {
		t.Errorf("stderr = %q, want an integrity error", stderr)
	}
}

func TestRestoreConfirmationRequiredWhenOverwriting(t *testing.T) {
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	arc := restoreBackup(t, src, "")

	dst := t.TempDir()
	keep := filepath.Join(dst, "etc/tinyship/config.yaml")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("LIVE\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	// No --yes and a mismatched confirmation: refuse, change nothing.
	prev := stdin
	stdin = strings.NewReader("not-the-hostname\n")
	code, stdout, stderr := run(t, "restore", arc, "--install-root", dst)
	stdin = prev
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "will be replaced") {
		t.Errorf("stdout should list what will be replaced; got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "nothing was changed") {
		t.Errorf("stderr = %q, want a refusal", stderr)
	}
	got, _ := os.ReadFile(keep)
	if string(got) != "LIVE\n" {
		t.Errorf("file changed without confirmation: %q", got)
	}
}

func TestRestoreSafetyCopyKeepsPreviousState(t *testing.T) {
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	arc := restoreBackup(t, src, "")

	dst := t.TempDir()
	keep := filepath.Join(dst, "etc/tinyship/config.yaml")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("LIVE\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stubReconcile(t, map[string]bool{}, nil)
	code, stdout, stderr := run(t, "restore", arc, "--install-root", dst, "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// The old file is replaced but its content survives in the
	// timestamped safety copy the command reports.
	matches, _ := filepath.Glob(keep + ".pre-restore-*")
	if len(matches) != 1 {
		t.Fatalf("safety copies = %v, want exactly one", matches)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil || string(got) != "LIVE\n" {
		t.Errorf("safety copy = %q, %v; want the previous content", got, err)
	}
	if !strings.Contains(stdout, "Previous state kept as") {
		t.Errorf("stdout should list the safety copies; got:\n%s", stdout)
	}
}

func TestRestoreStaleWALSafetyCopied(t *testing.T) {
	// A database stopped uncleanly leaves -wal/-shm next to it; the
	// restore must move them aside with the old DB, never leave them
	// beside the restored file (that would corrupt it).
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	arc := restoreBackup(t, src, "")

	dst := t.TempDir()
	db := filepath.Join(dst, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{db, db + "-wal", db + "-shm"} {
		if err := os.WriteFile(p, []byte("stale"), 0o660); err != nil {
			t.Fatal(err)
		}
	}
	stubReconcile(t, map[string]bool{}, nil)
	code, _, stderr := run(t, "restore", arc, "--install-root", dst, "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, side := range []string{db + "-wal", db + "-shm"} {
		if _, err := os.Stat(side); err == nil {
			t.Errorf("stale %s still sits next to the restored database", filepath.Base(side))
		}
		matches, _ := filepath.Glob(side + ".pre-restore-*")
		if len(matches) != 1 {
			t.Errorf("stale %s safety copies = %v, want one", filepath.Base(side), matches)
		}
	}
	// The restored database must open cleanly (no stale WAL confusion).
	s2, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("open restored db after WAL cleanup: %v", err)
	}
	s2.Close()
}

func TestRestoreReconcileReportsRunningAndUnknown(t *testing.T) {
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.CreateApp(context.Background(), store.CreateAppInput{
		Name: "running-app", ConfigJSON: `{"image":"nginx:alpine","port":8080}`, Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(context.Background(), store.CreateAppInput{
		Name: "down-app", ConfigJSON: `{"image":"nginx:alpine","port":8081}`, Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	arc := restoreBackup(t, src, "")

	dst := t.TempDir()
	// running-app has a container; down-app does not; one container
	// belongs to an app the restored database never heard of.
	stubReconcile(t, map[string]bool{running.ID: true, "01HXORPHAN0000000000000000": true}, nil)
	code, stdout, stderr := run(t, "restore", arc, "--install-root", dst, "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"running-app", "down-app", "needs a deploy", "01HXORPHAN"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "left running") {
		t.Errorf("stdout should say unknown containers were left running:\n%s", stdout)
	}
}

func TestRestoreReconcileDockerUnavailableIsNotFatal(t *testing.T) {
	src := t.TempDir()
	srcDB := filepath.Join(src, "var/lib/tinyship/tinyship.db")
	if err := os.MkdirAll(filepath.Dir(srcDB), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), srcDB)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	arc := restoreBackup(t, src, "")

	dst := t.TempDir()
	stubReconcile(t, nil, os.ErrDeadlineExceeded)
	code, stdout, stderr := run(t, "restore", arc, "--install-root", dst, "--yes")
	if code != 0 {
		t.Fatalf("exit = %d (a missing docker must not fail a restore), stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "Reconcile skipped") {
		t.Errorf("stdout should say reconcile was skipped:\n%s", stdout)
	}
}

func TestRestoreUnknownMemberRefused(t *testing.T) {
	// An archive holding a member the path table does not know must be
	// refused rather than written to a guessed location.
	var buf bytes.Buffer
	_, err := backup.Write(&buf, backup.Options{
		Files: []backup.File{{Name: "etc/passwd-evil", Data: []byte("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "evil.tsbak")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	code, _, stderr := run(t, "restore", p, "--install-root", dst, "--yes")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "unknown member") {
		t.Errorf("stderr = %q, want an unknown-member refusal", stderr)
	}
}

func TestRestoreRequiresExactlyOneFile(t *testing.T) {
	code, _, stderr := run(t, "restore")
	if code != 2 {
		t.Errorf("no argument: exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "exactly one") {
		t.Errorf("stderr = %q", stderr)
	}
}
