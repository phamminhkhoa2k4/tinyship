package api

// The hibernation toggle: POST /apps/{name}/hibernate flips an app
// between hibernating and always-awake without a redeploy — the flag
// changes when the sleep sweep may stop the container and which
// upstream the proxy dials, both of which the worker derives from the
// stored config on its next pass. Turning it off while the app sleeps
// deliberately leaves the wake path in place: the next request wakes
// the app through the relay, and the sweep then retires the machine.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/store"
)

// hibernateBody is the toggle request shape; an absent enabled field
// inverts the current value.
type hibernateBody struct {
	Enabled *bool `json:"enabled"`
}

// handleHibernate serves POST /apps/{name}/hibernate.
func (s *server) handleHibernate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND", "no such app")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	var body hibernateBody
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	cfg, err := appconfig.Parse([]byte(app.ConfigJSON))
	if err != nil {
		writeError(w, http.StatusConflict, "E_CONFLICT",
			fmt.Sprintf("app %q has an unreadable config; redeploy it", name))
		return
	}
	// Absent means "flip it": the endpoint exists to toggle, and an
	// explicit value is how a caller states the state it wants.
	enabled := !cfg.HibernateEnabled()
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	cfg.Hibernate.Enabled = &enabled
	if _, err := appconfig.Validate(cfg); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if err := s.store.UpdateAppConfig(r.Context(), app.ID, string(raw)); err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if err := s.store.AppendAudit(r.Context(), actorName(r), "app.hibernate", "app",
		app.ID, "updated", map[string]any{"app": name, "enabled": enabled}); err != nil {
		s.logger.Error("audit app.hibernate", "err", err)
	}
	updated, err := s.store.GetAppByID(r.Context(), app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, appToDTO(updated, true, nil))
}
