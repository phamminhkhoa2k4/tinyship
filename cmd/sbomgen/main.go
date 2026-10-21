// Command sbomgen writes the CycloneDX SBOM published with a release.
//
// It is build tooling, not part of the installed CLI.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/tinyship/tinyship/internal/release"
)

func main() {
	version := flag.String("version", "", "release version without the leading v")
	commit := flag.String("commit", "none", "release commit")
	modulesPath := flag.String("modules", "", "module JSON file (default: go list -m -json all)")
	outPath := flag.String("out", "", "output file (default: stdout)")
	flag.Parse()

	if *version == "" {
		fmt.Fprintln(os.Stderr, "sbomgen: -version is required")
		os.Exit(2)
	}
	modules, err := readModules(*modulesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbomgen: %v\n", err)
		os.Exit(1)
	}
	ts := time.Now().UTC()
	if raw := os.Getenv("SOURCE_DATE_EPOCH"); raw != "" {
		sec, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sbomgen: SOURCE_DATE_EPOCH: %v\n", err)
			os.Exit(1)
		}
		ts = time.Unix(sec, 0).UTC()
	}
	doc, err := release.BuildSBOM(modules, release.SBOMOptions{
		Version:   *version,
		Commit:    *commit,
		Timestamp: ts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbomgen: %v\n", err)
		os.Exit(1)
	}
	if *outPath == "" {
		if _, err := os.Stdout.Write(doc); err != nil {
			fmt.Fprintf(os.Stderr, "sbomgen: write stdout: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(*outPath, doc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "sbomgen: write %s: %v\n", *outPath, err)
		os.Exit(1)
	}
}

func readModules(path string) ([]release.Module, error) {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open module list: %w", err)
		}
		defer f.Close()
		return release.ParseModules(f)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("go", "list", "-m", "-json", "all")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return nil, fmt.Errorf("go list: %s: %w", msg, err)
		}
		return nil, fmt.Errorf("go list: %w", err)
	}
	return release.ParseModules(&stdout)
}
