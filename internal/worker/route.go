package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/caddy"
	"github.com/tinyship/tinyship/internal/store"
)

// CaddyConfigHashKey is the settings row holding the hash of the proxy
// config the worker last applied successfully.
const CaddyConfigHashKey = "caddy_config_hash"

// Caddy is the subset of the route engine the deploy pipeline needs. The
// production implementation is *caddy.Client over the admin socket;
// tests inject a fake.
type Caddy interface {
	CurrentConfig(ctx context.Context) (map[string]any, string, error)
	ApplyServer(ctx context.Context, server map[string]any) (string, error)
}

// ProbeFunc checks one hostname through the proxy and returns the HTTP
// status it observed.
type ProbeFunc func(ctx context.Context, hostname string) (int, error)

// DefaultProbeAddr is where the final check reaches the proxy on an
// installed host. TINYSHIP_CADDY_PROBE overrides it for development and
// tests.
const DefaultProbeAddr = "127.0.0.1:80"

// DefaultProbe sends one request through the loopback proxy listener
// with the hostname as the Host header: routing is proven even before
// public DNS or certificates exist, which is exactly the final check's
// job after a route switch. The probe address may be a full URL; an
// https:// address switches the scheme so test hosts can check routing
// through a TLS-terminating proxy.
func DefaultProbe(ctx context.Context, hostname string) (int, error) {
	addr := os.Getenv("TINYSHIP_CADDY_PROBE")
	if addr == "" {
		addr = DefaultProbeAddr
	}
	scheme := "http"
	target := addr
	if rest, ok := strings.CutPrefix(addr, "https://"); ok {
		scheme, target = "https", rest
	} else if rest, ok := strings.CutPrefix(addr, "http://"); ok {
		target = rest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+target+"/", nil)
	if err != nil {
		return 0, err
	}
	req.Host = hostname
	// This probe proves routing, not certificate identity: the
	// loopback target presents whatever issuer the host configured,
	// and trust-store contents must not decide whether a route switch
	// counts as healthy. A proxy with automatic HTTPS answers a plain
	// request with a redirect to https, and the followed request keeps
	// the same trust model — a locally issued certificate must not
	// fail a deploy that reached the app. The server name carries the
	// hostname on every handshake because the proxy refuses handshakes
	// whose SNI does not match a route, while the connection itself
	// goes to the loopback proxy address.
	client := &http.Client{Timeout: 10 * time.Second}
	client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // routing probe by design
			ServerName:         hostname,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// RegenerateRoutes rebuilds the proxy config from the database and
// applies it when the live proxy differs from both the database and the
// recorded hash. This is crash recovery in one function: after any
// restart the routes converge to the last succeeded deployments, no
// matter where the previous process died. The live check matters because
// the proxy starts from its baseline file and does not persist API
// updates across a service or host restart. A host that never routed
// anything and has no active domains is a no-op, so fresh installs do
// not touch the proxy baseline.
func RegenerateRoutes(ctx context.Context, s *store.Store, cc Caddy, log *slog.Logger) error {
	rows, err := s.ListRouteRows(ctx)
	if err != nil {
		return err
	}
	applied, err := s.GetSetting(ctx, CaddyConfigHashKey)
	if err != nil {
		return err
	}
	if applied == "" && len(rows) == 0 {
		return nil
	}
	server, err := caddy.BuildServer(rows, buildOptions(nil))
	if err != nil {
		return err
	}
	hash, err := caddy.Hash(server)
	if err != nil {
		return err
	}
	if applied == hash {
		live, _, err := cc.CurrentConfig(ctx)
		if err == nil {
			liveHash, hashErr := caddy.Hash(live)
			if hashErr == nil && liveHash == hash {
				log.Debug("worker: routes already current", "hash", hash)
				return nil
			}
			log.Info("worker: live proxy config differs; reapplying routes",
				"want", hash, "live", liveHash)
		} else {
			log.Info("worker: could not read live proxy config; reapplying routes",
				"err", err)
		}
	}
	got, err := cc.ApplyServer(ctx, server)
	if err != nil {
		return err
	}
	if err := s.PutSetting(ctx, CaddyConfigHashKey, got); err != nil {
		return err
	}
	log.Info("worker: routes regenerated", "hash", got, "routes", len(rows))
	return nil
}

// buildOptions maps the environment to one generation's options.
// TINYSHIP_CADDY_LISTEN and TINYSHIP_CADDY_AUTO_HTTPS=off exist for
// development and test hosts: they cannot bind privileged ports and
// have no public DNS, so the proxy listens on a high port with
// certificate automation disabled. TINYSHIP_CADDY_AUTO_HTTPS=internal
// is the test-CA profile: keep automation on and terminate TLS on the
// high port while the base config supplies an internal issuer.
// Installed hosts use the defaults.
func buildOptions(ov *caddy.Override) caddy.BuildOptions {
	opts := caddy.BuildOptions{Override: ov}
	if v := os.Getenv("TINYSHIP_CADDY_LISTEN"); v != "" {
		opts.Listen = v
	}
	switch os.Getenv("TINYSHIP_CADDY_AUTO_HTTPS") {
	case "off":
		opts.DisableAutoHTTPS = true
	case "internal":
		opts.ServeTLS = true
	}
	return opts
}

// applyServer generates, applies, and records the hash for one set of
// route rows. ov redirects one app's upstream to a port the database
// does not know yet (the new container during route_switch); nil means
// the database as-is.
func (p *Deployer) applyServer(ctx context.Context, rows []store.RouteRow, ov *caddy.Override) (string, error) {
	server, err := caddy.BuildServer(rows, buildOptions(ov))
	if err != nil {
		return "", &pipelineError{code: "E_CADDY_APPLY", err: err}
	}
	hash, err := p.Caddy.ApplyServer(ctx, server)
	if err != nil {
		var ue *caddy.UnreachableError
		if errors.As(err, &ue) {
			// Caddy restarting is transient; a rejected config is not.
			return "", &RetryableError{err}
		}
		return "", &pipelineError{code: "E_CADDY_APPLY", err: err}
	}
	if err := p.Store.PutSetting(ctx, CaddyConfigHashKey, hash); err != nil {
		return "", &RetryableError{err}
	}
	return hash, nil
}

// routeRows returns every routable row plus this app's own active
// domains. The database join only sees a domain once its app has a
// current deployment with a port, so a first deploy with domains would
// otherwise route nothing.
func (p *Deployer) routeRows(ctx context.Context, app *store.App) ([]store.RouteRow, error) {
	rows, err := p.Store.ListRouteRows(ctx)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, r := range rows {
		present[r.Hostname] = true
	}
	own, err := p.Store.ListDomains(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	for _, dom := range own {
		if dom.Status == store.DomainActive && !present[dom.Hostname] {
			rows = append(rows, store.RouteRow{
				DomainID: dom.ID, AppID: app.ID, Hostname: dom.Hostname,
			})
		}
	}
	return rows, nil
}

// stepRouteSwitch regenerates the proxy config with this app's domains
// pointing at the new container, then applies it atomically. The
// database still names the previous deployment as current (finalize
// moves that pointer), so every later regeneration from the database
// reproduces this exact config: the switch is idempotent and survives a
// crash at any point.
func (p *Deployer) stepRouteSwitch(ctx context.Context, d *deployRun) (any, error) {
	domains, err := p.Store.ListActiveDomains(ctx, d.app.ID)
	if err != nil {
		return nil, &RetryableError{err}
	}
	d.domains = domains
	if len(domains) == 0 {
		p.skip(ctx, d, "route_switch", map[string]any{
			"reason": "no active domains to route"})
		return nil, nil
	}
	if p.Caddy == nil {
		return nil, &pipelineError{code: "E_CADDY_APPLY", err: fmt.Errorf(
			"app has %d active domain(s) but no route engine is configured; "+
				"refusing to report success without routing", len(domains))}
	}
	rows, err := p.routeRows(ctx, d.app)
	if err != nil {
		return nil, &RetryableError{err}
	}
	hash, err := p.applyServer(ctx, rows, &caddy.Override{
		AppID: d.app.ID, HostPort: d.hostPort,
	})
	if err != nil {
		return nil, err
	}
	p.Logger.Info("deploy: route switched", "domains", len(domains),
		"host_port", d.hostPort, "config_hash", hash)
	return map[string]any{"domains": len(domains), "config_hash": hash,
		"host_port": d.hostPort}, nil
}

// stepFinalCheck probes every active domain through the proxy and fails
// the deployment unless all of them answer. Any status below 500 proves
// the request passed through the proxy and reached the app: a 404 from
// the app is still a working route. On failure the route is restored by
// regenerating from the database without the override (the current
// deployment pointer still names the last good deployment), and the
// run's recovery then brings the old container back.
func (p *Deployer) stepFinalCheck(ctx context.Context, d *deployRun) (any, error) {
	if len(d.domains) == 0 {
		p.skip(ctx, d, "final_check", map[string]any{
			"reason": "no active domains to probe"})
		return nil, nil
	}
	probe := p.Probe
	if probe == nil {
		probe = DefaultProbe
	}
	results := make([]map[string]any, 0, len(d.domains))
	for _, host := range d.domains {
		status, err := probe(ctx, host)
		if err != nil || status >= 500 {
			// Restore from the database as-is: a previous deployment
			// still being current brings its port back, and a first
			// deploy leaves the domain unrouted (the 404 fallback)
			// instead of pointing at a container that is being removed.
			if rows, rerr := p.Store.ListRouteRows(ctx); rerr != nil {
				p.Logger.Error("deploy: could not rebuild routes after failed final check",
					"err", rerr, "host", host)
			} else if _, aerr := p.applyServer(ctx, rows, nil); aerr != nil {
				// The deployment already fails; a failed restore is
				// logged loudly because traffic may still point at the
				// new container.
				p.Logger.Error("deploy: route restore after failed final check",
					"err", aerr, "host", host)
			} else {
				p.Logger.Info("deploy: route restored to previous deployment",
					"host", host)
			}
			return nil, &pipelineError{code: "E_FINAL_CHECK_FAILED", err: fmt.Errorf(
				"probe of %s failed (status=%d, err=%v); route restored to the "+
					"previous deployment", host, status, err)}
		}
		results = append(results, map[string]any{"host": host, "status": status})
	}
	p.Logger.Info("deploy: final check passed", "domains", len(d.domains))
	return map[string]any{"probes": results}, nil
}
