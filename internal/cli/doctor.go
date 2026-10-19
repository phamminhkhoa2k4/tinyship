package cli

// doctor is the host diagnosis command: every check reports pass,
// warn or fail with a concrete fix, nothing on the host is changed,
// and a single failing check makes the exit status non-zero so a
// script can gate on it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/tinyship/tinyship/internal/caddy"
	"github.com/tinyship/tinyship/internal/docker"
	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// Injection points let tests exercise each verdict without a host
// underneath them.
var (
	doctorSystemctl = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	}
	doctorLookPath   = exec.LookPath
	doctorLookupHost = net.LookupHost
	doctorRunDocker  = runDocker
	doctorStatfs     = func(path string) (uint64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			return 0, err
		}
		return uint64(st.Bavail) * uint64(st.Bsize) / (1024 * 1024), nil
	}
	doctorCaddyAdmin = func(ctx context.Context, socket string) error {
		_, _, err := caddy.NewClient(socket).CurrentConfig(ctx)
		return err
	}
)

const (
	doctorPass = "pass"
	doctorWarn = "warn"
	doctorFail = "fail"
	// doctorMinDiskMB is the free-space floor the installer enforces:
	// images, containers and backups all grow from it.
	doctorMinDiskMB = 5120
)

// doctorCheck is one verdict: what was checked, how it ended, what
// was observed, and how to fix a non-passing result.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type doctorSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type doctorReport struct {
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
}

func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("doctor", stderr, `usage: tinyship doctor [--json] [--install-root DIR]

Diagnose this host: Docker, the proxy, file and socket permissions,
free disk, DNS for every declared domain, both services, the database
schema, and drift between the database and the running containers.

Every check reports pass, warn or fail with a suggested fix. Nothing
on the host is changed. Exits non-zero when any check fails.
`)
	jsonOut := fs.Bool("json", false, "print the machine-readable report")
	rootFlag := fs.String("install-root", "", "install root prefix (default: the real host paths)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship doctor: unexpected arguments: %s\n", strings.Join(pos, " "))
		return 2
	}
	root := uninstallRoot(*rootFlag)

	checks := []doctorCheck{}
	checks = append(checks, checkDocker(ctx)...)
	checks = append(checks, checkProxy(ctx)...)
	checks = append(checks, checkFiles(root)...)
	checks = append(checks, checkDisk(root)...)
	checks = append(checks, checkServices(ctx)...)
	checks = append(checks, checkSchema(ctx, root)...)

	c := newAPIClient()
	checks = append(checks, checkDNS(ctx, c)...)
	checks = append(checks, checkReconcile(ctx, c)...)

	var report doctorReport
	report.Checks = checks
	for _, chk := range checks {
		switch chk.Status {
		case doctorPass:
			report.Summary.Pass++
		case doctorWarn:
			report.Summary.Warn++
		case doctorFail:
			report.Summary.Fail++
		}
	}

	if *jsonOut {
		raw, err := json.Marshal(report)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship doctor: encode report: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(raw))
	} else {
		for _, chk := range checks {
			fmt.Fprintf(stdout, "  %-4s %-26s %s\n",
				strings.ToUpper(chk.Status), chk.Name, chk.Detail)
			if chk.Hint != "" && chk.Status != doctorPass {
				fmt.Fprintf(stdout, "         fix: %s\n", chk.Hint)
			}
		}
		fmt.Fprintf(stdout, "\n%d pass, %d warn, %d fail\n",
			report.Summary.Pass, report.Summary.Warn, report.Summary.Fail)
	}
	if report.Summary.Fail > 0 {
		return 1
	}
	return 0
}

