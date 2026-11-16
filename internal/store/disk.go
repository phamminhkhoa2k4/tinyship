package store

// Host disk time series: one row per hourly observation of the
// filesystem holding Docker's data. The right-size forecast fits a
// trend through these rows; the worker prunes old rows when it samples.

import (
	"context"
	"fmt"
	"time"
)

// DiskSample is one observation of the Docker data filesystem.
type DiskSample struct {
	At      time.Time
	FreeMB  int64
	TotalMB int64
}

// AppendDiskSample records one observation. The primary key makes
// re-sampling within the same second idempotent.
func (s *Store) AppendDiskSample(ctx context.Context, in DiskSample) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_disk_samples (ts, free_mb, total_mb) VALUES (?, ?, ?)
		ON CONFLICT(ts) DO UPDATE SET free_mb = excluded.free_mb,
		                              total_mb = excluded.total_mb`,
		in.At.UTC().Unix(), in.FreeMB, in.TotalMB)
	if err != nil {
		return fmt.Errorf("store: append disk sample: %w", err)
	}
	return nil
}

// ListDiskSamples returns every sample at or after since, oldest first.
func (s *Store) ListDiskSamples(ctx context.Context, since time.Time) ([]DiskSample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, free_mb, total_mb FROM host_disk_samples
		WHERE ts >= ? ORDER BY ts ASC`, since.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("store: list disk samples: %w", err)
	}
	defer rows.Close()

	var out []DiskSample
	for rows.Next() {
		var (
			ts int64
			d  DiskSample
		)
		if err := rows.Scan(&ts, &d.FreeMB, &d.TotalMB); err != nil {
			return nil, fmt.Errorf("store: scan disk sample: %w", err)
		}
		d.At = time.Unix(ts, 0).UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list disk samples: %w", err)
	}
	return out, nil
}

// PruneDiskSamples drops samples older than before.
func (s *Store) PruneDiskSamples(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM host_disk_samples WHERE ts < ?`, before.UTC().Unix()); err != nil {
		return fmt.Errorf("store: prune disk samples: %w", err)
	}
	return nil
}
