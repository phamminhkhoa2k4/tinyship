// Package release builds the metadata published with a TinyShip binary.
package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sbomFormat  = "CycloneDX"
	specVersion = "1.5"
)

// Module is the subset of `go list -m -json all` output needed for an
// SBOM. Replace is set when the build uses a different module version.
type Module struct {
	Path    string  `json:"Path"`
	Version string  `json:"Version"`
	Main    bool    `json:"Main"`
	Replace *Module `json:"Replace,omitempty"`
}

// SBOMOptions carries the identity of the binary being described.
type SBOMOptions struct {
	Version   string
	Commit    string
	Timestamp time.Time
}

// SBOM is the CycloneDX document written beside a release binary.
type SBOM struct {
	BOMFormat   string      `json:"bomFormat"`
	SpecVersion string      `json:"specVersion"`
	Version     int         `json:"version"`
	Metadata    Metadata    `json:"metadata"`
	Components  []Component `json:"components,omitempty"`
}

// Metadata identifies the application component.
type Metadata struct {
	Timestamp string    `json:"timestamp,omitempty"`
	Component Component `json:"component"`
}

// Component is a CycloneDX component. Only the fields TinyShip emits are
// modelled here; unknown fields are not needed by consumers.
type Component struct {
	Type       string     `json:"type"`
	BOMRef     string     `json:"bom-ref,omitempty"`
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	PURL       string     `json:"purl,omitempty"`
	Properties []Property `json:"properties,omitempty"`
}

// Property is a namespaced key/value pair attached to a component.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ParseModules reads the concatenated JSON objects emitted by
// `go list -m -json all`.
func ParseModules(r io.Reader) ([]Module, error) {
	dec := json.NewDecoder(r)
	var modules []Module
	for {
		var m Module
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("release: parse module list: %w", err)
		}
		if m.Path == "" {
			return nil, errors.New("release: module list contains an empty path")
		}
		modules = append(modules, m)
	}
	if len(modules) == 0 {
		return nil, errors.New("release: module list is empty")
	}
	return modules, nil
}

// BuildSBOM returns a deterministic CycloneDX 1.5 document for the
// application and its module dependencies.
func BuildSBOM(modules []Module, opts SBOMOptions) ([]byte, error) {
	if len(modules) == 0 {
		return nil, errors.New("release: no modules for SBOM")
	}
	version := strings.TrimPrefix(opts.Version, "v")
	if version == "" {
		return nil, errors.New("release: SBOM needs a version")
	}
	ts := opts.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	mainRef := "pkg:golang/github.com/tinyship/tinyship@" + version
	doc := SBOM{
		BOMFormat:   sbomFormat,
		SpecVersion: specVersion,
		Version:     1,
		Metadata: Metadata{
			Timestamp: ts.UTC().Format(time.RFC3339),
			Component: Component{
				Type:    "application",
				BOMRef:  mainRef,
				Name:    "tinyship",
				Version: version,
				PURL:    mainRef,
				Properties: []Property{
					{Name: "tinyship:commit", Value: opts.Commit},
				},
			},
		},
	}

	seen := make(map[string]struct{})
	for _, m := range modules {
		if m.Main {
			continue
		}
		path, modVersion := m.Path, m.Version
		props := []Property(nil)
		if m.Replace != nil {
			props = append(props, Property{
				Name:  "tinyship:replaced",
				Value: m.Path + "@" + m.Version,
			})
			path = m.Replace.Path
			if m.Replace.Version != "" {
				modVersion = m.Replace.Version
			} else {
				// A local replacement has no release version to record.
				modVersion = "0.0.0-local"
			}
		}
		if path == "" {
			return nil, fmt.Errorf("release: module %q has an empty replacement path", m.Path)
		}
		ref := modulePURL(path, modVersion)
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		doc.Components = append(doc.Components, Component{
			Type:       "library",
			BOMRef:     ref,
			Name:       path,
			Version:    modVersion,
			PURL:       ref,
			Properties: props,
		})
	}
	sort.Slice(doc.Components, func(i, j int) bool {
		return doc.Components[i].BOMRef < doc.Components[j].BOMRef
	})

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("release: encode SBOM: %w", err)
	}
	return append(out, '\n'), nil
}

func modulePURL(path, version string) string {
	p := "pkg:golang/" + escapePURLPath(path)
	if version != "" {
		p += "@" + url.PathEscape(version)
	}
	return p
}

// escapePURLPath keeps the slashes that separate a Go module path while
// escaping characters that would break the purl.
func escapePURLPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
