package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// v010Schema is the schema as shipped in v0.1.0: the metrics table
// arrives later, and an upgrade must carry old data across it.
const v010Schema = 2

// TestMigrateUpgradesV010SchemaWithData builds the v0.1.0 schema by
// hand, plants data in it, then opens the store the normal way: the new
// migration must apply on top, keep the old rows, and leave the new
// table usable.
func TestMigrateUpgradesV010SchemaWithData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tinyship.db")

	s, err := OpenPlain(path)
	if err != nil {
		t.Fatalf("open plain: %v", err)
	}
	defer s.Close()

	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}
	seen := 0
	for _, m := range migrations {
		if m.Version > v010Schema {
			break
		}
		if _, err := s.db.Exec(m.SQL); err != nil {
			t.Fatalf("apply old migration %04d: %v", m.Version, err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(?,?,?,?)`,
			m.Version, m.Name, m.Checksum, time.Now().UTC().UnixMilli()); err != nil {
			t.Fatalf("record old migration %04d: %v", m.Version, err)
		}
		seen++
	}
	if seen != v010Schema {
		t.Fatalf("applied %d old migrations, want %d", seen, v010Schema)
	}

	// A v0.1-shaped app with its last observation.
	appID := createApp(t, s, "legacy-app")
	if err := s.UpsertContainerState(ctx, ContainerState{
		AppID: appID, ContainerID: "cid-old", State: "running",
		MemUsageMB: 42, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("plant container state: %v", err)
	}

	// The normal open path: pending migrations apply on top.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate to current: %v", err)
	}
	v, err := s.AppliedSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("applied version: %v", err)
	}
	if v != SchemaVersion() {
		t.Errorf("applied schema = %d, want %d", v, SchemaVersion())
	}

	st, err := s.GetContainerState(ctx, appID)
	if err != nil {
		t.Fatalf("old data after upgrade: %v", err)
	}
	if st.MemUsageMB != 42 || st.State != "running" {
		t.Errorf("old row = %+v, want intact", st)
	}

	if err := s.AppendMetricSample(ctx, MetricSample{
		AppID: appID, At: time.Now().UTC(), MemBytes: 42 << 20,
	}); err != nil {
		t.Fatalf("new table after upgrade: %v", err)
	}
	samples, err := s.ListMetricSamples(ctx, appID, time.Time{})
	if err != nil || len(samples) != 1 {
		t.Errorf("new table read = %d rows, err %v", len(samples), err)
	}
}
