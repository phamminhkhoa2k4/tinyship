package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCredentialNotFound is returned when a registry credential name has
// no row.
var ErrCredentialNotFound = errors.New("E_VALIDATION: registry credential not found")

// ErrCredentialInUse is returned when deleting a credential that apps
// still reference by name in their config.
var ErrCredentialInUse = errors.New("E_VALIDATION: registry credential is still referenced by apps")

// Credential is a registry login. SecretSealed is the token encrypted
// for the worker's key material; list-shaped reads use CredentialInfo
// so the sealed blob never leaves the store unless the worker asks.
type Credential struct {
	ID           string
	Name         string
	Registry     string
	Username     string
	SecretSealed []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CredentialInfo is the user-visible shape of a credential: no token.
type CredentialInfo struct {
	ID        string
	Name      string
	Registry  string
	Username  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertCredential creates or replaces a credential by name. Set is
// idempotent by design: rotating a token means writing the name again.
func (s *Store) UpsertCredential(ctx context.Context, name, registry, username string, secretSealed []byte) (*Credential, error) {
	if len(secretSealed) == 0 {
		return nil, errors.New("store: credential requires a sealed token")
	}
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO registry_credentials (id, name, registry, username,
		                                  secret_sealed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			registry = excluded.registry,
			username = excluded.username,
			secret_sealed = excluded.secret_sealed,
			updated_at = excluded.updated_at`,
		NewID(), name, registry, username, secretSealed, now, now)
	if err != nil {
		return nil, fmt.Errorf("store: upsert credential: %w", err)
	}
	return s.GetCredential(ctx, name)
}

// GetCredential fetches one credential by name with its sealed token
// (ErrCredentialNotFound when absent).
func (s *Store) GetCredential(ctx context.Context, name string) (*Credential, error) {
	var c Credential
	var createdMS, updatedMS int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, registry, username, secret_sealed, created_at, updated_at
		FROM registry_credentials WHERE name = ?`, name).
		Scan(&c.ID, &c.Name, &c.Registry, &c.Username, &c.SecretSealed,
			&createdMS, &updatedMS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q", ErrCredentialNotFound, name)
		}
		return nil, fmt.Errorf("store: get credential: %w", err)
	}
	c.CreatedAt, c.UpdatedAt = time.UnixMilli(createdMS).UTC(), time.UnixMilli(updatedMS).UTC()
	return &c, nil
}

// ListCredentials returns every credential without the sealed token,
// ordered by name.
func (s *Store) ListCredentials(ctx context.Context) ([]CredentialInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, registry, username, created_at, updated_at
		FROM registry_credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list credentials: %w", err)
	}
	defer rows.Close()
	var out []CredentialInfo
	for rows.Next() {
		var c CredentialInfo
		var createdMS, updatedMS int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Registry, &c.Username,
			&createdMS, &updatedMS); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = time.UnixMilli(createdMS).UTC(), time.UnixMilli(updatedMS).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCredential removes a credential by name. A credential that apps
// still reference is refused: deleting it out from under a config would
// turn the next deploy into an auth surprise.
func (s *Store) DeleteCredential(ctx context.Context, name string) error {
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var inUse int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM apps
			WHERE registry_credential_id IN
			      (SELECT id FROM registry_credentials WHERE name = ?)`, name).Scan(&inUse); err != nil {
			return fmt.Errorf("store: check credential use: %w", err)
		}
		if inUse > 0 {
			return fmt.Errorf("%w: %q", ErrCredentialInUse, name)
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM registry_credentials WHERE name = ?`, name)
		if err != nil {
			return fmt.Errorf("store: delete credential: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %q", ErrCredentialNotFound, name)
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return fmt.Errorf("%w: %q", ErrCredentialInUse, name)
		}
		return err
	}
	return nil
}
