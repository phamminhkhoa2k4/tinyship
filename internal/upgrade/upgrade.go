// Package upgrade orchestrates a TinyShip binary upgrade as a series
// of idempotent steps: verify the target build's checksum, read its
// identity, take a pre-upgrade backup of the database, swap the binary
// atomically, restart the services, and poll health. Any failure after
// the swap automatically rolls back to the previous binary and the
// backup. Every step tolerates a re-run, so a process killed midway
// converges when the command is started again.
//
// Effects that touch the outside world (downloading, exec'ing a
// candidate binary, service management, health, backup, database
// restore) arrive as injectable dependencies, which is what lets the
// failure and convergence paths be tested without a live host.
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// NextSuffix names the staging file next to the installed binary;
	// PrevSuffix keeps the binary being replaced for rollback.
	NextSuffix = ".next"
	PrevSuffix = ".prev"

	// maxFetchBytes bounds a download: a release binary is tens of
	// megabytes, so anything larger is a mistake or an attack.
	maxFetchBytes = 512 << 20

	// DefaultHealthTimeout bounds the post-restart health poll.
	DefaultHealthTimeout = 30 * time.Second
	healthPollInterval   = 500 * time.Millisecond
)

var (
	// ErrChecksumRequired means no usable checksum was supplied.
	ErrChecksumRequired = errors.New("upgrade: checksum required (pass --sha256 <hex> or --sha256-file <path>)")
	// ErrChecksumMismatch means the fetched build is not the one the
	// operator vouched for; it is deleted and nothing else happens.
	ErrChecksumMismatch = errors.New("upgrade: fetched binary fails its checksum")
	// ErrIncompatible means the target cannot safely replace what is
	// installed on this host (a downgrade in disguise).
	ErrIncompatible = errors.New("upgrade: target is not compatible with this host")
)

// VersionInfo mirrors `tinyship version --json`.
type VersionInfo struct {
	Version       string   `json:"version"`
	Commit        string   `json:"commit,omitempty"`
	SchemaVersion int      `json:"schema_version"`
	Migrations    []string `json:"migrations,omitempty"`
}

// Options locates the pieces an upgrade touches. BinaryPath is the
// installed binary (the file the services exec); a sandbox run points
// it inside the install root. AppliedSchema is the migration version
// the database is at, read by the caller before Run.
type Options struct {
	Source         string // URL (http/https) or local file path
	SHA256         string // expected checksum, 64 hex characters
	CheckOnly      bool   // report and stop before any change
	ManageServices bool   // restart + health + rollback (real host)
	BinaryPath     string
	DBPath         string
	BackupsDir     string
	AppliedSchema  int
	HealthURL      string // base URL of the control API
	HealthTimeout  time.Duration
}

// Deps are the externally-observable effects. Fetch, Probe and
// FileSHA256 default to real implementations; the rest are provided by
// the caller and required only where the flow actually calls them.
type Deps struct {
	Fetch      func(ctx context.Context, source, dest string) error
	Probe      func(ctx context.Context, binary string) (VersionInfo, error)
	FileSHA256 func(path string) (string, error)
	// Backup writes a pre-upgrade archive to dest atomically (a partial
	// file must never keep the final name, or a re-run would reuse it).
	Backup func(ctx context.Context, dest string) error
	// RestoreDB replaces dbPath with the database member of archive,
	// clearing stale WAL sidecars.
	RestoreDB func(ctx context.Context, archive, dbPath string) error
	Stop      func(ctx context.Context) error
	Restart   func(ctx context.Context) error
	Health    func(ctx context.Context) error
}

// Result is the step log and the machine-readable outcome.
type Result struct {
	Current           VersionInfo `json:"current"`
	Target            VersionInfo `json:"target"`
	AlreadyInstalled  bool        `json:"already_installed"`
	Swapped           bool        `json:"swapped"`
	RolledBack        bool        `json:"rolled_back"`
	PendingMigrations []string    `json:"pending_migrations,omitempty"`
	BackupPath        string      `json:"backup_path,omitempty"`
	BackupFound       bool        `json:"backup_found"`
	Blocked           string      `json:"blocked,omitempty"`
	Steps             []string    `json:"steps"`
	Notes             []string    `json:"notes,omitempty"`
}

