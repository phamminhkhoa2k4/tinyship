package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/policy"
)

// dockerRecorder stands in for the Docker phase so the command logic is
// exercised without a daemon (and without ever touching one).
type dockerRecorder struct {
	listCalled   bool
	purgeCalled  bool
	containers   []string
	volumes      []string
	network      bool
	listErr      error
	purgeArgs    []string
	purgeVolumes []string
	purgeNetwork bool
}

func stubDocker(t *testing.T, containers, volumes []string, network bool, listErr error) *dockerRecorder {
	t.Helper()
	rec := &dockerRecorder{
		containers: containers,
		volumes:    volumes,
		network:    network,
		listErr:    listErr,
	}
	prevList, prevPurge := managedDocker, purgeDocker
	managedDocker = func(ctx context.Context) ([]string, []string, bool, error) {
		rec.listCalled = true
		return rec.containers, rec.volumes, rec.network, rec.listErr
	}
	purgeDocker = func(ctx context.Context, c, v []string, n bool) error {
		rec.purgeCalled = true
		rec.purgeArgs = c
		rec.purgeVolumes = v
		rec.purgeNetwork = n
		return nil
	}
	t.Cleanup(func() { managedDocker, purgeDocker = prevList, prevPurge })
	return rec
}

// fakeInstall writes the artifacts an install produces under root.
func fakeInstall(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"etc/systemd/system/tinyship-api.service":          "[Unit]\n",
		"etc/systemd/system/tinyship-worker.service":       "[Unit]\n",
		"etc/systemd/system/caddy.service.d/tinyship.conf": "ExecStart=\n",
		"etc/caddy/caddy.json":                             "{}\n",
		"usr/local/bin/tinyship":                           "binary\n",
		"etc/tinyship/token":                               "tok\n",
		"etc/tinyship/config.yaml":                         "host: {}\n",
		"etc/tinyship/keys/secrets.key":                    "key\n",
		"etc/tinyship/keys/secrets.pub":                    "pub\n",
		"var/lib/tinyship/tinyship.db":                     "db\n",
		"run/tinyship/api.sock":                            "",
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
}

func present(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestUninstallDefaultKeepsDataAndNeverTouchesDocker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install-root")
	fakeInstall(t, root)
	rec := stubDocker(t, nil, nil, false, nil)

	code, stdout, stderr := run(t, "uninstall", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if rec.listCalled || rec.purgeCalled {
		t.Error("the default path must not talk to Docker at all")
	}
	// Control plane gone.
	for _, rel := range []string{
		"etc/systemd/system/tinyship-api.service",
		"etc/systemd/system/tinyship-worker.service",
		"etc/systemd/system/caddy.service.d/tinyship.conf",
		"usr/local/bin/tinyship",
	} {
		if present(filepath.Join(root, rel)) {
			t.Errorf("default uninstall kept %s", rel)
		}
	}
	// Everything else kept: containers/volumes are untouched by design,
	// and TinyShip's own data survives so a reinstall resumes.
	for _, rel := range []string{
		"var/lib/tinyship/tinyship.db",
		"etc/tinyship/token",
		"etc/tinyship/config.yaml",
		"etc/tinyship/keys/secrets.key",
		"etc/caddy/caddy.json",
	} {
		if !present(filepath.Join(root, rel)) {
			t.Errorf("default uninstall removed %s (must be kept)", rel)
		}
	}
	if !strings.Contains(stdout, "Kept:") {
		t.Errorf("output must state what was kept:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--purge") {
		t.Errorf("output must point at the purge option:\n%s", stdout)
	}
}

func TestUninstallPurgeListsAndRefusesWithoutConfirmation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install-root")
	fakeInstall(t, root)
	rec := stubDocker(t, []string{"abc1234 ts-blog-3"}, []string{"ts-blog-data"}, true, nil)
	swapStdin(t, "nope\n")

	code, stdout, stderr := run(t, "uninstall", "--purge", "--install-root", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (refused), stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if rec.purgeCalled {
		t.Error("refused purge must not delete Docker objects")
	}
	// The listing shows exactly what would go: containers, volumes, the
	// network, and the control-plane paths that exist.
	for _, want := range []string{
		"will be removed",
		"abc1234 ts-blog-3",
		"ts-blog-data",
		"network: " + policy.NetworkName,
		root + string(filepath.Separator) + filepath.Join("var", "lib", "tinyship"),
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing missing %q:\n%s", want, stdout)
		}
	}
	host, _ := os.Hostname()
	if !strings.Contains(stdout, host) {
		t.Errorf("confirmation prompt must show this host's name %q:\n%s", host, stdout)
	}
	if !strings.Contains(stderr, "nothing was removed") {
		t.Errorf("refusal must say nothing was removed: %q", stderr)
	}
	// Nothing on disk was touched either.
	if !present(filepath.Join(root, "var/lib/tinyship/tinyship.db")) {
		t.Error("refused purge removed data anyway")
	}
	if !present(filepath.Join(root, "usr/local/bin/tinyship")) {
		t.Error("refused purge removed the binary anyway")
	}
}

func TestUninstallPurgeYesRemovesOnlyWhatWasListed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install-root")
	fakeInstall(t, root)
	rec := stubDocker(t, []string{"abc1234 ts-blog-3"}, []string{"ts-blog-data"}, true, nil)

	code, stdout, stderr := run(t, "uninstall", "--purge", "--yes", "--install-root", root)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q, stdout = %q", code, stderr, stdout)
	}
	if !rec.purgeCalled {
		t.Fatal("purge did not run the Docker phase")
	}
	if len(rec.purgeArgs) != 1 || !strings.HasPrefix(rec.purgeArgs[0], "abc1234") {
		t.Errorf("purge containers = %q, want exactly the listed managed container", rec.purgeArgs)
	}
	if len(rec.purgeVolumes) != 1 || rec.purgeVolumes[0] != "ts-blog-data" {
		t.Errorf("purge volumes = %q, want exactly the listed managed volume", rec.purgeVolumes)
	}
	if !rec.purgeNetwork {
		t.Error("purge must remove the managed network")
	}
	// Every control-plane path is gone.
	for _, rel := range []string{
		"etc/systemd/system/tinyship-api.service",
		"etc/systemd/system/tinyship-worker.service",
		"etc/systemd/system/caddy.service.d/tinyship.conf",
		"usr/local/bin/tinyship",
		"etc/tinyship",
		"var/lib/tinyship",
		"run/tinyship",
		"etc/caddy/caddy.json",
	} {
		if present(filepath.Join(root, rel)) {
			t.Errorf("purge kept %s", rel)
		}
	}
	if !strings.Contains(stdout, "fully removed") {
		t.Errorf("purge must report completion:\n%s", stdout)
	}
	if !strings.Contains(stdout, "left untouched") {
		t.Errorf("purge must state what it never touches:\n%s", stdout)
	}
}

