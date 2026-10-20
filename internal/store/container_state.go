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
		       oom_kill_count, updated_at
		FROM container_state WHERE app_id = ?`, appID)
	return scanContainerState(row)
}

// ListContainerStates returns every stored sample keyed by app id.
func (s *Store) ListContainerStates(ctx context.Context) (map[string]*ContainerState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT app_id, container_id, state, health, mem_usage_mb,
		       oom_kill_count, updated_at
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
		&st.MemUsageMB, &st.OOMKillCount, &updated)
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
