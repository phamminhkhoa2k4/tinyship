package cli

// Admin token lifecycle commands. These run locally (no API round trip,
// the same trust boundary as the key generator): whoever can open the
// database already owns the host, so routing through the API would only
// add another way to read a credential. The plaintext token is written to
// the system token file and printed once; only its argon2id hash reaches
// the database.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/worker"
)

// tokenFilePath returns the admin token file, overridable for tests and
// staging via TINYSHIP_TOKEN_FILE.
func tokenFilePath() string {
	if p := os.Getenv("TINYSHIP_TOKEN_FILE"); p != "" {
		return p
	}
	return DefaultTokenFile
}

// tokenDBPath returns the database path used by the API and worker,
// overridable via TINYSHIP_DB.
func tokenDBPath() string {
	if p := os.Getenv("TINYSHIP_DB"); p != "" {
		return p
	}
	return worker.DefaultDBPath
}

func dispatchToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tinyship token: subcommand required: create, list, revoke, or rotate")
		usageToken(stderr)
		return 2
	}
	switch args[0] {
	case "create":
		return cmdTokenCreate(ctx, args[1:], stdout, stderr)
	case "list", "ls":
		return cmdTokenList(ctx, args[1:], stdout, stderr)
	case "revoke", "rm":
		return cmdTokenRevoke(ctx, args[1:], stdout, stderr)
	case "rotate":
		return cmdTokenRotate(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usageToken(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship token: unknown subcommand %q\n", args[0])
		usageToken(stderr)
		return 2
	}
}

func usageToken(w io.Writer) {
	fmt.Fprint(w, `usage:
  tinyship token create [--name <name>] [--token-file <path>]
  tinyship token list [--json]
  tinyship token revoke <name>
  tinyship token rotate [--name <name>] [--token-file <path>]

Issue, inspect, revoke, or rotate the admin tokens that authenticate the
local control API. create/store the plaintext in the system token file
(mode 0640, group tinyship-admin by default) and print it once; revoke
invalidates a token; rotate replaces it in place and rewrites the file.
Run 'tinyship token <subcommand> --help' for all flags.
`)
}

// openTokenStore opens the database for a local token command.
func openTokenStore(ctx context.Context, stderr io.Writer) *store.Store {
	s, err := store.Open(ctx, tokenDBPath())
	if err != nil {
		fmt.Fprintf(stderr, "tinyship token: open database (%s): %v\n", tokenDBPath(), err)
		return nil
	}
	return s
}

func cmdTokenCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("token create", stderr, `usage: tinyship token create [--name <name>] [--token-file <path>]

Issue a new admin token, store only its hash, write the plaintext to the
token file (mode 0640), and print it once. The token file must not
already exist; use rotate to replace it.
`)
	name := fs.String("name", "admin", "token name (unique among active tokens)")
	file := fs.String("token-file", "", "token file path (default the system token file)")
	jsonOut := fs.Bool("json", false, "print the issued token as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship token create: unexpected argument %q\n", pos[0])
		return 2
	}
	path := *file
	if path == "" {
		path = tokenFilePath()
	}

	s := openTokenStore(ctx, stderr)
	if s == nil {
		return 1
	}
	defer s.Close()
	raw, err := s.CreateToken(ctx, *name)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship token create: %v\n", err)
		return 1
	}
	if err := writeTokenFileExclusive(path, raw); err != nil {
		// The token row is already active; with nothing written to the
		// file it is unusable, so revoke it rather than leave an orphan
		// credential that would authenticate but be unreachable.
		_ = s.RevokeToken(ctx, *name)
		fmt.Fprintf(stderr, "tinyship token create: %v\n", err)
		return 1
	}
	return reportToken(raw, *name, *jsonOut, stdout, stderr)
}

func cmdTokenRotate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("token rotate", stderr, `usage: tinyship token rotate [--name <name>] [--token-file <path>]

Replace the named token in place (clearing any earlier revocation) so
exactly one live credential keeps the name, rewrite the token file, and
print the new token once. The previous value stops working immediately.
`)
	name := fs.String("name", "admin", "token name to rotate")
	file := fs.String("token-file", "", "token file path (default the system token file)")
	jsonOut := fs.Bool("json", false, "print the issued token as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship token rotate: unexpected argument %q\n", pos[0])
		return 2
	}
	path := *file
	if path == "" {
		path = tokenFilePath()
	}

	s := openTokenStore(ctx, stderr)
	if s == nil {
		return 1
	}
	defer s.Close()
	raw, err := s.RotateToken(ctx, *name)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship token rotate: %v\n", err)
		return 1
	}
	if err := writeTokenFileOverwrite(path, raw); err != nil {
		fmt.Fprintf(stderr, "tinyship token rotate: %v\n", err)
		return 1
	}
	return reportToken(raw, *name, *jsonOut, stdout, stderr)
}

