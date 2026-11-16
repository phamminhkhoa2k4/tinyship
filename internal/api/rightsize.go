package api

// Right-size: the view reads an app's observed peak over the standard
// window and proposes a new memory limit; apply recomputes the same
// suggestion server-side (the client never dictates the number),
// refuses it against the budget the worker will enforce anyway, then
// updates the config and queues a deploy through the normal pipeline.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/worker"
)

// Verdicts a suggestion view can carry.
const (
	verdictNoData = "no_data"
	verdictLower  = "lower"
	verdictRaise  = "raise"
	verdictOK     = "ok"
)

// rightsizeView is the GET /apps/{name}/rightsize response.
type rightsizeView struct {
	App          string   `json:"app"`
	WindowDays   int      `json:"window_days"`
	Samples      int      `json:"samples"`
	ObservedFrom string   `json:"observed_from,omitempty"`
	ObservedTo   string   `json:"observed_to,omitempty"`
	PeakMB       int      `json:"peak_mb"`
	CurrentMB    int      `json:"current_mb"`
	SuggestedMB  int      `json:"suggested_mb"`
	Verdict      string   `json:"verdict"`
	Confidence   string   `json:"confidence"`
	Reasons      []string `json:"reasons"`
	BudgetMB     int      `json:"budget_mb"`
	FreeMB       int      `json:"free_mb"`
}

// buildRightsize computes the suggestion view for one app.
func (s *server) buildRightsize(ctx context.Context, app *store.App) (*rightsizeView, error) {
	cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
	if err != nil {
		return nil, fmt.Errorf("stored config for %q is invalid: %w", app.Name, err)
	}
	view := &rightsizeView{
		App:        app.Name,
		WindowDays: worker.RightsizeWindowDays,
		CurrentMB:  cfg.Resources.MemoryMB,
		Verdict:    verdictNoData,
		Confidence: "none",
		Reasons:    []string{},
	}
	budget, err := buildBudgetView(ctx, s.store)
	if err != nil {
		return nil, err
	}
	view.BudgetMB = budget.BudgetMB
	view.FreeMB = budget.FreeMB

	since := time.Now().UTC().AddDate(0, 0, -worker.RightsizeWindowDays)
	stats, err := s.store.MetricStatsFor(ctx, app.ID, since)
	if err != nil {
		return nil, err
	}
	view.Samples = stats.Samples
	if stats.Samples == 0 {
		view.Reasons = append(view.Reasons,
			fmt.Sprintf("no samples in the last %d days; let the sampler run first",
				worker.RightsizeWindowDays))
		return view, nil
	}
	peakMB := int((stats.PeakMemBytes + (1 << 20) - 1) >> 20)
	view.PeakMB = peakMB
	view.SuggestedMB = worker.SuggestMemoryMB(stats.PeakMemBytes)
	view.ObservedFrom = stats.FirstAt.Format(time.RFC3339)
	view.ObservedTo = stats.LastAt.Format(time.RFC3339)

	switch {
	case view.SuggestedMB < view.CurrentMB:
		view.Verdict = verdictLower
	case view.SuggestedMB > view.CurrentMB:
		view.Verdict = verdictRaise
	default:
		view.Verdict = verdictOK
	}

	span := stats.LastAt.Sub(stats.FirstAt)
	// The window filter caps the span at the window itself, and the
	// first row inside it may be an hourly rollup up to an hour after
	// the boundary — so maturity is the window minus that alignment
	// slack, not the exact window length.
	mature := span >= time.Duration(worker.RightsizeWindowDays)*24*time.Hour-time.Hour
	if mature {
		view.Confidence = "full"
	} else {
		view.Confidence = "low"
		view.Reasons = append(view.Reasons, fmt.Sprintf(
			"only %.1f of %d days observed — treat the suggestion as low confidence",
			span.Hours()/24, worker.RightsizeWindowDays))
	}
	view.Reasons = append(view.Reasons,
		fmt.Sprintf("observed peak %d MiB across %d samples (%s to %s)",
			peakMB, stats.Samples, stats.FirstAt.Format("2006-01-02"),
			stats.LastAt.Format("2006-01-02")),
		fmt.Sprintf("peak x1.25 = %d MiB, rounded up to %d MiB (never below the observed peak)",
			peakMB*worker.RightsizeMarginNum/worker.RightsizeMarginDen, view.SuggestedMB),
	)
	switch view.Verdict {
	case verdictLower:
		view.Reasons = append(view.Reasons, fmt.Sprintf(
			"current limit %d MiB is %d MiB above the suggestion",
			view.CurrentMB, view.CurrentMB-view.SuggestedMB))
	case verdictRaise:
		view.Reasons = append(view.Reasons, fmt.Sprintf(
			"current limit %d MiB sits %d MiB under the observed peak",
			view.CurrentMB, peakMB-view.CurrentMB))
	default:
		view.Reasons = append(view.Reasons,
			fmt.Sprintf("current limit %d MiB already matches the suggestion", view.CurrentMB))
	}
	return view, nil
}

