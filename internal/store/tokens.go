package store

// Admin token primitives: issue, verify, list, and revoke the tokens
// that protect the local control API. Tokens are stored as an argon2id
// hash; the plaintext value exists only at issue time (and, separately,
// in the CLI's token file). Hash parameters live on the Store so tests
// can lower the cost while production keeps a strong interactive cost.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// ArgonParams sets the argon2id cost for token hashing.
type ArgonParams struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLen     int
}

// DefaultArgonParams is the production cost: interactive-login strength,
// cheap on a small VPS yet slow enough to blunt brute force. Constants
// marked provisional until the benchmark tunes them.
var DefaultArgonParams = ArgonParams{
	MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2, SaltLen: 16,
}

// ErrTokenInvalid is returned when a presented token matches no active
// token in the store.
var ErrTokenInvalid = errors.New("E_UNAUTHORIZED: token is not valid")

// ErrTokenRevoked is returned when a presented token matches a row that
// has been revoked.
var ErrTokenRevoked = errors.New("E_UNAUTHORIZED: token has been revoked")

// ErrTokenNotFound is returned when listing-free operations (revoke,
// rotate) are asked about a name that has no row.
var ErrTokenNotFound = errors.New("E_VALIDATION: token not found")

// ErrTokenExists is returned when creating a token whose name is taken.
var ErrTokenExists = errors.New("E_VALIDATION: a token with that name already exists")

// TokenInfo is the user-visible shape of a token: never the hash.
type TokenInfo struct {
	ID         string
	Name       string
	Scope      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// GenerateToken returns a new random admin token value with a short
// prefix so an accidental print is distinguishable from other text.
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate token: %w", err)
	}
	return "ts_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken derives an argon2id PHC string for raw using the given cost.
func hashToken(raw string, p ArgonParams) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("store: token salt: %w", err)
	}
	out := argon2.IDKey([]byte(raw), salt, p.Iterations, p.MemoryKiB,
		p.Parallelism, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(out)), nil
}

// tokenHashMatches re-derives the hash of raw from the stored PHC string
// and compares in constant time. A malformed stored hash is reported as
// an error so a store defect surfaces instead of silently failing closed.
func tokenHashMatches(raw, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("store: token hash has an unknown format")
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", new(int)); err != nil {
		return false, fmt.Errorf("store: token hash version: %w", err)
	}
	var mem, it, par int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &it, &par); err != nil {
		return false, fmt.Errorf("store: token hash params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("store: token hash salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("store: token hash digest: %w", err)
	}
	if mem == 0 || it == 0 || par < 1 {
		return false, errors.New("store: token hash has invalid params")
	}
	got := argon2.IDKey([]byte(raw), salt, uint32(it), uint32(mem), uint8(par),
		uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// CreateToken issues a new admin token named name, stores only its
// argon2id hash, and returns the plaintext exactly once. The plaintext
// cannot be recovered later, so this method is the only place it exists.
func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("store: token name is required")
	}
	existingActive, err := s.activeTokenExists(ctx, name)
	if err != nil {
		return "", err
	}
	if existingActive {
		return "", ErrTokenExists
	}
	raw, err := GenerateToken()
	if err != nil {
		return "", err
	}
	enc, err := hashToken(raw, s.argonParams())
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, token_hash, scope, created_at)
		VALUES (?, ?, ?, 'admin', ?)`,
		NewID(), name, enc, time.Now().UTC().UnixMilli())
	if err != nil {
		return "", fmt.Errorf("store: create token: %w", err)
	}
	return raw, nil
}

// RotateToken replaces the hash of the named token (creating it if the
// name has no row) and clears any previous revocation, so one name
// always maps to exactly one live credential. It returns the new
// plaintext once.
func (s *Store) RotateToken(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("store: token name is required")
	}
	raw, err := GenerateToken()
	if err != nil {
		return "", err
	}
	enc, err := hashToken(raw, s.argonParams())
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().UnixMilli()
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET token_hash = ?, revoked_at = NULL, created_at = ?
		WHERE name = ?`, enc, now, name)
	if err != nil {
		return "", fmt.Errorf("store: rotate token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO api_tokens (id, name, token_hash, scope, created_at)
			VALUES (?, ?, ?, 'admin', ?)`,
			NewID(), name, enc, now)
		if err != nil {
			return "", fmt.Errorf("store: rotate token (create): %w", err)
		}
	}
	return raw, nil
}

// activeTokenExists reports whether a non-revoked token already uses the
// name, guarding CreateToken against a second active admin.
func (s *Store) activeTokenExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_tokens
		WHERE name = ? AND revoked_at IS NULL`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: token exists check: %w", err)
	}
	return n > 0, nil
}

// VerifyToken checks raw against every active token and returns the
// owning name on success. It rejects revoked tokens with a typed error
// and updates last_used_at on success. Token counts are small, so an
// O(n) verify is deliberate and simple; a matching row short-circuits.
func (s *Store) VerifyToken(ctx context.Context, raw string) (string, error) {
	type rec struct {
		name    string
		hash    string
		revoked *int64
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, token_hash, revoked_at FROM api_tokens`)
	if err != nil {
		return "", fmt.Errorf("store: list tokens for verify: %w", err)
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.name, &r.hash, &r.revoked); err != nil {
			rows.Close()
			return "", err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, r := range recs {
		ok, err := tokenHashMatches(raw, r.hash)
		if err != nil {
			continue // a malformed stored row is not a valid credential
		}
		if !ok {
			continue
		}
		if r.revoked != nil {
			return "", ErrTokenRevoked
		}
		_, _ = s.db.ExecContext(ctx,
			`UPDATE api_tokens SET last_used_at = ? WHERE name = ?`,
			time.Now().UTC().UnixMilli(), r.name)
		return r.name, nil
	}
	return "", ErrTokenInvalid
}

// ListTokens returns every token without the hash, newest first.
func (s *Store) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, scope, created_at, last_used_at, revoked_at
		FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tokens: %w", err)
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		var createdMS int64
		var lastUsedMS, revokedMS *int64
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &createdMS,
			&lastUsedMS, &revokedMS); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(createdMS).UTC()
		if lastUsedMS != nil {
			v := time.UnixMilli(*lastUsedMS).UTC()
			t.LastUsedAt = &v
		}
		if revokedMS != nil {
			v := time.UnixMilli(*revokedMS).UTC()
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken marks the named token revoked (idempotent).
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET revoked_at = ?
		WHERE name = ? AND revoked_at IS NULL`,
		time.Now().UTC().UnixMilli(), name)
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// SetArgon2Params lowers or raises the hash cost. It exists for tests
// and local tuning; production keeps DefaultArgonParams.
func (s *Store) SetArgon2Params(p ArgonParams) { s.argon = p }

// argonParams returns the configured cost, substituting the default when
// none was set (so a zero-value Store still uses a safe cost).
func (s *Store) argonParams() ArgonParams {
	if s.argon.MemoryKiB == 0 {
		return DefaultArgonParams
	}
	return s.argon
}
