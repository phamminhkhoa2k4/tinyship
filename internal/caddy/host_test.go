package caddy

import (
	"strings"
	"testing"
)

func TestValidateHostnameAccepts(t *testing.T) {
	for _, name := range []string{
		"blog.example.com",
		"a.co",
		"api-v2.example-site.org",
		"x.y.z.example.com",
		"123.example.com",
		"xn--bcher-kva.example.com",
	} {
		if err := ValidateHostname(name); err != nil {
			t.Errorf("ValidateHostname(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateHostnameRejects(t *testing.T) {
	cases := []struct {
		name string
		want string // expected fragment in the error message
	}{
		{"", "empty"},
		{"localhost", "not routable"},
		{"LOCAL.example.com", "lowercase"},
		{"Blog.example.com", "lowercase"},
		{"*.example.com", "wildcards"},
		{"b?d.example.com", "wildcards"},
		{"127.0.0.1", "IP address"},
		{"::1", "IP address"},
		{"example..com", "empty label"},
		{"-bad.example.com", "hyphen"},
		{"bad-.example.com", "hyphen"},
		{"under_score.example.com", "RFC 1123"},
		{"exa mple.example.com", "RFC 1123"},
		// Config-injection shapes: anything that could break out of a
		// generated proxy config must be refused by the same character
		// rule, long before any config is formatted.
		{"example.com\nevil.example.com", "RFC 1123"},
		{"example.com}evil", "RFC 1123"},
		{"example.com\"quote", "RFC 1123"},
		{"example.com{foo}", "RFC 1123"},
		{"example.com.", "empty label"},
		{strings.Repeat("a", 64) + ".example.com", "63"},
		{strings.Repeat("a.", 130) + "com", "253"},
	}
	for _, tc := range cases {
		err := ValidateHostname(tc.name)
		if err == nil {
			t.Errorf("ValidateHostname(%q) = nil, want rejection mentioning %q", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateHostname(%q) = %q, want mention of %q", tc.name, err.Error(), tc.want)
		}
	}
}
