package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "tinyship.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// createApp inserts a minimal app row so jobs with app_id pass the foreign
// key (jobs.app_id REFERENCES apps(id)).
func createApp(t *testing.T, s *Store, name string) string {
	t.Helper()
	id := NewID()
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.Exec(`
INSERT INTO apps(id, name, config_json, config_version, desired_state,
                 deleting, created_at, updated_at)
VALUES(?,?,?,1,'running',0,?,?)`, id, name, "{}", now, now)
	if err != nil {
		t.Fatalf("create app %s: %v", name, err)
	}
	return id
}

// --- migrations -----------------------------------------------------

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tinyship.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Reopen: no pending migrations, must not fail or double-apply.
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var n int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != SchemaVersion() {
		t.Errorf("applied migrations = %d, want %d", n, SchemaVersion())
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// Simulate a future binary having migrated the DB.
	if _, err := s.db.Exec(
		"INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(9999,'future','x',0)"); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	err := s.Migrate(ctx)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Migrate() = %v, want ErrSchemaTooNew", err)
	}
}

func TestMigrateDetectsTamperedHistory(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.db.Exec(
		"UPDATE schema_migrations SET checksum='tampered' WHERE version=1"); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := s.Migrate(ctx)
	if err == nil || !contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Migrate() = %v, want checksum mismatch", err)
	}
}

// --- state machine ----------------------------------------------------------

