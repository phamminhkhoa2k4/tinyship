package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshot proves Snapshot produces a consistent, reopenable copy of
// a live database (the backup path depends on this).
func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	ctx := context.Background()
	s, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO settings(key,value,updated_at) VALUES('k','v',0)"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "snap.db")
	if err := s.Snapshot(ctx, dst); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := s.Snapshot(ctx, dst); err == nil {
		t.Error("second Snapshot to the same destination must fail (the file exists)")
	}
	s.Close()

	// The snapshot must open as a real TinyShip DB with the data present.
	s2, err := Open(ctx, dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer s2.Close()
	var v string
	if err := s2.db.QueryRowContext(ctx,
		"SELECT value FROM settings WHERE key='k'").Scan(&v); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if v != "v" {
		t.Errorf("value = %q, want v", v)
	}
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
}
