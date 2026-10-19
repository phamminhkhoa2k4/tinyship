package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Job lifecycle states. succeeded/failed/cancelled
// are terminal; every other transition must be rejected by the code below
// and covered by tests.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

// Terminal reports whether the state admits no further transitions.
func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// validTransitions is the authority for job state changes.
var validTransitions = map[JobState]map[JobState]bool{
	JobQueued: {JobRunning: true, JobCancelled: true},
	JobRunning: {
		JobSucceeded: true,
		JobFailed:    true,
		JobQueued:    true, // retryable error or lease expiry, via requeue
		JobCancelled: true, // cancel_requested observed at a safe point
	},
	JobSucceeded: {},
	JobFailed:    {},
	JobCancelled: {},
}

// CanTransition reports whether from → to is a legal job transition.
func CanTransition(from, to JobState) bool {
	return validTransitions[from][to]
}

// Lease and retry constants. Provisional until benchmarked; keep them
// here so tuning is a one-line change.
const (
	LeaseDuration    = 30 * time.Second
	HeartbeatEvery   = 10 * time.Second
	ReapEvery        = 10 * time.Second
	BackoffBase      = 5 * time.Second
	BackoffMax       = 300 * time.Second
	BackoffMaxJitter = 0.2 // ±20%
)

// ErrInvalidTransition is returned when a state change violates
// the valid-transitions table.
var ErrInvalidTransition = errors.New("store: invalid job state transition")

// ErrJobNotFound is returned when the target job does not exist.
var ErrJobNotFound = errors.New("store: job not found")

// ErrLeaseLost is returned when an owner tries to finish/requeue a job it no
// longer holds the lease for (reaper took it back).
var ErrLeaseLost = errors.New("store: lease lost")

// Job is a row of the jobs queue.
type Job struct {
	ID              string
	AppID           *string
	Type            string
	State           JobState
	PayloadJSON     string
	Attempts        int
	MaxAttempts     int
	LeaseOwner      *string
	LeaseUntil      *int64 // epoch ms UTC, nil when not running
	NextRunAt       int64  // epoch ms UTC
	CancelRequested bool
	LastError       *string
	IdempotencyKey  *string
	CreatedBy       string
	CreatedAt       int64
	UpdatedAt       int64
	FinishedAt      *int64
}

// Backoff returns the delay before the next attempt: min(base × 2^(n−1),
// BackoffMax) scaled by jitter ∈ [−0.2, +0.2]. attempts is the
// number of executions already made (1 after the first failure). base<=0
// uses BackoffBase (5s); tests inject a small base.
func Backoff(attempts int, jitter float64, base time.Duration) time.Duration {
	if base <= 0 {
		base = BackoffBase
	}
	if attempts < 1 {
		attempts = 1
	}
	d := base
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= BackoffMax {
			d = BackoffMax
			break
		}
	}
	if d > BackoffMax {
		d = BackoffMax
	}
	if jitter < -BackoffMaxJitter {
		jitter = -BackoffMaxJitter
	}
	if jitter > BackoffMaxJitter {
		jitter = BackoffMaxJitter
	}
	return time.Duration(float64(d) * (1 + jitter))
}

// CreateJobInput describes a new job.
type CreateJobInput struct {
	AppID          *string // nil for host-wide jobs (gc, reconcile)
	Type           string
	PayloadJSON    string
	MaxAttempts    int     // 0 → default 5
	IdempotencyKey *string // (app_id, key) unique; duplicates return the old job
	CreatedBy      string
	NextRunAt      time.Time // zero → now
}

