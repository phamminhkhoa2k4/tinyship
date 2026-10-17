package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrSchemaTooNew is returned when the database was migrated by a newer
// binary than this one; starting must be refused.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this binary")

// Migration is one forward-only schema step (NNNN_name.sql,
// sequential, each applied in one transaction, checksummed).
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Migrations returns the embedded migrations ordered by version.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		verPart, name, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q must be NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(verPart)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: bad version: %w", e.Name(), err)
		}
		raw, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(raw),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if i > 0 && out[i-1].Version == m.Version {
			return nil, fmt.Errorf("store: duplicate migration version %d", m.Version)
		}
	}
	return out, nil
}

// SchemaVersion returns the highest migration version this binary knows
// (reported by `tinyship version`).
func SchemaVersion() int {
	migrations, err := Migrations()
	if err != nil || len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}

// Migrate applies pending migrations. It refuses to run when the DB contains
// a version this binary does not know (ErrSchemaTooNew), and refuses a
// changed checksum on an already-applied migration.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY, name TEXT NOT NULL,
  checksum TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: ensure schema_migrations: %w", err)
	}

	applied := map[int]Migration{}
	rows, err := s.db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var m Migration
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[m.Version] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	known := map[int]Migration{}
	for _, m := range migrations {
		known[m.Version] = m
	}
	// Refuse newer-than-known schema.
	for version := range applied {
		if _, ok := known[version]; !ok {
			return fmt.Errorf("%w: db has migration %d, binary knows up to %d — upgrade the binary or restore a matching backup",
				ErrSchemaTooNew, version, SchemaVersion())
		}
	}
	// Refuse tampered history: an applied migration whose file changed.
	for _, m := range migrations {
		if a, ok := applied[m.Version]; ok && a.Checksum != m.Checksum {
			return fmt.Errorf("store: migration %04d_%s was modified after being applied (checksum mismatch); forward-only migrations must never change — add a new migration instead",
				m.Version, m.Name)
		}
	}

	for _, m := range migrations {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := s.applyOne(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyOne(ctx context.Context, m Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: apply migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES(?,?,?,?)",
		m.Version, m.Name, m.Checksum, time.Now().UTC().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: record migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %04d_%s: %w", m.Version, m.Name, err)
	}
	return nil
}
