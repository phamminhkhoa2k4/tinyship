package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fastParams makes token hashing cheap so the suite stays quick; the
// hashing still runs through the same code path as production.
var fastParams = ArgonParams{
	MemoryKiB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8,
}

func tokenStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.SetArgon2Params(fastParams)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateTokenReturnsPlaintextOnce(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	raw, err := s.CreateToken(ctx, "admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(raw, "ts_") || len(raw) < 10 {
		t.Errorf("token = %q, want ts_ prefix and reasonable length", raw)
	}

	// The hash is stored, not the plaintext: list must not leak it.
	toks, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("tokens = %d, want 1", len(toks))
	}
	if toks[0].Name != "admin" || toks[0].Scope != "admin" {
		t.Errorf("token info = %+v", toks[0])
	}
	all, err := s.DB().QueryContext(ctx, `SELECT token_hash FROM api_tokens`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer all.Close()
	if all.Next() {
		var h string
		_ = all.Scan(&h)
		if strings.Contains(h, raw) {
			t.Error("stored hash leaked the plaintext token")
		}
	}
}

func TestVerifyTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	raw, _ := s.CreateToken(ctx, "admin")

	name, err := s.VerifyToken(ctx, raw)
	if err != nil || name != "admin" {
		t.Fatalf("verify = (%q, %v), want admin", name, err)
	}

	// Wrong or missing token fails.
	if _, err := s.VerifyToken(ctx, "ts_wrongtoken"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("wrong token err = %v, want ErrTokenInvalid", err)
	}
	if _, err := s.VerifyToken(ctx, ""); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("empty token err = %v, want ErrTokenInvalid", err)
	}
}

func TestVerifyTokenRejectsRevoked(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	raw, _ := s.CreateToken(ctx, "admin")

	if err := s.RevokeToken(ctx, "admin"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.VerifyToken(ctx, raw); !errors.Is(err, ErrTokenRevoked) {
		t.Errorf("verify revoked err = %v, want ErrTokenRevoked", err)
	}
}

func TestCreateTokenUniqueActiveName(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	if _, err := s.CreateToken(ctx, "admin"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateToken(ctx, "admin"); !errors.Is(err, ErrTokenExists) {
		t.Errorf("duplicate name err = %v, want ErrTokenExists", err)
	}
}

func TestRevokeTokenNotFoundIsIdempotentSafe(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	if err := s.RevokeToken(ctx, "ghost"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("revoke missing err = %v, want ErrTokenNotFound", err)
	}
}

func TestRotateTokenReplacesCredential(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	first, _ := s.CreateToken(ctx, "admin")

	rotated, err := s.RotateToken(ctx, "admin")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated == first {
		t.Error("rotate returned the same token value")
	}
	// Old value no longer works; the new value does.
	if _, err := s.VerifyToken(ctx, first); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("old token after rotate err = %v, want invalid", err)
	}
	if name, err := s.VerifyToken(ctx, rotated); err != nil || name != "admin" {
		t.Errorf("new token after rotate = (%q, %v), want admin", name, err)
	}
	// Rotate of an absent name creates it.
	if _, err := s.RotateToken(ctx, "ci"); err != nil {
		t.Errorf("rotate new name: %v", err)
	}
}

func TestRotateTokenClearsRevocation(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	if _, err := s.CreateToken(ctx, "admin"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.RevokeToken(ctx, "admin"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rot, err := s.RotateToken(ctx, "admin")
	if err != nil {
		t.Fatalf("rotate after revoke: %v", err)
	}
	if name, err := s.VerifyToken(ctx, rot); err != nil || name != "admin" {
		t.Errorf("token after revoke+rotate = (%q, %v), want valid", name, err)
	}
}

func TestListTokensOrderedNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := tokenStore(t)
	if _, err := s.CreateToken(ctx, "old"); err != nil {
		t.Fatalf("create old: %v", err)
	}
	// Slight wait so created_at ordering is deterministic.
	time.Sleep(2 * time.Millisecond)
	if _, err := s.CreateToken(ctx, "new"); err != nil {
		t.Fatalf("create new: %v", err)
	}
	toks, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 2 || toks[0].Name != "new" || toks[1].Name != "old" {
		t.Errorf("list order = %+v, want new then old", toks)
	}
}

func TestTokenHashMatchesMalformedIsError(t *testing.T) {
	for _, bad := range []string{"", "garbage", "$argon2id$v=19$m=8,t=1,p=1$abc"} {
		if _, err := tokenHashMatches("ts_x", bad); err == nil {
			t.Errorf("tokenHashMatches(%q) = nil error, want an error", bad)
		}
	}
}
