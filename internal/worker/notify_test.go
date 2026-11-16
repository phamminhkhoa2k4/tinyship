package worker

// Dispatcher tests: real deliveries against a local receiver, the
// retry/defer decisions, the edge-triggered host checks, and the
// guarantee that none of it can be mistaken for a delivered event when
// no sink took it.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/notify"
	"github.com/tinyship/tinyship/internal/store"
)

// receivedWebhook is one captured receiver-side request.
type receivedWebhook struct {
	Header string
	Body   []byte
}

func putSinks(t *testing.T, s *store.Store, sinks ...notify.Sink) {
	t.Helper()
	raw, err := notify.EncodeSinks(sinks)
	if err != nil {
		t.Fatalf("encode sinks: %v", err)
	}
	if err := s.PutSetting(context.Background(), notify.SettingKey, raw); err != nil {
		t.Fatalf("put sinks: %v", err)
	}
}

func dispatcher(t *testing.T, s *store.Store) *NotifyDispatcher {
	t.Helper()
	return NewNotifyDispatcher(s, slog.New(slog.DiscardHandler))
}

func eventStatus(t *testing.T, s *store.Store, id int64) *store.NotifyEvent {
	t.Helper()
	ev, err := s.GetNotifyEvent(context.Background(), id)
	if err != nil {
		t.Fatalf("get event %d: %v", id, err)
	}
	return ev
}

func TestNotifyDispatcherDeliversSignedWebhook(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	var got []receivedWebhook
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, receivedWebhook{
			Header: r.Header.Get("X-TinyShip-Signature"), Body: raw,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	const secret = "receiver-side-secret"
	putSinks(t, s, notify.Sink{Kind: "webhook", URL: receiver.URL, Secret: secret})

	id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
		Type: "deploy_failed", AppName: "web", Title: "Deploy failed",
		Detail: map[string]any{"step": "pull"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	dispatcher(t, s).RunOnce(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	if err := notify.Verify(secret, got[0].Header, got[0].Body,
		time.Now(), time.Minute); err != nil {
		t.Errorf("signature verify: %v", err)
	}
	if !strings.Contains(string(got[0].Body), "Deploy failed") {
		t.Errorf("body = %s", got[0].Body)
	}
	ev := eventStatus(t, s, id)
	if ev.Status != "delivered" || ev.Attempts != 1 || ev.DeliveredAt == nil {
		t.Errorf("event = %s/attempts %d, want delivered once", ev.Status, ev.Attempts)
	}
	if len(ev.SentTo) != 1 || ev.SentTo[0] != "webhook" {
		t.Errorf("sent_to = %v, want [webhook]", ev.SentTo)
	}
}

func TestNotifyDispatcherRetriesFailedDelivery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "receiver exploded", http.StatusInternalServerError)
	}))
	defer receiver.Close()
	putSinks(t, s, notify.Sink{Kind: "webhook", URL: receiver.URL, Secret: "s"})

	id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
		Type: "test", Title: "t",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	dispatcher(t, s).RunOnce(ctx)

	ev := eventStatus(t, s, id)
	if ev.Status != "pending" || ev.Attempts != 1 {
		t.Fatalf("event = %s/attempts %d, want pending after one failed attempt",
			ev.Status, ev.Attempts)
	}
	if !strings.Contains(ev.LastError, "500") {
		t.Errorf("last_error = %q, want the status", ev.LastError)
	}
	if !ev.NextAttemptAt.After(time.Now().UTC().Add(10 * time.Second)) {
		t.Errorf("next attempt = %v, want pushed into the retry schedule", ev.NextAttemptAt)
	}

	// Not due yet: an immediate second pass must not burn attempt 2.
	dispatcher(t, s).RunOnce(ctx)
	if again := eventStatus(t, s, id); again.Attempts != 1 {
		t.Errorf("attempts before due = %d, want 1", again.Attempts)
	}
}

