package api

// App teardown and domain detach endpoints. Both are asynchronous:
// the API validates, marks the durable intent, queues a worker job,
// and returns 202. The worker does the slow, retryable work.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/worker"
)

// handleDeleteApp marks an app for deletion and queues its teardown.
// Marking is synchronous (so admission stops counting it immediately);
// the container and route cleanup is the worker's job.
func (s *server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND",
			fmt.Sprintf("no such app %q", name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	// Requeueing teardown for an app already marked is idempotent: the
	// worker converges, and a client retrying a lost response should
	// not see a spurious conflict.
	if !app.Deleting {
		if err := s.store.MarkAppDeleting(r.Context(), app.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
			return
		}
	}

	job, err := s.store.CreateJob(r.Context(), store.CreateJobInput{
		AppID:       &app.ID,
		Type:        worker.AppDeleteJobType,
		PayloadJSON: fmt.Sprintf(`{"app_id":%q}`, app.ID),
		CreatedBy:   "api",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	_ = s.store.AppendAudit(r.Context(), actorName(r), "app.delete", "app", app.ID,
		"queued", map[string]any{"job_id": job.ID})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"app":    name,
		"job_id": job.ID,
		"message": "deletion queued; the app stops serving once the " +
			"worker removes its containers and routes",
	})
}

// handleDeleteDomain detaches one hostname and queues a route replay
// so the proxy stops routing it without waiting for a redeploy.
func (s *server) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "E_NOT_FOUND",
			fmt.Sprintf("no such app %q", name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	hostname := strings.ToLower(strings.TrimSpace(r.PathValue("hostname")))
	if err := s.store.DeleteDomain(r.Context(), app.ID, hostname); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "E_NOT_FOUND",
				fmt.Sprintf("app %q does not own hostname %q", name, hostname))
			return
		}
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	job, err := s.store.CreateJob(r.Context(), store.CreateJobInput{
		AppID:       &app.ID,
		Type:        worker.DomainSyncJobType,
		PayloadJSON: fmt.Sprintf(`{"app_id":%q}`, app.ID),
		CreatedBy:   "api",
	})
	if err != nil {
		// The row is already gone; failing to queue the replay would
		// leave the proxy routing a detached hostname until the next
		// deploy. Report the queue failure so the client can retry.
		writeError(w, http.StatusInternalServerError, "E_INTERNAL",
			fmt.Sprintf("domain removed but route sync could not be queued: %v", err))
		return
	}
	_ = s.store.AppendAudit(r.Context(), actorName(r), "domain.delete", "app", app.ID,
		"ok", map[string]any{"hostname": hostname, "job_id": job.ID})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"app":      name,
		"hostname": hostname,
		"job_id":   job.ID,
		"message": "hostname detached; the route drops once the worker " +
			"regenerates proxy config",
	})
}