func reportToken(raw, name string, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		fmt.Fprintf(stdout, "{\"name\":%q,\"token\":%q}\n", name, raw)
		return 0
	}
	fmt.Fprintln(stdout, "Admin token issued (shown once, never again):")
	fmt.Fprintln(stdout, "  "+raw)
	fmt.Fprintf(stdout, "Name %s; the plaintext is stored in %s (mode 0640).\n", name, tokenFilePath())
	fmt.Fprintln(stdout, "Send it as Authorization: Bearer <token>; lost tokens rotate with: tinyship token rotate")
	return 0
}

func cmdTokenList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("token list", stderr, "usage: tinyship token list [--json]\n")
	jsonOut := fs.Bool("json", false, "print machine-readable output")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship token list: unexpected argument %q\n", pos[0])
		return 2
	}

	s := openTokenStore(ctx, stderr)
	if s == nil {
		return 1
	}
	defer s.Close()
	toks, err := s.ListTokens(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship token list: %v\n", err)
		return 1
	}
	if *jsonOut {
		fmt.Fprintln(stdout, tokensJSON(toks))
		return 0
	}
	if len(toks) == 0 {
		fmt.Fprintln(stdout, "No admin tokens. Create one with: tinyship token create")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCOPE\tCREATED\tLAST USED\tSTATUS")
	for _, t := range toks {
		status := "active"
		if t.RevokedAt != nil {
			status = "revoked " + fmtTime(t.RevokedAt)
		}
		last := "never"
		if t.LastUsedAt != nil {
			last = fmtTime(t.LastUsedAt)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Scope,
			fmtTime(&t.CreatedAt), last, status)
	}
	w.Flush()
	return 0
}

func tokensJSON(toks []store.TokenInfo) string {
	type row struct {
		Name       string     `json:"name"`
		Scope      string     `json:"scope"`
		CreatedAt  time.Time  `json:"created_at"`
		LastUsedAt *time.Time `json:"last_used_at,omitempty"`
		RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	}
	out := make([]row, 0, len(toks))
	for _, t := range toks {
		out = append(out, row{t.Name, t.Scope, t.CreatedAt, t.LastUsedAt, t.RevokedAt})
	}
	b, _ := json.Marshal(map[string]any{"tokens": out})
	return string(b)
}

func cmdTokenRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("token revoke", stderr, "usage: tinyship token revoke <name>\n")
	jsonOut := fs.Bool("json", false, "print the result as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship token revoke: exactly one token name is required")
		return 2
	}
	s := openTokenStore(ctx, stderr)
	if s == nil {
		return 1
	}
	defer s.Close()
	if err := s.RevokeToken(ctx, pos[0]); err != nil {
		fmt.Fprintf(stderr, "tinyship token revoke: %v\n", err)
		return 1
	}
	if *jsonOut {
		fmt.Fprintf(stdout, "{\"name\":%q,\"revoked\":true}\n", pos[0])
		return 0
	}
	fmt.Fprintf(stdout, "Token %s revoked; it no longer authenticates.\n", pos[0])
	return 0
}

// writeTokenFileExclusive writes the token path only when it does not
// exist yet, so a second create cannot silently clobber the live token.
func writeTokenFileExclusive(path, token string) error {
	return writeTokenFile(path, token, os.O_EXCL|os.O_CREATE|os.O_WRONLY)
}

func writeTokenFileOverwrite(path, token string) error {
	return writeTokenFile(path, token, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
}

func writeTokenFile(path, token string, flags int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("token file dir: %w", err)
	}
	f, err := os.OpenFile(path, flags, 0o640)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("token file %s already exists; use `tinyship token rotate` to replace it", path)
		}
		return fmt.Errorf("token file %s: %w", path, err)
	}
	if _, err := f.Write([]byte(token + "\n")); err != nil {
		f.Close()
		return fmt.Errorf("token file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("token file %s: %w", path, err)
	}
	// umask can loosen the create mode; re-assert it.
	if err := os.Chmod(path, 0o640); err != nil {
		return fmt.Errorf("token file %s chmod: %w", path, err)
	}
	return nil
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