func TestNotifyDispatcherWithoutSinksRecordsCause(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{Type: "test", Title: "t"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dispatcher(t, s).RunOnce(ctx)

	ev := eventStatus(t, s, id)
	if ev.Status != "pending" || ev.Attempts != 1 {
		t.Errorf("event = %s/%d, want a recorded failed attempt", ev.Status, ev.Attempts)
	}
	if !strings.Contains(ev.LastError, "no notification sinks") {
		t.Errorf("last_error = %q, want the no-sinks cause", ev.LastError)
	}
}

func TestNotifyDispatcherTargetFilterNeverMarksUnsentDelivered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	var requests int
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	putSinks(t, s, notify.Sink{Kind: "webhook", URL: receiver.URL, Secret: "s"})

	// Test event aimed at telegram only: nothing configured can take it.
	id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
		Type: "test", Title: "t",
		Detail: map[string]any{"target_kind": "telegram"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dispatcher(t, s).RunOnce(ctx)

	mu.Lock()
	if requests != 0 {
		mu.Unlock()
		t.Fatalf("receiver got %d requests, want 0", requests)
	}
	mu.Unlock()
	ev := eventStatus(t, s, id)
	if ev.Status == "delivered" {
		t.Fatal("event marked delivered without any matching sink")
	}
	if !strings.Contains(ev.LastError, "target") {
		t.Errorf("last_error = %q, want the target cause", ev.LastError)
	}
}

func TestNotifyDispatcherHourlyCapDefers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	received := make(chan struct{}, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	putSinks(t, s, notify.Sink{Kind: "webhook", URL: receiver.URL, Secret: "s"})

	// Fill the rolling window to the cap with delivered rows.
	for i := 0; i < NotifyHourlyCap; i++ {
		id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
			Type: "test", Title: "old",
		})
		if err != nil {
			t.Fatalf("enqueue filler %d: %v", i, err)
		}
		if err := s.MarkNotifyDelivered(ctx, id, []string{"webhook"}); err != nil {
			t.Fatalf("mark filler: %v", err)
		}
	}
	id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{Type: "test", Title: "now"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	dispatcher(t, s).RunOnce(ctx)

	select {
	case <-received:
		t.Fatal("receiver was called despite the hourly cap")
	default:
	}
	ev := eventStatus(t, s, id)
	if ev.Status != "pending" || ev.Attempts != 0 {
		t.Errorf("event = %s/%d, want deferred untouched", ev.Status, ev.Attempts)
	}
}

func TestNotifyDispatcherDiskLowIsEdgeTriggered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SetMinFreeDiskMB(ctx, 512); err != nil {
		t.Fatalf("set floor: %v", err)
	}

	d := dispatcher(t, s)
	free := 100
	d.DiskFree = func(string) (int, error) { return free, nil }

	d.RunOnce(ctx)
	events, err := s.ListNotifyEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 || events[0].Type != "disk_low" {
		t.Fatalf("events after first crossing = %+v, want one disk_low", events)
	}
	detail := map[string]any{}
	_ = json.Unmarshal([]byte(events[0].Detail), &detail)
	if detail["free_mb"] != float64(100) || detail["floor_mb"] != float64(512) {
		t.Errorf("detail = %v, want free 100 / floor 512", detail)
	}

	// Still low: no repeat (the queue would collapse it anyway, but the
	// edge must not even fire).
	d.RunOnce(ctx)
	if events, _ = s.ListNotifyEvents(ctx, 10); len(events) != 1 {
		t.Fatalf("still-low produced another event: %d rows", len(events))
	}

	// Above the floor re-arms the edge; crossing again fires once more,
	// collapsed by dedup into the existing row's repeat count.
	free = 4096
	d.RunOnce(ctx)
	free = 100
	d.RunOnce(ctx)
	if events, _ = s.ListNotifyEvents(ctx, 10); len(events) != 1 {
		t.Fatalf("re-crossing queued a new row: %d rows", len(events))
	}
	if events[0].DupCount != 2 {
		t.Errorf("dup_count = %d, want 2 (the repeat collapsed)", events[0].DupCount)
	}
}

