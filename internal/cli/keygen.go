package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/tinyship/tinyship/internal/secrypto"
)

// defaultKeysDir mirrors the installed key location; the worker and
// API read the same paths unless their environment overrides them.
const defaultKeysDir = "/etc/tinyship/keys"

// cmdKeygen creates the sealed-box key pair: the worker's private key
// and the API's public key. It runs entirely on the host (no API
// call): whoever can write the key directory already owns the host,
// so making an API round trip would only add a way to leak the key.
func cmdKeygen(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	_ = ctx
	fs := flagSet("keygen", stderr, `usage: tinyship keygen [--keys-dir DIR]

Generate the sealed-box key pair that protects stored secrets:

  DIR/secrets.key  0400, private key, read by the worker only
  DIR/secrets.pub  0640, public key, used by the API to seal

Existing files are never overwritten; rotating keys over stored
secrets would make them unreadable. Run this once at install time.
`)
	keysDir := fs.String("keys-dir", defaultKeysDir, "directory to write the key pair into")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship keygen: unexpected argument %q\n", pos[0])
		return 2
	}

	keyPath, pubPath, err := secrypto.WriteKeyPair(*keysDir)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship keygen: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Private key  %s (0400, worker only)\n", keyPath)
	fmt.Fprintf(stdout, "Public key   %s (0640, API seals with it)\n", pubPath)
	return 0
}
