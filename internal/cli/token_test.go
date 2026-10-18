package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

// tokenEnv points the token commands at a fresh DB and token file for one
// test and returns their paths.
func tokenEnv(t *testing.T) (string, string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "t.db")
	tf := filepath.Join(t.TempDir(), "token")
	t.Setenv("TINYSHIP_DB", db)
	t.Setenv("TINYSHIP_TOKEN_FILE", tf)
	return db, tf
}

func openAt(t *testing.T, db string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// tokenFromOutput pulls the issued token value out of the human-readable
// report (which also carries status text and a hint line).
func tokenFromOutput(s string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "ts_") {
				return f
			}
		}
	}
	return ""
}

func TestTokenCreateWritesFileAndPrintsOnce(t *testing.T) {
	db, tf := tokenEnv(t)

	code, stdout, stderr := run(t, "token", "create")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	raw := tokenFromOutput(stdout)
	if raw == "" {
		t.Fatalf("stdout = %q, want a ts_ token", stdout)
	}

	onDisk, err := os.ReadFile(tf)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != raw {
		t.Errorf("file = %q, want the printed token", onDisk)
	}
	info, err := os.Stat(tf)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o027 != 0 {
		t.Errorf("token file perms = %04o, want no group-write or other access (0640)", perm)
	}

	// The token verifies against the DB.
	s := openAt(t, db)
	if name, err := s.VerifyToken(context.Background(), raw); err != nil || name != "admin" {
		t.Errorf("verify = (%q, %v), want admin", name, err)
	}
}

func TestTokenCreateRefusesExistingFile(t *testing.T) {
	db, tf := tokenEnv(t)
	if err := os.WriteFile(tf, []byte("ts_old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "token", "create")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (file already exists)", code)
	}
	if !strings.Contains(stderr, "token rotate") {
		t.Errorf("stderr = %q, want a rotate hint", stderr)
	}
	// The pre-existing token must be untouched.
	if b, _ := os.ReadFile(tf); strings.TrimSpace(string(b)) != "ts_old" {
		t.Errorf("token file was clobbered: %q", b)
	}
	// The just-inserted token is revoked again so no orphan credential
	// survives the failed write.
	s := openAt(t, db)
	toks, err := s.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 1 || toks[0].RevokedAt == nil {
		t.Errorf("after a failed create the new token must be revoked, got %+v", toks)
	}
}

func TestTokenRotateReplacesFileAndInvalidatesOld(t *testing.T) {
	db, tf := tokenEnv(t)
	if code, _, stderr := run(t, "token", "create"); code != 0 {
		t.Fatalf("create exit = %d stderr = %q", code, stderr)
	}
	oldRaw := strings.TrimSpace(string(mustRead(t, tf)))

	code, stdout, stderr := run(t, "token", "rotate")
	if code != 0 {
		t.Fatalf("rotate exit = %d, stderr = %q", code, stderr)
	}
	newRaw := tokenFromOutput(stdout)
	if newRaw == "" || newRaw == oldRaw {
		t.Fatalf("rotate should return a fresh token, got %q", stdout)
	}
	fileRaw := strings.TrimSpace(string(mustRead(t, tf)))
	if fileRaw != newRaw {
		t.Errorf("file = %q, want the rotated token", fileRaw)
	}

	s := openAt(t, db)
	if name, err := s.VerifyToken(context.Background(), newRaw); err != nil || name != "admin" {
		t.Errorf("rotated token verify = (%q, %v), want admin", name, err)
	}
	if _, err := s.VerifyToken(context.Background(), oldRaw); !errors.Is(err, store.ErrTokenInvalid) {
		t.Errorf("old token still valid: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestTokenListShowsNamesNeverTokens(t *testing.T) {
	db, _ := tokenEnv(t)
	s := openAt(t, db)
	raw, _ := s.CreateToken(context.Background(), "admin")

	code, stdout, stderr := run(t, "token", "list")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "admin") {
		t.Errorf("list = %q, want admin", stdout)
	}
	if strings.Contains(stdout, raw) {
		t.Errorf("list leaked the token: %q", stdout)
	}

	code, stdout, _ = run(t, "token", "list", "--json")
	if code != 0 {
		t.Fatalf("json exit = %d", code)
	}
	if !strings.Contains(stdout, `"tokens"`) || strings.Contains(stdout, raw) {
		t.Errorf("json = %q, want tokens key without the raw token", stdout)
	}
}

func TestTokenRevokeInvalidates(t *testing.T) {
	db, _ := tokenEnv(t)
	s := openAt(t, db)
	raw, _ := s.CreateToken(context.Background(), "admin")

	code, stdout, stderr := run(t, "token", "revoke", "admin")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "revoked") {
		t.Errorf("stdout = %q", stdout)
	}
	if _, err := s.VerifyToken(context.Background(), raw); !errors.Is(err, store.ErrTokenRevoked) {
		t.Errorf("revoked token verify = %v, want ErrTokenRevoked", err)
	}
}

func TestTokenMissingDBFailsClearly(t *testing.T) {
	tokenEnv(t)
	t.Setenv("TINYSHIP_DB", filepath.Join(t.TempDir(), "does", "not", "exist", "x.db"))
	// The parent dir is absent, so the store cannot open; the command
	// must say so without panicking.
	code, _, stderr := run(t, "token", "list")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "open database") {
		t.Errorf("stderr = %q, want an open-database error", stderr)
	}
}
