package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBuild writes an executable shell script that answers
// `version --json` with the given payload, standing in for a real
// TinyShip binary without needing a compiled one.
func fakeBuild(t *testing.T, dir, name, version string, schema int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\necho '{\"version\":%q,\"commit\":\"test\",\"schema_version\":%d,\"migrations\":[\"0001_init\",\"0002_settings\",\"0003_backup\"]}'\n", version, schema)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// upgradeEnv builds an installed binary plus a candidate source file
// and returns the checksum of the source.
func upgradeEnv(t *testing.T) (binPath, srcPath, backupsDir, dbPath, sum string) {
	t.Helper()
	dir := t.TempDir()
	binPath = fakeBuild(t, dir, "tinyship", "0.1.0", 2)
	srcPath = fakeBuild(t, dir, "candidate", "0.2.0", 3)
	var err error
	sum, err = FileSHA256(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	backupsDir = filepath.Join(dir, "backups")
	dbPath = filepath.Join(dir, "tinyship.db")
	if err := os.WriteFile(dbPath, []byte("db"), 0o660); err != nil {
		t.Fatal(err)
	}
	return binPath, srcPath, backupsDir, dbPath, sum
}

// recordingDeps returns Deps with every externally-visible effect
// captured, all succeeding by default.
type recordingDeps struct {
	fetchCalls, probeCalls, backupCalls, restoreCalls int
	stopCalls, restartCalls, healthCalls              int
	restartErr, healthErr, backupErr                  error
	restoredFrom, restoredTo                          string
	fetchedSource                                     string
	deps                                              Deps
}

func recordDeps(t *testing.T) *recordingDeps {
	t.Helper()
	r := &recordingDeps{}
	r.deps = Deps{
		Fetch: func(ctx context.Context, source, dest string) error {
			r.fetchCalls++
			r.fetchedSource = source
			return copyFile(source, dest)
		},
		Probe: func(ctx context.Context, binary string) (VersionInfo, error) {
			r.probeCalls++
			return probeBinary(ctx, binary)
		},
		FileSHA256: FileSHA256,
		Backup: func(ctx context.Context, dest string) error {
			r.backupCalls++
			if r.backupErr != nil {
				return r.backupErr
			}
			return os.WriteFile(dest, []byte("archive"), 0o600)
		},
		RestoreDB: func(ctx context.Context, archive, dbPath string) error {
			r.restoreCalls++
			r.restoredFrom, r.restoredTo = archive, dbPath
			return nil
		},
		Stop: func(ctx context.Context) error {
			r.stopCalls++
			return nil
		},
		Restart: func(ctx context.Context) error {
			r.restartCalls++
			return r.restartErr
		},
		Health: func(ctx context.Context) error {
			r.healthCalls++
			return r.healthErr
		},
	}
	return r
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func testOptions(binPath, srcPath, backupsDir, dbPath, sum string) Options {
	return Options{
		Source:         srcPath,
		SHA256:         sum,
		ManageServices: true,
		BinaryPath:     binPath,
		DBPath:         dbPath,
		BackupsDir:     backupsDir,
		AppliedSchema:  2,
		HealthURL:      "http://127.0.0.1:1",
		HealthTimeout:  100 * time.Millisecond,
	}
}

func TestRunRequiresChecksum(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, _ := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, "")
	opts.SHA256 = ""
	r := recordDeps(t)
	_, err := Run(context.Background(), opts, r.deps)
	if !errors.Is(err, ErrChecksumRequired) {
		t.Fatalf("err = %v, want ErrChecksumRequired", err)
	}
	if r.fetchCalls != 0 {
		t.Error("nothing may be fetched without a checksum")
	}
}

func TestRunChecksumMismatchTouchesNothing(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, _ := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, strings.Repeat("ab", 32))
	r := recordDeps(t)
	_, err := Run(context.Background(), opts, r.deps)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	// The installed binary must be untouched and no partial staging
	// file may remain.
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.1.0" {
		t.Errorf("installed binary changed: %+v", v)
	}
	if _, err := os.Stat(binPath + NextSuffix + ".part"); err == nil {
		t.Error("partial download left behind")
	}
	if r.backupCalls != 0 {
		t.Error("a checksum mismatch must stop before backup")
	}
	if _, err := os.Stat(binPath + PrevSuffix); err == nil {
		t.Error("a checksum mismatch must stop before swap")
	}
}

