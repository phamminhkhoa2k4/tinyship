package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/secrypto"
)

func TestKeygenWritesKeyPair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	code, stdout, stderr := run(t, "keygen", "--keys-dir", dir)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	keyPath := filepath.Join(dir, "secrets.key")
	pubPath := filepath.Join(dir, "secrets.pub")
	for _, p := range []string{keyPath, pubPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("secrets.key perm = %v err=%v, want 0400", info.Mode().Perm(), err)
	}
	if !strings.Contains(stdout, keyPath) || !strings.Contains(stdout, pubPath) {
		t.Errorf("stdout does not name both files:\n%s", stdout)
	}

	// The pair must actually seal and open.
	pub, err := secrypto.LoadPublicKey(pubPath)
	if err != nil {
		t.Fatalf("load pub: %v", err)
	}
	priv, err := secrypto.LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("load priv: %v", err)
	}
	sealed, err := secrypto.Seal(pub, []byte("round-trip"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := secrypto.Open(priv, sealed)
	if err != nil || string(got) != "round-trip" {
		t.Fatalf("round trip = %q err=%v", got, err)
	}
}

func TestKeygenRefusesOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if code, _, _ := run(t, "keygen", "--keys-dir", dir); code != 0 {
		t.Fatalf("first keygen exit = %d", code)
	}
	code, _, stderr := run(t, "keygen", "--keys-dir", dir)
	if code == 0 {
		t.Fatal("second keygen overwrote existing keys, want refusal")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", stderr)
	}
}

func TestKeygenRejectsPositional(t *testing.T) {
	code, _, _ := run(t, "keygen", "somewhere")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}
