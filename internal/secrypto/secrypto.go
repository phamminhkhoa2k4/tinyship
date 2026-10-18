// Package secrypto seals and opens secrets with an X25519 sealed box
// sealed with XChaCha20-Poly1305. The API side only ever holds the
// public key (it seals); the worker holds the private key (it opens).
// A compromised API therefore cannot read secrets already stored.
package secrypto

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

// Sealed format: version(1) || ephemeral public key(32) ||
// XChaCha20 nonce(24) || ciphertext+tag.
const (
	versionByte   = 0x01
	headerLen     = 1 + ecdhX25519Len + chacha20poly1305.NonceSizeX
	ecdhX25519Len = 32
)

// kdfInfo binds derived keys to this construction so a blob sealed for
// one purpose can never open under another scheme.
const kdfInfo = "tinyship sealed secrets v1"

// ErrSealOpen is returned when a sealed blob fails to open: wrong
// private key, truncated data, or tampering.
var ErrSealOpen = errors.New("E_SECRETS_KEY: sealed data cannot be opened with this key")

// ErrKeyPermission is returned when a private key file is readable by
// more than its owner.
var ErrKeyPermission = errors.New("E_SECRETS_KEY: private key file allows group or other access; run chmod 400")

// ErrKeyMalformed is returned when a key file does not contain a
// well-formed 32-byte X25519 key in base64.
var ErrKeyMalformed = errors.New("E_SECRETS_KEY: key file does not contain a valid base64 X25519 key")

// GenerateKeyPair returns a fresh X25519 key pair as 32-byte values.
func GenerateKeyPair() (priv, pub []byte, err error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("secrypto: generate key: %w", err)
	}
	return key.Bytes(), key.PublicKey().Bytes(), nil
}

// Seal encrypts plaintext to pub. Each call uses a fresh ephemeral
// key pair, so blobs are unlinkable to each other and safe to store
// side by side.
func Seal(pub, plaintext []byte) ([]byte, error) {
	if len(pub) != ecdhX25519Len {
		return nil, fmt.Errorf("secrypto: public key is %d bytes, want %d", len(pub), ecdhX25519Len)
	}
	recipient, err := ecdh.X25519().NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("secrypto: public key: %w", err)
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("secrypto: ephemeral key: %w", err)
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, fmt.Errorf("secrypto: ecdh: %w", err)
	}
	key, err := sealKey(shared)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("secrypto: aead: %w", err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrypto: nonce: %w", err)
	}
	out := make([]byte, 0, headerLen+len(plaintext)+aead.Overhead())
	out = append(out, versionByte)
	out = append(out, ephemeral.PublicKey().Bytes()...)
	out = append(out, nonce...)
	// The ephemeral public key seals the header: swapping it in makes
	// the tag check fail instead of silently mixing key material.
	return aead.Seal(out, nonce, plaintext, out[:1+ecdhX25519Len]), nil
}

// Open decrypts a blob produced by Seal with the recipient's private
// key. Any failure (wrong key, tamper, truncation) reports ErrSealOpen.
func Open(priv, sealed []byte) ([]byte, error) {
	if len(priv) != ecdhX25519Len {
		return nil, fmt.Errorf("%w: private key is %d bytes, want %d", ErrSealOpen, len(priv), ecdhX25519Len)
	}
	if len(sealed) < headerLen {
		return nil, ErrSealOpen
	}
	if sealed[0] != versionByte {
		return nil, fmt.Errorf("%w: unknown version %d", ErrSealOpen, sealed[0])
	}
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("%w: private key: %v", ErrSealOpen, err)
	}
	ephPub, err := ecdh.X25519().NewPublicKey(sealed[1 : 1+ecdhX25519Len])
	if err != nil {
		return nil, ErrSealOpen
	}
	shared, err := key.ECDH(ephPub)
	if err != nil {
		return nil, ErrSealOpen
	}
	derived, err := sealKey(shared)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(derived)
	if err != nil {
		return nil, fmt.Errorf("secrypto: aead: %w", err)
	}
	nonce := sealed[1+ecdhX25519Len : headerLen]
	plaintext, err := aead.Open(nil, nonce, sealed[headerLen:], sealed[:1+ecdhX25519Len])
	if err != nil {
		return nil, ErrSealOpen
	}
	return plaintext, nil
}

func sealKey(shared []byte) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, shared, nil, kdfInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("secrypto: derive key: %w", err)
	}
	return key, nil
}

// LoadPrivateKey reads a base64 X25519 private key and refuses files
// any other account can read: the worker key must stay worker-only.
func LoadPrivateKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %04o", ErrKeyPermission, filepath.Base(path), info.Mode().Perm())
	}
	return readKeyFile(path)
}

// LoadPublicKey reads a base64 X25519 public key. No permission
// check: the public key only ever enables encryption.
func LoadPublicKey(path string) ([]byte, error) {
	return readKeyFile(path)
}

func readKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(trimSpace(string(raw)))
	if err != nil || len(key) != ecdhX25519Len {
		return nil, fmt.Errorf("%w: %s", ErrKeyMalformed, filepath.Base(path))
	}
	return key, nil
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// WriteKeyPair generates and stores a key pair in dir: secrets.key
// (0400, the worker's) and secrets.pub (0640, the API's). Existing
// files are never overwritten: rotating keys on top of stored secrets
// would strand them.
func WriteKeyPair(dir string) (keyPath, pubPath string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", fmt.Errorf("secrypto: create key dir: %w", err)
	}
	keyPath = filepath.Join(dir, "secrets.key")
	pubPath = filepath.Join(dir, "secrets.pub")
	for _, p := range []string{keyPath, pubPath} {
		if _, err := os.Lstat(p); err == nil {
			return "", "", fmt.Errorf("secrypto: %s already exists; refusing to overwrite", filepath.Base(p))
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("secrypto: stat %s: %w", filepath.Base(p), err)
		}
	}
	priv, pub, err := GenerateKeyPair()
	if err != nil {
		return "", "", err
	}
	// The private key is written 0400 from the start: no window where
	// it is group- or world-readable.
	if err := writeExclusive(keyPath, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o400); err != nil {
		return "", "", err
	}
	if err := writeExclusive(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o640); err != nil {
		return "", "", err
	}
	return keyPath, pubPath, nil
}

func writeExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("secrypto: create %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("secrypto: write %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrypto: close %s: %w", filepath.Base(path), err)
	}
	// umask can loosen the create mode; re-assert it explicitly.
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("secrypto: chmod %s: %w", filepath.Base(path), err)
	}
	return nil
}
