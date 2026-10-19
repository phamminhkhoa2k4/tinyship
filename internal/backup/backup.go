// Package backup writes and reads TinyShip configuration backups: one
// self-describing file holding a consistent database snapshot, the host
// configuration, the sealed-box key pair, and the admin token — the
// pieces a clean host needs to become this host again. Application
// volume data is deliberately absent (it belongs to the apps) and the
// backup output says so.
//
// Format v1: magic "TINYBK01", version byte, flags byte (bit0 =
// encrypted), then for encrypted archives argon2id cost params
// (m:4, t:4, p:1) + salt(16) + nonce(24) + XChaCha20-Poly1305 ciphertext,
// for plain archives the tar.gz stream. The whole header is AEAD
// associated data, so tampering with the flags or the KDF parameters
// breaks decryption instead of silently downgrading it. Inside the tar,
// manifest.json comes first and carries a SHA-256 for every member;
// restore refuses on any mismatch.
package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// Magic identifies a TinyShip backup; anything else is refused
	// before any parsing happens.
	Magic = "TINYBK01"
	// FormatVersion is the on-disk layout version this build writes.
	FormatVersion = 1
	// MaxArchiveSize bounds how much a reader is willing to buffer;
	// a configuration backup is small by construction, so a huge file
	// is a mistake or an attack, not a real archive.
	MaxArchiveSize = 1 << 30 // 1 GiB

	flagEncrypted = 1 << 0
	headerLen     = len(Magic) + 1 + 1
	saltLen       = 16
	kdfParamsLen  = 4 + 4 + 1
	nonceLen      = chacha20poly1305.NonceSizeX
)

// ErrWrongMagic means the file is not a TinyShip backup.
var ErrWrongMagic = errors.New("backup: not a TinyShip backup file")

// ErrPassphraseRequired means the archive is encrypted but no passphrase
// was supplied.
var ErrPassphraseRequired = errors.New("backup: this archive is encrypted; a passphrase is required (type it on stdin)")

// ErrBadPassphrase means decryption failed: the passphrase is wrong or
// the archive was modified. The AEAD cannot tell the two apart, and
// saying which would be an oracle.
var ErrBadPassphrase = errors.New("backup: wrong passphrase or corrupted archive")

// KDFParams is the argon2id cost used to stretch a backup passphrase.
// The zero value selects the production default; tests pass a cheap
// override (the same seam the token hasher uses).
type KDFParams struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// DefaultKDF mirrors the production token-hashing cost: slow enough to
// make passphrase guessing expensive, cheap enough for a small VPS.
// Provisional until the benchmark tunes it.
var DefaultKDF = KDFParams{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 2}

// File is one member of the archive: its path inside the archive and
// its full content. Callers buffer members (a configuration backup is
// small) so the manifest can carry checksums before the tar is written.
type File struct {
	Name string // slash path inside the archive, e.g. "etc/tinyship/config.yaml"
	Data []byte
}

// Options configures Write.
type Options struct {
	Files           []File
	Passphrase      string // empty writes a plain archive
	TinyshipVersion string // recorded for humans and upgrade checks
	SchemaVersion   int    // database schema version at snapshot time
	Hostname        string
	CreatedAt       time.Time // zero means time.Now()
	KDF             *KDFParams
}

// Member is one archived file with its checksum.
type Member struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the archive's self-description, stored as the first tar
// entry.
type Manifest struct {
	FormatVersion   int       `json:"format_version"`
	TinyshipVersion string    `json:"tinyship_version"`
	SchemaVersion   int       `json:"schema_version"`
	CreatedAt       time.Time `json:"created_at"`
	Hostname        string    `json:"hostname"`
	Encrypted       bool      `json:"encrypted"`
	Members         []Member  `json:"members"`
}

// Archive is a fully verified backup read from disk.
type Archive struct {
	Manifest *Manifest
	Files    map[string][]byte
}

