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
