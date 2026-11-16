package store

// Notification queue: rows are appended by producers that must never
// block (deploy, stats, backup) and consumed by the worker's dispatcher.
// The queue is durable so a webhook outage or a worker restart loses no
// event; retry state lives on the row itself.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DedupWindow collapses repeats of the same dedup_key into the row
// already queued (or recently delivered) instead of enqueueing again.
// Producers that need a longer silence (certificate warnings fire
// daily) pass their own window at enqueue time.
const DedupWindow = 300 * time.Second

// RetrySchedule is the delay before each retry attempt. Index0 is the
// first (immediate) delivery; after the last delay is exhausted the
// event is marked failed.
var RetrySchedule = []time.Duration{
	0,
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
}

// NotifyEvent is one queued notification.
type NotifyEvent struct {
	ID            int64
	Type          string
	AppName       string
	Title         string
	Detail        string // JSON object
	DedupKey      string
	DupCount      int
	Status        string // pending | delivered | failed
	Attempts      int
	SentTo        []string // sink kinds that already accepted this event
	NextAttemptAt time.Time
	DeliveredAt   *time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NotifyEventInput is the producer-side enqueue shape.
type NotifyEventInput struct {
	Type     string
	AppName  string
	Title    string
	Detail   map[string]any
	DedupKey string
	// DedupWindow overrides DedupWindow when > 0; a zero DedupKey
	// never dedups regardless of the window.
	DedupWindow time.Duration
}

// nextAttempt returns the time of attempt number n (1-based) relative
// to base, or the zero time once the schedule is exhausted.
func nextAttempt(base time.Time, n int) time.Time {
	if n < 1 || n > len(RetrySchedule) {
		return time.Time{}
	}
	return base.Add(RetrySchedule[n-1])
}

// EnqueueNotifyEvent appends one event. A repeat of the same dedup_key
// inside the window bumps the existing row's dup_count instead of
// queueing a second delivery — the receiver sees one message carrying
// the repeat count. Returns the row id (existing row on dedup).
func (s *Store) EnqueueNotifyEvent(ctx context.Context, in NotifyEventInput) (int64, error) {
	if in.Type == "" {
		return 0, errors.New("store: notify event type is required")
	}
	detail := "{}"
	if len(in.Detail) > 0 {
		b, err := json.Marshal(in.Detail)
		if err != nil {
			return 0, fmt.Errorf("store: notify detail: %w", err)
		}
		detail = string(b)
	}
	now := time.Now().UTC()
	if in.DedupKey != "" {
		window := in.DedupWindow
		if window <= 0 {
			window = DedupWindow
		}
		var id int64
		err := s.db.QueryRowContext(ctx, `
			UPDATE notify_events SET dup_count = dup_count + 1, updated_at = ?
			WHERE id = (
				SELECT id FROM notify_events
				WHERE dedup_key = ? AND created_at >= ?
				ORDER BY id DESC LIMIT 1)
			RETURNING id`,
			now.UnixMilli(), in.DedupKey,
			now.Add(-window).UnixMilli()).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("store: dedup notify event: %w", err)
		}
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO notify_events
			(type, app_name, title, detail, dedup_key, status,
			 next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		in.Type, in.AppName, in.Title, detail, in.DedupKey,
		now.UnixMilli(), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: enqueue notify event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: enqueue notify event: %w", err)
	}
	return id, nil
}

// DueNotifyEvents returns pending events whose next attempt is due,
// oldest first, limited to n.
func (s *Store) DueNotifyEvents(ctx context.Context, now time.Time, n int) ([]*NotifyEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, app_name, title, detail, dedup_key, dup_count,
		       status, attempts, sent_to, next_attempt_at, delivered_at,
		       last_error, created_at, updated_at
		FROM notify_events
		WHERE status = 'pending' AND next_attempt_at <= ?
		ORDER BY next_attempt_at, id LIMIT ?`,
		now.UnixMilli(), n)
	if err != nil {
		return nil, fmt.Errorf("store: due notify events: %w", err)
	}
	defer rows.Close()
	return scanNotifyEvents(rows)
}

// ListNotifyEvents returns the most recent events, newest first.
func (s *Store) ListNotifyEvents(ctx context.Context, n int) ([]*NotifyEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, app_name, title, detail, dedup_key, dup_count,
		       status, attempts, sent_to, next_attempt_at, delivered_at,
		       last_error, created_at, updated_at
		FROM notify_events
		ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("store: list notify events: %w", err)
	}
	defer rows.Close()
	return scanNotifyEvents(rows)
}

// GetNotifyEvent fetches one event by id (sql.ErrNoRows when absent).
func (s *Store) GetNotifyEvent(ctx context.Context, id int64) (*NotifyEvent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, type, app_name, title, detail, dedup_key, dup_count,
		       status, attempts, sent_to, next_attempt_at, delivered_at,
		       last_error, created_at, updated_at
		FROM notify_events WHERE id = ?`, id)
	ev, err := scanNotifyEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get notify event: %w", err)
	}
	return ev, nil
}

