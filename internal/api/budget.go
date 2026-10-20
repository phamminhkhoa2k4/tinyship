package api

// The host budget view: what apps may claim in total, what each running
// app currently holds, and what remains. The worker enforces the same
// numbers at admission time; this endpoint only reports them.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/worker"
)

// budgetGrant is one running app's memory grant in the budget view.
type budgetGrant struct {
	Name     string `json:"name"`
	MemoryMB int    `json:"memory_mb"`
}

// budgetView is the GET /budget response.
type budgetView struct {
	BudgetMB      int           `json:"budget_mb"`
	BudgetSource  string        `json:"budget_source"` // configured | auto
	MemTotalMB    int           `json:"mem_total_mb"`
	CommittedMB   int           `json:"committed_mb"`
	FreeMB        int           `json:"free_mb"`
	MinFreeDiskMB int           `json:"min_free_disk_mb"`
	CPUOvercommit float64       `json:"cpu_overcommit"`
	Apps          []budgetGrant `json:"apps"`
}

// buildBudgetView computes the view the budget endpoint serves. The
// worker's admission step applies the identical accounting, so a deploy
// accepted here will be admitted there.
func buildBudgetView(ctx context.Context, s *store.Store) (*budgetView, error) {
	settings, err := s.GetBudgetSettings(ctx)
	if err != nil {
		return nil, err
	}
	memTotal, err := worker.MemTotalMB()
	if err != nil {
		return nil, fmt.Errorf("read host memory: %w", err)
	}
	budget := settings.BudgetMB
	source := "configured"
	if budget == 0 {
		budget = worker.AutoBudgetMB(memTotal)
		source = "auto"
	}
	running, err := s.ListRunningApps(ctx)
	if err != nil {
		return nil, err
	}
	view := &budgetView{
		BudgetMB:      budget,
		BudgetSource:  source,
		MemTotalMB:    memTotal,
		MinFreeDiskMB: settings.MinFreeDiskMB,
		CPUOvercommit: settings.CPUOvercommit,
		Apps:          []budgetGrant{},
	}
	for _, a := range running {
		mb := appconfig.DefaultMemoryMB
		if cfg, err := appconfig.Parse([]byte(a.ConfigJSON)); err == nil {
			mb = cfg.Resources.MemoryMB
		}
		view.CommittedMB += mb
		view.Apps = append(view.Apps, budgetGrant{Name: a.Name, MemoryMB: mb})
	}
	view.FreeMB = budget - view.CommittedMB
	return view, nil
}

// handleBudget serves GET /budget.
func (s *server) handleBudget(w http.ResponseWriter, r *http.Request) {
	view, err := buildBudgetView(r.Context(), s.store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// setBudgetBody is the POST /budget request shape.
type setBudgetBody struct {
	// BudgetMB overrides the auto budget; 0 restores the auto
	// derivation from MemTotal.
	BudgetMB *int `json:"budget_mb"`
	// MinFreeDiskMB is the free-disk floor below which admission
	// refuses a deploy before pulling.
	MinFreeDiskMB *int `json:"min_free_disk_mb"`
}

// handleSetBudget serves POST /budget: the operator's explicit override
// of the host guardrails. It records an audit row because this changes
// what the admission guard will allow.
func (s *server) handleSetBudget(w http.ResponseWriter, r *http.Request) {
	var body setBudgetBody
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	if body.BudgetMB == nil && body.MinFreeDiskMB == nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION",
			"budget_mb or min_free_disk_mb is required")
		return
	}
	if body.BudgetMB != nil && *body.BudgetMB < 0 {
		writeError(w, http.StatusBadRequest, "E_VALIDATION",
			"budget_mb must be non-negative")
		return
	}
	if body.MinFreeDiskMB != nil &&
		(*body.MinFreeDiskMB < store.MinFreeDiskMBFloor ||
			*body.MinFreeDiskMB > store.MaxFreeDiskMB) {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", fmt.Sprintf(
			"min_free_disk_mb must be between %d and %d",
			store.MinFreeDiskMBFloor, store.MaxFreeDiskMB))
		return
	}
	detail := map[string]any{}
	if body.BudgetMB != nil {
		if err := s.store.SetBudgetMB(r.Context(), *body.BudgetMB); err != nil {
			writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
			return
		}
		detail["budget_mb"] = *body.BudgetMB
	}
	if body.MinFreeDiskMB != nil {
		if err := s.store.SetMinFreeDiskMB(r.Context(), *body.MinFreeDiskMB); err != nil {
			writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
			return
		}
		detail["min_free_disk_mb"] = *body.MinFreeDiskMB
	}
	if err := s.store.AppendAudit(r.Context(), actorName(r), "budget.set", "host", "",
		"ok", detail); err != nil {
		s.logger.Error("audit budget.set", "err", err)
	}
	view, err := buildBudgetView(r.Context(), s.store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// deployBody is the optional deploy request shape; an empty body means
// no overrides.
type deployBody struct {
	// Force admits the deploy past the RAM budget. It never bypasses
	// the free-disk floor or the security policy.
	Force bool `json:"force,omitempty"`
}

// readDeployBody decodes the optional deploy overrides. An absent or
// empty body is valid and yields zero values.
func readDeployBody(r *http.Request) (*deployBody, error) {
	if r.ContentLength == 0 {
		return &deployBody{}, nil
	}
	var body deployBody
	if err := readJSONBody(r, &body); err != nil {
		return nil, err
	}
	return &body, nil
}
