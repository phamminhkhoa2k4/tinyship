package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithIO(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersionText(t *testing.T) {
	code, stdout, stderr := run(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"Version", "Commit", "DB schema"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	code, stdout, stderr := run(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", stdout, err)
	}
	for _, key := range []string{"version", "commit", "schema_version"} {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON missing key %q: %s", key, stdout)
		}
	}
}

func TestVersionUnknownFlag(t *testing.T) {
	code, _, stderr := run(t, "version", "--bogus")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr = %q, want unknown flag message", stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "frobnicate")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestEmptyArgsIsUsageError(t *testing.T) {
	code, _, _ := run(t)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestHelpMentionsServices(t *testing.T) {
	code, stdout, _ := run(t, "help")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "api") || !strings.Contains(stdout, "worker") {
		t.Errorf("help should mention service subcommands:\n%s", stdout)
	}
}
