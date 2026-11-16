package store

// Notification queue tests: dedup collapse, the retry schedule and its
// terminal failure, partial progress across sinks, and the hourly-cap
// counter the dispatcher defers on.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEnqueueNotifyEventBasics(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type:    "deploy_failed",
		AppName: "web",
		Title:   "Deploy failed",
		Detail:  map[string]any{"step": "pull", "code": "E_PULL"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ev, err := s.GetNotifyEvent(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ev.Type != "deploy_failed" || ev.AppName != "web" || ev.Title != "Deploy failed" {
		t.Errorf("event = %+v, want deploy_failed/web", ev)
	}
	if ev.Status != "pending" || ev.Attempts != 0 || ev.DupCount != 1 {
		t.Errorf("status/attempts/dup = %s/%d/%d, want pending/0/1",
			ev.Status, ev.Attempts, ev.DupCount)
	}
	if ev.Detail == "" || ev.DedupKey != "" {
		t.Errorf("detail/dedup = %q/%q", ev.Detail, ev.DedupKey)
	}
	if !ev.NextAttemptAt.After(time.Now().UTC().Add(-time.Minute)) {
		t.Errorf("next_attempt_at = %v, want due now", ev.NextAttemptAt)
	}
}

func TestEnqueueNotifyEventRequiresType(t *testing.T) {
	s := openTest(t)
	if _, err := s.EnqueueNotifyEvent(context.Background(), NotifyEventInput{}); err == nil {
		t.Fatal("enqueue without type: want error")
	}
}

func TestEnqueueNotifyEventDedupCollapsesRepeats(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	first, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "oom", AppName: "web", Title: "killed", DedupKey: "oom:app-1",
	})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "oom", AppName: "web", Title: "killed", DedupKey: "oom:app-1",
	})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	if second != first {
		t.Fatalf("dedup id = %d, want the existing row %d", second, first)
	}
	ev, err := s.GetNotifyEvent(ctx, first)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ev.DupCount != 2 {
		t.Errorf("dup_count = %d, want 2", ev.DupCount)
	}

	// A different key is its own event.
	third, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "oom", AppName: "api", Title: "killed", DedupKey: "oom:app-2",
	})
	if err != nil {
		t.Fatalf("enqueue third: %v", err)
	}
	if third == first {
		t.Errorf("distinct dedup_key collapsed into %d", first)
	}

	// Outside the window the same key is a fresh event.
	older := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := s.db.Exec(
		`UPDATE notify_events SET created_at = ? WHERE id = ?`,
		older.UnixMilli(), first); err != nil {
		t.Fatalf("age row: %v", err)
	}
	fresh, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "oom", AppName: "web", Title: "killed", DedupKey: "oom:app-1",
	})
	if err != nil {
		t.Fatalf("enqueue after window: %v", err)
	}
	if fresh == first {
		t.Errorf("event inside expired window collapsed; want a new row")
	}
}

func TestEnqueueNotifyEventCustomWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "cert_expiring", Title: "cert", DedupKey: "cert_expiring:example.com",
		DedupWindow: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Age the row two days: still inside the seven-day re-fire window.
	twoDays := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := s.db.Exec(
		`UPDATE notify_events SET created_at = ? WHERE id = ?`,
		twoDays.UnixMilli(), id); err != nil {
		t.Fatalf("age row: %v", err)
	}
	again, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "cert_expiring", Title: "cert", DedupKey: "cert_expiring:example.com",
		DedupWindow: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("enqueue again: %v", err)
	}
	if again != id {
		t.Errorf("row id = %d, want the existing %d inside the custom window", again, id)
	}
}

func TestNotifyRetryFollowsScheduleThenFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
		Type: "test", Title: "t",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// First failure: attempt 1, next after the first retry delay.
	if err := s.NotifyRetry(ctx, id, nil, "status 500"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	ev, err := s.GetNotifyEvent(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ev.Status != "pending" || ev.Attempts != 1 || ev.LastError != "status 500" {
		t.Errorf("status/attempts/err = %s/%d/%q, want pending/1/status 500",
			ev.Status, ev.Attempts, ev.LastError)
	}
	wantNext := time.Now().UTC().Add(RetrySchedule[1])
	if d := ev.NextAttemptAt.Sub(wantNext); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("next attempt = %v, want about %v", ev.NextAttemptAt, wantNext)
	}

	// Work through the rest of the schedule; the row must land failed
	// exactly when the schedule runs out, not before.
	for i := 1; i < len(RetrySchedule); i++ {
		if err := s.NotifyRetry(ctx, id, nil, "still down"); err != nil {
			t.Fatalf("retry %d: %v", i+1, err)
		}
	}
	ev, err = s.GetNotifyEvent(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ev.Status != "failed" {
		t.Errorf("status after exhausting %d attempts = %s, want failed",
			ev.Attempts, ev.Status)
	}
	if ev.Attempts != len(RetrySchedule) {
		t.Errorf("attempts = %d, want %d", ev.Attempts, len(RetrySchedule))
	}
}

func TestNotifyRetryKeepsPartialProgress(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "t"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.NotifyRetry(ctx, id, []string{"webhook"}, "telegram: status 500"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	ev, err := s.GetNotifyEvent(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(ev.SentTo) != 1 || ev.SentTo[0] != "webhook" {
		t.Errorf("sent_to = %v, want [webhook]", ev.SentTo)
	}
}

func TestMarkNotifyDelivered(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "t"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := s.MarkNotifyDelivered(ctx, id, []string{"webhook", "slack"}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	ev, err := s.GetNotifyEvent(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ev.Status != "delivered" || ev.Attempts != 1 || ev.DeliveredAt == nil {
		t.Errorf("status/attempts/delivered_at = %s/%d/%v, want delivered/1/set",
			ev.Status, ev.Attempts, ev.DeliveredAt)
	}
	if len(ev.SentTo) != 2 {
		t.Errorf("sent_to = %v, want both sinks", ev.SentTo)
	}
	if ev.LastError != "" {
		t.Errorf("last_error = %q, want cleared", ev.LastError)
	}
}

func TestDueNotifyEventsOnlyPendingAndDue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	due, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "due"})
	if err != nil {
		t.Fatalf("enqueue due: %v", err)
	}
	pendingFuture, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "later"})
	if err != nil {
		t.Fatalf("enqueue later: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE notify_events SET next_attempt_at = ? WHERE id = ?`,
		time.Now().UTC().Add(time.Hour).UnixMilli(), pendingFuture); err != nil {
		t.Fatalf("defer row: %v", err)
	}
	delivered, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "done"})
	if err != nil {
		t.Fatalf("enqueue done: %v", err)
	}
	if err := s.MarkNotifyDelivered(ctx, delivered, []string{"webhook"}); err != nil {
		t.Fatalf("mark: %v", err)
	}

	got, err := s.DueNotifyEvents(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(got) != 1 || got[0].ID != due {
		t.Fatalf("due events = %+v, want only id %d", got, due)
	}

	// Limit honored.
	limited, err := s.DueNotifyEvents(ctx, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("due limit: %v", err)
	}
	if len(limited) != 0 {
		t.Errorf("limit 0 returned %d rows", len(limited))
	}
}

func TestCountNotifyDeliveredSinceAndPrune(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "t"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	from := time.Now().UTC().Add(-time.Minute)
	n, err := s.CountNotifyDeliveredSince(ctx, from)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count before delivery = %d, want 0", n)
	}
	if err := s.MarkNotifyDelivered(ctx, id, []string{"webhook"}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	n, err = s.CountNotifyDeliveredSince(ctx, from)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count after delivery = %d, want 1", n)
	}
	// Not counted outside the window.
	n, err = s.CountNotifyDeliveredSince(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("future window count = %d, want 0", n)
	}

	// Prune removes the delivered row but keeps a pending one.
	pending, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{Type: "test", Title: "p"})
	if err != nil {
		t.Fatalf("enqueue pending: %v", err)
	}
	if err := s.PruneNotifyEvents(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := s.GetNotifyEvent(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("delivered row after prune: err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetNotifyEvent(ctx, pending); err != nil {
		t.Errorf("pending row pruned: %v", err)
	}
}

func TestListNotifyEventsNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.EnqueueNotifyEvent(ctx, NotifyEventInput{
			Type: "test", Title: "t",
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	got, err := s.ListNotifyEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("list = %d rows, want 3", len(got))
	}
	if got[0].ID < got[1].ID || got[1].ID < got[2].ID {
		t.Errorf("order not newest first: %d, %d, %d", got[0].ID, got[1].ID, got[2].ID)
	}
}
