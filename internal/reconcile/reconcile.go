// Package reconcile compares what the database believes with the
// containers that are actually running. Every result is a report:
// nothing is started, stopped or removed, so the operator stays the
// one who decides what to do about drift.
package reconcile

import (
	"sort"

	"github.com/tinyship/tinyship/internal/store"
)

// Report is one comparison's result, with every list sorted so two
// runs over the same state print the same lines.
type Report struct {
	// UpToDate wants to run and holds a running container.
	UpToDate []string `json:"up_to_date"`
	// NeedDeploy wants to run but has no running container.
	NeedDeploy []string `json:"need_deploy"`
	// Stopped is intentionally not running (stopped by config or
	// being deleted); its absence is not drift.
	Stopped []string `json:"stopped"`
	// UnknownRunning lists container ids (not app names: there is no
	// database row to carry one) of running managed containers the
	// database does not know about.
	UnknownRunning []string `json:"unknown_running"`
}

// Drift reports whether reality differs from the database's intent:
// an app that should run but does not, or a container running with no
// row behind it. A deliberately stopped app is not drift.
func (r Report) Drift() bool {
	return len(r.NeedDeploy) > 0 || len(r.UnknownRunning) > 0
}

// App is the slice of application state the comparison reads.
type App struct {
	ID           string
	Name         string
	DesiredState string
	Deleting     bool
}

// FromStore maps database rows to the comparison's view.
func FromStore(apps []*store.App) []App {
	out := make([]App, len(apps))
	for i, a := range apps {
		out[i] = App{
			ID:           a.ID,
			Name:         a.Name,
			DesiredState: a.DesiredState,
			Deleting:     a.Deleting,
		}
	}
	return out
}

// Compare classifies every app against the set of app ids holding a
// running managed container.
func Compare(apps []App, running map[string]bool) Report {
	var r Report
	inDB := make(map[string]bool, len(apps))
	for _, app := range apps {
		inDB[app.ID] = true
		switch {
		case app.DesiredState != "running" || app.Deleting:
			r.Stopped = append(r.Stopped, app.Name)
		case running[app.ID]:
			r.UpToDate = append(r.UpToDate, app.Name)
		default:
			r.NeedDeploy = append(r.NeedDeploy, app.Name)
		}
	}
	// Running managed containers with no row behind them: this
	// database predates them. Report, never touch.
	for id := range running {
		if !inDB[id] {
			r.UnknownRunning = append(r.UnknownRunning, id)
		}
	}
	sort.Strings(r.UpToDate)
	sort.Strings(r.NeedDeploy)
	sort.Strings(r.Stopped)
	sort.Strings(r.UnknownRunning)
	return r
}
