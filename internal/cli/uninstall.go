package cli

// Local uninstall: removes the control plane the installer created.
// Without purge it keeps app containers, volumes and every piece of
// TinyShip state (database, keys, configuration, token) so a later
// install resumes where this one stopped; purge additionally removes
// the managed containers, volumes and bridge network the deployment
// stack owns plus all state, after listing exactly what will go and
// requiring the operator to type the hostname (or pass --yes).
//
// It runs on the host itself: whoever can stop the services and delete
// the binary already controls the machine, so routing through the API
// would only add a way to disable the control plane remotely. Docker
// objects are selected only by the TinyShip managed label, so anything
// the platform does not own is never touched.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tinyship/tinyship/internal/policy"
)

// uninstallRoot returns the install root prefix for an uninstall run:
// the --install-root flag wins, then TINYSHIP_INSTALL_ROOT; an empty
// value means the real host.
func uninstallRoot(flagRoot string) string {
	if flagRoot != "" {
		return flagRoot
	}
	return os.Getenv("TINYSHIP_INSTALL_ROOT")
}

// runDocker runs the docker CLI and returns its stdout. Tests stub it
// so the decision logic runs without a daemon.
var runDocker = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).Output()
}

// managedDocker lists the Docker objects TinyShip owns, identified only
// by the managed label. The bridge network is checked by inspecting the
// label on the well-known name, so a foreign network that happens to
// share the name is reported (and later removed) only if it carries the
// label. A missing docker CLI or daemon is reported as an error so the
// caller can warn and continue with the file phase.
var managedDocker = func(ctx context.Context) (containers, volumes []string, network bool, err error) {
	out, cerr := runDocker(ctx, "ps", "-a",
		"--filter", "label="+policy.LabelManaged+"="+policy.ManagedValue,
		"--format", "{{.ID}} {{.Names}}")
	if cerr != nil {
		return nil, nil, false, fmt.Errorf("docker ps: %w", cerr)
	}
	containers = splitLines(out)

	out, verr := runDocker(ctx, "volume", "ls", "-q",
		"--filter", "label="+policy.LabelManaged+"="+policy.ManagedValue)
	if verr != nil {
		return nil, nil, false, fmt.Errorf("docker volume ls: %w", verr)
	}
	volumes = splitLines(out)

	out, nerr := runDocker(ctx, "network", "inspect", policy.NetworkName,
		"--format", "{{index .Labels \""+policy.LabelManaged+"\"}}")
	if nerr == nil && strings.TrimSpace(string(out)) == policy.ManagedValue {
		network = true
	}
	return containers, volumes, network, nil
}

