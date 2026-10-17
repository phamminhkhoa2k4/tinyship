// Package version carries build-time identity and the schema version this
// binary understands.
package version

var (
	// Version is the release version; overridden at build time via
	// -ldflags "-X github.com/tinyship/tinyship/internal/version.Version=...".
	Version = "0.1.0-dev"
	// Commit is the git commit; overridden at build time.
	Commit = "none"
	// SchemaVersion is the highest migration version this binary knows.
	// The store package sets it from its migration list at init time so
	// `tinyship version` can report DB compatibility without opening the
	// database. A DB with a higher version must cause startup refusal.
	SchemaVersion = 0
)
