package reconcile

import (
	"reflect"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

func TestCompareClassifiesEveryApp(t *testing.T) {
	apps := []App{
		{ID: "a1", Name: "running-web", DesiredState: "running"},
		{ID: "a2", Name: "down-api", DesiredState: "running"},
		{ID: "a3", Name: "stopped-by-config", DesiredState: "stopped"},
		{ID: "a4", Name: "being-deleted", DesiredState: "running", Deleting: true},
	}
	running := map[string]bool{"a1": true, "orphan9": true}

	got := Compare(apps, running)

	if !reflect.DeepEqual(got.UpToDate, []string{"running-web"}) {
		t.Errorf("up_to_date = %v, want [running-web]", got.UpToDate)
	}
	if !reflect.DeepEqual(got.NeedDeploy, []string{"down-api"}) {
		t.Errorf("need_deploy = %v, want [down-api]", got.NeedDeploy)
	}
	wantStopped := []string{"being-deleted", "stopped-by-config"}
	if !reflect.DeepEqual(got.Stopped, wantStopped) {
		t.Errorf("stopped = %v, want %v (sorted)", got.Stopped, wantStopped)
	}
	if !reflect.DeepEqual(got.UnknownRunning, []string{"orphan9"}) {
		t.Errorf("unknown_running = %v, want [orphan9]", got.UnknownRunning)
	}
}

func TestDriftIgnoresDeliberateStops(t *testing.T) {
	// A stopped-by-config app and an unknown id are different claims:
	// only reality-vs-intent differences count as drift.
	stopped := Compare([]App{{ID: "a1", Name: "web", DesiredState: "stopped"}}, map[string]bool{})
	if stopped.Drift() {
		t.Error("a deliberately stopped app must not count as drift")
	}

	missing := Compare([]App{{ID: "a1", Name: "web", DesiredState: "running"}}, map[string]bool{})
	if !missing.Drift() {
		t.Error("an app that should run without a container is drift")
	}

	orphan := Compare(nil, map[string]bool{"old": true})
	if !orphan.Drift() {
		t.Error("a running container the database ignores is drift")
	}

	inSync := Compare([]App{{ID: "a1", Name: "web", DesiredState: "running"}}, map[string]bool{"a1": true})
	if inSync.Drift() {
		t.Error("an app running from its own row is not drift")
	}
}

func TestCompareSortsDeterministically(t *testing.T) {
	apps := []App{
		{ID: "z", Name: "zeta", DesiredState: "running"},
		{ID: "a", Name: "alpha", DesiredState: "running"},
		{ID: "m", Name: "mid", DesiredState: "running"},
	}
	got := Compare(apps, map[string]bool{
		"z": true, "a": true, "m": true, // the apps' own containers
		"q2": true, "q1": true, "q3": true, // three the database ignores
	})
	want := []string{"alpha", "mid", "zeta"}
	if !reflect.DeepEqual(got.UpToDate, want) {
		t.Errorf("up_to_date = %v, want %v", got.UpToDate, want)
	}
	wantOrphans := []string{"q1", "q2", "q3"}
	if !reflect.DeepEqual(got.UnknownRunning, wantOrphans) {
		t.Errorf("unknown_running = %v, want %v", got.UnknownRunning, wantOrphans)
	}
}

func TestFromStoreKeepsTheComparisonFields(t *testing.T) {
	rows := []*store.App{
		{ID: "a1", Name: "web", DesiredState: "running", ConfigJSON: "{}"},
		{ID: "a2", Name: "api", DesiredState: "running", Deleting: true},
	}
	got := FromStore(rows)
	want := []App{
		{ID: "a1", Name: "web", DesiredState: "running"},
		{ID: "a2", Name: "api", DesiredState: "running", Deleting: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromStore = %+v, want %+v", got, want)
	}
}

func TestDriftIgnoresHibernatingApps(t *testing.T) {
	// Asleep on purpose: no container is the expected state, so the
	// absence must never be reported as drift.
	sleeping := Compare([]App{{
		ID: "a1", Name: "web", DesiredState: "running",
		HibernateState: store.HibernateSleeping,
	}}, map[string]bool{})
	if sleeping.Drift() {
		t.Error("a sleeping app must not count as drift")
	}
	if !reflect.DeepEqual(sleeping.Hibernating, []string{"web"}) {
		t.Errorf("hibernating = %v, want [web]", sleeping.Hibernating)
	}

	// A wake in flight is equally deliberate: the relay is bringing
	// the container up right now.
	waking := Compare([]App{{
		ID: "a1", Name: "web", DesiredState: "running",
		HibernateState: store.HibernateWaking,
	}}, map[string]bool{})
	if waking.Drift() {
		t.Error("an app mid-wake must not count as drift")
	}

	// Hibernation off means the app must be up: same claim as before.
	off := Compare([]App{{
		ID: "a1", Name: "web", DesiredState: "running",
	}}, map[string]bool{})
	if !off.Drift() {
		t.Error("an app without hibernation and without a container is drift")
	}
}

func TestFromStoreCarriesHibernateState(t *testing.T) {
	rows := []*store.App{
		{ID: "a1", Name: "web", DesiredState: "running",
			HibernateState: store.HibernateSleeping, ConfigJSON: "{}"},
		{ID: "a2", Name: "api", DesiredState: "running", ConfigJSON: "{}"},
	}
	got := FromStore(rows)
	if got[0].HibernateState != store.HibernateSleeping {
		t.Errorf("state = %q, want %q", got[0].HibernateState, store.HibernateSleeping)
	}
	if got[1].HibernateState != "" {
		t.Errorf("state = %q, want empty for an app without hibernation", got[1].HibernateState)
	}
}
