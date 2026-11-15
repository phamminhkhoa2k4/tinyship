package store

import (
	"context"
	"fmt"
	"time"
)

// MetricSample is one observed resource reading for an app. At is the
// observation time (hourly boundary for a rollup, Aggregated true).
type MetricSample struct {
	AppID         string
	At            time.Time
	Aggregated    bool
	CPUPercent    float64
	MemBytes      int64
	MemLimitBytes int64
}

// AppendMetricSample records one raw observation. A conflict on the
// (app_id, ts) key never overwrites a rollup row and otherwise keeps the
// stored row: a duplicate second holds the same data.
func (s *Store) AppendMetricSample(ctx context.Context, in MetricSample) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_metric_samples
			(app_id, ts, cpu_percent, mem_bytes, mem_limit_bytes)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(app_id, ts) DO UPDATE SET
			cpu_percent = excluded.cpu_percent,
			mem_bytes = excluded.mem_bytes,
			mem_limit_bytes = excluded.mem_limit_bytes
		WHERE app_metric_samples.aggregated = 0`,
		in.AppID, in.At.UTC().Unix(), in.CPUPercent, in.MemBytes, in.MemLimitBytes)
	if err != nil {
		return fmt.Errorf("store: append metric sample: %w", err)
	}
	return nil
}

// RollupMetricSamples folds every raw sample older than rawBefore into
// one hourly row per app and hour, then deletes those raw rows — in a
// single transaction, so a crash leaves either the raw rows or the
// rollup, never a half state. The peak memory survives the fold because
// right-sizing must never suggest below an observed peak.
func (s *Store) RollupMetricSamples(ctx context.Context, rawBefore time.Time) error {
	cutoff := rawBefore.UTC().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: rollup metric samples: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO app_metric_samples
			(app_id, ts, aggregated, cpu_percent, mem_bytes, mem_limit_bytes)
		SELECT app_id, (ts / 3600) * 3600, 1,
		       AVG(cpu_percent), MAX(mem_bytes), MAX(mem_limit_bytes)
		FROM app_metric_samples
		WHERE aggregated = 0 AND ts < ?
		GROUP BY app_id, ts / 3600
		ON CONFLICT(app_id, ts) DO UPDATE SET
			cpu_percent = excluded.cpu_percent,
			mem_bytes = MAX(app_metric_samples.mem_bytes, excluded.mem_bytes),
			mem_limit_bytes = MAX(app_metric_samples.mem_limit_bytes, excluded.mem_limit_bytes)`,
		cutoff); err != nil {
		return fmt.Errorf("store: rollup metric samples: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM app_metric_samples WHERE aggregated = 0 AND ts < ?`,
		cutoff); err != nil {
		return fmt.Errorf("store: rollup metric samples: %w", err)
	}
	return tx.Commit()
}

// PruneMetricSamples drops one app's rows (raw or rollup) older than
// before, enforcing that app's retention cap.
func (s *Store) PruneMetricSamples(ctx context.Context, appID string, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM app_metric_samples WHERE app_id = ? AND ts < ?`,
		appID, before.UTC().Unix())
	if err != nil {
		return fmt.Errorf("store: prune metric samples: %w", err)
	}
	return nil
}

// ListMetricSamples returns one app's samples at or after since, oldest
// first: raw rows and hourly rollups together.
func (s *Store) ListMetricSamples(ctx context.Context, appID string, since time.Time) ([]MetricSample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, aggregated, cpu_percent, mem_bytes, mem_limit_bytes
		FROM app_metric_samples
		WHERE app_id = ? AND ts >= ?
		ORDER BY ts ASC`, appID, since.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("store: list metric samples: %w", err)
	}
	defer rows.Close()

	var out []MetricSample
	for rows.Next() {
		var (
			m   MetricSample
			ts  int64
			agg int
		)
		if err := rows.Scan(&ts, &agg, &m.CPUPercent, &m.MemBytes, &m.MemLimitBytes); err != nil {
			return nil, fmt.Errorf("store: scan metric sample: %w", err)
		}
		m.AppID = appID
		m.At = time.Unix(ts, 0).UTC()
		m.Aggregated = agg != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list metric samples: %w", err)
	}
	return out, nil
}