// Run executes the upgrade (or just the compatibility report).
func Run(ctx context.Context, opts Options, deps Deps) (*Result, error) {
	res := &Result{}
	if deps.Fetch == nil {
		deps.Fetch = fetchSource
	}
	if deps.Probe == nil {
		deps.Probe = probeBinary
	}
	if deps.FileSHA256 == nil {
		deps.FileSHA256 = FileSHA256
	}
	expected, err := normalizeSHA(opts.SHA256)
	if err != nil {
		return res, err
	}
	if opts.BinaryPath == "" {
		return res, errors.New("upgrade: no binary path resolved")
	}
	if opts.ManageServices && (deps.Restart == nil || deps.Health == nil || deps.Stop == nil) {
		return res, errors.New("upgrade: service management requested but Stop/Restart/Health were not provided")
	}

	// Identity of the installed binary. A probe failure is a note, not
	// a stop: a broken or missing binary is exactly what an upgrade
	// may need to replace.
	if _, err := os.Stat(opts.BinaryPath); err == nil {
		if v, perr := deps.Probe(ctx, opts.BinaryPath); perr == nil {
			res.Current = v
		} else {
			res.Notes = append(res.Notes, "installed binary could not be probed: "+perr.Error())
		}
	} else {
		res.Notes = append(res.Notes, "no installed binary at "+opts.BinaryPath)
	}
	installedSum, _ := deps.FileSHA256(opts.BinaryPath)

	nextPath := opts.BinaryPath + NextSuffix
	if installedSum != "" && installedSum == expected {
		// A previous run was killed after the swap: the target build is
		// already in place. Skip download and swap so the rollback point
		// (.prev and the backup) from that run survives.
		res.AlreadyInstalled = true
		res.Target = res.Current
		res.Steps = append(res.Steps, "installed binary already matches the target build; download and swap skipped")
		_ = os.Remove(nextPath)
		_ = os.Remove(nextPath + ".part")
	} else {
		partPath := nextPath + ".part"
		_ = os.Remove(partPath)
		res.Steps = append(res.Steps, "fetch "+opts.Source+" to "+nextPath)
		if err := deps.Fetch(ctx, opts.Source, partPath); err != nil {
			return res, err
		}
		sum, err := deps.FileSHA256(partPath)
		if err != nil {
			_ = os.Remove(partPath)
			return res, err
		}
		if sum != expected {
			_ = os.Remove(partPath)
			return res, fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, sum, expected)
		}
		// The candidate earns the right to be exec'd only after its
		// checksum matched what the operator vouched for.
		if err := os.Chmod(partPath, 0o755); err != nil {
			_ = os.Remove(partPath)
			return res, fmt.Errorf("upgrade: mark fetched binary executable: %w", err)
		}
		if err := os.Rename(partPath, nextPath); err != nil {
			_ = os.Remove(partPath)
			return res, fmt.Errorf("upgrade: stage fetched binary: %w", err)
		}
		v, err := deps.Probe(ctx, nextPath)
		if err != nil {
			return res, fmt.Errorf("upgrade: cannot read the target build's version: %w", err)
		}
		res.Target = v
		res.Steps = append(res.Steps, fmt.Sprintf("target build reads as version %s (schema %d)", v.Version, v.SchemaVersion))
	}

	res.PendingMigrations = pendingMigrations(res.Target.Migrations, opts.AppliedSchema)

	// Compatibility: the database may not end up ahead of the binary
	// that must read it, and a lower target schema is a downgrade.
	if res.Target.SchemaVersion < opts.AppliedSchema {
		res.Blocked = fmt.Sprintf("the target build understands schema %d but this database is at schema %d; a newer build is required (downgrades are not supported — restore an older backup instead)",
			res.Target.SchemaVersion, opts.AppliedSchema)
	} else if res.Target.SchemaVersion < res.Current.SchemaVersion {
		res.Blocked = fmt.Sprintf("the target build understands schema %d but %s is installed at schema %d; downgrades are not supported (install a build at or above the current schema)",
			res.Target.SchemaVersion, res.Current.Version, res.Current.SchemaVersion)
	}
	if res.Blocked != "" {
		if opts.CheckOnly {
			return res, nil
		}
		return res, fmt.Errorf("%w: %s", ErrIncompatible, res.Blocked)
	}
	if opts.CheckOnly {
		res.Steps = append(res.Steps, "check only; nothing was changed")
		return res, nil
	}

	// The rollback point: one archive per target build, named after its
	// checksum so a re-run of the same upgrade reuses the backup taken
	// before the first swap (a backup taken after a restart could
	// already hold migrated data and would be wrong to roll back to).
	if opts.BackupsDir != "" {
		res.BackupPath = filepath.Join(opts.BackupsDir, "pre-upgrade-"+expected[:12]+".tsbak")
		if _, err := os.Stat(res.BackupPath); err == nil {
			res.BackupFound = true
		}
	}
	if !res.AlreadyInstalled {
		if res.BackupPath == "" {
			return res, errors.New("upgrade: no backups directory resolved; refusing to swap without a rollback point")
		}
		if deps.Backup == nil {
			return res, errors.New("upgrade: auto-backup not available; refusing to swap without a rollback point")
		}
		if !res.BackupFound {
			if err := os.MkdirAll(opts.BackupsDir, 0o700); err != nil {
				return res, fmt.Errorf("upgrade: create backups directory: %w", err)
			}
			res.Steps = append(res.Steps, "pre-upgrade backup to "+res.BackupPath)
			if err := deps.Backup(ctx, res.BackupPath); err != nil {
				// The archive must not keep the final name half-written.
				_ = os.Remove(res.BackupPath)
				return res, fmt.Errorf("upgrade: auto-backup failed, nothing was changed: %w", err)
			}
		} else {
			res.Steps = append(res.Steps, "reusing the pre-upgrade backup from an interrupted run: "+res.BackupPath)
		}
	} else if res.BackupPath != "" {
		res.Steps = append(res.Steps, "pre-upgrade backup from the interrupted run: "+res.BackupPath)
	}

	// Atomic swap: current → .prev (keeps the rollback binary), staged
	// → current. A kill between the two renames leaves the installed
	// binary missing but .prev intact; the next run completes it.
	if !res.AlreadyInstalled {
		if _, err := os.Stat(opts.BinaryPath); err == nil {
			if err := os.Rename(opts.BinaryPath, opts.BinaryPath+PrevSuffix); err != nil {
				return res, fmt.Errorf("upgrade: move the current binary aside: %w", err)
			}
			res.Steps = append(res.Steps, "previous binary kept at "+opts.BinaryPath+PrevSuffix)
		}
		if err := os.Rename(nextPath, opts.BinaryPath); err != nil {
			return res, fmt.Errorf("upgrade: install the new binary: %w", err)
		}
		res.Swapped = true
		res.Steps = append(res.Steps, "new binary installed at "+opts.BinaryPath)
	}

	if !opts.ManageServices {
		res.Steps = append(res.Steps, "services are not managed from here; restart and health check were skipped")
		return res, nil
	}

	if err := deps.Restart(ctx); err != nil {
		return res, res.rollback(ctx, opts, deps, fmt.Errorf("service restart failed: %w", err))
	}
	res.Steps = append(res.Steps, "services restarted")
	if err := deps.Health(ctx); err != nil {
		return res, res.rollback(ctx, opts, deps, fmt.Errorf("post-upgrade health check failed: %w", err))
	}
	res.Steps = append(res.Steps, "health check passed")
	return res, nil
}

