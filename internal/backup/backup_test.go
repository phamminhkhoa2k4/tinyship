package backup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// testKDF keeps passphrase tests fast (same seam as the token hasher).
var testKDF = &KDFParams{MemoryKiB: 8, Iterations: 1, Parallelism: 1}

func sampleOptions(passphrase string) Options {
	return Options{
		Files: []File{
			{Name: "db/tinyship.db", Data: []byte("sqlite snapshot bytes")},
			{Name: "etc/tinyship/config.yaml", Data: []byte("host: {}\n")},
			{Name: "etc/tinyship/keys/secrets.key", Data: []byte("private\n")},
		},
		Passphrase:      passphrase,
		TinyshipVersion: "0.1.0-dev",
		SchemaVersion:   2,
		Hostname:        "vps-test",
		CreatedAt:       time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		KDF:             testKDF,
	}
}

func TestRoundTripPlain(t *testing.T) {
	var buf bytes.Buffer
	want, err := Write(&buf, sampleOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	if want.Encrypted {
		t.Error("plain archive must not be marked encrypted")
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte(Magic)) {
		t.Fatalf("missing magic prefix: %q", buf.Bytes()[:8])
	}

	got, err := Read(&buf, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest.SchemaVersion != 2 || got.Manifest.Hostname != "vps-test" {
		t.Errorf("manifest metadata wrong: %+v", got.Manifest)
	}
	if len(got.Files) != 3 {
		t.Errorf("files = %d, want 3", len(got.Files))
	}
	if string(got.Files["etc/tinyship/config.yaml"]) != "host: {}\n" {
		t.Errorf("config content wrong: %q", got.Files["etc/tinyship/config.yaml"])
	}
	// The manifest itself is not a restorable file.
	if _, ok := got.Files["manifest.json"]; ok {
		t.Error("manifest.json must not be exposed as a restorable member")
	}
}

func TestRoundTripEncrypted(t *testing.T) {
	var buf bytes.Buffer
	want, err := Write(&buf, sampleOptions("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if !want.Encrypted {
		t.Fatal("encrypted archive not marked encrypted")
	}
	plain := bytes.Clone(buf.Bytes())

	// The member bytes must not appear in the ciphertext.
	if bytes.Contains(plain, []byte("sqlite snapshot bytes")) {
		t.Error("plaintext member leaked into the encrypted archive")
	}

	if _, err := Read(bytes.NewReader(plain), "correct horse"); err != nil {
		t.Fatalf("decrypt with right passphrase: %v", err)
	}
	if _, err := Read(bytes.NewReader(plain), ""); !errors.Is(err, ErrPassphraseRequired) {
		t.Errorf("no passphrase: err = %v, want ErrPassphraseRequired", err)
	}
	if _, err := Read(bytes.NewReader(plain), "wrong"); !errors.Is(err, ErrBadPassphrase) {
		t.Errorf("wrong passphrase: err = %v, want ErrBadPassphrase", err)
	}
}

func TestPassphraseOnPlainArchiveRefused(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Write(&buf, sampleOptions("")); err != nil {
		t.Fatal(err)
	}
	_, err := Read(&buf, "guess")
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("err = %v, want 'not encrypted' complaint", err)
	}
}

func TestTamperedHeaderBreaksDecryption(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Write(&buf, sampleOptions("pass me")); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	// Flip the encrypted flag: the header is AEAD associated data, so
	// this must fail loudly rather than downgrade to plain parsing.
	tampered := bytes.Clone(raw)
	tampered[len(Magic)+1] = 0
	if _, err := Read(bytes.NewReader(tampered), "pass me"); err == nil {
		t.Error("clearing the encrypted flag must not be accepted")
	}

	// Corrupt a ciphertext byte: same refusal, no partial content.
	tampered = bytes.Clone(raw)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := Read(bytes.NewReader(tampered), "pass me"); !errors.Is(err, ErrBadPassphrase) {
		t.Errorf("corrupted ciphertext: err = %v, want ErrBadPassphrase", err)
	}
}

func TestTamperedPlainArchiveFailsChecksum(t *testing.T) {
	// Tampering with gzip bytes surfaces as a parse failure; the
	// content-level guarantee is that a manifest whose checksum
	// disagrees with the stored member is refused by verify().
	m := &Manifest{FormatVersion: FormatVersion, Members: []Member{
		{Name: "x", SHA256: "0000000000000000000000000000000000000000000000000000000000000000", Size: 3},
	}}
	err := verify(m, map[string][]byte{"x": []byte("abc")})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("verify with wrong checksum: err = %v, want checksum failure", err)
	}

	// And a flipped byte anywhere in a real plain archive must not be
	// silently accepted.
	var buf bytes.Buffer
	if _, err := Write(&buf, sampleOptions("")); err != nil {
		t.Fatal(err)
	}
	raw := bytes.Clone(buf.Bytes())
	raw[len(raw)-3] ^= 0xff
	if _, err := Read(bytes.NewReader(raw), ""); err == nil {
		t.Error("a flipped byte in a plain archive must fail the read")
	}
}

func TestVerifyRefusesMissingAndExtraMembers(t *testing.T) {
	m := &Manifest{FormatVersion: FormatVersion, Members: []Member{
		{Name: "a", SHA256: "aa", Size: 1},
	}}
	if err := verify(m, nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing member: err = %v, want 'missing'", err)
	}
	m2 := &Manifest{FormatVersion: FormatVersion}
	if err := verify(m2, map[string][]byte{"sneaky": []byte("x")}); err == nil ||
		!strings.Contains(err.Error(), "not listed") {
		t.Errorf("extra member: err = %v, want 'not listed'", err)
	}
}

func TestWriteRejectsUnsafeMemberNames(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "/abs/path", "a/../..", "", `win\path`} {
		_, err := Write(&bytes.Buffer{}, Options{Files: []File{{Name: name, Data: []byte("x")}}})
		if err == nil {
			t.Errorf("member name %q must be refused", name)
		}
	}
}