// CreateJob inserts a job, or returns the existing job when an idempotent
// duplicate arrives. The unique index jobs_idem is the
// authority; this method handles the race by re-selecting on conflict.
func (s *Store) CreateJob(ctx context.Context, in CreateJobInput) (*Job, error) {
	var out *Job
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		j, err := createJob(ctx, tx, in)
		if err != nil {
			return err
		}
		out = j
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// createJob is CreateJob's body against one transaction, so a job can
// be committed together with rows it must never be visible without.
func createJob(ctx context.Context, tx *sql.Tx, in CreateJobInput) (*Job, error) {
	if in.Type == "" {
		return nil, errors.New("store: job type is required")
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = 5
	}
	now := time.Now().UTC().UnixMilli()
	next := now
	if !in.NextRunAt.IsZero() {
		next = in.NextRunAt.UTC().UnixMilli()
	}
	id := NewID()

	var out Job
	if in.IdempotencyKey != nil && *in.IdempotencyKey != "" {
		existing, err := findIdempotent(ctx, tx, in.AppID, *in.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}
	var appID any
	if in.AppID != nil {
		appID = *in.AppID
	}
	var idemKey any
	if in.IdempotencyKey != nil && *in.IdempotencyKey != "" {
		idemKey = *in.IdempotencyKey
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO jobs(id, app_id, type, state, payload_json, attempts, max_attempts,
                 next_run_at, idempotency_key, created_by, created_at, updated_at)
VALUES(?,?,?,'queued',?,0,?,?,?,?,?,?)`,
		id, appID, in.Type, in.PayloadJSON, in.MaxAttempts,
		next, idemKey, in.CreatedBy, now, now)
	if err != nil {
		return nil, fmt.Errorf("store: insert job: %w", err)
	}
	if err := scanJob(ctx, tx.QueryRowContext(ctx, jobSelect+" WHERE id = ?", id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func findIdempotent(ctx context.Context, tx *sql.Tx, appID *string, key string) (*Job, error) {
	var j Job
	var err error
	if appID == nil {
		err = scanJob(ctx, tx.QueryRowContext(ctx,
			jobSelect+" WHERE idempotency_key = ? AND app_id IS NULL", key), &j)
	} else {
		err = scanJob(ctx, tx.QueryRowContext(ctx,
			jobSelect+" WHERE idempotency_key = ? AND app_id = ?", key, *appID), &j)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: lookup idempotency key: %w", err)
	}
	return &j, nil
}

const jobSelect = `SELECT id, app_id, type, state, payload_json, attempts,
  max_attempts, lease_owner, lease_until, next_run_at, cancel_requested,
  last_error, idempotency_key, created_by, created_at, updated_at, finished_at
  FROM jobs`

func scanJob(ctx context.Context, row *sql.Row, j *Job) error {
	var appID, leaseOwner, lastError, idemKey sql.NullString
	var state string
	var cancelReq int
	var leaseUntil, finishedAt sql.NullInt64
	var nextRunAt, createdAt, updatedAt int64
	err := row.Scan(&j.ID, &appID, &j.Type, &state, &j.PayloadJSON,
		&j.Attempts, &j.MaxAttempts, &leaseOwner, &leaseUntil, &nextRunAt,
		&cancelReq, &lastError, &idemKey, &j.CreatedBy, &createdAt, &updatedAt,
		&finishedAt)
	if err != nil {
		return err
	}
	j.State = JobState(state)
	j.NextRunAt, j.CreatedAt, j.UpdatedAt = nextRunAt, createdAt, updatedAt
	j.CancelRequested = cancelReq != 0
	if appID.Valid {
		j.AppID = &appID.String
	}
	if leaseOwner.Valid {
		j.LeaseOwner = &leaseOwner.String
	}
	if leaseUntil.Valid {
		v := leaseUntil.Int64
		j.LeaseUntil = &v
	}
	if lastError.Valid {
		j.LastError = &lastError.String
	}
	if idemKey.Valid {
		j.IdempotencyKey = &idemKey.String
	}
	if finishedAt.Valid {
		v := finishedAt.Int64
		j.FinishedAt = &v
	}
	return nil
}

// GetJob fetches one job by id.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	var j Job
	err := scanJob(ctx, s.db.QueryRowContext(ctx, jobSelect+" WHERE id = ?", id), &j)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job %s: %w", id, err)
	}
	return &j, nil
}

// Claim atomically takes the next runnable job for owner. Returns
// (nil, nil) when nothing is claimable. lease<=0 uses
// LeaseDuration. The per-app exclusion in the WHERE clause plus
// jobs_one_running_per_app make it impossible to run two jobs of one app
// concurrently, even if this query were wrong.
func (s *Store) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration) (*Job, error) {
	if lease <= 0 {
		lease = LeaseDuration
	}
	ms := now.UTC().UnixMilli()
	var claimed *Job
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `
SELECT id FROM jobs j
WHERE j.state = 'queued' AND j.next_run_at <= ?
  AND (j.app_id IS NULL OR NOT EXISTS (
        SELECT 1 FROM jobs r WHERE r.app_id = j.app_id AND r.state = 'running'))
ORDER BY j.next_run_at, j.created_at LIMIT 1`, ms).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // nothing to claim
		}
		if err != nil {
			return fmt.Errorf("store: select claimable: %w", err)
		}

		leaseUntil := ms + lease.Milliseconds()
		res, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state='running', lease_owner=?, lease_until=?, attempts=attempts+1, updated_at=?
WHERE id=? AND state='queued'`,
			owner, leaseUntil, ms, id)
		if err != nil {
			return fmt.Errorf("store: claim update: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil // lost the race; caller retries on next tick
		}
		var j Job
		if err := scanJob(ctx, tx.QueryRowContext(ctx, jobSelect+" WHERE id = ?", id), &j); err != nil {
			return fmt.Errorf("store: read claimed job: %w", err)
		}
		claimed = &j
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// Heartbeat extends the lease of a running job owned by owner.
// Returns false when the lease was already lost (reaper reclaimed it).
// lease<=0 uses LeaseDuration.
func (s *Store) Heartbeat(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) (bool, error) {
	if lease <= 0 {
		lease = LeaseDuration
	}
	until := now.UTC().UnixMilli() + lease.Milliseconds()
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET lease_until=?, updated_at=?
WHERE id=? AND state='running' AND lease_owner=?`,
		until, now.UTC().UnixMilli(), jobID, owner)
	if err != nil {
		return false, fmt.Errorf("store: heartbeat: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetJobRunning keeps a claimed job running (no-op transition guard used by
// the worker to assert it still owns the job before a long step).
func (s *Store) AssertOwned(ctx context.Context, jobID, owner string) error {
	var state, leaseOwner string
	err := s.db.QueryRowContext(ctx,
		"SELECT state, COALESCE(lease_owner,'') FROM jobs WHERE id=?", jobID).
		Scan(&state, &leaseOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrJobNotFound
	}
	if err != nil {
		return err
	}
	if JobState(state) != JobRunning || leaseOwner != owner {
		return ErrLeaseLost
	}
	return nil
}

// FinishJob transitions running → succeeded, failed, or cancelled;
// last_error must be clear and secret-free — callers are
// responsible for the content. Terminal states reject further changes.
func (s *Store) FinishJob(ctx context.Context, jobID, owner string, to JobState, lastError string, now time.Time) error {
	if to != JobSucceeded && to != JobFailed && to != JobCancelled {
		return fmt.Errorf("%w: finish may only go to succeeded/failed/cancelled, not %s", ErrInvalidTransition, to)
	}
	ms := now.UTC().UnixMilli()
	var le any
	if lastError != "" {
		le = lastError
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state=?, last_error=?, lease_owner=NULL, lease_until=NULL,
  finished_at=?, updated_at=?
WHERE id=? AND state='running' AND lease_owner=?`,
		to, le, ms, ms, jobID, owner)
	if err != nil {
		return fmt.Errorf("store: finish job: %w", err)
	}
	n, _ := res.RowsAffected()
	switch {
	case n == 1:
		return nil
	default:
		return s.transitionError(ctx, jobID, owner, to)
	}
}

// RequeueJob transitions running → queued for retry with backoff.
// The caller computes the delay; jitter is applied here. base<=0 uses
// BackoffBase.
func (s *Store) RequeueJob(ctx context.Context, jobID, owner string, lastError string, attempts int, now time.Time, base time.Duration) error {
	delay := Backoff(attempts, rand.Float64()*2*BackoffMaxJitter-BackoffMaxJitter, base)
	next := now.UTC().Add(delay).UnixMilli()
	ms := now.UTC().UnixMilli()
	var le any
	if lastError != "" {
		le = lastError
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state='queued', last_error=?, lease_owner=NULL, lease_until=NULL,
  next_run_at=?, updated_at=?
WHERE id=? AND state='running' AND lease_owner=?`,
		le, next, ms, jobID, owner)
	if err != nil {
		return fmt.Errorf("store: requeue job: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return nil
	}
	return s.transitionError(ctx, jobID, owner, JobQueued)
}

// CancelJob cancels a queued job immediately. A running job is
// marked cancel_requested; the worker stops at the next safe point (AC2).
// Terminal jobs are rejected.
func (s *Store) CancelJob(ctx context.Context, jobID string, now time.Time) error {
	ms := now.UTC().UnixMilli()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, "SELECT state FROM jobs WHERE id=?", jobID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		if err != nil {
			return err
		}
		switch JobState(state) {
		case JobQueued:
			_, err = tx.ExecContext(ctx, `
UPDATE jobs SET state='cancelled', cancel_requested=1, finished_at=?, updated_at=?
WHERE id=? AND state='queued'`, ms, ms, jobID)
		case JobRunning:
			_, err = tx.ExecContext(ctx, `
UPDATE jobs SET cancel_requested=1, updated_at=?
WHERE id=? AND state='running'`, ms, jobID)
		default:
			return fmt.Errorf("%w: job is %s", ErrInvalidTransition, state)
		}
		if err != nil {
			return fmt.Errorf("store: cancel job: %w", err)
		}
		return nil
	})
}

// CancelRequested reports whether the worker should stop at the next safe
// point.
func (s *Store) CancelRequested(ctx context.Context, jobID string) (bool, error) {
	var req int
	err := s.db.QueryRowContext(ctx,
		"SELECT cancel_requested FROM jobs WHERE id=?", jobID).Scan(&req)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrJobNotFound
	}
	if err != nil {
		return false, err
	}
	return req != 0, nil
}

// ReapExpired reclaims jobs whose lease expired: back to queued
// with backoff, or failed with E_LEASE_EXPIRED when out of attempts.
// Returns the affected job ids per outcome.
func (s *Store) ReapExpired(ctx context.Context, now time.Time) (requeued, failed []string, err error) {
	ms := now.UTC().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
SELECT id, attempts, max_attempts FROM jobs
WHERE state='running' AND lease_until IS NOT NULL AND lease_until < ?`, ms)
	if err != nil {
		return nil, nil, fmt.Errorf("store: select expired: %w", err)
	}
	type expired struct {
		id          string
		attempts    int
		maxAttempts int
	}
	var list []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.attempts, &e.maxAttempts); err != nil {
			rows.Close()
			return nil, nil, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	for _, e := range list {
		if e.attempts >= e.maxAttempts {
			_, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state='failed', last_error='E_LEASE_EXPIRED: lease expired and no attempts left',
  lease_owner=NULL, lease_until=NULL, finished_at=?, updated_at=?
WHERE id=? AND state='running'`, ms, ms, e.id)
			if err != nil {
				return requeued, failed, fmt.Errorf("store: reap fail %s: %w", e.id, err)
			}
			failed = append(failed, e.id)
			continue
		}
		// The old owner's lease is gone: clearing lease_owner here both frees
		// the per-app lock and makes any stale FinishJob hit ErrLeaseLost.
		next := now.Add(Backoff(e.attempts, rand.Float64()*2*BackoffMaxJitter-BackoffMaxJitter, 0)).UnixMilli()
		_, err := s.db.ExecContext(ctx, `
UPDATE jobs SET state='queued', lease_owner=NULL, lease_until=NULL,
  next_run_at=?, updated_at=?
WHERE id=? AND state='running'`, next, ms, e.id)
		if err != nil {
			return requeued, failed, fmt.Errorf("store: reap requeue %s: %w", e.id, err)
		}
		requeued = append(requeued, e.id)
	}
	return requeued, failed, nil
}

// transitionError distinguishes "job vanished", "lease lost", and "illegal
// transition" for callers of FinishJob/RequeueJob.
func (s *Store) transitionError(ctx context.Context, jobID, owner string, to JobState) error {
	var state, leaseOwner sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT state, lease_owner FROM jobs WHERE id=?", jobID).Scan(&state, &leaseOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrJobNotFound
	}
	if err != nil {
		return err
	}
	if JobState(state.String) != JobRunning {
		return fmt.Errorf("%w: %s is %s, not running", ErrInvalidTransition, jobID, state.String)
	}
	if leaseOwner.String != owner {
		return fmt.Errorf("%w: job %s is owned by %q", ErrLeaseLost, jobID, leaseOwner.String)
	}
	return fmt.Errorf("%w: running → %s", ErrInvalidTransition, to)
}
