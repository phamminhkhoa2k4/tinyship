package api

// Domain attach tests: strict hostname validation, rate limiting,
// cross-app uniqueness, and the DNS check that decides active versus
// pending_dns. The resolver is injected; the host's public address comes
// from the settings row so tests never touch the network.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

const hostPublicIP = "203.0.113.10"

func postDomain(t *testing.T, srv *httptest.Server, app, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/apps/"+app+"/domains", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestAddDomainValidatesHostname(t *testing.T) {
	srv, _ := testServer(t, t.TempDir())
	createApp(t, srv, testConfig)

	for _, body := range []string{
		`{"hostname": ""}`,
		`{"hostname": "localhost"}`,
		`{"hostname": "*.example.com"}`,
		`{"hostname": "127.0.0.1"}`,
		`{"hostname": "exa_mple.example.com"}`,
		`{"hostname": "-bad.example.com"}`,
		`{"unknown_field": true, "hostname": "ok.example.com"}`,
	} {
		resp := postDomain(t, srv, "web", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
		if code := errCode(t, resp); code != "E_VALIDATION" {
			t.Errorf("body %s: code = %s, want E_VALIDATION", body, code)
		}
	}
}

// TestAddDomainNormalizesCase proves mixed-case input is stored
// lowercased (DNS names are case-insensitive; the route config is
// always lowercase).
func TestAddDomainNormalizesCase(t *testing.T) {
	s := newDomainServer(t, func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(hostPublicIP)}, nil
	})
	createApp(t, s.srv, testConfig)

	resp := postDomain(t, s.srv, "web", `{"hostname":"Blog.Example.com"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	if body["hostname"] != "blog.example.com" {
		t.Errorf("hostname = %v, want blog.example.com", body["hostname"])
	}
}

func TestAddDomainDNSCheckDecidesStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolved   []string
		wantStatus string
		wantWarn   string
	}{
		{"points here", []string{hostPublicIP}, "active", ""},
		{"points elsewhere", []string{"198.51.100.7"}, "pending_dns", "not routed"},
		{"private record only", []string{"10.0.0.5"}, "pending_dns", "not routed"},
		{"resolution empty", nil, "pending_dns", "not routed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDomainServer(t, func(string) ([]net.IP, error) {
				var out []net.IP
				for _, ip := range tc.resolved {
					out = append(out, net.ParseIP(ip))
				}
				return out, nil
			})
			createApp(t, s.srv, testConfig)

			resp := postDomain(t, s.srv, "web", `{"hostname":"blog.example.com"}`)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("status = %d, want 201", resp.StatusCode)
			}
			body := decodeBody(t, resp)
			if body["status"] != tc.wantStatus {
				t.Errorf("status = %v, want %s", body["status"], tc.wantStatus)
			}
			if tc.wantWarn == "" {
				if _, ok := body["warnings"]; ok {
					t.Errorf("unexpected warnings: %v", body["warnings"])
				}
			} else if !strings.Contains(fmt.Sprint(body["warnings"]), tc.wantWarn) {
				t.Errorf("warnings = %v, want mention of %q", body["warnings"], tc.wantWarn)
			}
		})
	}
}

func TestAddDomainSkipDNSCheckActivates(t *testing.T) {
	s := newDomainServer(t, func(string) ([]net.IP, error) {
		return nil, fmt.Errorf("no DNS yet")
	})
	createApp(t, s.srv, testConfig)

	resp := postDomain(t, s.srv, "web",
		`{"hostname":"blog.example.com","skip_dns_check":true}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	if body["status"] != "active" {
		t.Errorf("status = %v, want active", body["status"])
	}
	if !strings.Contains(fmt.Sprint(body["warnings"]), "DNS check skipped") {
		t.Errorf("warnings = %v, want skip notice", body["warnings"])
	}
}

