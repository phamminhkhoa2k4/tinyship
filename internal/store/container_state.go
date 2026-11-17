package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ContainerState is the last resource sample kept for an app's container.
// It is the durable half of the stats view: live CPU is measured on
// request, while memory and OOM history survive worker restarts.
type ContainerState struct {
	AppID        string
	ContainerID  string
	State        string
	Health       string
	MemUsageMB   int
	OOMKillCount int
	UpdatedAt    time.Time

	// Hibernate counters: sleep/wake cycles observed for the app and
	// the wake-latency totals behind the stats view's average/max. The
	// flap window is the timestamp of the cycle burst's first sleep
	// plus how many sleeps it holds.
	HibernateSleepCount      int
	HibernateWakeCount       int
	HibernateWakeTotalMs     int64
	HibernateWakeMaxMs       int64
	HibernateFlapWindowStart int64
	HibernateFlapCount       int
}

// UpsertContainerState replaces the sample for one app. The caller decides
// how the OOM count accumulates; the store just persists the value.
func (s *Store) UpsertContainerState(ctx context.Context, in ContainerState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO container_state
			(app_id, container_id, state, health, mem_usage_mb, oom_kill_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id) DO UPDATE SET
			container_id = excluded.container_id,
			state = excluded.state,
			health = excluded.health,
			mem_usage_mb = excluded.mem_usage_mb,
			oom_kill_count = excluded.oom_kill_count,
			updated_at = excluded.updated_at`,
		in.AppID, in.ContainerID, in.State, in.Health, in.MemUsageMB,
		in.OOMKillCount, in.UpdatedAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: upsert container state: %w", err)
	}
	return nil
}

// GetContainerState returns the stored sample for one app, or ErrNotFound
// when the app has never been sampled.
func (s *Store) GetContainerState(ctx context.Context, appID string) (*ContainerState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT app_id, container_id, state, health, mem_usage_mb,
		       oom_kill_count, updated_at,
		       hibernate_sleep_count, hibernate_wake_count,
		       hibernate_wake_total_ms, hibernate_wake_max_ms,
		       hibernate_flap_window_start, hibernate_flap_count
		FROM container_state WHERE app_id = ?`, appID)
	return scanContainerState(row)
}

// ListContainerStates returns every stored sample keyed by app id.
func (s *Store) ListContainerStates(ctx context.Context) (map[string]*ContainerState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT app_id, container_id, state, health, mem_usage_mb,
		       oom_kill_count, updated_at,
		       hibernate_sleep_count, hibernate_wake_count,
		       hibernate_wake_total_ms, hibernate_wake_max_ms,
		       hibernate_flap_window_start, hibernate_flap_count
		FROM container_state`)
	if err != nil {
		return nil, fmt.Errorf("store: list container state: %w", err)
	}
	defer rows.Close()

	out := map[string]*ContainerState{}
	for rows.Next() {
		st, err := scanContainerState(rows)
		if err != nil {
			return nil, err
		}
		out[st.AppID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list container state: %w", err)
	}
	return out, nil
}

type containerStateScanner interface {
	Scan(dest ...any) error
}

func scanContainerState(row containerStateScanner) (*ContainerState, error) {
	var (
		st      ContainerState
		updated int64
	)
	err := row.Scan(&st.AppID, &st.ContainerID, &st.State, &st.Health,
		&st.MemUsageMB, &st.OOMKillCount, &updated,
		&st.HibernateSleepCount, &st.HibernateWakeCount,
		&st.HibernateWakeTotalMs, &st.HibernateWakeMaxMs,
		&st.HibernateFlapWindowStart, &st.HibernateFlapCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan container state: %w", err)
	}
	st.UpdatedAt = time.UnixMilli(updated).UTC()
	return &st, nil
}

// AppHasActiveJob reports whether the app has a queued or running job.
// The image collector uses it to stand back while a deploy may be
// pulling or swapping containers.
func (s *Store) AppHasActiveJob(ctx context.Context, appID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM jobs
		WHERE app_id = ? AND state IN ('queued','running')
		LIMIT 1`, appID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: active job lookup: %w", err)
	}
	return true, nil
}

// ensureHibernateRow creates the sample row an app has never had, so
// the counters can be bumped before the first stats tick stores one.
// The blank columns match what the sampler would have written.
func ensureHibernateRow(ctx context.Context, tx *sql.Tx, appID string, nowMS int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO container_state
			(app_id, container_id, state, health, mem_usage_mb, updated_at)
		VALUES (?, '', '', '', 0, ?)`, appID, nowMS)
	if err != nil {
		return fmt.Errorf("store: ensure hibernate row: %w", err)
	}
	return nil
}

// RecordHibernateSleep registers one sleep cycle: the counter grows
// and the flap window advances (a window older than flapWindow, or
// none yet, restarts). It returns the number of sleeps inside the
// current window so the caller can warn exactly once when a burst
// crosses the flapping threshold.
func (s *Store) RecordHibernateSleep(ctx context.Context, appID string, now time.Time, flapWindow time.Duration) (int, error) {
	ts := now.UTC().UnixMilli()
	windowMS := flapWindow.Milliseconds()
	var count int
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := ensureHibernateRow(ctx, tx, appID, ts); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE container_state SET
			  hibernate_sleep_count = hibernate_sleep_count + 1,
			  hibernate_flap_window_start = CASE
			    WHEN hibernate_flap_window_start = 0 OR ? - hibernate_flap_window_start > ?
			    THEN ? ELSE hibernate_flap_window_start END,
			  hibernate_flap_count = CASE
			    WHEN hibernate_flap_window_start = 0 OR ? - hibernate_flap_window_start > ?
			    THEN 1 ELSE hibernate_flap_count + 1 END,
			  updated_at = ?
			WHERE app_id = ?`,
			ts, windowMS, ts, ts, windowMS, ts, appID); err != nil {
			return fmt.Errorf("store: record hibernate sleep: %w", err)
		}
		return tx.QueryRowContext(ctx,
			`SELECT hibernate_flap_count FROM container_state WHERE app_id = ?`,
			appID).Scan(&count)
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// RecordHibernateWake registers one successful wake and its latency:
// count, running total (for the average), and maximum.
func (s *Store) RecordHibernateWake(ctx context.Context, appID string, duration time.Duration, now time.Time) error {
	ms := duration.Milliseconds()
	if ms < 0 {
		ms = 0
	} else if ms == 0 && duration > 0 {
		// A finished wake always took time. Truncating a sub-millisecond
		// one to 0 would make the stats view show a wake with no timing
		// at all; keep the coarsest honest value instead.
		ms = 1
	}
	ts := now.UTC().UnixMilli()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if err := ensureHibernateRow(ctx, tx, appID, ts); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE container_state SET
			  hibernate_wake_count = hibernate_wake_count + 1,
			  hibernate_wake_total_ms = hibernate_wake_total_ms + ?,
			  hibernate_wake_max_ms = MAX(hibernate_wake_max_ms, ?),
			  updated_at = ?
			WHERE app_id = ?`, ms, ms, ts, appID); err != nil {
			return fmt.Errorf("store: record hibernate wake: %w", err)
		}
		return nil
	})
}
