package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Env kinds, mirrored from the schema CHECK on app_env.kind.
const (
	EnvPlain  = "plain"
	EnvSecret = "secret"
)

// ErrEnvNotFound is returned when an env key does not exist for the app.
var ErrEnvNotFound = errors.New("E_VALIDATION: env key not found")

// ErrEnvInvalid is returned when the kind and value columns would
// violate the app_env CHECK (wrong pairing of clear and sealed data).
var ErrEnvInvalid = errors.New("E_VALIDATION: env value does not match kind")

// EnvVar is one row of app_env. Sealed secrets arrive as ValueSealed;
// only the worker's key material can open them, so callers that serve
// users must never echo ValueSealed back.
type EnvVar struct {
	Key         string
	Kind        string
	ValuePlain  string
	ValueSealed []byte
	UpdatedAt   time.Time
}

// SetAppEnv upserts one environment variable. kind picks the column
// pair: plain stores ValuePlain and refuses a sealed payload; secret
// stores ValueSealed and refuses a clear value. The schema CHECK backs
// this up, but a clear store error beats a constraint violation.
func (s *Store) SetAppEnv(ctx context.Context, appID, key, kind, valuePlain string, valueSealed []byte) error {
	switch kind {
	case EnvPlain:
		if valueSealed != nil {
			return fmt.Errorf("%w: plain env must not carry a sealed value", ErrEnvInvalid)
		}
	case EnvSecret:
		if valueSealed == nil {
			return fmt.Errorf("%w: secret env must carry a sealed value", ErrEnvInvalid)
		}
		if valuePlain != "" {
			return fmt.Errorf("%w: secret env must not carry a clear value", ErrEnvInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown env kind %q", ErrEnvInvalid, kind)
	}
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_env (app_id, key, kind, value_plain, value_sealed, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id, key) DO UPDATE SET
			kind = excluded.kind,
			value_plain = excluded.value_plain,
			value_sealed = excluded.value_sealed,
			updated_at = excluded.updated_at`,
		appID, key, kind, plainValueFor(kind, valuePlain), sealedValueFor(kind, valueSealed), now)
	if err != nil {
		return fmt.Errorf("store: set app env: %w", err)
	}
	return nil
}

// UnsetAppEnv removes one environment variable (ErrEnvNotFound when the
// key is absent).
func (s *Store) UnsetAppEnv(ctx context.Context, appID, key string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM app_env WHERE app_id = ? AND key = ?`, appID, key)
	if err != nil {
		return fmt.Errorf("store: unset app env: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEnvNotFound
	}
	return nil
}

// ListAppEnv returns every environment variable of an app ordered by
// key. Sealed values are included: the worker decrypts them, and the
// API response layer decides what a user may see.
func (s *Store) ListAppEnv(ctx context.Context, appID string) ([]EnvVar, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, kind, value_plain, value_sealed, updated_at
		FROM app_env WHERE app_id = ? ORDER BY key`, appID)
	if err != nil {
		return nil, fmt.Errorf("store: list app env: %w", err)
	}
	defer rows.Close()
	var out []EnvVar
	for rows.Next() {
		var e EnvVar
		var plain sql.NullString
		var sealed []byte
		var updatedMS int64
		if err := rows.Scan(&e.Key, &e.Kind, &plain, &sealed, &updatedMS); err != nil {
			return nil, err
		}
		e.ValuePlain = plain.String
		e.ValueSealed = sealed
		e.UpdatedAt = time.UnixMilli(updatedMS).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountAppEnv returns how many env keys an app has; the API enforces
// the per-app variable cap with it.
func (s *Store) CountAppEnv(ctx context.Context, appID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_env WHERE app_id = ?`, appID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count app env: %w", err)
	}
	return n, nil
}

// plainValueFor picks the value_plain column: a plain env keeps its
// value even when empty (the schema CHECK requires NOT NULL), a secret
// stores NULL there.
func plainValueFor(kind, valuePlain string) any {
	if kind == EnvSecret {
		return nil
	}
	return valuePlain
}

// sealedValueFor picks the value_sealed column: only secrets carry a
// sealed payload; plain rows keep NULL.
func sealedValueFor(kind string, valueSealed []byte) any {
	if kind != EnvSecret {
		return nil
	}
	return valueSealed
}