// checkDocker proves the engine answers and speaks a supported API.
func checkDocker(ctx context.Context) []doctorCheck {
	out, err := doctorRunDocker(ctx, "version", "--format", "{{.Server.APIVersion}}")
	if err != nil {
		detail := firstLine(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return []doctorCheck{{
			Name:   "docker",
			Status: doctorFail,
			Detail: "engine unreachable: " + detail,
			Hint:   "install Docker Engine (API >= " + docker.MinEngineAPI + ") and start the daemon",
		}}
	}
	api := strings.TrimSpace(string(out))
	if !docker.APIAtLeast(api, docker.MinEngineAPI) {
		return []doctorCheck{{
			Name:   "docker",
			Status: doctorFail,
			Detail: fmt.Sprintf("server API %s is below the required %s", api, docker.MinEngineAPI),
			Hint:   "upgrade Docker Engine",
		}}
	}
	return []doctorCheck{{
		Name:   "docker",
		Status: doctorPass,
		Detail: fmt.Sprintf("server API %s (>= %s required)", api, docker.MinEngineAPI),
	}}
}

// checkProxy proves the proxy binary exists and its admin socket
// answers. A missing socket only warns: hosts that run without
// declared domains do not need the proxy up. A socket that exists but
// refuses to answer is a broken installed state and fails.
func checkProxy(ctx context.Context) []doctorCheck {
	if _, err := doctorLookPath("caddy"); err != nil {
		return []doctorCheck{{
			Name:   "caddy",
			Status: doctorWarn,
			Detail: "caddy binary not found",
			Hint:   "install Caddy 2: without it declared domains cannot be routed",
		}}
	}
	socket := os.Getenv("TINYSHIP_CADDY_ADMIN")
	if socket == "" {
		socket = caddy.DefaultSocket
	}
	if _, err := os.Lstat(socket); err != nil {
		return []doctorCheck{{
			Name:   "caddy",
			Status: doctorWarn,
			Detail: "admin socket " + socket + " is missing (proxy not running)",
			Hint:   "start the proxy: systemctl start caddy",
		}}
	}
	if err := doctorCaddyAdmin(ctx, socket); err != nil {
		return []doctorCheck{{
			Name:   "caddy",
			Status: doctorFail,
			Detail: "admin socket is not answering: " + firstLine(err.Error()),
			Hint:   "check the proxy logs: journalctl -u caddy -n 50",
		}}
	}
	return []doctorCheck{{
		Name:   "caddy",
		Status: doctorPass,
		Detail: "binary present, admin socket answering",
	}}
}

// checkFiles verifies the install table's modes and, on a real host,
// its owners. Under an --install-root prefix the dedicated accounts
// may not exist, so only modes are enforced there.
func checkFiles(root string) []doctorCheck {
	realHost := root == ""
	var checks []doctorCheck
	for _, t := range restoreTargets(root) {
		if t.member == "_" {
			continue
		}
		name := "file " + t.path
		st, err := os.Lstat(t.path)
		if err != nil {
			hint := "run the installer to create it"
			if os.IsPermission(err) {
				hint = "not readable as this user; re-run with sudo"
			}
			checks = append(checks, doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: "missing or unreadable: " + firstLine(err.Error()),
				Hint:   hint,
			})
			continue
		}
		want := unixMode(t.mode).Perm()
		if got := st.Mode().Perm(); got != want {
			checks = append(checks, doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("mode %04o, want %04o", got, want),
				Hint:   fmt.Sprintf("chmod %04o %s", want, t.path),
			})
			continue
		}
		if !realHost {
			checks = append(checks, doctorCheck{
				Name: name, Status: doctorPass,
				Detail: fmt.Sprintf("mode %04o", want),
			})
			continue
		}
		uid, uerr := lookupUID(t.owner)
		gid, gerr := lookupGID(t.group)
		sys, sok := st.Sys().(*syscall.Stat_t)
		if uerr != nil || gerr != nil || !sok {
			// The dedicated accounts are not created on this host
			// (yet); the mode verdict above still stands.
			checks = append(checks, doctorCheck{
				Name: name, Status: doctorWarn,
				Detail: fmt.Sprintf("mode %04o; owner %s:%s unverifiable (account missing)", want, t.owner, t.group),
				Hint:   "create the tinyship accounts: run the installer",
			})
			continue
		}
		if sys.Uid != uint32(uid) || sys.Gid != uint32(gid) {
			checks = append(checks, doctorCheck{
				Name: name, Status: doctorFail,
				Detail: fmt.Sprintf("owned %d:%d, want %s:%s", sys.Uid, sys.Gid, t.owner, t.group),
				Hint:   fmt.Sprintf("chown %s:%s %s", t.owner, t.group, t.path),
			})
			continue
		}
		checks = append(checks, doctorCheck{
			Name: name, Status: doctorPass,
			Detail: fmt.Sprintf("mode %04o, owner %s:%s", want, t.owner, t.group),
		})
	}
	return checks
}

