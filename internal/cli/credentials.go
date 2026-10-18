package cli

// Registry credential commands: store a private-registry login, list
// logins, and delete one. The token is read from stdin or a file —
// never argv — because argv is visible in the process table and shell
// history. The API seals the token on arrival; nothing here can read
// it back.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
)

// dispatchCredential handles the `tinyship credential <subcommand>`
// group.
func dispatchCredential(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tinyship credential: subcommand required: set, list, or delete")
		usageCredential(stderr)
		return 2
	}
	switch args[0] {
	case "set", "add":
		return cmdCredentialSet(ctx, args[1:], stdout, stderr)
	case "list", "ls":
		return cmdCredentialList(ctx, args[1:], stdout, stderr)
	case "delete", "rm", "remove":
		return cmdCredentialDelete(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usageCredential(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship credential: unknown subcommand %q\n", args[0])
		usageCredential(stderr)
		return 2
	}
}

func usageCredential(w io.Writer) {
	fmt.Fprint(w, `usage:
  tinyship credential set <name> --registry <host> --username <user> [--token-file <path>]
  tinyship credential list [--json]
  tinyship credential delete <name> [--json]

Set reads the token from stdin by default (or from --token-file), so
it never touches argv. Tokens are stored encrypted and never returned.
Reference a credential from an app config with registry_credential.
Run 'tinyship credential set --help' for all flags.
`)
}

// credentialView is the API's credential DTO (deliberately tokenless).
type credentialView struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
	Username string `json:"username"`
}

func cmdCredentialSet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("credential set", stderr, `usage: tinyship credential set <name> --registry <host> --username <user> [--token-file <path>]

Store a private-registry login. The token is read from stdin unless
--token-file names a file. Re-running with the same name rotates the
token in place.
`)
	registry := fs.String("registry", "", "registry host (e.g. ghcr.io)")
	username := fs.String("username", "", "registry username or robot name")
	tokenFile := fs.String("token-file", "", "read the token from this file instead of stdin")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship credential set: a credential name is required")
		return 2
	}
	if *registry == "" || *username == "" {
		fmt.Fprintln(stderr, "tinyship credential set: --registry and --username are required")
		return 2
	}

	var tokenRaw []byte
	var err error
	if *tokenFile != "" {
		tokenRaw, err = os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship credential set: read token file: %v\n", err)
			return 1
		}
	} else {
		// No terminal echo suppression here: piping keeps the token
		// out of argv and history, which is the threat this guards.
		tokenRaw, err = io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "tinyship credential set: read stdin: %v\n", err)
			return 1
		}
	}
	token := strings.TrimRight(string(tokenRaw), "\r\n")
	if token == "" {
		fmt.Fprintln(stderr, "tinyship credential set: token is empty")
		return 2
	}

	payload, _ := json.Marshal(map[string]any{
		"name": pos[0], "registry": *registry, "username": *username, "token": token,
	})
	c := newAPIClient()
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/credentials", payload, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship credential set: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	cred, _ := out["credential"].(map[string]any)
	fmt.Fprintf(stdout, "Credential %s stored for %s as %s (token sealed; never shown again).\n",
		cred["name"], cred["registry"], cred["username"])
	fmt.Fprintln(stdout, "Reference it from an app config with registry_credential.")
	return 0
}

func cmdCredentialList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("credential list", stderr, "usage: tinyship credential list [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "tinyship credential list: unexpected argument %q\n", pos[0])
		return 2
	}

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodGet, "/credentials", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship credential list: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship credential list: %v\n", apiError(resp))
		return 1
	}
	var body struct {
		Credentials []credentialView `json:"credentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintf(stderr, "tinyship credential list: decode: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(body)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	if len(body.Credentials) == 0 {
		fmt.Fprintln(stdout, "No registry credentials stored.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREGISTRY\tUSERNAME")
	for _, cv := range body.Credentials {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", cv.Name, cv.Registry, cv.Username)
	}
	tw.Flush()
	return 0
}

func cmdCredentialDelete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("credential delete", stderr, "usage: tinyship credential delete <name> [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship credential delete: a credential name is required")
		return 2
	}

	c := newAPIClient()
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodDelete, "/credentials/"+pos[0], nil, &out); err != nil {
		fmt.Fprintf(stderr, "tinyship credential delete: %v\n", err)
		return 1
	}
	if *jsonOut {
		raw, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	fmt.Fprintf(stdout, "Credential %s deleted.\n", pos[0])
	return 0
}
