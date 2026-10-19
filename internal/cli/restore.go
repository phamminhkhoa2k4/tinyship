package cli

// Local restore: writes a backup file's contents back onto this host
// and reports what still needs a deploy. It runs on the host itself
// (the same trust boundary as backup and uninstall). The archive is
// fully verified — decryption and every member checksum — before any
// file on disk is touched, and existing state is moved aside to a
// timestamped safety copy rather than deleted, so a bad restore is
// reversible by hand.
//
// The reconcile step is report-only: it lists which apps have a running
// container and which need `tinyship deploy`, plus any running managed
// container the restored database does not know about. Restoring never
// mutates Docker state — the operator stays the one who decides.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/backup"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// restoreSystemctl runs systemctl; tests stub it so the flow runs
// without systemd.
var restoreSystemctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
}

// runningAppContainers returns the set of app IDs with a running
// managed container. Tests stub it so reconcile runs without a daemon.
var runningAppContainers = func(ctx context.Context) (map[string]bool, error) {
	out, err := runDocker(ctx, "ps",
		"--filter", "label="+policy.LabelManaged+"="+policy.ManagedValue,
		"--format", "{{.Label \""+policy.LabelApp+"\"}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	set := make(map[string]bool)
	for _, id := range splitLines(out) {
		set[id] = true
	}
	return set, nil
}

// restoreTarget is one archive member's destination on disk with the
// ownership and mode the path table assigns it. owner/group empty means
// root. Ownership is best-effort: on a host where the installer has not
// created the users yet (a fresh machine restore) the files land root
// owned and the report says so.
type restoreTarget struct {
	member string
	path   string
	mode   os.FileMode
	owner  string
	group  string
}

// restoreTargets maps archive members to their destinations under the
// install root. The database and token honor the same environment
// overrides the backup read them with, so a sandbox round trip lands
// back where the services look.
func restoreTargets(root string) []restoreTarget {
	base := root
	if base == "" {
		base = "/"
	}
	dbPath, configPath, keysDir, tokenPath, _ := backupPaths(root)
	return []restoreTarget{
		{member: "var/lib/tinyship/tinyship.db", path: dbPath, mode: 0o660, owner: "tinyship", group: "tinyship-db"},
		{member: "etc/tinyship/config.yaml", path: configPath, mode: 0o640, owner: "root", group: "tinyship"},
		{member: "etc/tinyship/keys/secrets.key", path: filepath.Join(keysDir, "secrets.key"), mode: 0o400, owner: "tinyship-worker", group: "root"},
		{member: "etc/tinyship/keys/secrets.pub", path: filepath.Join(keysDir, "secrets.pub"), mode: 0o640, owner: "root", group: "tinyship"},
		{member: "etc/tinyship/token", path: tokenPath, mode: 0o640, owner: "root", group: "tinyship-admin"},
		{member: "_", path: filepath.Join(base, "etc/tinyship"), mode: 0o750, owner: "root", group: "tinyship"},
		{member: "_", path: keysDir, mode: 0o750, owner: "root", group: "tinyship"},
		// The data directory is setgid (2770): files created inside it
		// later must land in the tinyship-db group without anyone
		// having to remember the group.
		{member: "_", path: filepath.Join(base, "var/lib/tinyship"), mode: 0o2770, owner: "tinyship", group: "tinyship-db"},
		{member: "_", path: filepath.Join(base, "var/lib/tinyship/backups"), mode: 0o700, owner: "root", group: "root"},
	}
}

// chownPath best-effort sets owner:group; a missing user (fresh machine
// before install) is reported by the caller, not fatal here.
// unixMode translates a Unix octal mode, which may carry the
// setuid/setgid/sticky bits at 0o4000/0o2000/0o1000, into a Go
// FileMode. Go keeps those bits in named flags instead of the octal
// value, so passing 0o2770 straight to Chmod would silently drop the
// setgid bit and the data directory would lose the group inheritance
// the services rely on.
func unixMode(mode os.FileMode) os.FileMode {
	perm := mode & os.ModePerm
	if mode&0o4000 != 0 {
		perm |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		perm |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		perm |= os.ModeSticky
	}
	return perm
}

func chownPath(path, owner, group string) error {
	uid, err := lookupUID(owner)
	if err != nil {
		return err
	}
	gid, err := lookupGID(group)
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func lookupUID(name string) (int, error) {
	if name == "" {
		return 0, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return -1, fmt.Errorf("user %s: %w", name, err)
	}
	return strconv.Atoi(u.Uid)
}

func lookupGID(name string) (int, error) {
	if name == "" {
		return 0, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		return -1, fmt.Errorf("group %s: %w", name, err)
	}
	return strconv.Atoi(g.Gid)
}

func cmdRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("restore", stderr, `usage: tinyship restore <file> [--yes] [--passphrase-file <path>]
                     [--json] [--install-root DIR]

Restore the database snapshot, host configuration, sealed-box key pair
and admin token from a backup file. The archive is verified (decrypt
and every member checksum) before anything on disk changes; existing
files are moved to <name>.pre-restore-<UTC> instead of being deleted.

Application volume data is not part of a backup: after restoring, run
'deploy' for each app to bring its container back. The reconcile report
at the end lists exactly which apps need it.
`)
	yes := fs.Bool("yes", false, "skip the confirmation prompt when replacing existing state")
	passphraseFile := fs.String("passphrase-file", "", "read the passphrase from this file (encrypted archives also accept stdin)")
	jsonOut := fs.Bool("json", false, "print the restore and reconcile report as JSON")
	rootFlag := fs.String("install-root", "", "install root prefix (default: TINYSHIP_INSTALL_ROOT, empty on a real host)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship restore: exactly one backup file is required")
		return 2
	}

	root := uninstallRoot(*rootFlag)
	realHost := root == "" // same rule as backup: the real host is root-only
	if realHost && os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "tinyship restore: must run as root on the host (re-run with sudo); or use --install-root for a sandbox tree")
		return 1
	}

	// Verify the archive completely before touching any state.
	f, err := os.Open(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "tinyship restore: open %s: %v\n", pos[0], err)
		return 1
	}
	// An encrypted archive needs a passphrase before Read can verify
	// anything; probe the header so stdin is consulted exactly when it
	// will be needed (and not for plain archives).
	encrypted, perr := backup.ArchiveEncrypted(f)
	if perr != nil {
		f.Close()
		fmt.Fprintf(stderr, "tinyship restore: %v\n", perr)
		return 1
	}
	needPassphrase := encrypted && *passphraseFile == ""
	passphrase, err := backupPassphrase(needPassphrase, *passphraseFile, stdin)
	if err != nil {
		f.Close()
		fmt.Fprintf(stderr, "tinyship restore: %v\n", err)
		return 1
	}
	arc, err := backup.Read(f, passphrase)
	f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "tinyship restore: %v\n", err)
		return 1
	}
	m := arc.Manifest
	// A backup from a newer TinyShip carries a schema this binary
	// cannot read. Refusing keeps forward-only migration honest.
	if m.SchemaVersion > store.SchemaVersion() {
		fmt.Fprintf(stderr, "tinyship restore: the backup was taken at schema %d but this binary understands %d; upgrade the binary first (tinyship upgrade), then restore\n",
			m.SchemaVersion, store.SchemaVersion())
		return 1
	}

	targets := restoreTargets(root)
	memberDest := make(map[string]restoreTarget, 6)
	for _, t := range targets {
		if t.member != "_" {
			memberDest[t.member] = t
		}
	}
	for _, mem := range m.Members {
		if _, ok := memberDest[mem.Name]; !ok {
			fmt.Fprintf(stderr, "tinyship restore: the archive holds unknown member %q; refusing to guess a destination\n", mem.Name)
			return 1
		}
	}

	// Confirmation only when something would actually be replaced.
	var existing []string
	for _, t := range targets {
		if t.member == "_" {
			continue
		}
		if _, err := os.Lstat(t.path); err == nil {
			existing = append(existing, t.path)
		}
	}
	if len(existing) > 0 && !*yes {
		host, _ := os.Hostname()
		fmt.Fprintln(stdout, "The following files will be replaced (each moved to <name>.pre-restore-<UTC> first):")
		for _, p := range existing {
			fmt.Fprintf(stdout, "  %s\n", p)
		}
		fmt.Fprintf(stdout, "Type the hostname %q to confirm: ", host)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(line) != host {
			fmt.Fprintln(stderr, "tinyship restore: confirmation did not match this host's name; nothing was changed")
			return 1
		}
	}

	if realHost {
		restoreStopServices(ctx, stdout, stderr)
	}

	// Safety copies first: rename, never delete, so a hand-rolled
	// rollback is always possible.
	ts := time.Now().UTC().Format("20060102T150405Z")
	var safetyCopies []string
	for _, t := range targets {
		if t.member == "_" {
			continue
		}
		if _, err := os.Lstat(t.path); err == nil {
			cp := t.path + ".pre-restore-" + ts
			if err := os.Rename(t.path, cp); err != nil {
				fmt.Fprintf(stderr, "tinyship restore: move %s aside: %v\n", t.path, err)
				return 1
			}
			safetyCopies = append(safetyCopies, cp)
		}
		// Stale WAL sidecars belong to the moved-away database; leaving
		// them next to the restored file would corrupt it.
		for _, side := range []string{t.path + "-wal", t.path + "-shm"} {
			if _, err := os.Lstat(side); err == nil {
				if err := os.Rename(side, side+".pre-restore-"+ts); err != nil {
					fmt.Fprintf(stderr, "tinyship restore: move %s aside: %v\n", side, err)
					return 1
				}
				safetyCopies = append(safetyCopies, side+".pre-restore-"+ts)
			}
		}
	}

	// Directories the members live in (mode first so files land in a
	// correctly-scoped tree even on a fresh machine).
	var missingIdentity bool
	warnChown := func(p string, err error) {
		msg := err.Error()
		// A fresh machine has no TinyShip users yet; that is expected
		// and summarised at the end rather than per file.
		if strings.Contains(msg, "unknown user") || strings.Contains(msg, "unknown group") {
			missingIdentity = true
			return
		}
		fmt.Fprintf(stderr, "tinyship restore: chown %s: %v\n", p, err)
	}
	for _, t := range targets {
		if t.member != "_" {
			continue
		}
		if err := os.MkdirAll(t.path, unixMode(t.mode)); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: create %s: %v\n", t.path, err)
			return 1
		}
		if err := os.Chmod(t.path, unixMode(t.mode)); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: chmod %s: %v\n", t.path, err)
			return 1
		}
		if realHost {
			if err := chownPath(t.path, t.owner, t.group); err != nil {
				warnChown(t.path, err)
			}
		}
	}

	// Members: write, chmod, best-effort chown.
	for _, mem := range m.Members {
		t := memberDest[mem.Name]
		if err := os.MkdirAll(filepath.Dir(t.path), 0o750); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: create %s: %v\n", filepath.Dir(t.path), err)
			return 1
		}
		if err := os.WriteFile(t.path, arc.Files[mem.Name], unixMode(t.mode)); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: write %s: %v\n", t.path, err)
			return 1
		}
		// umask can loosen the create mode; re-assert it.
		if err := os.Chmod(t.path, unixMode(t.mode)); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: chmod %s: %v\n", t.path, err)
			return 1
		}
		if realHost {
			if err := chownPath(t.path, t.owner, t.group); err != nil {
				warnChown(t.path, err)
			}
		}
	}

	// Open the database through the store: a backup older than this
	// binary is migrated forward here, on the restored file, before any
	// service reads it.
	dbPath := memberDest["var/lib/tinyship/tinyship.db"].path
	appliedBefore := m.SchemaVersion
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship restore: open restored database: %v\n", err)
		return 1
	}
	if err := s.Close(); err != nil {
		fmt.Fprintf(stderr, "tinyship restore: close database: %v\n", err)
		return 1
	}
	// Opening the DB as root recreated its sidecars root-owned; hand
	// them to the services (the installer does the same after bootstrap).
	if realHost {
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			if _, err := os.Lstat(p); err == nil {
				if err := chownPath(p, "tinyship", "tinyship-db"); err != nil {
					warnChown(p, err)
				}
			}
		}
	}

	if realHost {
		restoreStartServices(ctx, stdout, stderr)
	}

	fmt.Fprintf(stdout, "Restored %d files from %s (taken %s, schema %d, binary schema %d).\n",
		len(m.Members), pos[0], m.CreatedAt.UTC().Format(time.RFC3339), appliedBefore, store.SchemaVersion())
	if len(safetyCopies) > 0 {
		fmt.Fprintln(stdout, "Previous state kept as:")
		for _, cp := range safetyCopies {
			fmt.Fprintf(stdout, "  %s\n", cp)
		}
	}
	if missingIdentity {
		fmt.Fprintln(stdout, "Some files are root-owned because the TinyShip users do not exist yet; re-run the installer to create them, then restore again or chown by hand.")
	}
	fmt.Fprintln(stdout, "Application volume data was not in the backup; run 'tinyship deploy <app>' to bring containers back.")

	return reportReconcile(ctx, dbPath, stdout, stderr, *jsonOut)
}