// MarkNotifyDelivered records a successful attempt: the listed sinks
// accepted the event, so retries skip them.
func (s *Store) MarkNotifyDelivered(ctx context.Context, id int64, sentTo []string) error {
	now := time.Now().UTC()
	sent, err := json.Marshal(sentTo)
	if err != nil {
		return fmt.Errorf("store: mark notify delivered: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE notify_events SET
			status = 'delivered', attempts = attempts + 1,
			sent_to = ?, delivered_at = ?, last_attempt_at = ?,
			last_error = '', updated_at = ?
		WHERE id = ?`,
		string(sent), now.UnixMilli(), now.UnixMilli(),
		now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: mark notify delivered: %w", err)
	}
	return nil
}

// NotifyRetry records a failed attempt: sentTo keeps the sinks that
// already accepted (partial progress across sinks), the next attempt
// follows the retry schedule, and exhausting the schedule marks the
// event failed.
func (s *Store) NotifyRetry(ctx context.Context, id int64, sentTo []string, cause string) error {
	now := time.Now().UTC()
	ev, err := s.GetNotifyEvent(ctx, id)
	if err != nil {
		return err
	}
	sent, err := json.Marshal(sentTo)
	if err != nil {
		return fmt.Errorf("store: notify retry: %w", err)
	}
	attempts := ev.Attempts + 1
	next := nextAttempt(now, attempts+1)
	status := "pending"
	if next.IsZero() {
		status = "failed"
		next = now
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE notify_events SET
			attempts = ?, sent_to = ?, status = ?,
			next_attempt_at = ?, last_attempt_at = ?,
			last_error = ?, updated_at = ?
		WHERE id = ?`,
		attempts, string(sent), status,
		next.UnixMilli(), now.UnixMilli(), cause, now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("store: notify retry: %w", err)
	}
	return nil
}

// CountNotifyDeliveredSince counts events delivered at or after t —
// the global hourly send cap the dispatcher enforces by deferring.
func (s *Store) CountNotifyDeliveredSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notify_events WHERE delivered_at >= ?`,
		t.UnixMilli()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count delivered notify events: %w", err)
	}
	return n, nil
}

// PruneNotifyEvents drops delivered and failed rows older than before.
func (s *Store) PruneNotifyEvents(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM notify_events
		WHERE status != 'pending' AND updated_at < ?`,
		before.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: prune notify events: %w", err)
	}
	return nil
}

func scanNotifyEvents(rows *sql.Rows) ([]*NotifyEvent, error) {
	var out []*NotifyEvent
	for rows.Next() {
		ev, err := scanNotifyEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func scanNotifyEvent(row interface{ Scan(...any) error }) (*NotifyEvent, error) {
	var (
		ev            NotifyEvent
		detail, sent  string
		nextAt, cread int64
		updated       int64
		deliveredAt   sql.NullInt64
	)
	if err := row.Scan(&ev.ID, &ev.Type, &ev.AppName, &ev.Title, &detail,
		&ev.DedupKey, &ev.DupCount, &ev.Status, &ev.Attempts, &sent,
		&nextAt, &deliveredAt, &ev.LastError, &cread, &updated); err != nil {
		return nil, err
	}
	ev.Detail = detail
	if sent != "" {
		_ = json.Unmarshal([]byte(sent), &ev.SentTo)
	}
	ev.NextAttemptAt = time.UnixMilli(nextAt).UTC()
	ev.CreatedAt = time.UnixMilli(cread).UTC()
	ev.UpdatedAt = time.UnixMilli(updated).UTC()
	if deliveredAt.Valid {
		t := time.UnixMilli(deliveredAt.Int64).UTC()
		ev.DeliveredAt = &t
	}
	return &ev, nil
}
