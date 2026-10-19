package cli

// Self-upgrade command: replaces the installed binary with a build the
// operator vouched for by checksum, taking a rollback point first and
// verifying health after the swap. It runs on the host itself (the same
// trust boundary as backup and uninstall): swapping the binary and
// restarting the services needs the privileges that already control the
// machine, and routing through the API would be a way to make the
// control plane replace itself remotely.
//
// The sequencing lives in internal/upgrade; this file resolves paths,
// supplies the real effects (backup, service management, health, and
// the database restore used by rollback) and prints the report.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tinyship/tinyship/internal/backup"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/upgrade"
)

func cmdUpgrade(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("upgrade", stderr, `usage: tinyship upgrade --from <url|file> --sha256 <hex> | --sha256-file <path>
                     [--check] [--json] [--install-root DIR]

Upgrade this TinyShip installation to the build at --from. The build's
SHA-256 must be supplied and is verified before anything else happens;
only a build that passed the checksum is exec'd to learn its version
and schema. Before the binary is swapped, the database and host state
are captured in a pre-upgrade backup; the services are restarted and
health is polled. If the restart or the health check fails, the
previous binary and the database backup are restored automatically.

An upgrade never downgrades: a build that understands an older schema
than the installed binary or the database is refused with guidance.

 --check   report the target version, the migrations that would run and
           the verdict, then stop without changing anything.
`)
	from := fs.String("from", "", "URL (http/https) or local file path of the new binary")
	sha := fs.String("sha256", "", "expected SHA-256 of the new binary (64 hex characters)")
	shaFile := fs.String("sha256-file", "", "file holding the expected SHA-256 (sha256sum output is accepted)")
	check := fs.Bool("check", false, "report compatibility only; change nothing")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	rootFlag := fs.String("install-root", "", "install root prefix (default: TINYSHIP_INSTALL_ROOT, empty on a real host)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship upgrade: unexpected argument %q\n", pos[0])
		return 2
	}
	if *from == "" {
		fmt.Fprintln(stderr, "tinyship upgrade: --from is required (URL or local file path of the new binary)")
		return 2
	}

	root := uninstallRoot(*rootFlag)
	realHost := root == ""
	if realHost && os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "tinyship upgrade: must run as root on the host (the binary and services are root-owned); or use --install-root for a sandbox tree")
		return 1
	}
	expected, err := upgradeChecksum(*sha, *shaFile)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship upgrade: %v\n", err)
		return 1
	}

	base := root
	if base == "" {
		base = "/"
	}
	dbPath, _, _, _, backupsDir := backupPaths(root)
	opts := upgrade.Options{
		Source:         *from,
		SHA256:         expected,
		CheckOnly:      *check,
		ManageServices: realHost,
		BinaryPath:     filepath.Join(base, "usr/local/bin/tinyship"),
		DBPath:         dbPath,
		BackupsDir:     backupsDir,
		AppliedSchema:  appliedSchemaOrZero(ctx, dbPath),
		HealthURL:      apiBaseURL(),
		HealthTimeout:  upgrade.DefaultHealthTimeout,
	}
	deps := upgrade.Deps{
		Backup: func(ctx context.Context, dest string) error {
			return writeBackupAtomic(ctx, root, dest)
		},
		RestoreDB: func(ctx context.Context, archive, db string) error {
			return restoreDBFromBackup(archive, db, realHost)
		},
	}
	if realHost {
		deps.Stop = func(ctx context.Context) error {
			return systemctlRun(ctx, "stop", "tinyship-api.service", "tinyship-worker.service")
		}
		// The restart command finishing does not mean the units came
		// up; ask systemd explicitly before polling health.
		deps.Restart = func(ctx context.Context) error {
			if err := systemctlRun(ctx, "restart", "tinyship-api.service", "tinyship-worker.service"); err != nil {
				return err
			}
			return systemctlRun(ctx, "is-active", "--quiet", "tinyship-api.service", "tinyship-worker.service")
		}
		deps.Health = func(ctx context.Context) error {
			return upgrade.PollHealth(ctx, opts.HealthURL, opts.HealthTimeout)
		}
	}

	res, runErr := upgrade.Run(ctx, opts, deps)
	return reportUpgrade(res, runErr, opts.AppliedSchema, opts.BinaryPath, *jsonOut, stdout, stderr)
}

// upgradeChecksum resolves the expected checksum from the flag pair.
// The file form accepts sha256sum output ("hex  name") as well as a
// bare hex line.
func upgradeChecksum(inline, file string) (string, error) {
	if inline != "" && file != "" {
		return "", errors.New("--sha256 and --sha256-file are mutually exclusive")
	}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read checksum file: %w", err)
		}
		fields := strings.Fields(string(raw))
		if len(fields) == 0 {
			return "", upgrade.ErrChecksumRequired
		}
		return strings.TrimPrefix(fields[0], "*"), nil
	}
	return inline, nil
}

