package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Domain attach statuses.
const (
	DomainPendingDNS = "pending_dns"
	DomainActive     = "active"
	DomainError      = "error"
)

// ErrDomainTaken is returned when a hostname already belongs to another
// app. Hostnames are globally unique: two apps claiming one name would
// make routing ambiguous.
var ErrDomainTaken = errors.New("E_DOMAIN_TAKEN: hostname belongs to another app")

// Domain is a row of the domains table.
type Domain struct {
	ID        string
	AppID     string
	Hostname  string
	IsPrimary bool
	Status    string
	CreatedAt time.Time
}

// RouteRow is one active hostname joined with the upstream host port of
// its app's current deployment. Rows without a routable upstream (no
// current deployment, or a deployment that never got a port) are
// excluded by the join: one broken app must not break everyone's routes.
type RouteRow struct {
	DomainID string
	AppID    string
	Hostname string
	HostPort int
}

// AddDomain attaches a hostname to an app as pending_dns. Re-attaching a
// hostname the same app already owns is idempotent (it returns the
// existing row so a DNS re-check can promote it). A hostname owned by a
// different app fails with ErrDomainTaken.
func (s *Store) AddDomain(ctx context.Context, appID, hostname string) (*Domain, bool, error) {
	now := time.Now().UTC()
	id := NewIDAt(now)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO domains (id, app_id, hostname, status, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, appID, hostname, DomainPendingDNS, now.UnixMilli())
	if err != nil {
		if !strings.Contains(err.Error(), "UNIQUE constraint failed: domains.hostname") {
			return nil, false, fmt.Errorf("store: add domain: %w", err)
		}
		// Already present: same app means an idempotent re-attach; a
		// different app means the name is taken.
		existing, err := s.GetDomainByHost(ctx, hostname)
		if err != nil {
			return nil, false, fmt.Errorf("store: load existing domain: %w", err)
		}
		if existing.AppID != appID {
			return nil, false, ErrDomainTaken
		}
		return existing, false, nil
	}
	d, err := s.GetDomain(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return d, true, nil
}

// GetDomain fetches one domain by id (ErrNotFound when absent).
func (s *Store) GetDomain(ctx context.Context, id string) (*Domain, error) {
	d, err := s.scanDomain(s.db.QueryRowContext(ctx, `
		SELECT id, app_id, hostname, is_primary, status, created_at
		FROM domains WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get domain: %w", err)
	}
	return d, nil
}

// GetDomainByHost fetches one domain by hostname (ErrNotFound when
// absent).
func (s *Store) GetDomainByHost(ctx context.Context, hostname string) (*Domain, error) {
	d, err := s.scanDomain(s.db.QueryRowContext(ctx, `
		SELECT id, app_id, hostname, is_primary, status, created_at
		FROM domains WHERE hostname = ?`, hostname))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get domain by host: %w", err)
	}
	return d, nil
}

func (s *Store) scanDomain(row scannable) (*Domain, error) {
	var (
		d         Domain
		primary   int
		createdMS int64
	)
	if err := row.Scan(&d.ID, &d.AppID, &d.Hostname, &primary,
		&d.Status, &createdMS); err != nil {
		return nil, err
	}
	d.IsPrimary = primary != 0
	d.CreatedAt = time.UnixMilli(createdMS).UTC()
	return &d, nil
}

// DeleteDomain removes one hostname from an app (ErrNotFound when the
// app does not own that hostname). The caller regenerates proxy routes
// afterwards; this function only touches the row.
func (s *Store) DeleteDomain(ctx context.Context, appID, hostname string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM domains WHERE app_id = ? AND hostname = ?`, appID, hostname)
	if err != nil {
		return fmt.Errorf("store: delete domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDomainStatus moves a domain between pending_dns/active/error.
func (s *Store) SetDomainStatus(ctx context.Context, id, status string) error {
	switch status {
	case DomainPendingDNS, DomainActive, DomainError:
	default:
		return fmt.Errorf("store: unknown domain status %q", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE domains SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("store: set domain status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListDomains returns an app's domains ordered by primary first then
// hostname.
func (s *Store) ListDomains(ctx context.Context, appID string) ([]*Domain, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, app_id, hostname, is_primary, status, created_at
		FROM domains WHERE app_id = ?
		ORDER BY is_primary DESC, hostname`, appID)
	if err != nil {
		return nil, fmt.Errorf("store: list domains: %w", err)
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		var (
			d         Domain
			primary   int
			createdMS int64
		)
		if err := rows.Scan(&d.ID, &d.AppID, &d.Hostname, &primary,
			&d.Status, &createdMS); err != nil {
			return nil, err
		}
		d.IsPrimary = primary != 0
		d.CreatedAt = time.UnixMilli(createdMS).UTC()
		out = append(out, &d)
	}
	return out, rows.Err()
}

// RecordDomainAddAttempt stores one attach attempt for rate limiting and
// prunes attempts older than an hour so the table stays bounded.
func (s *Store) RecordDomainAddAttempt(ctx context.Context, hostname string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO domain_add_attempts (hostname, at) VALUES (?, ?)`,
		hostname, now.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: record domain add attempt: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM domain_add_attempts WHERE at < ?`,
		now.UTC().Add(-time.Hour).UnixMilli())
	if err != nil {
		return fmt.Errorf("store: prune domain add attempts: %w", err)
	}
	return nil
}

// CountDomainAdds returns attach attempts for a hostname since the given
// time (the rate-limit window).
func (s *Store) CountDomainAdds(ctx context.Context, hostname string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM domain_add_attempts WHERE hostname = ? AND at >= ?`,
		hostname, since.UTC().UnixMilli()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count domain add attempts: %w", err)
	}
	return n, nil
}

// ListRouteRows returns every routable active hostname (domain active ×
// app's current deployment's host port), sorted for deterministic config
// generation.
func (s *Store) ListRouteRows(ctx context.Context) ([]RouteRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.id, d.app_id, d.hostname, dep.host_port
		FROM domains d
		JOIN apps a ON a.id = d.app_id
		JOIN deployments dep ON dep.id = a.current_deployment_id
		WHERE d.status = 'active' AND dep.host_port IS NOT NULL
		ORDER BY d.hostname`)
	if err != nil {
		return nil, fmt.Errorf("store: list route rows: %w", err)
	}
	defer rows.Close()
	var out []RouteRow
	for rows.Next() {
		var r RouteRow
		if err := rows.Scan(&r.DomainID, &r.AppID, &r.Hostname, &r.HostPort); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetSetting reads one settings value (empty string when absent).
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get setting: %w", err)
	}
	return v, nil
}

// PutSetting writes one settings value.
func (s *Store) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value,
		                               updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: put setting: %w", err)
	}
	return nil
}
