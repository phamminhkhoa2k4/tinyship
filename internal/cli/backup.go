package cli

// Local backup: writes one self-describing file with everything the
// control plane needs to be rebuilt — a consistent database snapshot,
// the host configuration, the sealed-box key pair and the admin token.
// Application volume data is deliberately excluded (it belongs to the
// apps and is restored by redeploying them); the output says so.
//
// It runs on the host itself (the same trust boundary as uninstall and
// the token commands): whoever can read the database and the private key
// already owns the machine, so routing through the API would only add
// another way to exfiltrate a credential. Passphrases never appear in
// argv: --encrypt reads stdin, --passphrase-file reads a file.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/backup"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/version"
)

// backupPaths returns every source path and the default output
// directory, rooted at the install root. An empty root means the real
// host: joining it with a relative element would yield a cwd-relative
// path, so the empty root becomes "/" here (the same rule as the
// uninstall paths). TINYSHIP_DB and TINYSHIP_TOKEN_FILE keep their
// usual precedence so sandbox runs point at their scratch files.
func backupPaths(root string) (db, config, keysDir, token, outDir string) {
	base := root
	if base == "" {
		base = "/"
	}
	db = filepath.Join(base, "var/lib/tinyship/tinyship.db")
	if p := os.Getenv("TINYSHIP_DB"); p != "" {
		db = p
	}
	config = filepath.Join(base, "etc/tinyship/config.yaml")
	keysDir = filepath.Join(base, "etc/tinyship/keys")
	token = filepath.Join(base, "etc/tinyship/token")
	if p := os.Getenv("TINYSHIP_TOKEN_FILE"); p != "" {
		token = p
	}
	outDir = filepath.Join(base, "var/lib/tinyship/backups")
	return db, config, keysDir, token, outDir
}

// backupPassphrase resolves the encryption passphrase from the flag
// pair. Empty means no encryption (the caller warns loudly).
func backupPassphrase(encrypt bool, passphraseFile string, in io.Reader) (string, error) {
	if encrypt && passphraseFile != "" {
		return "", fmt.Errorf("--encrypt and --passphrase-file are mutually exclusive")
	}
	if passphraseFile != "" {
		raw, err := os.ReadFile(passphraseFile)
		if err != nil {
			return "", fmt.Errorf("read passphrase file: %w", err)
		}
		return checkPassphrase(trimPassphrase(string(raw)))
	}
	if encrypt {
		// One line, not ReadAll: on an interactive terminal the next
		// prompt (restore's confirmation) must still find stdin open,
		// and passphrases never contain newlines anyway.
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("the passphrase is empty (stdin closed); type a real passphrase")
			}
			return "", fmt.Errorf("read passphrase from stdin: %w", err)
		}
		return checkPassphrase(trimPassphrase(line))
	}
	return "", nil
}

// trimPassphrase strips the trailing newline an echo or pipe adds.
func trimPassphrase(s string) string {
	return strings.TrimRight(s, "\r\n")
}

// checkPassphrase refuses an empty passphrase: empty means "no
// encryption" further down the stack, so accepting it would silently
// write a plaintext file the operator believes is protected.
func checkPassphrase(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("the passphrase is empty (after stripping the trailing newline); type a real passphrase")
	}
	return p, nil
}

func cmdBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("backup", stderr, `usage: tinyship backup [-o <file>] [--encrypt | --passphrase-file <path>]
                     [--json] [--install-root DIR]

Write one backup file with the database snapshot (consistent, taken
with VACUUM INTO while services may be running), the host configuration,
the sealed-box key pair, and the admin token. Application volume data is
not included: apps restart empty after a restore.

Without --encrypt or --passphrase-file the file is NOT protected and
contains your secret key; keep it on this host only. With --encrypt the
passphrase is read from stdin so it never lands in shell history.
`)
	out := fs.String("o", "", "output file (default <data>/backups/tinyship-backup-<UTC>.tsbak)")
	encrypt := fs.Bool("encrypt", false, "read the passphrase from stdin and encrypt the archive")
	passphraseFile := fs.String("passphrase-file", "", "read the passphrase from this file")
	jsonOut := fs.Bool("json", false, "print the backup summary as JSON")
	rootFlag := fs.String("install-root", "", "install root prefix (default: TINYSHIP_INSTALL_ROOT, empty on a real host)")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship backup: unexpected argument %q\n", pos[0])
		return 2
	}

	root := uninstallRoot(*rootFlag)
	realHost := root == ""
	if realHost && os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "tinyship backup: must run as root on the host (the database and keys are root-readable only); or use --install-root for a sandbox tree")
		return 1
	}
	passphrase, err := backupPassphrase(*encrypt, *passphraseFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}

	dbPath, configPath, keysDir, tokenPath, outDir := backupPaths(root)
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "tinyship backup: no database at %s; install first\n", dbPath)
		return 1
	}

	// Snapshot without migrating: a backup must not change the state it
	// is preserving. The applied schema version is read from the data,
	// so the manifest describes the snapshot, not this binary.
	s, err := store.OpenPlain(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship backup: open database %s: %v\n", dbPath, err)
		return 1
	}
	schema, err := s.AppliedSchemaVersion(ctx)
	if err != nil {
		s.Close()
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}
	tmp, err := os.MkdirTemp("", "tinyship-backup-")
	if err != nil {
		s.Close()
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	snapPath := filepath.Join(tmp, "tinyship.db")
	if err := s.Snapshot(ctx, snapPath); err != nil {
		s.Close()
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}
	if err := s.Close(); err != nil {
		fmt.Fprintf(stderr, "tinyship backup: close database: %v\n", err)
		return 1
	}

	files := []backup.File{{Name: "var/lib/tinyship/tinyship.db"}}
	var missing []string
	optional := []struct{ name, path string }{
		{"etc/tinyship/config.yaml", configPath},
		{"etc/tinyship/keys/secrets.key", filepath.Join(keysDir, "secrets.key")},
		{"etc/tinyship/keys/secrets.pub", filepath.Join(keysDir, "secrets.pub")},
		{"etc/tinyship/token", tokenPath},
	}
	for _, f := range optional {
		data, err := os.ReadFile(f.path)
		if err != nil {
			missing = append(missing, f.path)
			continue
		}
		files = append(files, backup.File{Name: f.name, Data: data})
	}
	snap, err := os.ReadFile(snapPath)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship backup: read snapshot: %v\n", err)
		return 1
	}
	files[0].Data = snap

	target := *out
	if target == "" {
		ts := time.Now().UTC().Format("20060102T150405Z")
		target = filepath.Join(outDir, "tinyship-backup-"+ts+".tsbak")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		fmt.Fprintf(stderr, "tinyship backup: create backup directory: %v\n", err)
		return 1
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			fmt.Fprintf(stderr, "tinyship backup: %s already exists; choose another -o path\n", target)
			return 1
		}
		fmt.Fprintf(stderr, "tinyship backup: create %s: %v\n", target, err)
		return 1
	}
	hostname, _ := os.Hostname()
	manifest, err := backup.Write(f, backup.Options{
		Files:           files,
		Passphrase:      passphrase,
		TinyshipVersion: version.Version,
		SchemaVersion:   schema,
		Hostname:        hostname,
	})
	if err != nil {
		f.Close()
		os.Remove(target)
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}
	// umask can loosen the create mode; a file holding the secret key
	// must stay owner-only.
	if err := os.Chmod(target, 0o600); err != nil {
		fmt.Fprintf(stderr, "tinyship backup: chmod: %v\n", err)
		return 1
	}
	return reportBackup(target, manifest, missing, *jsonOut, stdout, stderr)
}

// reportBackup prints the human summary (or JSON) plus the two standing
// notices: what was skipped, and that volume data is not in the file.
func reportBackup(target string, m *backup.Manifest, missing []string, jsonOut bool, stdout, stderr io.Writer) int {
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship backup: %v\n", err)
		return 1
	}
	for _, p := range missing {
		fmt.Fprintf(stderr, "tinyship backup: not found, skipped: %s\n", p)
	}
	if jsonOut {
		out := struct {
			Path          string   `json:"path"`
			Bytes         int64    `json:"bytes"`
			Encrypted     bool     `json:"encrypted"`
			SchemaVersion int      `json:"schema_version"`
			Members       []string `json:"members"`
		}{target, info.Size(), m.Encrypted, m.SchemaVersion, memberNames(m)}
		b, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Backup written: %s (%d bytes, schema %d)\n", target, info.Size(), m.SchemaVersion)
	fmt.Fprintf(stdout, "Members:\n")
	for _, name := range memberNames(m) {
		fmt.Fprintf(stdout, "  %s\n", name)
	}
	fmt.Fprintln(stdout, "Application volume data is NOT in this file; apps restart empty after a restore (redeploy them).")
	if !m.Encrypted {
		fmt.Fprintln(stderr, "WARNING: this backup is NOT encrypted and contains your secret key; anyone holding the file can decrypt your secrets. Use --encrypt before copying it anywhere.")
	} else {
		fmt.Fprintln(stdout, "Encrypted; the passphrase is required to restore and is not stored anywhere.")
	}
	return 0
}

// memberNames returns the manifest member names in sorted order (the
// archive writes them sorted, so this mirrors the file).
func memberNames(m *backup.Manifest) []string {
	names := make([]string, 0, len(m.Members))
	for _, mem := range m.Members {
		names = append(names, mem.Name)
	}
	return names
}
