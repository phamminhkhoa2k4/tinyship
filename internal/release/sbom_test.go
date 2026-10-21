package release

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseModules(t *testing.T) {
	input := `{"Path":"github.com/tinyship/tinyship","Main":true}
{"Path":"github.com/docker/docker","Version":"v28.5.2+incompatible"}
{"Path":"example.com/old","Version":"v1.0.0","Replace":{"Path":"example.com/new","Version":"v2.0.0"}}
`
	mods, err := ParseModules(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 3 {
		t.Fatalf("got %d modules, want 3", len(mods))
	}
	if !mods[0].Main {
		t.Error("first module should be the main module")
	}
	if mods[2].Replace == nil || mods[2].Replace.Path != "example.com/new" {
		t.Errorf("replacement not parsed: %+v", mods[2])
	}
}

func TestParseModulesRejectsBadInput(t *testing.T) {
	for _, input := range []string{"{", `{"Version":"v1.0.0"}`} {
		if _, err := ParseModules(strings.NewReader(input)); err == nil {
			t.Errorf("ParseModules(%q) succeeded, want an error", input)
		}
	}
}

func TestBuildSBOM(t *testing.T) {
	mods := []Module{
		{Path: "github.com/tinyship/tinyship", Main: true},
		{Path: "github.com/docker/docker", Version: "v28.5.2+incompatible"},
		{Path: "example.com/old", Version: "v1.0.0", Replace: &Module{Path: "example.com/new", Version: "v2.0.0"}},
	}
	raw, err := BuildSBOM(mods, SBOMOptions{
		Version:   "v0.1.0-rc.1",
		Commit:    "abc1234",
		Timestamp: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc SBOM
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("SBOM is not valid JSON: %v\n%s", err, raw)
	}
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != "1.5" {
		t.Errorf("format = %s/%s, want CycloneDX/1.5", doc.BOMFormat, doc.SpecVersion)
	}
	if doc.Metadata.Component.Version != "0.1.0-rc.1" {
		t.Errorf("version = %q, want 0.1.0-rc.1", doc.Metadata.Component.Version)
	}
	if got := doc.Metadata.Component.Properties[0].Value; got != "abc1234" {
		t.Errorf("commit = %q, want abc1234", got)
	}
	if len(doc.Components) != 2 {
		t.Fatalf("got %d components, want 2: %+v", len(doc.Components), doc.Components)
	}
	// Components are sorted by bom-ref, so the replacement comes first.
	if doc.Components[0].Name != "example.com/new" || doc.Components[0].Version != "v2.0.0" {
		t.Errorf("replacement component = %+v", doc.Components[0])
	}
	if doc.Components[1].PURL != "pkg:golang/github.com/docker/docker@v28.5.2+incompatible" {
		t.Errorf("purl = %q", doc.Components[1].PURL)
	}
}

func TestBuildSBOMIsDeterministic(t *testing.T) {
	mods := []Module{
		{Path: "github.com/tinyship/tinyship", Main: true},
		{Path: "z.example/mod", Version: "v1.0.0"},
		{Path: "a.example/mod", Version: "v1.0.0"},
	}
	opts := SBOMOptions{Version: "0.1.0", Timestamp: time.Unix(0, 0).UTC()}
	a, err := BuildSBOM(mods, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildSBOM(mods, opts)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("two SBOM builds differ")
	}
}

func TestBuildSBOMNeedsVersion(t *testing.T) {
	_, err := BuildSBOM([]Module{{Path: "github.com/tinyship/tinyship", Main: true}}, SBOMOptions{})
	if err == nil {
		t.Fatal("BuildSBOM succeeded without a version")
	}
}