// restoreStopServices stops the two services without disabling them
// (restore is not uninstall). A missing tool or unit is reported and
// skipped: on a fresh machine there is nothing to stop.
func restoreStopServices(ctx context.Context, stdout, stderr io.Writer) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(stderr, "tinyship restore: systemctl not found; service stop skipped")
		return
	}
	for _, unit := range []string{"tinyship-api.service", "tinyship-worker.service"} {
		if out, err := restoreSystemctl(ctx, "stop", unit); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: stop %s: %s\n", unit, strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(stdout, "stopped %s\n", unit)
		}
	}
}

func restoreStartServices(ctx context.Context, stdout, stderr io.Writer) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(stderr, "tinyship restore: systemctl not found; service start skipped")
		return
	}
	for _, unit := range []string{"tinyship-api.service", "tinyship-worker.service"} {
		if out, err := restoreSystemctl(ctx, "start", unit); err != nil {
			fmt.Fprintf(stderr, "tinyship restore: start %s: %s\n", unit, strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(stdout, "started %s\n", unit)
		}
	}
}

// reportReconcile compares the restored database with what is actually
// running and prints a report-only summary: which apps have a running
// container, which need a deploy, and which running containers the
// restored database does not know. It never changes Docker state.
func reportReconcile(ctx context.Context, dbPath string, stdout, stderr io.Writer, jsonOut bool) int {
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship restore: reconcile: open database: %v\n", err)
		return 1
	}
	defer s.Close()
	apps, err := s.ListApps(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship restore: reconcile: list apps: %v\n", err)
		return 1
	}
	running, derr := runningAppContainers(ctx)
	if derr != nil {
		fmt.Fprintf(stderr, "tinyship restore: reconcile skipped: %v\n", derr)
		if jsonOut {
			fmt.Fprintln(stdout, `{"reconcile":"skipped","reason":"docker unavailable"}`)
		} else {
			fmt.Fprintln(stdout, "Reconcile skipped (Docker unavailable); the apps above still need a deploy check by hand.")
		}
		return 0
	}

	var needDeploy, upToDate, stopped []string
	for _, app := range apps {
		switch {
		case app.DesiredState != "running" || app.Deleting:
			stopped = append(stopped, app.Name)
		case running[app.ID]:
			upToDate = append(upToDate, app.Name)
		default:
			needDeploy = append(needDeploy, app.Name)
		}
	}
	// Running managed containers whose app is absent from the restored
	// database: the backup predates them. Report, never touch.
	inDB := make(map[string]bool, len(apps))
	for _, app := range apps {
		inDB[app.ID] = true
	}
	var orphans []string
	for id := range running {
		if !inDB[id] {
			orphans = append(orphans, id)
		}
	}

	if jsonOut {
		fmt.Fprintf(stdout, "{\"reconcile\":\"done\",\"up_to_date\":%q,\"need_deploy\":%q,\"stopped\":%q,\"unknown_running\":%q}\n",
			strings.Join(upToDate, ","), strings.Join(needDeploy, ","),
			strings.Join(stopped, ","), strings.Join(orphans, ","))
		return 0
	}
	fmt.Fprintln(stdout, "Reconcile report (read-only; nothing was restarted or removed):")
	if len(upToDate) > 0 {
		fmt.Fprintf(stdout, "  running:            %s\n", strings.Join(upToDate, ", "))
	}
	if len(needDeploy) > 0 {
		fmt.Fprintf(stdout, "  needs a deploy:     %s\n", strings.Join(needDeploy, ", "))
		fmt.Fprintln(stdout, "    bring each back with: tinyship deploy <app>")
	}
	if len(stopped) > 0 {
		fmt.Fprintf(stdout, "  stopped by config:  %s\n", strings.Join(stopped, ", "))
	}
	if len(orphans) > 0 {
		fmt.Fprintf(stdout, "  running but unknown to the restored database: %s\n", strings.Join(orphans, ", "))
		fmt.Fprintln(stdout, "    they were left running; remove them by hand if they are stale")
	}
	if len(apps) == 0 {
		fmt.Fprintln(stdout, "  the restored database holds no apps yet")
	}
	return 0
}
