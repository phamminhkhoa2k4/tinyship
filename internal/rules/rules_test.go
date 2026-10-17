package rules

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Patterns are written so their own source never matches: each word-boundary
// pattern is preceded by the raw word "b" inside its literal, the section-sign
// pattern is a unicode escape, and the remaining shapes are assembled from
// fragments at runtime.
var bannedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bSPEC\b`),
	regexp.MustCompile(`\bBACKLOG\b`),
	regexp.MustCompile(`\bADR-\d`),
	regexp.MustCompile(`\bUS-\d`),
	regexp.MustCompile(`\bD-\d`),
	regexp.MustCompile(`\bE\d{1,2}\b`),
	regexp.MustCompile(`\bGroup \d`),
	regexp.MustCompile("\\.m" + "d\\b"),
}

var bannedSubstrings = []string{"doc" + "s/", "\u00A7"}

// matchesReference reports which banned reference shapes appear in line.
func matchesReference(line string) []string {
	var hits []string
	for _, rx := range bannedPatterns {
		if rx.MatchString(line) {
			hits = append(hits, rx.String())
		}
	}
	for _, s := range bannedSubstrings {
		if strings.Contains(line, s) {
			hits = append(hits, fmt.Sprintf("%q", s))
		}
	}
	return hits
}

func scanTarget(name string) bool {
	switch {
	case strings.HasSuffix(name, ".go"),
		strings.HasSuffix(name, ".sh"),
		strings.HasSuffix(name, ".yml"),
		strings.HasSuffix(name, ".yaml"):
		return true
	case name == "Makefile":
		return true
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

// TestNoDocumentationReferencesInCode scans every source file in the module
// and fails when a line carries a documentation reference.
func TestNoDocumentationReferencesInCode(t *testing.T) {
	root := moduleRoot(t)
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", ".agents", ".harness-core":
				return fs.SkipDir
			}
			return nil
		}
		if !scanTarget(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, hit := range matchesReference(line) {
				violations = append(violations,
					fmt.Sprintf("%s:%d uses %s", rel, i+1, hit))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// TestDetectorCatchesReferenceShapes proves the matcher flags every banned
// shape and stays quiet on legitimate code, using lines assembled at runtime.
func TestDetectorCatchesReferenceShapes(t *testing.T) {
	caught := []string{
		"SP" + "EC \u00A77.1",
		"BACK" + "LOG entry",
		"AD" + "R-001 decided it",
		"U" + "S-5.2 asked for it",
		"D" + "-10 chose the loopback bind",
		"E" + "6 is the routing epic",
		"Gro" + "up 4 shipped it",
		"doc" + "s/plans/active/next" + ".m" + "d",
		"readme" + ".m" + "d path",
		"plain \u00A7 sign",
	}
	for _, line := range caught {
		if hits := matchesReference(line); len(hits) == 0 {
			t.Errorf("detector missed %q", line)
		}
	}
	quiet := []string{
		"ContainerSpec maps to HostConfig",
		"E_" + "VALIDATION is the error code",
		"01ARZ3NNEJKX2E6QNGX is a fixture id",
		"logStore is the slice of store.Store",
		"config.md5 is not a doc name",
		"GroupID field on the job row",
	}
	for _, line := range quiet {
		if hits := matchesReference(line); len(hits) > 0 {
			t.Errorf("detector false-positived %q on %v", line, hits)
		}
	}
}

// TestDocumentationIsUntracked proves documentation files never enter the
// git index; it skips when the checkout has no git metadata.
func TestDocumentationIsUntracked(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.Command("git", "-C", root, "ls-files")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.Error); ok && ee.Err == exec.ErrNotFound {
			t.Skip("git not available")
		}
		if _, ok := err.(*exec.ExitError); ok {
			t.Skip("git ls-files failed (no git metadata checkout)")
		}
		t.Fatal(err)
	}
	for _, f := range strings.Fields(string(out)) {
		switch {
		case strings.HasSuffix(f, ".m"+"d"):
			t.Errorf("documentation file is tracked: %s", f)
		case strings.HasPrefix(f, "doc"+"s/"):
			t.Errorf("documentation tree is tracked: %s", f)
		case f == "LICENSE" || f == "AGENTS"+".m"+"d":
			t.Errorf("local-only file is tracked: %s", f)
		}
	}
}