// checkMemberName rejects names that could escape the restore
// destination: absolute paths, parent traversal, empty names, and
// backslashes (archives are slash-delimited everywhere).
func checkMemberName(name string) error {
	if name == "" {
		return fmt.Errorf("backup: empty member name")
	}
	if strings.Contains(name, `\`) {
		return fmt.Errorf("backup: member name %q must use forward slashes", name)
	}
	if path.IsAbs(name) || name[0] == '/' {
		return fmt.Errorf("backup: member name %q must be relative", name)
	}
	if path.Clean(name) != name || strings.HasPrefix(name, "..") {
		return fmt.Errorf("backup: member name %q escapes the archive", name)
	}
	return nil
}

// Write serializes opts to w and returns the manifest it embedded.
func Write(w io.Writer, opts Options) (*Manifest, error) {
	if len(opts.Files) == 0 {
		return nil, errors.New("backup: nothing to back up (no members)")
	}
	createdAt := opts.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	encrypted := opts.Passphrase != ""

	manifest := &Manifest{
		FormatVersion:   FormatVersion,
		TinyshipVersion: opts.TinyshipVersion,
		SchemaVersion:   opts.SchemaVersion,
		CreatedAt:       createdAt,
		Hostname:        opts.Hostname,
		Encrypted:       encrypted,
	}
	seen := make(map[string]bool, len(opts.Files))
	for _, f := range opts.Files {
		if err := checkMemberName(f.Name); err != nil {
			return nil, err
		}
		if f.Name == "manifest.json" {
			return nil, errors.New("backup: the member name manifest.json is reserved")
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("backup: duplicate member %q", f.Name)
		}
		seen[f.Name] = true
		sum := sha256.Sum256(f.Data)
		manifest.Members = append(manifest.Members, Member{
			Name:   f.Name,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(f.Data)),
		})
	}
	sort.Slice(manifest.Members, func(i, j int) bool {
		return manifest.Members[i].Name < manifest.Members[j].Name
	})

	// Manifest first so a reader can verify members as they arrive.
	mj, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("backup: marshal manifest: %w", err)
	}
	var tarGz bytes.Buffer
	if err := writeTarGz(&tarGz, manifest, mj, opts.Files); err != nil {
		return nil, err
	}

	header := make([]byte, 0, headerLen+saltLen+kdfParamsLen+nonceLen)
	header = append(header, Magic...)
	header = append(header, FormatVersion)
	if encrypted {
		header = append(header, flagEncrypted)
	} else {
		header = append(header, 0)
	}

	if !encrypted {
		if _, err := w.Write(header); err != nil {
			return nil, fmt.Errorf("backup: write: %w", err)
		}
		if _, err := w.Write(tarGz.Bytes()); err != nil {
			return nil, fmt.Errorf("backup: write: %w", err)
		}
		return manifest, nil
	}

	p := DefaultKDF
	if opts.KDF != nil {
		p = *opts.KDF
	}
	if p.MemoryKiB == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return nil, errors.New("backup: invalid KDF parameters")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("backup: salt: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("backup: nonce: %w", err)
	}
	header = appendU32(header, p.MemoryKiB)
	header = appendU32(header, p.Iterations)
	header = append(header, p.Parallelism)
	header = append(header, salt...)
	header = append(header, nonce...)

	key := argon2.IDKey([]byte(opts.Passphrase), salt, p.Iterations,
		p.MemoryKiB, p.Parallelism, chacha20poly1305.KeySize)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("backup: cipher: %w", err)
	}
	// The header travels in the clear and is bound as associated data:
	// rewriting flags or KDF params makes the ciphertext unreadable.
	ciphertext := aead.Seal(nil, nonce, tarGz.Bytes(), header)

	out := make([]byte, 0, len(header)+len(ciphertext))
	out = append(out, header...)
	out = append(out, ciphertext...)
	if _, err := w.Write(out); err != nil {
		return nil, fmt.Errorf("backup: write: %w", err)
	}
	return manifest, nil
}

// Read parses and fully verifies a backup. Every member's checksum is
// checked against the manifest and every manifest member must be
// present; nothing is returned to the caller before both hold.
func Read(r io.Reader, passphrase string) (*Archive, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(MaxArchiveSize+headerLen+saltLen+kdfParamsLen+nonceLen+1)))
	if err != nil {
		return nil, fmt.Errorf("backup: read: %w", err)
	}
	if len(raw) > MaxArchiveSize {
		return nil, fmt.Errorf("backup: archive exceeds the %d byte limit", MaxArchiveSize)
	}
	if len(raw) < headerLen || string(raw[:len(Magic)]) != Magic {
		return nil, ErrWrongMagic
	}
	if raw[len(Magic)] != FormatVersion {
		return nil, fmt.Errorf("backup: format version %d is not supported by this binary (expects %d)",
			raw[len(Magic)], FormatVersion)
	}
	flags := raw[len(Magic)+1]
	payload := raw[headerLen:]
	encrypted := flags&flagEncrypted != 0

	if encrypted {
		if passphrase == "" {
			return nil, ErrPassphraseRequired
		}
		if len(payload) < saltLen+kdfParamsLen+nonceLen+chacha20poly1305.Overhead {
			return nil, fmt.Errorf("backup: truncated encrypted archive")
		}
		// The AAD is everything before the ciphertext, exactly as Write
		// sealed it: magic, version, flags, KDF params, salt, nonce.
		hdr := raw[:headerLen+kdfParamsLen+saltLen+nonceLen]
		off := 0
		mem := readU32(payload[off:])
		off += 4
		iter := readU32(payload[off:])
		off += 4
		par := payload[off]
		off += 1
		salt := payload[off : off+saltLen]
		off += saltLen
		nonce := payload[off : off+nonceLen]
		off += nonceLen
		ciphertext := payload[off:]
		if mem == 0 || iter == 0 || par == 0 {
			return nil, fmt.Errorf("backup: invalid KDF parameters in header")
		}
		key := argon2.IDKey([]byte(passphrase), salt, iter, mem, par,
			chacha20poly1305.KeySize)
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, fmt.Errorf("backup: cipher: %w", err)
		}
		plain, err := aead.Open(nil, nonce, ciphertext, hdr)
		if err != nil {
			return nil, ErrBadPassphrase
		}
		payload = plain
	} else if passphrase != "" {
		// A passphrase on a plain archive is a user mistake worth
		// surfacing rather than silently ignoring.
		return nil, errors.New("backup: this archive is not encrypted (no passphrase was needed)")
	}

	manifest, files, err := parseTarGz(payload)
	if err != nil {
		return nil, err
	}
	if manifest.Encrypted != encrypted {
		return nil, fmt.Errorf("backup: encryption flag disagrees with the manifest (tampered?)")
	}
	if err := verify(manifest, files); err != nil {
		return nil, err
	}
	return &Archive{Manifest: manifest, Files: files}, nil
}

// verify checks that the member set matches the manifest exactly and
// every checksum holds.
func verify(m *Manifest, files map[string][]byte) error {
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("backup: manifest format version %d is not supported (expects %d)",
			m.FormatVersion, FormatVersion)
	}
	listed := make(map[string]bool, len(m.Members))
	for _, mem := range m.Members {
		listed[mem.Name] = true
		data, ok := files[mem.Name]
		if !ok {
			return fmt.Errorf("backup: member %q is listed in the manifest but missing", mem.Name)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != mem.SHA256 {
			return fmt.Errorf("backup: member %q fails its checksum (stored %s, got %s)",
				mem.Name, mem.SHA256[:12], got[:12])
		}
		if int64(len(data)) != mem.Size {
			return fmt.Errorf("backup: member %q size mismatch (manifest %d, actual %d)",
				mem.Name, mem.Size, len(data))
		}
	}
	for name := range files {
		if !listed[name] {
			return fmt.Errorf("backup: member %q is not listed in the manifest", name)
		}
	}
	return nil
}

func writeTarGz(w io.Writer, m *Manifest, manifestJSON []byte, files []File) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("backup: gzip: %w", err)
	}
	tw := newTarWriter(gz)
	if err := tw.add("manifest.json", manifestJSON); err != nil {
		return err
	}
	byName := make(map[string][]byte, len(files))
	for _, f := range files {
		byName[f.Name] = f.Data
	}
	// Follow manifest order so output is deterministic for identical
	// input (comparable archives in tests, simpler diffing in support).
	for _, mem := range m.Members {
		if err := tw.add(mem.Name, byName[mem.Name]); err != nil {
			return err
		}
	}
	if err := tw.close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backup: gzip: %w", err)
	}
	return nil
}

func parseTarGz(payload []byte) (*Manifest, map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("backup: not a gzip stream: %w", err)
	}
	defer gz.Close()
	tr := newTarReader(gz)
	var manifest *Manifest
	files := make(map[string][]byte)
	for {
		name, data, err := tr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("backup: read archive: %w", err)
		}
		if name == "manifest.json" {
			if manifest != nil {
				return nil, nil, errors.New("backup: duplicate manifest entry")
			}
			var m Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, nil, fmt.Errorf("backup: parse manifest: %w", err)
			}
			manifest = &m
			continue
		}
		if err := checkMemberName(name); err != nil {
			return nil, nil, err
		}
		if _, dup := files[name]; dup {
			return nil, nil, fmt.Errorf("backup: duplicate member %q", name)
		}
		files[name] = data
	}
	if manifest == nil {
		return nil, nil, errors.New("backup: no manifest inside the archive")
	}
	// The tar terminator ends the logical stream before gzip reaches
	// its own trailer; draining forces gzip to validate the CRC of
	// every byte, which is what catches a flipped byte in the tail.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return nil, nil, fmt.Errorf("backup: archive integrity: %w", err)
	}
	return manifest, files, nil
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func readU32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
