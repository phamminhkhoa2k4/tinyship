package cli

// Domain commands: attach a hostname to an app and list an app's
// domains. The API performs validation, rate limiting, and the DNS
// check; the CLI renders the outcome and warns loudly when a domain is
// not routed yet.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
)

// dispatchDomain handles the `tinyship domain <subcommand>` group.
func dispatchDomain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "tinyship domain: subcommand required: add or list")
		usageDomain(stderr)
		return 2
	}
	switch args[0] {
	case "add":
		return cmdDomainAdd(ctx, args[1:], stdout, stderr)
	case "list", "ls":
		return cmdDomainList(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usageDomain(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tinyship domain: unknown subcommand %q\n", args[0])
		usageDomain(stderr)
		return 2
	}
}

func usageDomain(w io.Writer) {
	fmt.Fprint(w, `usage:
  tinyship domain add <app> <hostname> [--skip-dns-check] [--json]
  tinyship domain list <app> [--json]

Domains only route after a succeeded deploy. A domain whose DNS does
not point at this host stays pending_dns and receives no traffic.
Run 'tinyship domain add --help' for all flags.
`)
}

// domainView is the API's domain DTO as the CLI renders it.
type domainView struct {
	ID        string   `json:"id"`
	Hostname  string   `json:"hostname"`
	Status    string   `json:"status"`
	IsPrimary bool     `json:"is_primary"`
	Warnings  []string `json:"warnings"`
}

func cmdDomainAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("domain add", stderr,
		"usage: tinyship domain add <app> <hostname> [--skip-dns-check] [--json]\n")
	skipDNS := fs.Bool("skip-dns-check", false,
		"attach without verifying DNS points at this host")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "tinyship domain add: an app name and a hostname are required")
		return 2
	}
	app, hostname := pos[0], pos[1]

	payload := map[string]any{"hostname": hostname}
	if *skipDNS {
		payload["skip_dns_check"] = true
	}
	body, _ := json.Marshal(payload)
	c := newAPIClient()
	path := "/apps/" + app + "/domains"
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship domain add: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship domain add: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(raw))
		return 0
	}
	var d domainView
	if err := json.Unmarshal(raw, &d); err != nil {
		fmt.Fprintf(stderr, "tinyship domain add: decode: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Domain %s attached to %s (status: %s)\n", d.Hostname, app, d.Status)
	if d.Status == "pending_dns" {
		fmt.Fprintln(stdout, "  NOT ROUTED YET: point the hostname's A/AAAA record at this"+
			" host, then re-run `tinyship domain add` to activate it")
	}
	for _, warning := range d.Warnings {
		fmt.Fprintf(stdout, "  warning: %s\n", warning)
	}
	return 0
}

func cmdDomainList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flagSet("domain list", stderr, "usage: tinyship domain list <app> [--json]\n")
	jsonOut := fs.Bool("json", false, "print the API response verbatim as JSON")
	pos, code := parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "tinyship domain list: exactly one app name is required")
		return 2
	}
	app := pos[0]

	c := newAPIClient()
	resp, err := c.do(ctx, http.MethodGet, "/apps/"+app+"/domains", nil)
	if err != nil {
		fmt.Fprintf(stderr, "tinyship domain list: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "tinyship domain list: %v\n", apiError(resp))
		return 1
	}
	raw, _ := io.ReadAll(resp.Body)
	if *jsonOut {
		fmt.Fprint(stdout, string(raw))
		return 0
	}
	var body struct {
		Domains []domainView `json:"domains"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		fmt.Fprintf(stderr, "tinyship domain list: decode: %v\n", err)
		return 1
	}
	if len(body.Domains) == 0 {
		fmt.Fprintf(stdout, "No domains on %s. Attach one with: tinyship domain add %s <hostname>\n",
			app, app)
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTNAME\tSTATUS\tPRIMARY")
	for _, d := range body.Domains {
		primary := "-"
		if d.IsPrimary {
			primary = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", d.Hostname, d.Status, primary)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "tinyship domain list: %v\n", err)
		return 1
	}
	return 0
}