// checkDisk keeps the installer's free-space floor visible: images,
// containers and backups all grow from it.
func checkDisk(root string) []doctorCheck {
	path := root
	if path == "" {
		path = "/"
	}
	free, err := doctorStatfs(path)
	if err != nil {
		return []doctorCheck{{
			Name:   "disk",
			Status: doctorFail,
			Detail: "cannot stat " + path + ": " + firstLine(err.Error()),
			Hint:   "check the filesystem holding the install root",
		}}
	}
	if free < doctorMinDiskMB {
		return []doctorCheck{{
			Name:   "disk",
			Status: doctorFail,
			Detail: fmt.Sprintf("free disk %d MiB is below the %d MiB minimum", free, doctorMinDiskMB),
			Hint:   "free space: docker system prune, remove old backups",
		}}
	}
	return []doctorCheck{{
		Name:   "disk",
		Status: doctorPass,
		Detail: fmt.Sprintf("free disk %d MiB (>= %d required)", free, doctorMinDiskMB),
	}}
}

// checkServices reads both units' state; it never starts them.
func checkServices(ctx context.Context) []doctorCheck {
	if _, err := doctorLookPath("systemctl"); err != nil {
		return []doctorCheck{{
			Name:   "services",
			Status: doctorWarn,
			Detail: "systemctl not found; service state unknown",
			Hint:   "install systemd, or run 'tinyship api' and 'tinyship worker' by hand",
		}}
	}
	var checks []doctorCheck
	for _, unit := range []string{"tinyship-api.service", "tinyship-worker.service"} {
		out, err := doctorSystemctl(ctx, "is-active", unit)
		state := strings.TrimSpace(string(out))
		if state == "" && err != nil {
			state = firstLine(err.Error())
		}
		if err == nil && state == "active" {
			checks = append(checks, doctorCheck{Name: unit, Status: doctorPass, Detail: "active"})
			continue
		}
		checks = append(checks, doctorCheck{
			Name:   unit,
			Status: doctorFail,
			Detail: "state: " + state,
			Hint:   fmt.Sprintf("systemctl start %s (logs: journalctl -u %s -n 50)", unit, unit),
		})
	}
	return checks
}

// checkSchema compares the on-disk schema with what the binary
// expects. Behind heals itself on the next API start; ahead means the
// binary predates its own database.
func checkSchema(ctx context.Context, root string) []doctorCheck {
	dbPath, _, _, _, _ := backupPaths(root)
	if _, err := os.Lstat(dbPath); err != nil {
		return []doctorCheck{{
			Name:   "database schema",
			Status: doctorFail,
			Detail: "database not found at " + dbPath,
			Hint:   "run the installer to create it",
		}}
	}
	s, err := store.OpenPlain(dbPath)
	if err != nil {
		hint := "run the installer"
		if os.IsPermission(err) {
			hint = "not readable as this user; re-run with sudo"
		}
		return []doctorCheck{{
			Name:   "database schema",
			Status: doctorFail,
			Detail: "cannot open the database: " + firstLine(err.Error()),
			Hint:   hint,
		}}
	}
	defer s.Close()
	applied, err := s.AppliedSchemaVersion(ctx)
	if err != nil {
		return []doctorCheck{{
			Name:   "database schema",
			Status: doctorFail,
			Detail: "cannot read the schema version: " + firstLine(err.Error()),
			Hint:   "re-run with sudo, or restore the database from a backup",
		}}
	}
	want := store.SchemaVersion()
	switch {
	case applied == want:
		return []doctorCheck{{
			Name:   "database schema",
			Status: doctorPass,
			Detail: fmt.Sprintf("schema %d matches the binary", applied),
		}}
	case applied < want:
		return []doctorCheck{{
			Name:   "database schema",
			Status: doctorWarn,
			Detail: fmt.Sprintf("schema %d is behind the binary's %d", applied, want),
			Hint:   "start the API once; it migrates the database forward on boot",
		}}
	default:
		return []doctorCheck{{
			Name:   "database schema",
			Status: doctorFail,
			Detail: fmt.Sprintf("schema %d is newer than the binary's %d", applied, want),
			Hint:   "upgrade the binary first (tinyship upgrade), then restart the services",
		}}
	}
}