// appliedSchemaOrZero reads the migration version the database has
// applied. A missing database counts as schema zero: nothing applied
// yet, and nothing may be opened (a check must not create files).
func appliedSchemaOrZero(ctx context.Context, dbPath string) int {
	if _, err := os.Stat(dbPath); err != nil {
		return 0
	}
	s, err := store.OpenPlain(dbPath)
	if err != nil {
		return 0
	}
	defer s.Close()
	v, err := s.AppliedSchemaVersion(ctx)
	if err != nil {
		return 0
	}
	return v
}

func apiBaseURL() string {
	if b := os.Getenv("TINYSHIP_API"); b != "" {
		return b
	}
	return DefaultAPIBase
}

// systemctlRun runs one systemctl invocation and wraps a failure with
// the unit output.
func systemctlRun(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %s (%w)", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// restoreDBFromBackup replaces the database at dbPath with the
// database member of a backup archive. Stale WAL sidecars are removed
// before the swap: a leftover -wal beside a restored file would
// corrupt it.
func restoreDBFromBackup(archive, dbPath string, realHost bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	arc, err := backup.Read(f, "")
	if err != nil {
		return err
	}
	data, ok := arc.Files["var/lib/tinyship/tinyship.db"]
	if !ok {
		return fmt.Errorf("the archive %s holds no database member", archive)
	}
	tmp := dbPath + ".rollback"
	if err := os.WriteFile(tmp, data, 0o660); err != nil {
		return err
	}
	// Ownership is best-effort: getting the data back matters more
	// than its owner when the users are missing.
	if realHost {
		_ = chownPath(tmp, "tinyship", "tinyship-db")
	}
	if err := os.Chmod(tmp, 0o660); err != nil {
		os.Remove(tmp)
		return err
	}
	for _, side := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(side); err != nil && !os.IsNotExist(err) {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// reportUpgrade prints the step log and the verdict, as JSON or in
// human form. A blocked verdict exits non-zero even without an error so
// scripts can branch on compatibility.
func reportUpgrade(res *upgrade.Result, runErr error, appliedSchema int, binaryPath string, jsonOut bool, stdout, stderr io.Writer) int {
	if res == nil {
		fmt.Fprintf(stderr, "tinyship upgrade: %v\n", runErr)
		return 1
	}
	if jsonOut {
		out := struct {
			*upgrade.Result
			AppliedSchema int    `json:"applied_schema"`
			Error         string `json:"error,omitempty"`
		}{res, appliedSchema, errText(runErr)}
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "tinyship upgrade: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "Current: %s (schema %d)\n", orDash(res.Current.Version), res.Current.SchemaVersion)
		fmt.Fprintf(stdout, "Target:  %s (schema %d)\n", orDash(res.Target.Version), res.Target.SchemaVersion)
		fmt.Fprintf(stdout, "Database schema: %d\n", appliedSchema)
		if len(res.PendingMigrations) > 0 {
			fmt.Fprintf(stdout, "Migrations that will run (%d):\n", len(res.PendingMigrations))
			for _, m := range res.PendingMigrations {
				fmt.Fprintf(stdout, "  %s\n", m)
			}
		} else {
			fmt.Fprintln(stdout, "No migrations will run.")
		}
		if len(res.Steps) > 0 {
			fmt.Fprintln(stdout, "Steps:")
			for _, s := range res.Steps {
				fmt.Fprintf(stdout, "  %s\n", s)
			}
		}
		for _, n := range res.Notes {
			fmt.Fprintf(stdout, "Note: %s\n", n)
		}
		switch {
		case res.Blocked != "":
			fmt.Fprintf(stdout, "Verdict: BLOCKED — %s\n", res.Blocked)
		case res.RolledBack:
			fmt.Fprintln(stdout, "Verdict: FAILED and rolled back to the previous version.")
		case runErr != nil:
			// A run that failed before anything was swapped (bad
			// checksum, refused source, backup failure): label the
			// report as failed instead of leaving the neutral verdict.
			fmt.Fprintf(stdout, "Verdict: FAILED — %s\n", errText(runErr))
		case res.Swapped:
			fmt.Fprintf(stdout, "Verdict: %s is now installed at %s.\n", res.Target.Version, binaryPath)
		case res.AlreadyInstalled:
			fmt.Fprintln(stdout, "Verdict: the target build was already installed; the upgrade finished converging.")
		default:
			fmt.Fprintln(stdout, "Verdict: compatible; nothing was changed.")
		}
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "tinyship upgrade: %v\n", runErr)
		return 1
	}
	if res.Blocked != "" {
		return 1
	}
	return 0
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
