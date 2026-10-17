package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/caddy"
	"github.com/tinyship/tinyship/internal/store"
)

// PublicIPsSetting is the settings key the installer writes with the
// host's public addresses; the domain DNS check compares against it.
const PublicIPsSetting = "public_ips"

// DomainAddLimit is how many times one hostname may be attached per hour.
// Certificate authorities rate-limit hard, and a throttled name stalls
// issuance for hours.
const DomainAddLimit = 5

// Resolver looks up a hostname's addresses (A and AAAA).
type Resolver func(ctx context.Context, hostname string) ([]net.IP, error)

// domainBody is the attach request shape.
type domainBody struct {
	Hostname     string `json:"hostname"`
	SkipDNSCheck bool   `json:"skip_dns_check,omitempty"`
}

// domainDTO is one domain in API responses.
type domainDTO struct {
	ID        string    `json:"id"`
	Hostname  string    `json:"hostname"`
	Status    string    `json:"status"`
	IsPrimary bool      `json:"is_primary"`
	Warnings  []string  `json:"warnings,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func domainToDTO(d *store.Domain, warnings []string) domainDTO {
	return domainDTO{
		ID: d.ID, Hostname: d.Hostname, Status: d.Status,
		IsPrimary: d.IsPrimary, Warnings: warnings, CreatedAt: d.CreatedAt,
	}
}

// handleAddDomain attaches a hostname to an app. The flow: validate the
// hostname, enforce the per-hostname rate limit, reject names owned by
// another app, then check DNS unless skipped. A hostname whose A/AAAA
// records do not point at this host stays pending_dns (and is never
// routed); re-attaching it after fixing DNS promotes it to active.
func (s *server) handleAddDomain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND",
			fmt.Sprintf("no such app %q; create it with `tinyship app create`", name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	var body domainBody
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	host := strings.ToLower(strings.TrimSpace(body.Hostname))
	if err := caddy.ValidateHostname(host); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}

	// Rate limit before any expensive work; every attempt counts,
	// successful or not.
	now := time.Now().UTC()
	count, err := s.store.CountDomainAdds(r.Context(), host, now.Add(-time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if count >= DomainAddLimit {
		writeError(w, http.StatusTooManyRequests, "E_DOMAIN_RATE_LIMITED",
			fmt.Sprintf("hostname %q was attached %d times in the last hour; "+
				"wait for the window to pass to avoid certificate rate limits", host, count))
		return
	}
	if err := s.store.RecordDomainAddAttempt(r.Context(), host, now); err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	d, created, err := s.store.AddDomain(r.Context(), app.ID, host)
	if errors.Is(err, store.ErrDomainTaken) {
		writeError(w, http.StatusConflict, "E_DOMAIN_TAKEN",
			fmt.Sprintf("hostname %q belongs to another app; hostnames are globally unique", host))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	// Already active: nothing to re-check, return as-is.
	if !created && d.Status == store.DomainActive {
		writeJSON(w, http.StatusOK, domainToDTO(d, nil))
		return
	}

	var warnings []string
	if body.SkipDNSCheck {
		// The user vouches for the DNS state; routing still requires a
		// succeeded deploy.
		if err := s.store.SetDomainStatus(r.Context(), d.ID, store.DomainActive); err != nil {
			writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
			return
		}
		d.Status = store.DomainActive
		warnings = append(warnings,
			"DNS check skipped: certificates are only issued once records point at this host")
	} else {
		match, err := s.hostPointsHere(r.Context(), host)
		switch {
		case err != nil:
			// Resolution failure is not an error: the domain sits in
			// pending_dns and the CLI tells the user what to fix.
			warnings = append(warnings,
				fmt.Sprintf("DNS lookup failed (%v); domain stays pending_dns and is not routed", err))
		case !match:
			warnings = append(warnings,
				"no A/AAAA record points at this host; domain stays pending_dns and is not routed")
		default:
			if err := s.store.SetDomainStatus(r.Context(), d.ID, store.DomainActive); err != nil {
				writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
				return
			}
			d.Status = store.DomainActive
		}
	}
	writeJSON(w, http.StatusCreated, domainToDTO(d, warnings))
}

// handleListDomains returns an app's domains.
func (s *server) handleListDomains(w http.ResponseWriter, r *http.Request) {
	app, err := s.store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	domains, err := s.store.ListDomains(r.Context(), app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	out := make([]domainDTO, 0, len(domains))
	for _, d := range domains {
		out = append(out, domainToDTO(d, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": out})
}

// hostPointsHere reports whether any of the hostname's resolved
// addresses is a public address of this host. The installer-recorded
// list wins; without it the outbound address is auto-detected.
func (s *server) hostPointsHere(ctx context.Context, hostname string) (bool, error) {
	addrs, err := s.resolve(ctx, hostname)
	if err != nil {
		return false, err
	}
	ours, err := s.publicIPs(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() {
			continue
		}
		for _, o := range ours {
			if a.Equal(o) {
				return true, nil
			}
		}
	}
	return false, nil
}

// publicIPs returns this host's public addresses: the settings value
// written at install time, or the outbound address as a fallback.
func (s *server) publicIPs(ctx context.Context) ([]net.IP, error) {
	if raw, err := s.store.GetSetting(ctx, PublicIPsSetting); err == nil && raw != "" {
		var out []net.IP
		for _, part := range strings.Split(raw, ",") {
			if ip := net.ParseIP(strings.TrimSpace(part)); ip != nil {
				out = append(out, ip)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	// UDP dial sends no packets; it only asks the kernel which source
	// address would reach the internet.
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return nil, fmt.Errorf("detect outbound address: %w", err)
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return nil, fmt.Errorf("parse outbound address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("outbound address %q is not an IP", host)
	}
	return []net.IP{ip}, nil
}

// jsonDecoder builds a strict decoder for a bounded request body:
// unknown fields are rejected and the body cannot exceed a small cap.
func jsonDecoder(r *http.Request) *json.Decoder {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8192))
	dec.DisallowUnknownFields()
	return dec
}

// readJSONBody decodes a strict JSON body into out, rejecting unknown
// fields and oversized payloads.
func readJSONBody(r *http.Request, out any) error {
	if err := jsonDecoder(r).Decode(out); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