// rollback returns the host to the pre-upgrade state after a failed
// restart or health check: previous binary back, database restored from
// the pre-upgrade backup, services restarted. The binary and the
// database are restored as a pair — rolling one back without the other
// would leave a version the other side cannot read.
func (res *Result) rollback(ctx context.Context, opts Options, deps Deps, cause error) error {
	res.RolledBack = true
	res.Steps = append(res.Steps, "rolling back: "+cause.Error())
	if deps.Stop != nil {
		if err := deps.Stop(ctx); err != nil {
			res.Steps = append(res.Steps, "warning: stopping services before rollback failed: "+err.Error())
		}
	}
	prevPath := opts.BinaryPath + PrevSuffix
	binaryRestored := false
	if _, err := os.Stat(prevPath); err == nil {
		if err := os.Rename(prevPath, opts.BinaryPath); err != nil {
			res.Steps = append(res.Steps, "CRITICAL: could not restore the previous binary: "+err.Error())
		} else {
			binaryRestored = true
			res.Steps = append(res.Steps, "previous binary restored to "+opts.BinaryPath)
		}
	} else {
		res.Steps = append(res.Steps, "no previous binary at "+prevPath+"; the installed binary was kept")
	}
	if !binaryRestored {
		res.Steps = append(res.Steps, "database left as-is so it stays consistent with the binary that remained installed")
	} else if res.BackupPath != "" && deps.RestoreDB != nil {
		if _, err := os.Stat(res.BackupPath); err == nil {
			if err := deps.RestoreDB(ctx, res.BackupPath, opts.DBPath); err != nil {
				res.Steps = append(res.Steps, "CRITICAL: database restore failed: "+err.Error())
			} else {
				res.Steps = append(res.Steps, "database restored from "+res.BackupPath)
			}
		} else {
			res.Steps = append(res.Steps, "no pre-upgrade backup found; the database was not restored")
		}
	} else {
		res.Steps = append(res.Steps, "database restore capability not provided; the database was not restored")
	}
	if deps.Restart != nil {
		if err := deps.Restart(ctx); err != nil {
			res.Steps = append(res.Steps, "CRITICAL: restart after rollback failed: "+err.Error())
		} else {
			res.Steps = append(res.Steps, "services restarted on the previous version")
		}
	}
	return fmt.Errorf("%w (rolled back to the previous version; see the step log)", cause)
}