func TestRunDowngradeBlocked(t *testing.T) {
	dir := t.TempDir()
	// Installed binary knows schema 3; the candidate only knows 2.
	binPath := fakeBuild(t, dir, "tinyship", "0.2.0", 3)
	srcPath := fakeBuild(t, dir, "candidate", "0.1.0", 2)
	sum, err := FileSHA256(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(binPath, srcPath, filepath.Join(dir, "backups"), filepath.Join(dir, "db"), sum)
	r := recordDeps(t)
	_, err = Run(context.Background(), opts, r.deps)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
	if !strings.Contains(err.Error(), "downgrades are not supported") {
		t.Errorf("error should explain the block: %v", err)
	}
	if r.backupCalls != 0 {
		t.Error("no backup may be taken for a blocked upgrade")
	}
}

func TestRunBlockedWhenDatabaseAheadOfTarget(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	// The database is already at schema 4 but the target only knows 3:
	// the new binary would refuse to start against it.
	opts.AppliedSchema = 4
	r := recordDeps(t)
	_, err := Run(context.Background(), opts, r.deps)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
	if !strings.Contains(err.Error(), "newer build is required") {
		t.Errorf("error should point at the database being ahead: %v", err)
	}
}

func TestRunCheckOnlyChangesNothing(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	opts.CheckOnly = true
	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if r.backupCalls != 0 || r.stopCalls != 0 || r.restartCalls != 0 || r.healthCalls != 0 {
		t.Error("check mode must not touch backup or services")
	}
	if _, err := os.Stat(binPath + PrevSuffix); err == nil {
		t.Error("check mode must not create a rollback binary")
	}
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.1.0" {
		t.Error("check mode replaced the installed binary")
	}
	if len(res.PendingMigrations) == 0 {
		t.Error("check must report the migrations that will run")
	}
	if res.Target.Version != "0.2.0" {
		t.Errorf("target version = %q, want 0.2.0", res.Target.Version)
	}
}

func TestRunCheckReportsBlockedWithoutError(t *testing.T) {
	dir := t.TempDir()
	binPath := fakeBuild(t, dir, "tinyship", "0.2.0", 3)
	srcPath := fakeBuild(t, dir, "candidate", "0.1.0", 2)
	sum, err := FileSHA256(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions(binPath, srcPath, filepath.Join(dir, "backups"), filepath.Join(dir, "db"), sum)
	opts.CheckOnly = true
	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("a blocked check reports the block, it does not error: %v", err)
	}
	if res.Blocked == "" {
		t.Error("check of a downgrade must set the blocked reason")
	}
}

func TestRunSuccessSwapsAndRestarts(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	if !res.Swapped {
		t.Error("expected a swap")
	}
	if r.backupCalls != 1 || r.restartCalls != 1 || r.healthCalls != 1 {
		t.Errorf("calls: backup=%d restart=%d health=%d, want 1 each",
			r.backupCalls, r.restartCalls, r.healthCalls)
	}
	if res.RolledBack {
		t.Error("a healthy upgrade must not roll back")
	}
	// The new binary answers, the old one is the rollback point.
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.2.0" {
		t.Errorf("installed version = %s, want 0.2.0", v.Version)
	}
	if v, _ := probeBinary(context.Background(), binPath+PrevSuffix); v.Version != "0.1.0" {
		t.Errorf(".prev version = %s, want 0.1.0", v.Version)
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Errorf("pre-upgrade backup missing: %v", err)
	}
	// The staged file was consumed by the swap.
	if _, err := os.Stat(binPath + NextSuffix); err == nil {
		t.Error(".next should not survive the swap")
	}
}