// purgeDocker removes exactly the listed managed objects. Container and
// volume entries carry an id or name followed by a display name; only
// the first field is used. Failures are collected (the caller warns and
// continues; re-running converges) rather than aborting the file phase.
var purgeDocker = func(ctx context.Context, containers, volumes []string, network bool) error {
	var failed []string
	if len(containers) > 0 {
		args := []string{"rm", "-f"}
		for _, c := range containers {
			if f := strings.Fields(c); len(f) > 0 {
				args = append(args, f[0])
			}
		}
		if _, err := runDocker(ctx, args...); err != nil {
			failed = append(failed, fmt.Sprintf("containers: %v", err))
		}
	}
	if len(volumes) > 0 {
		args := append([]string{"volume", "rm"}, volumes...)
		if _, err := runDocker(ctx, args...); err != nil {
			failed = append(failed, fmt.Sprintf("volumes: %v", err))
		}
	}
	if network {
		if _, err := runDocker(ctx, "network", "rm", policy.NetworkName); err != nil {
			failed = append(failed, fmt.Sprintf("network %s: %v", policy.NetworkName, err))
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

func splitLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func cmdUninstall(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("uninstall", stderr, `usage: tinyship uninstall [--purge] [--yes] [--install-root DIR]

Remove the TinyShip control plane from this host. Without --purge the
binary and the two services are removed while app containers, volumes,
the database, the keys, the configuration and the admin token are kept,
so a later install resumes where this one stopped.

 --purge  also remove every managed container, volume and network
            (selected only by the TinyShip label, so foreign objects
            are never touched) plus the database, keys, configuration
            and token. Everything to be removed is listed first and
            the hostname must be typed to confirm, or pass --yes.
 --yes    non-interactive confirmation for --purge.
`)
	purge := fs.Bool("purge", false, "also remove managed containers, volumes, network and all TinyShip data")
	yes := fs.Bool("yes", false, "skip the purge confirmation prompt")
	rootFlag := fs.String("install-root", "", "install root prefix (default: TINYSHIP_INSTALL_ROOT, empty on a real host)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship uninstall: unexpected argument %q\n", pos[0])
		return 2
	}

	root := uninstallRoot(*rootFlag)
	realHost := root == ""
	if realHost && os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "tinyship uninstall: requires root on a real host (re-run with sudo)")
		return 1
	}

	unitPaths := []string{
		filepath.Join(root, "etc/systemd/system/tinyship-api.service"),
		filepath.Join(root, "etc/systemd/system/tinyship-worker.service"),
	}
	dropinPath := filepath.Join(root, "etc/systemd/system/caddy.service.d/tinyship.conf")
	binaryPath := filepath.Join(root, "usr/local/bin/tinyship")
	baselinePath := filepath.Join(root, "etc/caddy/caddy.json")
	etcPath := filepath.Join(root, "etc/tinyship")
	dataPath := filepath.Join(root, "var/lib/tinyship")
	runPath := filepath.Join(root, "run/tinyship")

	if *purge {
		containers, volumes, network, err := managedDocker(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship uninstall: Docker phase skipped: %v\n", err)
			containers, volumes, network = nil, nil, false
		}
		listPurgeTargets(stdout, containers, volumes, network, []string{etcPath, dataPath, runPath, baselinePath})

		if !*yes {
			host, herr := os.Hostname()
			if herr != nil {
				host = ""
			}
			fmt.Fprintf(stdout, "Type the hostname %q to confirm: ", host)
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			if strings.TrimSpace(line) != host {
				fmt.Fprintln(stderr, "tinyship uninstall: confirmation did not match this host's name; nothing was removed")
				return 1
			}
		}

		if err := purgeDocker(ctx, containers, volumes, network); err != nil {
			fmt.Fprintf(stderr, "tinyship uninstall: docker cleanup reported: %v (re-run to converge)\n", err)
		}
		if len(containers) > 0 {
			fmt.Fprintf(stdout, "removed %d managed containers\n", len(containers))
		}
		if len(volumes) > 0 {
			fmt.Fprintf(stdout, "removed %d managed volumes\n", len(volumes))
		}
		if network {
			fmt.Fprintf(stdout, "removed the %s network\n", policy.NetworkName)
		}
	}

	stopServices(ctx, realHost, stdout, stderr)

	// Control plane on every run: units, the proxy drop-in, the binary.
	// The binary is removed last so this process keeps running from the
	// unlinked inode (Linux allows that; the kernel holds the code).
	kill := append([]string{}, unitPaths...)
	kill = append(kill, dropinPath)
	if *purge {
		kill = append(kill, etcPath, dataPath, runPath, baselinePath)
	}
	kill = append(kill, binaryPath)
	if err := removePaths(stdout, stderr, kill...); err != nil {
		fmt.Fprintf(stderr, "tinyship uninstall: %v\n", err)
		return 1
	}

	if *purge {
		if realHost {
			purgeIdentity(ctx, stderr)
		}
		fmt.Fprintln(stdout, "TinyShip fully removed: managed containers, volumes, network, services, binary, configuration, keys, database and token.")
		fmt.Fprintln(stdout, "Docker Engine, the proxy and anything without the TinyShip label were left untouched.")
		return 0
	}

	fmt.Fprintln(stdout, "TinyShip control plane removed (binary and services). Kept:")
	for _, p := range []string{dataPath, etcPath, baselinePath} {
		if _, err := os.Lstat(p); err == nil {
			fmt.Fprintf(stdout, "  %s\n", p)
		}
	}
	fmt.Fprintln(stdout, "  app containers, volumes and routes")
	fmt.Fprintln(stdout, "Remove the rest with: tinyship uninstall --purge")
	return 0
}