// normalizeSHA validates and lowercases a caller-supplied checksum.
func normalizeSHA(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", ErrChecksumRequired
	}
	if len(s) != 64 {
		return "", fmt.Errorf("upgrade: checksum must be 64 hex characters, got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("upgrade: checksum is not valid hex: %w", err)
	}
	return s, nil
}

// pendingMigrations lists the target build's migrations whose version
// is above what the database already applied. An unparseable name is
// kept rather than hidden: better to show something odd than to omit a
// migration from the report.
func pendingMigrations(target []string, applied int) []string {
	var out []string
	for _, name := range target {
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || v > applied {
			out = append(out, name)
		}
	}
	return out
}

// FileSHA256 returns the lowercase hex SHA-256 of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("upgrade: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchSource copies a local file or downloads an http(s) URL to dest.
func fetchSource(ctx context.Context, source, dest string) error {
	if u, err := url.Parse(source); err == nil && u.Scheme != "" {
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("upgrade: unsupported source scheme %q (use http, https, or a local file path)", u.Scheme)
		}
		return fetchURL(ctx, u.String(), dest)
	}
	return fetchFile(source, dest)
}

func fetchFile(source, dest string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("upgrade: open source %s: %w", source, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("upgrade: create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, io.LimitReader(in, maxFetchBytes)); err != nil {
		out.Close()
		return fmt.Errorf("upgrade: copy %s: %w", source, err)
	}
	return out.Close()
}

func fetchURL(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("upgrade: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upgrade: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upgrade: download %s: HTTP %s", url, resp.Status)
	}
	if resp.ContentLength > maxFetchBytes {
		return fmt.Errorf("upgrade: download %s is larger than the %d byte limit", url, maxFetchBytes)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("upgrade: create %s: %w", dest, err)
	}
	// LimitReader stops one byte past the cap so exceeding the size can
	// be detected rather than silently truncated.
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxFetchBytes+1))
	if err != nil {
		out.Close()
		return fmt.Errorf("upgrade: download %s: %w", url, err)
	}
	if n > maxFetchBytes {
		out.Close()
		return fmt.Errorf("upgrade: download %s is larger than the %d byte limit", url, maxFetchBytes)
	}
	return out.Close()
}

// probeBinary reads a build's identity by asking it — the only source
// that knows what it really is. Only called on builds whose checksum
// was already verified (or on the installed binary).
func probeBinary(ctx context.Context, binary string) (VersionInfo, error) {
	cmd := exec.CommandContext(ctx, binary, "version", "--json")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return VersionInfo{}, fmt.Errorf("%s version --json: %s", filepath.Base(binary), strings.TrimSpace(string(ee.Stderr)))
		}
		return VersionInfo{}, fmt.Errorf("%s version --json: %w", filepath.Base(binary), err)
	}
	var v VersionInfo
	if err := json.Unmarshal(out, &v); err != nil {
		return VersionInfo{}, fmt.Errorf("parse %s version output: %w", filepath.Base(binary), err)
	}
	return v, nil
}

// PollHealth GETs /healthz until it answers 200 or the timeout passes.
func PollHealth(ctx context.Context, baseURL string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = probeHealthz(ctx, baseURL)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthPollInterval):
		}
	}
}

func probeHealthz(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered HTTP %s", resp.Status)
	}
	return nil
}
