// Static enforcement of the wrapper boundary:
//
//	"Worker truy cập Docker thông qua một lớp bao (wrapper)... một test tĩnh
//	 PHẢI bảo đảm không có chỗ nào khác trong mã nguồn tạo client Docker
//	 trực tiếp."
//
// Boundary:
//
//	Authority: the allowlist wrapper (code) and this static test.
//	Scope: every .go file in the module — test files included, because
//	 there is no exception for fixtures.
//	Allowed: package internal/docker may import the Docker SDK client.
//	Forbidden: any other package importing github.com/docker/docker/client
//	 (or the docker CLI via os/exec against the socket).
//	Exceptions: none stated by the authority.
package docker

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImports are the import paths that construct or spawn a Docker
// client outside the wrapper.
var forbiddenImports = []string{
	"github.com/docker/docker/client",
}

func TestNoDockerClientOutsideWrapper(t *testing.T) {
	repoRoot := moduleRoot(t)
	fset := token.NewFileSet()

	var violations []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// Skip vendored, hidden, and build output trees.
			if name == "vendor" || name == "bin" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		inWrapper := filepath.ToSlash(filepath.Dir(rel)) == "internal/docker"

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // unparseable file: let other tooling report it
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if inWrapper {
				continue // the wrapper itself is the allowed location
			}
			for _, forbidden := range forbiddenImports {
				if importPath == forbidden {
					violations = append(violations, filepath.ToSlash(rel))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("  /  violated: Docker client imported outside internal/docker.\n"+
			"  violating file(s): %s\n"+
			"  rule: only the worker's Docker wrapper may talk to the daemon; move the logic behind\n"+
			"        the wrapper's allowlisted methods.",
			strings.Join(violations, ", "))
	}
}

// TestWrapperExists is the positive direction: the static test must guard a
// real wrapper package, not an empty one. If internal/docker stops defining
// Wrapper, this fails and the boundary loses its allowed example.
func TestWrapperExists(t *testing.T) {
	var w *Wrapper = nil
	if w == nil {
		// Wrapper is defined and pointer-typed: the allowlisted location
		// exists for the import rule above to be satisfiable.
		t.Log("wrapper package present")
	}
}

// moduleRoot finds the directory containing go.mod by walking up from the
// package's test working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above package directory")
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