func TestJobTransitionTable(t *testing.T) {
	// every legal and illegal transition.
	legal := map[[2]JobState]bool{
		{JobQueued, JobRunning}:    true, // claim
		{JobQueued, JobCancelled}:  true, // user cancels queued job
		{JobRunning, JobSucceeded}: true,
		{JobRunning, JobFailed}:    true,
		{JobRunning, JobQueued}:    true, // retry/lease expiry
		{JobRunning, JobCancelled}: true, // cancel_requested observed
	}
	states := []JobState{JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled}
	for _, from := range states {
		for _, to := range states {
			want := legal[[2]JobState{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s → %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestJobStateDirectUpdatesRejectedBySchema(t *testing.T) {
	// Even if code bypassed CanTransition, FinishJob on an unclaimed job
	// must be rejected.
	ctx := context.Background()
	s := openTest(t)
	j, _ := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t"})
	err := s.FinishJob(ctx, j.ID, "nobody", JobSucceeded, "", time.Now())
	if !errors.Is(err, ErrLeaseLost) && !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("finish without claim = %v, want ErrLeaseLost or ErrInvalidTransition", err)
	}
}

// --- atomic claim ---------------------------------------------------

func TestClaimSingleWorker(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	created, err := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	j, err := s.Claim(ctx, "worker-1", time.Now(), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if j == nil || j.ID != created.ID {
		t.Fatalf("claim = %+v, want job %s", j, created.ID)
	}
	if j.State != JobRunning || j.Attempts != 1 || j.LeaseOwner == nil || *j.LeaseOwner != "worker-1" {
		t.Errorf("claimed job wrong: state=%s attempts=%d owner=%v", j.State, j.Attempts, j.LeaseOwner)
	}

	// Per-app/per-type pool is empty now.
	none, err := s.Claim(ctx, "worker-2", time.Now(), 0)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if none != nil {
		t.Errorf("second claim = %+v, want nil", none)
	}
}

func TestClaimConcurrentOnlyOneWinner(t *testing.T) {
	// Many workers race for the same job, repeatedly.
	ctx := context.Background()
	s := openTest(t)

	for round := 0; round < 20; round++ {
		created, err := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t"})
		if err != nil {
			t.Fatalf("round %d create: %v", round, err)
		}

		const workers = 8
		type attempt struct {
			job *Job
			who string
		}
		results := make(chan attempt, workers)
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			go func(id int) {
				who := "w" + string(rune('a'+id))
				j, err := s.Claim(ctx, who, time.Now(), 0)
				if err != nil {
					errs <- err
					return
				}
				results <- attempt{j, who} // job is nil when the worker lost
			}(i)
		}
		winners := 0
		var winnerOwner string
		for i := 0; i < workers; i++ {
			select {
			case a := <-results:
				if a.job == nil {
					continue
				}
				winners++
				winnerOwner = a.who
				if a.job.ID != created.ID {
					t.Fatalf("round %d: winner holds %s, want %s", round, a.job.ID, created.ID)
				}
			case err := <-errs:
				t.Fatalf("round %d claim error: %v", round, err)
			case <-time.After(5 * time.Second):
				t.Fatalf("round %d: workers did not finish", round)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d winners, want exactly 1", round, winners)
		}
		// Release the per-app lock for the next round.
		if err := s.FinishJob(ctx, created.ID, winnerOwner, JobSucceeded, "", time.Now()); err != nil {
			t.Fatalf("round %d finish: %v", round, err)
		}
	}
}

// --- lease expiry recovery ------------------------------------------

func TestReapExpiredRequeuesOrFails(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	j, err := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t", MaxAttempts: 2})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now()
	if _, err := s.Claim(ctx, "dead", now, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// First expiry: back to queued with backoff.
	requeued, failed, err := s.ReapExpired(ctx, now.Add(LeaseDuration+time.Second))
	if err != nil {
		t.Fatalf("reap 1: %v", err)
	}
	if len(requeued) != 1 || len(failed) != 0 {
		t.Fatalf("reap 1: requeued=%v failed=%v", requeued, failed)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.State != JobQueued || got.NextRunAt <= now.Add(LeaseDuration).UnixMilli() {
		t.Errorf("after reap 1: state=%s next_run_at=%d", got.State, got.NextRunAt)
	}

	// Claim again after the backoff (attempts=2), expire again: out of
	// attempts → failed with E_LEASE_EXPIRED.
	if _, err := s.Claim(ctx, "dead", now.Add(2*LeaseDuration), 0); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	requeued, failed, err = s.ReapExpired(ctx, now.Add(3*LeaseDuration+2*time.Second))
	if err != nil {
		t.Fatalf("reap 2: %v", err)
	}
	if len(requeued) != 0 || len(failed) != 1 {
		t.Fatalf("reap 2: requeued=%v failed=%v", requeued, failed)
	}
	got, _ = s.GetJob(ctx, j.ID)
	if got.State != JobFailed || got.LastError == nil || !contains(*got.LastError, "E_LEASE_EXPIRED") {
		t.Errorf("after reap 2: state=%s last_error=%v", got.State, got.LastError)
	}
}

func TestHeartbeatExtendsLeaseAndLosingItIsVisible(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now()
	j, _ := s.Claim(ctx, "w1", now, 0)
	if ok, _ := s.Heartbeat(ctx, j.ID, "w1", now.Add(20*time.Second), 0); !ok {
		t.Fatal("heartbeat by owner should succeed")
	}
	if ok, _ := s.Heartbeat(ctx, j.ID, "intruder", now.Add(20*time.Second), 0); ok {
		t.Fatal("heartbeat by non-owner must fail")
	}
	// Heartbeat at +20s extended the lease to +50s; reaping at +61s must
	// take the job back.
	if _, _, err := s.ReapExpired(ctx, now.Add(2*LeaseDuration+time.Second)); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if ok, _ := s.Heartbeat(ctx, j.ID, "w1", now.Add(2*LeaseDuration+2*time.Second), 0); ok {
		t.Fatal("heartbeat after reap must fail")
	}
	if err := s.AssertOwned(ctx, j.ID, "w1"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("AssertOwned = %v, want ErrLeaseLost", err)
	}
}

// --- backoff --------------------------------------------------------

func TestBackoffGrowsAndCaps(t *testing.T) {
	// attempts = number of executions already made.
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{7, 300 * time.Second}, // 5*2^6 = 320 → capped
		{50, 300 * time.Second},
		{0, 5 * time.Second}, // defensive
	}
	for _, c := range cases {
		got := Backoff(c.attempts, 0, 0)
		if got != c.want {
			t.Errorf("Backoff(%d, 0) = %v, want %v", c.attempts, got, c.want)
		}
	}
	// Jitter stays within ±20% and never leaves [base*0.8, max*1.2].
	for i := 0; i < 100; i++ {
		got := Backoff(1, float64(i)/100*0.4-0.2, 0)
		if got < 4*time.Second || got > 6*time.Second {
			t.Fatalf("Backoff with jitter out of range: %v", got)
		}
	}
}

// --- idempotency ----------------------------------------------------

func TestIdempotencyKeyReturnsExistingJob(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	key := "deploy-trigger"
	app := createApp(t, s, "app-one")

	first, err := s.CreateJob(ctx, CreateJobInput{Type: "deploy", AppID: &app, IdempotencyKey: &key, CreatedBy: "ci"})
	if err != nil {
		t.Fatalf("create 1: %v", err)
	}
	second, err := s.CreateJob(ctx, CreateJobInput{Type: "deploy", AppID: &app, IdempotencyKey: &key, CreatedBy: "ci"})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("idempotent create returned different jobs: %s vs %s", first.ID, second.ID)
	}

	// Different key → different job.
	other := "other-key"
	third, err := s.CreateJob(ctx, CreateJobInput{Type: "deploy", AppID: &app, IdempotencyKey: &other, CreatedBy: "ci"})
	if err != nil {
		t.Fatalf("create 3: %v", err)
	}
	if third.ID == first.ID {
		t.Error("different key must create a new job")
	}

	// Same key, different app → different job (uniqueness is per app).
	app2 := createApp(t, s, "app-two")
	fourth, err := s.CreateJob(ctx, CreateJobInput{Type: "deploy", AppID: &app2, IdempotencyKey: &key, CreatedBy: "ci"})
	if err != nil {
		t.Fatalf("create 4: %v", err)
	}
	if fourth.ID == first.ID {
		t.Error("same key on another app must create a new job")
	}
}

// --- per-app lock ---------------------------------------------------

func TestPerAppLockSerializesJobs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	app := createApp(t, s, "app-locked")

	j1, _ := s.CreateJob(ctx, CreateJobInput{Type: "deploy", AppID: &app, CreatedBy: "t"})
	j2, _ := s.CreateJob(ctx, CreateJobInput{Type: "app_stop", AppID: &app, CreatedBy: "t"})

	now := time.Now()
	got, err := s.Claim(ctx, "w1", now, 0)
	if err != nil || got == nil || got.ID != j1.ID {
		t.Fatalf("claim 1 = %v, %v", got, err)
	}
	// Same app blocked while running: only j2 exists in the
	// queue and it belongs to the locked app.
	none, _ := s.Claim(ctx, "w2", now, 0)
	if none != nil {
		t.Fatalf("per-app lock broken: claimed %s while %s running", none.ID, j1.ID)
	}

	// A different app is not blocked.
	app2 := createApp(t, s, "app-free")
	j3, _ := s.CreateJob(ctx, CreateJobInput{Type: "deploy", AppID: &app2, CreatedBy: "t"})
	got, _ = s.Claim(ctx, "w2", time.Now(), 0)
	if got == nil || got.ID != j3.ID {
		t.Fatalf("other app blocked: got %v, want %s", got, j3.ID)
	}

	// Finishing releases the lock (AC3).
	if err := s.FinishJob(ctx, j1.ID, "w1", JobSucceeded, "", time.Now()); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, _ = s.Claim(ctx, "w1", time.Now(), 0)
	if got == nil || got.ID != j2.ID {
		t.Fatalf("lock not released: got %v, want %s", got, j2.ID)
	}
}

// --- cancellation ---------------------------------------------------

func TestCancelQueuedAndRunning(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	queued, _ := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t"})
	now := time.Now()
	if err := s.CancelJob(ctx, queued.ID, now); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	got, _ := s.GetJob(ctx, queued.ID)
	if got.State != JobCancelled || !got.CancelRequested {
		t.Errorf("queued cancel: state=%s req=%v", got.State, got.CancelRequested)
	}
	// Terminal states reject further transitions.
	if err := s.CancelJob(ctx, queued.ID, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("cancel terminal = %v, want ErrInvalidTransition", err)
	}

	running, _ := s.CreateJob(ctx, CreateJobInput{Type: "deploy", CreatedBy: "t"})
	now = time.Now() // refresh: running was created after the previous now
	if _, err := s.Claim(ctx, "w1", now, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.CancelJob(ctx, running.ID, now); err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	got, _ = s.GetJob(ctx, running.ID)
	if got.State != JobRunning || !got.CancelRequested {
		t.Errorf("running cancel: state=%s req=%v (must stay running until safe point)", got.State, got.CancelRequested)
	}
	if ok, _ := s.CancelRequested(ctx, running.ID); !ok {
		t.Error("CancelRequested must report true")
	}
	// Worker observes the flag and stops at the safe point → cancelled.
	if err := s.FinishJob(ctx, running.ID, "w1", JobCancelled, "cancelled by user", now); err != nil {
		t.Fatalf("finish cancelled: %v", err)
	}
}

func TestFinishJobOnlyFromRunning(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	j, _ := s.CreateJob(ctx, CreateJobInput{Type: "gc", CreatedBy: "t"})
	now := time.Now()
	// Never claimed: FinishJob must not silently succeed.
	if err := s.FinishJob(ctx, j.ID, "w1", JobSucceeded, "", now); !errors.Is(err, ErrLeaseLost) && !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("finish unclaimed = %v, want ErrLeaseLost or ErrInvalidTransition", err)
	}
	if _, err := s.Claim(ctx, "w1", now, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Finish to an illegal target is rejected even by the owner.
	if err := s.FinishJob(ctx, j.ID, "w1", JobQueued, "", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("finish→queued = %v, want ErrInvalidTransition", err)
	}
	if err := s.FinishJob(ctx, j.ID, "w1", JobFailed, "E_VALIDATION: bad image", now); err != nil {
		t.Fatalf("finish failed: %v", err)
	}
	got, _ := s.GetJob(ctx, j.ID)
	if got.LastError == nil || *got.LastError != "E_VALIDATION: bad image" {
		t.Errorf("last_error = %v", got.LastError)
	}
	// Terminal: no further transitions.
	if err := s.CancelJob(ctx, j.ID, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("cancel terminal = %v, want ErrInvalidTransition", err)
	}
}

// --- ULID ids ---------------------------------------------------------

func TestIDsAreUniqueAndSortable(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b {
		t.Fatal("ULIDs must differ")
	}
	if len(a) != 26 {
		t.Errorf("ULID length = %d, want 26", len(a))
	}
	// Sortability comes from the time component; verify with explicit
	// timestamps (same-ms entropy is not orderable by design).
	early := NewIDAt(time.UnixMilli(1000))
	late := NewIDAt(time.UnixMilli(2000))
	if early >= late {
		t.Errorf("earlier ULID not smaller: %s >= %s", early, late)
	}
}