// TestUninstallDockerSelectionFiltersOnTheManagedLabel proves the only
// selector the uninstaller ever passes is the TinyShip label: an
// unlabeled container or volume cannot match any of these commands, so
// a purge cannot remove one.
func TestUninstallDockerSelectionFiltersOnTheManagedLabel(t *testing.T) {
	var calls [][]string
	prev := runDocker
	runDocker = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		switch {
		case args[0] == "ps":
			return []byte("abc1234 ts-blog-3\n"), nil
		case args[0] == "volume" && args[1] == "ls":
			return []byte("ts-blog-data\n"), nil
		case args[0] == "network" && args[1] == "inspect":
			return []byte(policy.ManagedValue + "\n"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { runDocker = prev })

	containers, volumes, network, err := managedDocker(context.Background())
	if err != nil {
		t.Fatalf("managedDocker: %v", err)
	}
	if len(containers) != 1 || containers[0] != "abc1234 ts-blog-3" {
		t.Errorf("containers = %q", containers)
	}
	if len(volumes) != 1 || volumes[0] != "ts-blog-data" {
		t.Errorf("volumes = %q", volumes)
	}
	if !network {
		t.Error("labeled network not detected")
	}
	wantFilter := "label=" + policy.LabelManaged + "=" + policy.ManagedValue
	var sawLabelFilter bool
	for _, c := range calls {
		for i, a := range c {
			if a == "--filter" && i+1 < len(c) && c[i+1] == wantFilter {
				sawLabelFilter = true
			}
		}
	}
	if !sawLabelFilter {
		t.Errorf("no command filtered on %q; calls: %v", wantFilter, calls)
	}
	// The removal argv targets exactly the listed objects, by name/id.
	calls = nil
	if err := purgeDocker(context.Background(), containers, volumes, network); err != nil {
		t.Fatalf("purgeDocker: %v", err)
	}
	want := [][]string{
		{"rm", "-f", "abc1234"},
		{"volume", "rm", "ts-blog-data"},
		{"network", "rm", policy.NetworkName},
	}
	if len(calls) != len(want) {
		t.Fatalf("removal calls = %v, want %v", calls, want)
	}
	for i := range want {
		if strings.Join(calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, calls[i], want[i])
		}
	}
}

// TestUninstallNetworkNameCollisionRespectsTheLabel: a foreign network
// that happens to use the well-known name is reported as not ours and
// never passed to removal.
func TestUninstallNetworkNameCollisionRespectsTheLabel(t *testing.T) {
	prev := runDocker
	runDocker = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "network" && args[1] == "inspect" {
			return []byte("\n"), nil // label absent
		}
		return nil, nil
	}
	t.Cleanup(func() { runDocker = prev })

	_, _, network, err := managedDocker(context.Background())
	if err != nil {
		t.Fatalf("managedDocker: %v", err)
	}
	if network {
		t.Error("unlabeled network must not be treated as managed")
	}
}

func TestUninstallRequiresRootOnARealHost(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the root requirement cannot trigger")
	}
	t.Setenv("TINYSHIP_INSTALL_ROOT", "")
	code, _, stderr := run(t, "uninstall")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "requires root") {
		t.Errorf("stderr = %q, want the root requirement stated", stderr)
	}
}

func TestUninstallRejectsUnexpectedArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "install-root")
	code, _, _ := run(t, "uninstall", "--install-root", root, "extra")
	if code != 2 {
		t.Errorf("exit = %d, want 2 for an unexpected argument", code)
	}
}
