package caddy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// Override redirects one app's upstream to a fresh host port during a
// deploy: the new container's port is not the app's current deployment
// port yet, but its domains must already point at it. Empty AppID means
// no override.
type Override struct {
	AppID    string
	HostPort int
}

// BuildServer generates the full `tinyship` server object from database
// rows. The shape is closed: reverse_proxy to 127.0.0.1:<port> only, a
// terminal host match per route, and a final 404 so unknown hostnames
// hit nothing. Output is deterministic (routes sorted by hostname) so
// the config hash is stable across regenerations.
func BuildServer(rows []store.RouteRow, ov *Override) (map[string]any, error) {
	sorted := append([]store.RouteRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Hostname < sorted[j].Hostname })

	routes := make([]any, 0, len(sorted)+1)
	seen := map[string]bool{}
	for _, r := range sorted {
		if err := ValidateHostname(r.Hostname); err != nil {
			return nil, err
		}
		if seen[r.Hostname] {
			return nil, fmt.Errorf("caddy: hostname %q appears twice in route rows", r.Hostname)
		}
		seen[r.Hostname] = true

		port := r.HostPort
		if ov != nil && ov.AppID != "" && r.AppID == ov.AppID {
			port = ov.HostPort
		}
		if port < policy.MinHostPort || port > policy.MaxHostPort {
			return nil, fmt.Errorf("caddy: hostname %q maps to port %d outside the managed range %d-%d",
				r.Hostname, port, policy.MinHostPort, policy.MaxHostPort)
		}
		routes = append(routes, map[string]any{
			"@id": fmt.Sprintf("ts-route-%s", r.DomainID),
			"match": []any{map[string]any{
				"host": []string{r.Hostname},
			}},
			"handle": []any{map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": fmt.Sprintf("127.0.0.1:%d", port)}},
			}},
			"terminal": true,
		})
	}
	// The catch-all 404 must stay last so no hostname falls through to a
	// neighboring app.
	routes = append(routes, map[string]any{
		"handle": []any{map[string]any{
			"handler":     "static_response",
			"status_code": 404,
		}},
	})

	return map[string]any{
		"listen":          []string{":443"},
		"routes":          routes,
		"strict_sni_host": true,
	}, nil
}

// Hash returns the hex sha256 of the canonical JSON encoding. It is the
// identity of a generated config: stored in settings and compared after
// every apply.
func Hash(server map[string]any) (string, error) {
	raw, err := json.Marshal(server)
	if err != nil {
		return "", fmt.Errorf("caddy: encode server config: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