func TestRunHealthFailureRollsBackBinaryAndDB(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	r.healthErr = errors.New("healthz refused")
	res, err := Run(context.Background(), opts, r.deps)
	if err == nil {
		t.Fatal("a failed health check must fail the upgrade")
	}
	if !res.RolledBack {
		t.Fatal("expected a rollback")
	}
	// Both halves of the rollback happened: the old binary is back and
	// the database was restored from the pre-upgrade archive.
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.1.0" {
		t.Errorf("binary after rollback = %s, want 0.1.0", v.Version)
	}
	if r.restoreCalls != 1 {
		t.Fatalf("restore calls = %d, want 1", r.restoreCalls)
	}
	if r.restoredTo != dbPath {
		t.Errorf("restored to %q, want %q", r.restoredTo, dbPath)
	}
	if r.stopCalls == 0 {
		t.Error("services must be stopped before the database is swapped back")
	}
	// The final restart brought the old version back up.
	if r.restartCalls < 2 {
		t.Errorf("restart calls = %d, want at least 2 (failed one + rollback one)", r.restartCalls)
	}
}

func TestRunRestartFailureRollsBack(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	r.restartErr = errors.New("unit failed")
	res, err := Run(context.Background(), opts, r.deps)
	if err == nil || !res.RolledBack {
		t.Fatalf("a failed restart must trigger a rollback (err=%v rolled=%v)", err, res.RolledBack)
	}
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.1.0" {
		t.Errorf("binary after rollback = %s, want 0.1.0", v.Version)
	}
	if r.healthCalls != 0 {
		t.Error("health must not be polled after a failed restart")
	}
}

// A run killed after the swap but before restart leaves the target
// build installed with .prev holding the old one. Re-running must skip
// the swap (keeping that rollback point) and finish restart + health.
func TestRunConvergesWhenKilledAfterSwap(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	// Arrange the post-kill state: binary = target, .prev = old,
	// backup from the interrupted run already on disk.
	if err := copyFile(srcPath, binPath); err != nil {
		t.Fatal(err)
	}
	oldBin := fakeBuild(t, t.TempDir(), "tinyship", "0.1.0", 2)
	if err := copyFile(oldBin, binPath+PrevSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupsDir, "pre-upgrade-"+sum[:12]+".tsbak"), []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	if !res.AlreadyInstalled {
		t.Error("expected the already-installed path")
	}
	if res.Swapped {
		t.Error("the swap must be skipped so .prev keeps the old binary")
	}
	if r.fetchCalls != 0 {
		t.Error("nothing to download when the target build is already installed")
	}
	if r.backupCalls != 0 {
		t.Error("the interrupted run's backup must be reused, not recreated")
	}
	if !res.BackupFound {
		t.Error("the existing pre-upgrade backup must be reported")
	}
	// The rollback point survived and restart + health completed.
	if v, _ := probeBinary(context.Background(), binPath+PrevSuffix); v.Version != "0.1.0" {
		t.Errorf(".prev after re-run = %s, want 0.1.0 (the rollback point)", v.Version)
	}
	if r.restartCalls != 1 || r.healthCalls != 1 {
		t.Errorf("restart=%d health=%d, want 1 each to converge", r.restartCalls, r.healthCalls)
	}
}

// A run killed mid-swap leaves the installed binary missing but .prev
// and .next in place. Re-running must complete the swap without
// clobbering .prev.
func TestRunConvergesWhenKilledMidSwap(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	oldBin := binPath
	// Post-kill state: binary renamed to .prev, .next fully staged.
	if err := os.Rename(binPath, binPath+PrevSuffix); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(srcPath, binPath+NextSuffix); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(oldBin, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	if !res.Swapped {
		t.Error("the interrupted swap must complete")
	}
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.2.0" {
		t.Errorf("binary = %s, want 0.2.0", v.Version)
	}
	if v, _ := probeBinary(context.Background(), binPath+PrevSuffix); v.Version != "0.1.0" {
		t.Errorf(".prev = %s, want 0.1.0 (must not be clobbered)", v.Version)
	}
}

