// Package caddy generates the reverse-proxy server configuration from
// validated database rows and applies it through the Caddy admin API on
// a Unix socket. It is the only package that formats proxy config, and
// nothing a user supplies reaches the config except a hostname that
// passed validation.
package caddy

import (
	"fmt"
	"net"
	"strings"
)

// ValidateHostname enforces the hostname rules: lowercase RFC 1123,
// total length at most 253, each label at most 63, no wildcards, no IP
// literals, no localhost. Everything route-related funnels through it.
func ValidateHostname(name string) error {
	if name == "" {
		return fmt.Errorf("caddy: hostname is empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("caddy: hostname %q exceeds 253 characters", name)
	}
	if name == "localhost" {
		return fmt.Errorf("caddy: hostname %q is not routable", name)
	}
	if strings.ContainsAny(name, "*?[]") {
		return fmt.Errorf("caddy: hostname %q must not contain wildcards", name)
	}
	if net.ParseIP(name) != nil {
		return fmt.Errorf("caddy: hostname %q is an IP address", name)
	}
	if strings.ToLower(name) != name {
		return fmt.Errorf("caddy: hostname %q must be lowercase", name)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return fmt.Errorf("caddy: hostname %q has an empty label", name)
		}
		if len(label) > 63 {
			return fmt.Errorf("caddy: hostname %q has a label over 63 characters", name)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("caddy: hostname %q has a label starting or ending with a hyphen", name)
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return fmt.Errorf("caddy: hostname %q contains %q outside RFC 1123", name, string(r))
		}
	}
	return nil
}