// checkDNS resolves every declared domain. Non-resolution warns
// rather than fails: DNS may still be propagating, but until the name
// points here nothing routes and no certificate is issued.
func checkDNS(ctx context.Context, c *apiClient) []doctorCheck {
	apps, err := fetchStatusApps(ctx, c)
	if err != nil {
		return []doctorCheck{{
			Name:   "dns",
			Status: doctorWarn,
			Detail: "skipped: API unreachable: " + firstLine(err.Error()),
			Hint:   "start the API: tinyship api",
		}}
	}
	var checks []doctorCheck
	for _, app := range apps {
		var view struct {
			Domains []domainView `json:"domains"`
		}
		if err := c.doJSON(ctx, http.MethodGet, "/apps/"+app.Name+"/domains", nil, &view); err != nil {
			checks = append(checks, doctorCheck{
				Name:   "dns (" + app.Name + ")",
				Status: doctorWarn,
				Detail: "skipped: " + firstLine(err.Error()),
				Hint:   "check the API logs",
			})
			continue
		}
		for _, d := range view.Domains {
			ips, lerr := doctorLookupHost(d.Hostname)
			name := "dns " + d.Hostname
			if lerr != nil {
				checks = append(checks, doctorCheck{
					Name:   name,
					Status: doctorWarn,
					Detail: "does not resolve (status " + d.Status + ")",
					Hint: "point an A/AAAA record at this host; until then the hostname " +
						"stays " + d.Status + " and receives no traffic",
				})
				continue
			}
			checks = append(checks, doctorCheck{
				Name:   name,
				Status: doctorPass,
				Detail: "resolves to " + strings.Join(ips, ", ") + " (" + d.Status + ")",
			})
		}
	}
	if len(checks) == 0 {
		return []doctorCheck{{
			Name:   "dns",
			Status: doctorPass,
			Detail: "no domains declared",
		}}
	}
	return checks
}

// checkReconcile reports drift between the database's intent and the
// containers actually running. Report-only, like the worker's boot
// check: doctor never starts or stops anything.
func checkReconcile(ctx context.Context, c *apiClient) []doctorCheck {
	apps, err := fetchStatusApps(ctx, c)
	if err != nil {
		return []doctorCheck{{
			Name:   "reconcile",
			Status: doctorWarn,
			Detail: "skipped: API unreachable: " + firstLine(err.Error()),
			Hint:   "start the API: tinyship api",
		}}
	}
	rep, err := statusReconcile(ctx, apps)
	if err != nil {
		return []doctorCheck{{
			Name:   "reconcile",
			Status: doctorWarn,
			Detail: "unknown: docker unavailable: " + firstLine(err.Error()),
			Hint:   "start the Docker daemon",
		}}
	}
	if !rep.Drift() {
		return []doctorCheck{{
			Name:   "reconcile",
			Status: doctorPass,
			Detail: fmt.Sprintf("in sync: %d running, %d stopped by config",
				len(rep.UpToDate), len(rep.Stopped)),
		}}
	}
	return []doctorCheck{{
		Name:   "reconcile",
		Status: doctorFail,
		Detail: driftSummary(rep),
		Hint: fmt.Sprintf(
			"deploy what is down (tinyship deploy <app>); containers the database ignores: docker ps --filter label=%s=%s",
			policy.LabelManaged, policy.ManagedValue),
	}}
}

// firstLine keeps multi-line tool errors to their first meaningful
// line so one check stays one line.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return s
}