// A run killed mid-download leaves a partial .part; the re-run must
// replace it and proceed.
func TestRunConvergesWhenKilledMidDownload(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	if err := os.WriteFile(binPath+NextSuffix+".part", []byte("garbage"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	if !res.Swapped {
		t.Error("expected the upgrade to complete")
	}
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.2.0" {
		t.Errorf("binary = %s, want 0.2.0", v.Version)
	}
}

func TestRunBackupFailureStopsBeforeSwap(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	r.backupErr = errors.New("disk full")
	_, err := Run(context.Background(), opts, r.deps)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v, want a nothing-changed failure", err)
	}
	if v, _ := probeBinary(context.Background(), binPath); v.Version != "0.1.0" {
		t.Error("the installed binary must be untouched when the backup fails")
	}
	// A half-written archive must not keep the rollback name.
	matches, _ := filepath.Glob(filepath.Join(backupsDir, "pre-upgrade-*"))
	if len(matches) != 0 {
		t.Errorf("partial backup kept its final name: %v", matches)
	}
}

func TestRunRollbackKeepsDatabaseWhenNoPrevBinary(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	// The target build is already installed (a previous run was killed
	// after the swap) but its .prev was removed: there is no binary to
	// go back to, so the database must stay where it is — pairing a
	// rolled-back database with the new binary would leave a version
	// mismatch on restart.
	if err := copyFile(srcPath, binPath); err != nil {
		t.Fatal(err)
	}
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	r := recordDeps(t)
	r.healthErr = errors.New("down")
	res, err := Run(context.Background(), opts, r.deps)
	if err == nil {
		t.Fatal("expected failure")
	}
	if r.restoreCalls != 0 {
		t.Error("without the old binary the database must stay put so both sides remain consistent")
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "left as-is") {
		t.Errorf("steps should explain the decision: %v", res.Steps)
	}
}

func TestRunNoServiceManagementInSandboxMode(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	opts.ManageServices = false
	r := recordDeps(t)
	res, err := Run(context.Background(), opts, r.deps)
	if err != nil {
		t.Fatalf("sandbox upgrade failed: %v", err)
	}
	if !res.Swapped {
		t.Error("the file-level swap must still happen in a sandbox")
	}
	if r.stopCalls != 0 || r.restartCalls != 0 || r.healthCalls != 0 {
		t.Error("services must not be touched from a sandbox")
	}
}

func TestRunManageServicesWithoutDepsRefused(t *testing.T) {
	binPath, srcPath, backupsDir, dbPath, sum := upgradeEnv(t)
	opts := testOptions(binPath, srcPath, backupsDir, dbPath, sum)
	_, err := Run(context.Background(), opts, Deps{})
	if err == nil || !strings.Contains(err.Error(), "not provided") {
		t.Fatalf("err = %v, want a missing-dependency refusal", err)
	}
}

func TestPendingMigrationsListsAboveApplied(t *testing.T) {
	got := pendingMigrations([]string{"0001_init", "0002_settings", "0003_backup"}, 2)
	if len(got) != 1 || got[0] != "0003_backup" {
		t.Errorf("pending = %v, want [0003_backup]", got)
	}
	// Everything is pending on a fresh database.
	if got := pendingMigrations([]string{"0001_init"}, 0); len(got) != 1 {
		t.Errorf("fresh database pending = %v, want everything", got)
	}
	// An unparseable name is shown, not hidden.
	if got := pendingMigrations([]string{"weird"}, 5); len(got) != 1 {
		t.Errorf("unparseable migration = %v, want it reported", got)
	}
}

func TestPollHealthSucceedsAndTimesOut(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if err := PollHealth(context.Background(), ok.URL, time.Second); err != nil {
		t.Errorf("healthy endpoint: %v", err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	if err := PollHealth(context.Background(), dead.URL, 300*time.Millisecond); err == nil {
		t.Error("an unreachable endpoint must time out with an error")
	}
}
