package store

// Hibernate state-machine storage: the state column's allowed values
// and the cycle counters the stats view and the flap warning read.

import (
	"context"
	"testing"
	"time"
)

func TestSetHibernateStateRejectsUnknownValue(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appID := createApp(t, s, "web")

	for _, state := range []string{"", HibernateAwake, HibernateSleeping, HibernateWaking} {
		if err := s.SetHibernateState(ctx, appID, state); err != nil {
			t.Errorf("state %q: %v", state, err)
		}
	}
	for _, bad := range []string{"napping", "AWAKE", "asleep"} {
		if err := s.SetHibernateState(ctx, appID, bad); err == nil {
			t.Errorf("state %q must be rejected", bad)
		}
	}
	// A rejected write must not have moved the machine.
	app, err := s.GetAppByID(ctx, appID)
	if err != nil {
		t.Fatalf("reload app: %v", err)
	}
	if app.HibernateState != HibernateWaking {
		t.Errorf("state = %q, want %q after the rejected writes", app.HibernateState, HibernateWaking)
	}
	if err := s.SetHibernateState(ctx, "no-such-app", HibernateAwake); err != ErrNotFound {
		t.Errorf("missing app err = %v, want ErrNotFound", err)
	}
}

func TestRecordHibernateSleepCountsAndOpensFlapWindow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appID := createApp(t, s, "web")
	start := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)

	// Three cycles inside one window: the count grows and the window
	// stays put.
	for i := 0; i < 3; i++ {
		got, err := s.RecordHibernateSleep(ctx, appID,
			start.Add(time.Duration(i)*time.Minute), 15*time.Minute)
		if err != nil {
			t.Fatalf("sleep %d: %v", i, err)
		}
		if want := i + 1; got != want {
			t.Errorf("flap count = %d, want %d", got, want)
		}
	}
	// A quiet spell past the window restarts it at one.
	got, err := s.RecordHibernateSleep(ctx, appID,
		start.Add(20*time.Minute), 15*time.Minute)
	if err != nil {
		t.Fatalf("sleep after window: %v", err)
	}
	if got != 1 {
		t.Errorf("flap count after window = %d, want 1", got)
	}

	st, err := s.GetContainerState(ctx, appID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.HibernateSleepCount != 4 {
		t.Errorf("sleep count = %d, want 4", st.HibernateSleepCount)
	}
}

func TestRecordHibernateWakeTracksAverageAndMax(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appID := createApp(t, s, "web")
	now := time.Now().UTC()

	for _, d := range []time.Duration{400 * time.Millisecond, 1500 * time.Millisecond} {
		if err := s.RecordHibernateWake(ctx, appID, d, now); err != nil {
			t.Fatalf("record wake: %v", err)
		}
	}
	st, err := s.GetContainerState(ctx, appID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.HibernateWakeCount != 2 {
		t.Errorf("wake count = %d, want 2", st.HibernateWakeCount)
	}
	if st.HibernateWakeTotalMs < 1900 || st.HibernateWakeTotalMs > 2000 {
		t.Errorf("wake total = %dms, want about 1900", st.HibernateWakeTotalMs)
	}
	if st.HibernateWakeMaxMs < 1400 || st.HibernateWakeMaxMs > 1600 {
		t.Errorf("wake max = %dms, want about 1500", st.HibernateWakeMaxMs)
	}

	// A negative duration (clock skew) must not corrupt the totals.
	if err := s.RecordHibernateWake(ctx, appID, -time.Second, now); err != nil {
		t.Fatalf("record negative wake: %v", err)
	}
	st, err = s.GetContainerState(ctx, appID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.HibernateWakeTotalMs < 1900 {
		t.Errorf("total after negative = %d, want it unchanged at least", st.HibernateWakeTotalMs)
	}
}

func TestHibernateCountersSurviveStatsUpsert(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appID := createApp(t, s, "web")
	now := time.Now().UTC()

	if _, err := s.RecordHibernateSleep(ctx, appID, now, time.Minute); err != nil {
		t.Fatalf("record sleep: %v", err)
	}
	// The sampler rewrites the resource columns on every tick; the
	// cycle counters must not be part of that write.
	if err := s.UpsertContainerState(ctx, ContainerState{
		AppID: appID, ContainerID: "c1", State: "running",
		MemUsageMB: 12, OOMKillCount: 3, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	st, err := s.GetContainerState(ctx, appID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if st.HibernateSleepCount != 1 || st.MemUsageMB != 12 || st.OOMKillCount != 3 {
		t.Errorf("row = %+v, want sleep count 1 with the fresh sample", st)
	}
	if st.State != "running" {
		t.Errorf("state = %q, want running", st.State)
	}
}

// openTestStore opens a fresh store for these tests.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/hibernate.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHostPortCandidatesNeverOfferTheRelaySlot(t *testing.T) {
	const minPort, maxPort, relayPort = 20000, 29999, 29999
	got := hostPortCandidates(minPort, minPort, maxPort, relayPort)
	if len(got) != maxPort-minPort {
		t.Fatalf("candidates = %d, want %d (the range minus the relay slot)",
			len(got), maxPort-minPort)
	}
	seen := map[int]bool{}
	for i, p := range got {
		if p == relayPort {
			t.Fatalf("candidate %d is the relay slot %d", i, relayPort)
		}
		if p < minPort || p > maxPort {
			t.Fatalf("candidate %d = %d, outside the managed range", i, p)
		}
		if seen[p] {
			t.Fatalf("candidate %d = %d duplicated", i, p)
		}
		seen[p] = true
	}
	// The scan starts where the caller asked and wraps exactly once.
	if got[0] != minPort {
		t.Errorf("first candidate = %d, want %d", got[0], minPort)
	}
	mid := hostPortCandidates(25000, minPort, maxPort, relayPort)
	if mid[0] != 25000 {
		t.Errorf("first candidate = %d, want the requested start 25000", mid[0])
	}
	if mid[len(mid)-1] != 24999 {
		t.Errorf("last candidate = %d, want the wrap to 24999", mid[len(mid)-1])
	}
	// An exhausted range is reported as no candidates rather than a
	// slot outside the range.
	if got := hostPortCandidates(30000, 29999, 29999, relayPort); len(got) != 0 {
		t.Errorf("exhausted range = %v, want none", got)
	}
}
