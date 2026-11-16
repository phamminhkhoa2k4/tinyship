package api

// Notification configuration and event inspection: the sink list lives
// in database settings (never config.yaml), the operator reaches it
// through the CLI backed by these routes, and events are readable so a
// test delivery can be confirmed without watching a phone.

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tinyship/tinyship/internal/notify"
	"github.com/tinyship/tinyship/internal/store"
)

// handleNotifyConfig serves GET /notify: the configured sinks (with
// their signing secrets — the admin token is the confidentiality
// boundary) plus the supported kinds.
func (s *server) handleNotifyConfig(w http.ResponseWriter, r *http.Request) {
	raw, err := s.store.GetSetting(r.Context(), notify.SettingKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	sinks, err := notify.ParseSinks(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if sinks == nil {
		sinks = []notify.Sink{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sinks": sinks,
		"kinds": notify.Kinds,
	})
}

// handleAddNotifySink serves POST /notify/sinks: validate one sink and
// store it, replacing any existing sink of the same kind (one channel
// per kind keeps "remove" unambiguous). A webhook without a secret gets
// a fresh auto-generated one so it signs out of the box.
func (s *server) handleAddNotifySink(w http.ResponseWriter, r *http.Request) {
	var sink notify.Sink
	if err := readJSONBody(r, &sink); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	if err := sink.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	if sink.Kind == "webhook" && sink.Secret == "" {
		secret, err := notify.NewSecret()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
			return
		}
		sink.Secret = secret
	}
	raw, err := s.store.GetSetting(r.Context(), notify.SettingKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	sinks, err := notify.ParseSinks(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	replaced := false
	for i, existing := range sinks {
		if existing.Kind == sink.Kind {
			sinks[i] = sink
			replaced = true
			break
		}
	}
	if !replaced {
		sinks = append(sinks, sink)
	}
	encoded, err := notify.EncodeSinks(sinks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if err := s.store.PutSetting(r.Context(), notify.SettingKey, encoded); err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	// The sink list carries credentials, so every change is audited
	// with the field names only — never the values.
	_ = s.store.AppendAudit(r.Context(), actorName(r), "notify.sink.set",
		"setting", sink.Kind, "ok", map[string]any{"kind": sink.Kind})
	writeJSON(w, http.StatusOK, map[string]any{"sink": sink})
}

// handleRemoveNotifySink serves DELETE /notify/sinks/{kind}: drop every
// sink of that kind.
func (s *server) handleRemoveNotifySink(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	found := false
	raw, err := s.store.GetSetting(r.Context(), notify.SettingKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	sinks, err := notify.ParseSinks(raw)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	kept := sinks[:0]
	for _, sink := range sinks {
		if sink.Kind == kind {
			found = true
			continue
		}
		kept = append(kept, sink)
	}
	if !found {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND",
			fmt.Sprintf("no %q notification sink configured", kind))
		return
	}
	encoded, err := notify.EncodeSinks(kept)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if err := s.store.PutSetting(r.Context(), notify.SettingKey, encoded); err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	_ = s.store.AppendAudit(r.Context(), actorName(r), "notify.sink.remove",
		"setting", kind, "ok", map[string]any{"kind": kind})
	writeJSON(w, http.StatusOK, map[string]any{"removed": kind})
}

// handleTestNotify serves POST /notify/test: queue a test event the
// dispatcher delivers on its next tick. An optional target_kind limits
// delivery to one channel so each sink can be verified on its own.
func (s *server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TargetKind string `json:"target_kind"`
	}
	if r.ContentLength > 0 {
		if err := readJSONBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
			return
		}
	}
	if body.TargetKind != "" {
		valid := false
		for _, k := range notify.Kinds {
			if k == body.TargetKind {
				valid = true
				break
			}
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "E_VALIDATION",
				fmt.Sprintf("unknown target_kind %q (want %s)",
					body.TargetKind, joinKinds()))
			return
		}
	}
	detail := map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339),
	}
	if body.TargetKind != "" {
		detail["target_kind"] = body.TargetKind
	}
	id, err := s.store.EnqueueNotifyEvent(r.Context(), store.NotifyEventInput{
		Type:   "test",
		Title:  "Test notification",
		Detail: detail,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id})
}

// handleNotifyEvents serves GET /notify/events?n=N: the most recent
// events, newest first, so a test or a failure is confirmable.
func (s *server) handleNotifyEvents(w http.ResponseWriter, r *http.Request) {
	n := 20
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "E_VALIDATION",
				"n must be between 1 and 200")
			return
		}
		n = parsed
	}
	events, err := s.store.ListNotifyEvents(r.Context(), n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		out = append(out, notifyEventJSON(ev))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func joinKinds() string {
	s := ""
	for i, k := range notify.Kinds {
		if i > 0 {
			s += ", "
		}
		s += k
	}
	return s
}

// notifyEventJSON shapes one queue row for the API (and the CLI view).
func notifyEventJSON(ev *store.NotifyEvent) map[string]any {
	m := map[string]any{
		"id":           ev.ID,
		"type":         ev.Type,
		"app":          ev.AppName,
		"title":        ev.Title,
		"detail":       ev.Detail,
		"dup_count":    ev.DupCount,
		"status":       ev.Status,
		"attempts":     ev.Attempts,
		"sent_to":      ev.SentTo,
		"next_attempt": ev.NextAttemptAt.UTC().Format(time.RFC3339),
		"created_at":   ev.CreatedAt.UTC().Format(time.RFC3339),
		"last_error":   ev.LastError,
	}
	if ev.DeliveredAt != nil {
		m["delivered_at"] = ev.DeliveredAt.UTC().Format(time.RFC3339)
	}
	return m
}
