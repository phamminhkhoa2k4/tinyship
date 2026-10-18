package secrypto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	priv, pub, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	secret := []byte("ghcrp_tokensupersecretdo-not-log")
	sealed, err := Seal(pub, secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("sealed blob contains the plaintext")
	}
	got, err := Open(priv, sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("opened %q, want %q", got, secret)
	}
}

func TestSealIsFreshPerCall(t *testing.T) {
	priv, pub, _ := GenerateKeyPair()
	plain := []byte("same input")
	a, err := Seal(pub, plain)
	if err != nil {
		t.Fatalf("seal a: %v", err)
	}
	b, err := Seal(pub, plain)
	if err != nil {
		t.Fatalf("seal b: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two seals of the same plaintext are identical; nonce or ephemeral key is reused")
	}
	for _, blob := range [][]byte{a, b} {
		got, err := Open(priv, blob)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("open = %q err=%v, want %q", got, err, plain)
		}
	}
}

func TestOpenRejectsWrongKeyAndTampering(t *testing.T) {
	priv, pub, _ := GenerateKeyPair()
	otherPriv, _, _ := GenerateKeyPair()
	sealed, err := Seal(pub, []byte("payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := Open(otherPriv, sealed); !errors.Is(err, ErrSealOpen) {
		t.Errorf("wrong key = %v, want ErrSealOpen", err)
	}
	for _, pos := range []int{0, 1, 1 + ecdhX25519Len, len(sealed) - 1} {
		corrupt := append([]byte(nil), sealed...)
		corrupt[pos] ^= 0x01
		if _, err := Open(priv, corrupt); !errors.Is(err, ErrSealOpen) {
			t.Errorf("flip byte %d = %v, want ErrSealOpen", pos, err)
		}
	}
	if _, err := Open(priv, sealed[:len(sealed)-4]); !errors.Is(err, ErrSealOpen) {
		t.Errorf("truncated = %v, want ErrSealOpen", err)
	}
	if _, err := Open(priv, []byte{0x02, 0x00}); !errors.Is(err, ErrSealOpen) {
		t.Errorf("unknown version = %v, want ErrSealOpen", err)
	}
}

func TestSealRejectsBadPublicKey(t *testing.T) {
	if _, err := Seal(make([]byte, 16), []byte("x")); err == nil {
		t.Error("short public key accepted, want error")
	}
}

func TestWriteKeyPairAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	keyPath, pubPath, err := WriteKeyPair(dir)
	if err != nil {
		t.Fatalf("write key pair: %v", err)
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o400 {
		t.Errorf("secrets.key = %04o, want 0400", perm)
	}
	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat pub: %v", err)
	}
	if perm := pubInfo.Mode().Perm(); perm != 0o640 {
		t.Errorf("secrets.pub = %04o, want 0640", perm)
	}

	// Round trip through the actual files: seal with the public key on
	// disk, open with the private key on disk.
	pub, err := LoadPublicKey(pubPath)
	if err != nil {
		t.Fatalf("load pub: %v", err)
	}
	priv, err := LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatalf("load priv: %v", err)
	}
	sealed, err := Seal(pub, []byte("from disk"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := Open(priv, sealed)
	if err != nil || string(got) != "from disk" {
		t.Fatalf("file round trip = %q err=%v", got, err)
	}

	// Refuse to overwrite: stranding stored secrets is worse than failing.
	if _, _, err := WriteKeyPair(dir); err == nil {
		t.Error("second WriteKeyPair overwrote existing keys, want refusal")
	}
}

func TestLoadPrivateKeyRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	keyPath, _, err := WriteKeyPair(dir)
	if err != nil {
		t.Fatalf("write key pair: %v", err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := LoadPrivateKey(keyPath); !errors.Is(err, ErrKeyPermission) {
		t.Errorf("loose key = %v, want ErrKeyPermission", err)
	}
}

func TestLoadKeyFileMalformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "secrets.key")
	if err := os.WriteFile(bad, []byte("not base64!!\n"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadPrivateKey(bad); !errors.Is(err, ErrKeyMalformed) {
		t.Errorf("garbage key = %v, want ErrKeyMalformed", err)
	}
	short := filepath.Join(dir, "short.key")
	if err := os.WriteFile(short, []byte("AAAA\n"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadPrivateKey(short); !errors.Is(err, ErrKeyMalformed) {
		t.Errorf("short key = %v, want ErrKeyMalformed", err)
	}
	if _, err := LoadPublicKey(filepath.Join(dir, "missing.pub")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing pub = %v, want os.ErrNotExist", err)
	}
}