func TestNotifyDispatcherWarnsOnExpiringCertificate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	app, err := s.CreateApp(ctx, store.CreateAppInput{
		Name: "web", ConfigJSON: "{}", Actor: "test",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	dom, _, err := s.AddDomain(ctx, app.ID, "shop.example.com")
	if err != nil {
		t.Fatalf("add domain: %v", err)
	}
	if err := s.SetDomainStatus(ctx, dom.ID, "active"); err != nil {
		t.Fatalf("activate domain: %v", err)
	}

	d := dispatcher(t, s)
	probes := map[string]time.Time{
		"shop.example.com": time.Now().Add(5 * 24 * time.Hour), // inside the window
	}
	d.CertExpiry = func(host string) (time.Time, error) {
		notAfter, ok := probes[host]
		if !ok {
			t.Errorf("cert probed for unknown host %q", host)
			return time.Time{}, context.Canceled
		}
		return notAfter, nil
	}

	d.RunOnce(ctx)
	events, err := s.ListNotifyEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 || events[0].Type != "cert_expiring" {
		t.Fatalf("events = %+v, want one cert_expiring", events)
	}
	detail := map[string]any{}
	_ = json.Unmarshal([]byte(events[0].Detail), &detail)
	if detail["hostname"] != "shop.example.com" {
		t.Errorf("hostname = %v", detail["hostname"])
	}
	if days, ok := detail["expires_in_days"].(float64); !ok || days < 4 || days > 5 {
		t.Errorf("expires_in_days = %v, want about 5", detail["expires_in_days"])
	}

	// Within the pacing window the probe does not run again, and the
	// queue's dedup window would collapse a same-day re-fire anyway.
	d.RunOnce(ctx)
	if events, _ = s.ListNotifyEvents(ctx, 10); len(events) != 1 {
		t.Fatalf("second pass produced %d events, want still 1", len(events))
	}

	// A certificate far from expiry never warns.
	probes["shop.example.com"] = time.Now().Add(90 * 24 * time.Hour)
	d.lastCertCheck = time.Time{} // re-arm the pacing for the test
	// Retire the existing warning row so a fresh (non-)firing is
	// observable: delivered, then pruned past the retention cutoff.
	if err := s.MarkNotifyDelivered(ctx, events[0].ID, []string{"webhook"}); err != nil {
		t.Fatalf("retire row: %v", err)
	}
	if err := s.PruneNotifyEvents(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	d.RunOnce(ctx)
	if events, _ = s.ListNotifyEvents(ctx, 10); len(events) != 0 {
		t.Fatalf("healthy certificate produced %d events", len(events))
	}
}

func TestSNIExpiryRejectsUnreachableProxy(t *testing.T) {
	// Nothing listens on this port, so the dial must fail rather than
	// invent an expiry.
	if _, err := SNIExpiry("localhost"); err == nil {
		t.Fatal("SNIExpiry against a dead port: want error")
	}
}

func TestNotifyDispatcherPersistsAcrossRestart(t *testing.T) {
	// The queue is durable: a pending row survives a dispatcher
	// replacement (worker restart) and is delivered by the next one.
	path := filepath.Join(t.TempDir(), "tinyship.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	received := make(chan []byte, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received <- raw
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	putSinks(t, s, notify.Sink{Kind: "webhook", URL: receiver.URL, Secret: "s"})

	id, err := s.EnqueueNotifyEvent(ctx, store.NotifyEventInput{
		Type: "backup_failed", Title: "Backup failed",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	s.Close()

	// "Restart": reopen the same database with a fresh dispatcher.
	s2, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	d := NewNotifyDispatcher(s2, slog.New(slog.DiscardHandler))
	d.RunOnce(ctx)

	select {
	case raw := <-received:
		if !strings.Contains(string(raw), "Backup failed") {
			t.Errorf("body = %s", raw)
		}
	default:
		t.Fatal("event was not delivered after the restart")
	}
	if ev, err := s2.GetNotifyEvent(ctx, id); err != nil || ev.Status != "delivered" {
		t.Errorf("event after restart = %+v/%v, want delivered", ev, err)
	}
}