func TestAddDomainRateLimitAndTaken(t *testing.T) {
	s := newDomainServer(t, func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(hostPublicIP)}, nil
	})
	createApp(t, s.srv, testConfig)

	// Attach a second app so the uniqueness check has a taker.
	other := strings.Replace(testConfig, `"web"`, `"api"`, 1)
	createApp(t, s.srv, other)

	// First attach succeeds; re-attach of the same app is idempotent.
	for i := 0; i < 2; i++ {
		resp := postDomain(t, s.srv, "web", `{"hostname":"blog.example.com"}`)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("attach %d: status = %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Another app may not take the name.
	resp := postDomain(t, s.srv, "api", `{"hostname":"blog.example.com"}`)
	if resp.StatusCode != http.StatusConflict || errCode(t, resp) != "E_DOMAIN_TAKEN" {
		t.Errorf("cross-app attach: status = %d code = %s, want 409 E_DOMAIN_TAKEN",
			resp.StatusCode, errCode(t, resp))
	}

	// Every attempt counts: after the limit the same app is refused too.
	// Attempts 1-5 pass the check; the 6th sees 5 in the window.
	for i := 0; i < DomainAddLimit; i++ {
		resp := postDomain(t, s.srv, "web", `{"hostname":"rate.example.com"}`)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d rate-limited early", i+1)
		}
		resp.Body.Close()
	}
	resp = postDomain(t, s.srv, "web", `{"hostname":"rate.example.com"}`)
	if resp.StatusCode != http.StatusTooManyRequests ||
		errCode(t, resp) != "E_DOMAIN_RATE_LIMITED" {
		t.Errorf("over limit: status = %d code = %s, want 429 E_DOMAIN_RATE_LIMITED",
			resp.StatusCode, errCode(t, resp))
	}
}

func TestAddDomainReattachPromotesPending(t *testing.T) {
	s := newDomainServer(t, func(string) ([]net.IP, error) {
		return nil, fmt.Errorf("nxdomain")
	})
	createApp(t, s.srv, testConfig)

	resp := postDomain(t, s.srv, "web", `{"hostname":"blog.example.com"}`)
	if body := decodeBody(t, resp); body["status"] != "pending_dns" {
		t.Fatalf("first attach = %v, want pending_dns", body["status"])
	}

	// DNS is fixed: the re-attach re-checks and promotes.
	s.resolve = func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(hostPublicIP)}, nil
	}
	resp = postDomain(t, s.srv, "web", `{"hostname":"blog.example.com"}`)
	if body := decodeBody(t, resp); body["status"] != "active" {
		t.Errorf("reattach = %v, want active", body["status"])
	}
}

func TestListDomains(t *testing.T) {
	s := newDomainServer(t, func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(hostPublicIP)}, nil
	})
	createApp(t, s.srv, testConfig)
	resp := postDomain(t, s.srv, "web", `{"hostname":"blog.example.com"}`)
	resp.Body.Close()

	resp, err := http.Get(s.srv.URL + "/apps/web/domains")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Domains []domainDTO `json:"domains"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Domains) != 1 || body.Domains[0].Hostname != "blog.example.com" {
		t.Errorf("domains = %+v", body.Domains)
	}

	resp, err = http.Get(s.srv.URL + "/apps/ghost/domains")
	if err != nil {
		t.Fatalf("get ghost: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown app status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// domainServer is a mux built with a custom resolver and a fixed host
// public IP, so DNS outcomes are scripted per test.
type domainServer struct {
	srv     *httptest.Server
	store   *store.Store
	resolve func(string) ([]net.IP, error)
}

func newDomainServer(t *testing.T, resolve func(string) ([]net.IP, error)) *domainServer {
	t.Helper()
	ds := &domainServer{resolve: resolve}
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tinyship.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ds.store = s
	if err := s.PutSetting(context.Background(), PublicIPsSetting, hostPublicIP); err != nil {
		t.Fatalf("put public ips: %v", err)
	}
	mux := newMuxWith(s, t.TempDir(), slog.New(slog.DiscardHandler),
		func(_ context.Context, hostname string) ([]net.IP, error) {
			return ds.resolve(hostname)
		}, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ds.srv = srv
	return ds
}