// handleRightsize serves GET /apps/{name}/rightsize.
func (s *server) handleRightsize(w http.ResponseWriter, r *http.Request) {
	app, err := s.store.GetAppByName(r.Context(), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	view, err := s.buildRightsize(r.Context(), app)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleApplyRightsize serves POST /apps/{name}/rightsize: recompute,
// refuse, update, deploy.
func (s *server) handleApplyRightsize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app, err := s.store.GetAppByName(ctx, r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	view, err := s.buildRightsize(ctx, app)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	switch view.Verdict {
	case verdictNoData:
		writeError(w, http.StatusConflict, "E_NO_DATA",
			fmt.Sprintf("no samples in the last %d days for %q; nothing to apply",
				worker.RightsizeWindowDays, app.Name))
		return
	case verdictOK:
		writeError(w, http.StatusConflict, "E_NO_CHANGE",
			fmt.Sprintf("%q is already right-sized at %d MiB", app.Name, view.CurrentMB))
		return
	}
	pending, err := s.store.HasPendingJob(ctx, app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if pending {
		writeError(w, http.StatusConflict, "E_CONFLICT",
			fmt.Sprintf("a job is already queued or running for %q; wait for it to finish", app.Name))
		return
	}
	// The same budget arithmetic admission applies, before touching
	// anything: others' grants plus the suggestion must fit. Admission
	// re-checks at deploy time; this keeps a refused apply from ever
	// rewriting the config.
	settings, err := s.store.GetBudgetSettings(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	memTotal, err := worker.MemTotalMB()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	budgetMB := settings.BudgetMB
	if budgetMB == 0 {
		budgetMB = worker.AutoBudgetMB(memTotal)
	}
	running, err := s.store.ListRunningApps(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	committed := 0
	for _, other := range running {
		if other.ID == app.ID {
			continue
		}
		mb := appconfig.DefaultMemoryMB
		if otherCfg, err := appconfig.Parse([]byte(other.ConfigJSON)); err == nil {
			mb = otherCfg.Resources.MemoryMB
		}
		committed += mb
	}
	if over := committed + view.SuggestedMB - budgetMB; over > 0 {
		writeError(w, http.StatusConflict, "E_BUDGET_EXCEEDED", fmt.Sprintf(
			"suggested limit does not fit the host budget: %d MiB committed by other apps + "+
				"%d MiB suggested = %d MiB, budget %d MiB, over by %d MiB",
			committed, view.SuggestedMB, committed+view.SuggestedMB, budgetMB, over))
		return
	}

	cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
	if err != nil {
		writeError(w, http.StatusConflict, "E_VALIDATION",
			fmt.Sprintf("stored config for %q is invalid: %v", app.Name, err))
		return
	}
	oldMB := cfg.Resources.MemoryMB
	cfg.Resources.MemoryMB = view.SuggestedMB
	if _, err := appconfig.Validate(cfg); err != nil {
		writeError(w, http.StatusConflict, "E_VALIDATION", err.Error())
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if err := s.store.UpdateAppConfig(ctx, app.ID, string(raw)); err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	_ = s.store.AppendAudit(ctx, actorName(r), "app.rightsize", "app", app.ID, "applied", map[string]any{
		"old_memory_mb": oldMB, "new_memory_mb": view.SuggestedMB,
		"peak_mb": view.PeakMB, "confidence": view.Confidence,
	})

	// Re-read: the snapshot and version the deploy job carries must be
	// the updated config, not the one this request started from.
	app, err = s.store.GetAppByID(ctx, app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	dep, job, err := s.queueDeploy(ctx, app, cfg, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"deployment_id": dep.ID,
		"job_id":        job.ID,
		"seq":           dep.Seq,
		"state":         dep.State,
		"old_memory_mb": oldMB,
		"new_memory_mb": view.SuggestedMB,
	})
}