// listPurgeTargets prints exactly what a purge would remove, so the
// confirmation that follows is informed rather than blind.
func listPurgeTargets(stdout io.Writer, containers, volumes []string, network bool, paths []string) {
	fmt.Fprintln(stdout, "The following TinyShip-managed objects will be removed:")
	if len(containers) == 0 && len(volumes) == 0 && !network {
		fmt.Fprintln(stdout, "  docker: none present (no containers, volumes or network carry the TinyShip label)")
	}
	if len(containers) > 0 {
		fmt.Fprintf(stdout, "  containers (%d, label %s=true):\n", len(containers), policy.LabelManaged)
		for _, c := range containers {
			fmt.Fprintf(stdout, "    %s\n", c)
		}
	}
	if len(volumes) > 0 {
		fmt.Fprintf(stdout, "  volumes (%d, label %s=true):\n", len(volumes), policy.LabelManaged)
		for _, v := range volumes {
			fmt.Fprintf(stdout, "    %s\n", v)
		}
	}
	if network {
		fmt.Fprintf(stdout, "  network: %s (bridge)\n", policy.NetworkName)
	}
	fmt.Fprintln(stdout, "Control-plane files and data:")
	any := false
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			fmt.Fprintf(stdout, "    %s\n", p)
			any = true
		}
	}
	if !any {
		fmt.Fprintln(stdout, "    (none present)")
	}
}

// stopServices disables and stops the two services and reloads systemd.
// A missing tool or a missing unit is reported, not fatal: the file
// removals still run, which is the state that matters.
func stopServices(ctx context.Context, realHost bool, stdout, stderr io.Writer) {
	if !realHost {
		return // a sandbox tree never had services
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Fprintln(stderr, "tinyship uninstall: systemctl not found; service stop skipped (unit files are removed on disk)")
		return
	}
	for _, unit := range []string{"tinyship-api.service", "tinyship-worker.service"} {
		out, err := exec.CommandContext(ctx, "systemctl", "disable", "--now", unit).CombinedOutput()
		if err != nil {
			fmt.Fprintf(stderr, "tinyship uninstall: %s: %s\n", unit, strings.TrimSpace(string(out)))
			continue
		}
		fmt.Fprintf(stdout, "stopped and disabled %s\n", unit)
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "tinyship uninstall: daemon-reload: %s\n", strings.TrimSpace(string(out)))
	}
}

// purgeIdentity removes the users and groups the installer created,
// dropping the invoking admin from the admin group first so the group
// can go. Docker's own group and package are left alone: the operator
// may have had Docker before TinyShip. Everything here is best effort;
// a leftover identity is reported and harmless.
func purgeIdentity(ctx context.Context, stderr io.Writer) {
	if member := os.Getenv("SUDO_USER"); member != "" {
		_ = exec.CommandContext(ctx, "gpasswd", "-d", member, "tinyship-admin").Run()
	}
	for _, cmd := range [][]string{
		{"userdel", "tinyship-worker"},
		{"userdel", "tinyship"},
		{"groupdel", "tinyship-admin"},
		{"groupdel", "caddy-admin"},
		{"groupdel", "tinyship-db"},
		{"groupdel", "tinyship"},
	} {
		if out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			fmt.Fprintf(stderr, "tinyship uninstall: %s skipped: %s\n",
				strings.Join(cmd, " "), strings.TrimSpace(string(out)))
		}
	}
}

// removePaths deletes each existing path, skipping absent ones, and
// returns the first failure so the caller can report partial progress.
func removePaths(stdout, stderr io.Writer, paths ...string) error {
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("could not remove %s: %w", p, err)
		}
		fmt.Fprintf(stdout, "removed %s\n", p)
	}
	return nil
}