func TestWriteRejectsDuplicatesAndReservedName(t *testing.T) {
	_, err := Write(&bytes.Buffer{}, Options{Files: []File{
		{Name: "a", Data: []byte("1")}, {Name: "a", Data: []byte("2")},
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate member: err = %v", err)
	}
	_, err = Write(&bytes.Buffer{}, Options{Files: []File{
		{Name: "manifest.json", Data: []byte("{}")},
	}})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("reserved name: err = %v", err)
	}
}

func TestReadRefusesForeignFiles(t *testing.T) {
	_, err := Read(strings.NewReader("PK\x03\x04 not a backup"), "")
	if !errors.Is(err, ErrWrongMagic) {
		t.Errorf("err = %v, want ErrWrongMagic", err)
	}
}

func TestReadRefusesUnsupportedFormatVersion(t *testing.T) {
	raw := append([]byte(Magic), 99, 0)
	raw = append(raw, []byte("junk")...)
	_, err := Read(bytes.NewReader(raw), "")
	if err == nil || !strings.Contains(err.Error(), "format version") {
		t.Errorf("err = %v, want format version complaint", err)
	}
}

func TestDeterministicOutputForIdenticalInput(t *testing.T) {
	// Two plain archives of the same content must be byte-identical:
	// manifests carry checksums, tar headers are fixed, gzip gets no
	// timestamp from us. Encrypted archives differ (random salt), which
	// the salt test covers separately.
	var a, b bytes.Buffer
	if _, err := Write(&a, sampleOptions("")); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(&b, sampleOptions("")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("identical input must produce identical plain archives")
	}
}

func TestEncryptedArchivesUseFreshSalt(t *testing.T) {
	var a, b bytes.Buffer
	if _, err := Write(&a, sampleOptions("same pass")); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(&b, sampleOptions("same pass")); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two encrypted archives must not be byte-identical (salt reuse would be a flaw)")
	}
}
