// Package store owns the SQLite database: schema migrations and the
// durable job queue. Every open connection gets the PRAGMAs
// required by design: WAL, foreign keys on, busy_timeout 5000, synchronous
// FULL (durability of job state beats throughput).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver; no cgo, no system libsqlite3
)

// Store wraps a *sql.DB holding TinyShip state.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the database at path with required PRAGMAs
// and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	s, err := OpenPlain(path)
	if err != nil {
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// OpenPlain opens the database without running migrations.
func OpenPlain(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	// modernc.org/sqlite applies _pragma entries to every pooled connection,
	// which is required: pragmas like foreign_keys are per-connection.
	// _txlock=immediate makes every transaction take the write lock up front
	// for claim correctness: read-then-write must not upgrade and
	// deadlock against a concurrent claimer.
	dsn := "file:" + url.PathEscape(abs) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(FULL)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite allows one writer; a small pool plus busy_timeout is enough and
	// avoids SQLITE_BUSY storms under the worker heartbeat.
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle for tests and future read paths.
// Product code should prefer Store methods.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// WithTx runs fn inside a transaction (IMMEDIATE via DSN _txlock),
// committing on success and rolling back on error.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// contains reports whether sub appears in s (used in error assertions).
func contains(s, sub string) bool { return strings.Contains(s, sub) }
