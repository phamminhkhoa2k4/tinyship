package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tinyship.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func runWorker(t *testing.T, s *store.Store, cfg Config) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg.DBPath = "test"
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := RunStore(ctx, s, cfg); err != nil {
			t.Errorf("RunStore: %v", err)
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop within 5s")
		}
	}
}

func waitJobState(t *testing.T, s *store.Store, id string, want store.JobState) *store.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, err := s.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if j.State == want {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	j, _ := s.GetJob(context.Background(), id)
	t.Fatalf("job %s state = %s, want %s (last_error=%v)", id, j.State, want, j.LastError)
	return nil
}

func TestWorkerRunsHandlerAndSucceeds(t *testing.T) {
	s := testStore(t)
	ran := make(chan struct{}, 1)
	stop := runWorker(t, s, Config{
		ClaimInterval: 10 * time.Millisecond,
		Handlers: map[string]Handler{
			"gc": func(ctx context.Context, job *store.Job) error {
				ran <- struct{}{}
				return nil
			},
		},
	})
	defer stop()

	job, err := s.CreateJob(context.Background(), store.CreateJobInput{Type: "gc", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitJobState(t, s, job.ID, store.JobSucceeded)

	select {
	case <-ran:
	default:
		t.Error("handler never ran")
	}
}

func TestWorkerNoHandlerFailsPermanently(t *testing.T) {
	s := testStore(t)
	stop := runWorker(t, s, Config{ClaimInterval: 10 * time.Millisecond})
	defer stop()

	job, err := s.CreateJob(context.Background(), store.CreateJobInput{Type: "gc", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := waitJobState(t, s, job.ID, store.JobFailed)
	if got.LastError == nil || !contains(*got.LastError, "E_NO_HANDLER") {
		t.Errorf("last_error = %v, want E_NO_HANDLER", got.LastError)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no-retry failure)", got.Attempts)
	}
}

func TestWorkerRetriesRetryableThenSucceeds(t *testing.T) {
	s := testStore(t)
	attempts := 0
	stop := runWorker(t, s, Config{
		ClaimInterval: 10 * time.Millisecond,
		Lease:         2 * time.Second,
		BackoffBase:   10 * time.Millisecond,
		Handlers: map[string]Handler{
			"deploy": func(ctx context.Context, job *store.Job) error {
				attempts++
				if attempts < 3 {
					return &RetryableError{Err: errors.New("registry 503")}
				}
				return nil
			},
		},
	})
	defer stop()

	job, err := s.CreateJob(context.Background(), store.CreateJobInput{Type: "deploy", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitJobState(t, s, job.ID, store.JobSucceeded)
	got, _ := s.GetJob(context.Background(), job.ID)
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", got.Attempts)
	}
}

func TestWorkerPermanentErrorDoesNotRetry(t *testing.T) {
	s := testStore(t)
	calls := 0
	stop := runWorker(t, s, Config{
		ClaimInterval: 10 * time.Millisecond,
		Handlers: map[string]Handler{
			"deploy": func(ctx context.Context, job *store.Job) error {
				calls++
				return fmt.Errorf("E_VALIDATION: image not found")
			},
		},
	})
	defer stop()

	job, err := s.CreateJob(context.Background(), store.CreateJobInput{Type: "deploy", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := waitJobState(t, s, job.ID, store.JobFailed)
	if calls != 1 {
		t.Errorf("handler calls = %d, want 1", calls)
	}
	if got.LastError == nil || !contains(*got.LastError, "E_VALIDATION") {
		t.Errorf("last_error = %v", got.LastError)
	}
}

func TestWorkerExhaustsMaxAttempts(t *testing.T) {
	s := testStore(t)
	stop := runWorker(t, s, Config{
		ClaimInterval: 10 * time.Millisecond,
		Lease:         2 * time.Second,
		BackoffBase:   10 * time.Millisecond,
		Handlers: map[string]Handler{
			"gc": func(ctx context.Context, job *store.Job) error {
				return &RetryableError{Err: errors.New("network down")}
			},
		},
	})
	defer stop()

	job, err := s.CreateJob(context.Background(), store.CreateJobInput{
		Type: "gc", CreatedBy: "t", MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := waitJobState(t, s, job.ID, store.JobFailed)
	if got.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (stopped at max)", got.Attempts)
	}
	if got.LastError == nil || !contains(*got.LastError, "network down") {
		t.Errorf("last_error = %v", got.LastError)
	}
}

func TestWorkerHonorsCancelAtSafePoint(t *testing.T) {
	s := testStore(t)
	started := make(chan struct{})
	stop := runWorker(t, s, Config{
		ClaimInterval: 10 * time.Millisecond,
		Handlers: map[string]Handler{
			"deploy": func(ctx context.Context, job *store.Job) error {
				close(started)
				<-ctx.Done() // long step observes cancellation
				return ctx.Err()
			},
		},
	})
	defer stop()

	job, err := s.CreateJob(context.Background(), store.CreateJobInput{Type: "deploy", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	<-started
	if err := s.CancelJob(context.Background(), job.ID, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := waitJobState(t, s, job.ID, store.JobCancelled)
	if got.LastError == nil || !contains(*got.LastError, "cancelled") {
		t.Errorf("last_error = %v", got.LastError)
	}
}

// TestWorkerRecoversOrphanedJob simulates a crashed worker: a claimed job
// whose lease expires must be picked up again and completed.
func TestWorkerRecoversOrphanedJob(t *testing.T) {
	s := testStore(t)

	// "Crashed worker" claims the job and disappears (no heartbeat). A short
	// lease lets the live worker's reaper reclaim it quickly in the test.
	job, err := s.CreateJob(context.Background(), store.CreateJobInput{Type: "gc", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Claim(context.Background(), "crashed-worker", time.Now(), 50*time.Millisecond); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A live worker starts with a fast reaper and short backoff and completes
	// the recovered job.
	stop := runWorker(t, s, Config{
		ClaimInterval: 10 * time.Millisecond,
		ReapInterval:  20 * time.Millisecond,
		Lease:         2 * time.Second,
		BackoffBase:   10 * time.Millisecond,
		Handlers: map[string]Handler{
			"gc": func(ctx context.Context, job *store.Job) error { return nil },
		},
	})
	defer stop()

	got := waitJobState(t, s, job.ID, store.JobSucceeded)
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (crashed claim + recovery)", got.Attempts)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
