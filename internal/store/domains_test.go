package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// deployedApp creates an app whose current deployment holds a host port,
// so route-row queries have a routable upstream to join.
func deployedApp(t *testing.T, s *Store, name string, port int) string {
	t.Helper()
	ctx := context.Background()
	appID := createApp(t, s, name)
	d := newDeployment(t, s, appID)
	if _, err := s.db.Exec(
		`UPDATE deployments SET host_port = ? WHERE id = ?`, port, d.ID); err != nil {
		t.Fatalf("assign port: %v", err)
	}
	if err := s.SucceedDeployment(ctx, appID, d.ID); err != nil {
		t.Fatalf("succeed deployment: %v", err)
	}
	return appID
}

func TestAddDomainNewAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	d, created, err := s.AddDomain(ctx, appID, "blog.example.com")
	if err != nil || !created {
		t.Fatalf("AddDomain = %+v created=%v err=%v", d, created, err)
	}
	if d.Status != DomainPendingDNS {
		t.Errorf("initial status = %q, want pending_dns", d.Status)
	}

	same, created, err := s.AddDomain(ctx, appID, "blog.example.com")
	if err != nil || created {
		t.Fatalf("re-attach = %+v created=%v err=%v; want idempotent", same, created, err)
	}
	if same.ID != d.ID {
		t.Errorf("re-attach id = %q, want original %q", same.ID, d.ID)
	}
}

func TestAddDomainTakenByOtherApp(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	web := createApp(t, s, "web")
	api := createApp(t, s, "api")

	if _, _, err := s.AddDomain(ctx, web, "shared.example.com"); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	_, _, err := s.AddDomain(ctx, api, "shared.example.com")
	if !errors.Is(err, ErrDomainTaken) {
		t.Errorf("second attach = %v, want ErrDomainTaken", err)
	}
}

func TestSetDomainStatus(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	d, _, err := s.AddDomain(ctx, appID, "blog.example.com")
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if err := s.SetDomainStatus(ctx, d.ID, DomainActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, err := s.GetDomain(ctx, d.ID)
	if err != nil || got.Status != DomainActive {
		t.Errorf("after activate = %+v %v", got, err)
	}
	if err := s.SetDomainStatus(ctx, d.ID, "sideways"); err == nil {
		t.Error("unknown status accepted; want error")
	}
	if err := s.SetDomainStatus(ctx, "nope", DomainActive); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing domain = %v, want ErrNotFound", err)
	}
}

func TestListDomainsPrimaryFirst(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")
	now := time.Now().UTC().UnixMilli()
	for _, row := range []struct {
		id, host  string
		isPrimary int
	}{
		{NewID(), "b.example.com", 0},
		{NewID(), "a.example.com", 1},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO domains (id, app_id, hostname, is_primary, status, created_at)
			 VALUES (?, ?, ?, ?, 'active', ?)`,
			row.id, appID, row.host, row.isPrimary, now); err != nil {
			t.Fatalf("insert domain: %v", err)
		}
	}
	domains, err := s.ListDomains(ctx, appID)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) != 2 || domains[0].Hostname != "a.example.com" || !domains[0].IsPrimary {
		t.Errorf("order = %+v, want primary a.example.com first", domains)
	}
}

func TestDomainAddAttemptRateLimit(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		if err := s.RecordDomainAddAttempt(ctx, "blog.example.com", now); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	n, err := s.CountDomainAdds(ctx, "blog.example.com", now.Add(-time.Hour))
	if err != nil || n != 3 {
		t.Errorf("count = %d, %v; want 3", n, err)
	}
	// Other hostnames do not share the counter.
	if n, err := s.CountDomainAdds(ctx, "other.example.com", now.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("other hostname count = %d, %v; want 0", n, err)
	}
	// Attempts older than the window fall outside the count.
	old := now.Add(-2 * time.Hour)
	if err := s.RecordDomainAddAttempt(ctx, "stale.example.com", old); err != nil {
		t.Fatalf("record stale: %v", err)
	}
	if n, err := s.CountDomainAdds(ctx, "stale.example.com", now.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("stale count = %d, %v; want 0 (pruned)", n, err)
	}
}

func TestListRouteRows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	web := deployedApp(t, s, "web", 20017)
	plain := createApp(t, s, "plain") // no deployment at all

	if _, _, err := s.AddDomain(ctx, web, "blog.example.com"); err != nil {
		t.Fatalf("attach web: %v", err)
	}
	if _, err := s.GetDomain(ctx, ""); err == nil {
		t.Fatal("sanity: empty id lookup should fail")
	}
	blog, err := s.GetDomainByHost(ctx, "blog.example.com")
	if err != nil {
		t.Fatalf("GetDomainByHost: %v", err)
	}
	if err := s.SetDomainStatus(ctx, blog.ID, DomainActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	// A pending domain must not appear in routes.
	if _, _, err := s.AddDomain(ctx, web, "pending.example.com"); err != nil {
		t.Fatalf("attach pending: %v", err)
	}
	// An active domain on an app with no deployment is not routable.
	if _, _, err := s.AddDomain(ctx, plain, "dead.example.com"); err != nil {
		t.Fatalf("attach plain: %v", err)
	}
	plainDomain, err := s.GetDomainByHost(ctx, "dead.example.com")
	if err != nil {
		t.Fatalf("GetDomainByHost dead: %v", err)
	}
	if err := s.SetDomainStatus(ctx, plainDomain.ID, DomainActive); err != nil {
		t.Fatalf("activate dead: %v", err)
	}

	rows, err := s.ListRouteRows(ctx)
	if err != nil {
		t.Fatalf("ListRouteRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only blog.example.com", rows)
	}
	r := rows[0]
	if r.Hostname != "blog.example.com" || r.AppID != web || r.HostPort != 20017 || r.DomainID != blog.ID {
		t.Errorf("row = %+v", r)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if v, err := s.GetSetting(ctx, "caddy_config_hash"); err != nil || v != "" {
		t.Errorf("absent = %q, %v; want empty", v, err)
	}
	if err := s.PutSetting(ctx, "caddy_config_hash", "abc123"); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if v, err := s.GetSetting(ctx, "caddy_config_hash"); err != nil || v != "abc123" {
		t.Errorf("after put = %q, %v", v, err)
	}
	if err := s.PutSetting(ctx, "caddy_config_hash", "def456"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if v, err := s.GetSetting(ctx, "caddy_config_hash"); err != nil || v != "def456" {
		t.Errorf("after overwrite = %q, %v", v, err)
	}
}
