package caddy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

func TestBuildServerShape(t *testing.T) {
	rows := []store.RouteRow{
		{DomainID: "dom2", AppID: "appB", Hostname: "b.example.com", HostPort: 20018},
		{DomainID: "dom1", AppID: "appA", Hostname: "a.example.com", HostPort: 20017},
	}
	server, err := BuildServer(rows, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}

	raw, err := json.Marshal(server)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	// Deterministic order: sorted by hostname, not input order.
	if strings.Index(body, "a.example.com") > strings.Index(body, "b.example.com") {
		t.Errorf("routes are not sorted by hostname: %s", body)
	}
	// Closed shape: only reverse_proxy upstreams to loopback ports.
	if strings.Contains(body, "handler\":\"fastcgi") ||
		!strings.Contains(body, "\"handler\":\"reverse_proxy\"") {
		t.Errorf("unexpected handlers in %s", body)
	}
	for _, port := range []string{"127.0.0.1:20017", "127.0.0.1:20018"} {
		if !strings.Contains(body, port) {
			t.Errorf("missing upstream %s in %s", port, body)
		}
	}
	// Catch-all 404 is the last route.
	routes := server["routes"].([]any)
	last := routes[len(routes)-1].(map[string]any)
	handle := last["handle"].([]any)[0].(map[string]any)
	if handle["handler"] != "static_response" || handle["status_code"] != 404 {
		t.Errorf("last route = %+v, want static 404 fallback", last)
	}
	// strict_sni_host and listen stay fixed.
	if server["strict_sni_host"] != true {
		t.Errorf("strict_sni_host = %v, want true", server["strict_sni_host"])
	}
	if listen := server["listen"].([]string); len(listen) != 1 || listen[0] != ":443" {
		t.Errorf("listen = %v, want [:443]", listen)
	}
}

func TestBuildServerOverride(t *testing.T) {
	rows := []store.RouteRow{
		{DomainID: "dom1", AppID: "appA", Hostname: "a.example.com", HostPort: 20017},
		{DomainID: "dom2", AppID: "appB", Hostname: "b.example.com", HostPort: 20018},
	}
	server, err := BuildServer(rows, BuildOptions{Override: &Override{AppID: "appA", HostPort: 20099}})
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	body, _ := json.Marshal(server)
	if !strings.Contains(string(body), "127.0.0.1:20099") {
		t.Errorf("override port missing in %s", body)
	}
	if strings.Contains(string(body), "127.0.0.1:20017") {
		t.Errorf("stale port for overridden app in %s", body)
	}
	if !strings.Contains(string(body), "127.0.0.1:20018") {
		t.Errorf("other app's port must stay in %s", body)
	}
}

func TestBuildServerOptions(t *testing.T) {
	rows := []store.RouteRow{
		{DomainID: "dom1", AppID: "appA", Hostname: "a.example.com", HostPort: 20017},
	}

	// Zero options produce the installed-host shape.
	def, err := BuildServer(rows, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer default: %v", err)
	}
	if listen := def["listen"].([]string); len(listen) != 1 || listen[0] != DefaultListen {
		t.Errorf("default listen = %v, want [%s]", listen, DefaultListen)
	}
	if _, present := def["automatic_https"]; present {
		t.Error("default config must leave automatic HTTPS at Caddy's default")
	}

	// Development hosts replace the listen address and disable
	// certificate automation explicitly.
	dev, err := BuildServer(rows, BuildOptions{Listen: ":8443", DisableAutoHTTPS: true})
	if err != nil {
		t.Fatalf("BuildServer dev: %v", err)
	}
	if listen := dev["listen"].([]string); len(listen) != 1 || listen[0] != ":8443" {
		t.Errorf("dev listen = %v, want [:8443]", listen)
	}
	if ah, ok := dev["automatic_https"].(map[string]any); !ok || ah["disable"] != true {
		t.Errorf("automatic_https = %v, want {disable: true}", dev["automatic_https"])
	}
	// The route content is identical: only the environment knobs differ.
	if len(def["routes"].([]any)) != len(dev["routes"].([]any)) {
		t.Error("dev options changed the route set")
	}
}

func TestBuildServerRejectsBadRows(t *testing.T) {
	if _, err := BuildServer([]store.RouteRow{
		{DomainID: "d", AppID: "a", Hostname: "*.example.com", HostPort: 20017},
	}, BuildOptions{}); err == nil {
		t.Error("wildcard hostname accepted; want rejection")
	}
	if _, err := BuildServer([]store.RouteRow{
		{DomainID: "d1", AppID: "a", Hostname: "x.example.com", HostPort: 20017},
		{DomainID: "d2", AppID: "a", Hostname: "x.example.com", HostPort: 20018},
	}, BuildOptions{}); err == nil {
		t.Error("duplicate hostname accepted; want rejection")
	}
	if _, err := BuildServer([]store.RouteRow{
		{DomainID: "d", AppID: "a", Hostname: "x.example.com", HostPort: 80},
	}, BuildOptions{}); err == nil {
		t.Error("port outside managed range accepted; want rejection")
	}
	if _, err := BuildServer([]store.RouteRow{
		{DomainID: "d", AppID: "a", Hostname: "x.example.com", HostPort: 20017},
	}, BuildOptions{Override: &Override{AppID: "a", HostPort: policy.MinHostPort - 1}}); err == nil {
		t.Error("override outside managed range accepted; want rejection")
	}
}

func TestHashDeterministicAndSensitive(t *testing.T) {
	rows := []store.RouteRow{
		{DomainID: "dom1", AppID: "appA", Hostname: "a.example.com", HostPort: 20017},
	}
	s1, err := BuildServer(rows, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	s2, err := BuildServer(rows, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer 2: %v", err)
	}
	h1, err := Hash(s1)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	h2, _ := Hash(s2)
	if h1 != h2 || len(h1) != 64 {
		t.Errorf("hash = %s, %s; want equal 64-char hex", h1, h2)
	}

	changed, err := BuildServer([]store.RouteRow{
		{DomainID: "dom1", AppID: "appA", Hostname: "a.example.com", HostPort: 20018},
	}, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer changed: %v", err)
	}
	h3, _ := Hash(changed)
	if h3 == h1 {
		t.Error("port change did not change the hash")
	}
}
