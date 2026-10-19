package api

// Rollback queues a deployment of the app's previous good release: the
// most recent succeeded (or superseded-by-a-good-one) deployment with a
// pinned digest. The new deployment carries that release's config
// snapshot and pulls by its digest; env vars stay live, so rolling back
// the image never rolls back the operator's current settings.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/store"
	"github.com/tinyship/tinyship/internal/worker"
)

// handleRollback serves POST /apps/{name}/rollback.
func (s *server) handleRollback(w http.ResponseWriter, r *http.Request) {
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

	// The target is the most recent good release other than the one
	// serving now; with nothing to go back to, say so plainly.
	var exclude string
	if app.CurrentDeploymentID != nil {
		exclude = *app.CurrentDeploymentID
	}
	target, err := s.store.FindRollbackSource(r.Context(), app.ID, exclude)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "E_NO_ROLLBACK_TARGET",
			fmt.Sprintf("app %q has no earlier successful deployment to roll back to", name))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if target.ImageDigest == nil || *target.ImageDigest == "" {
		writeError(w, http.StatusConflict, "E_NO_ROLLBACK_TARGET",
			fmt.Sprintf("deployment %s has no pinned digest; cannot redeploy it by digest", target.ID))
		return
	}

	// Rollback runs the target release's config with its digest pinned,
	// so the pipeline pulls exactly what ran before even if the tag has
	// moved or the local image was pruned.
	cfg, err := appconfig.Parse([]byte(target.ConfigSnapshotJSON))
	if err != nil {
		writeError(w, http.StatusConflict, "E_VALIDATION",
			fmt.Sprintf("stored config for deployment %s is invalid: %v", target.ID, err))
		return
	}
	cfg.Digest = *target.ImageDigest
	cfg.Tag = ""
	snapshot, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}

	// Named writable volumes keep their data across an image rollback;
	// the CLI repeats this warning and asks for confirmation.
	var warnings []string
	for _, v := range cfg.Volumes {
		if !v.ReadOnly {
			warnings = append(warnings,
				"W_VOLUME_DATA_RETAINED: named volume "+v.Name+
					" keeps its data; a rollback does not revert what the app wrote")
			break
		}
	}

	depID := store.NewID()
	// One transaction, as in handleDeploy: the job must never be
	// claimable before its deployment row exists.
	dep, job, err := s.store.CreateDeploymentJob(r.Context(),
		store.CreateDeploymentInput{
			ID:                   depID,
			AppID:                app.ID,
			Kind:                 "rollback",
			ImageRef:             cfg.Image + "@" + *target.ImageDigest,
			ConfigSnapshotJSON:   string(snapshot),
			ConfigVersion:        target.ConfigVersion,
			PreviousDeploymentID: app.CurrentDeploymentID,
			RollbackOfID:         &target.ID,
		},
		store.CreateJobInput{
			AppID:       &app.ID,
			Type:        worker.DeployJobType,
			PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, depID),
			CreatedBy:   "api",
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	if err := s.store.AppendAudit(r.Context(), actorName(r), "rollback", "deployment", dep.ID,
		"queued", map[string]any{
			"app": name, "rollback_of": target.ID, "rollback_of_seq": target.Seq,
		}); err != nil {
		s.logger.Error("audit rollback", "err", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"deployment_id":   dep.ID,
		"job_id":          job.ID,
		"seq":             dep.Seq,
		"state":           dep.State,
		"kind":            dep.Kind,
		"rollback_of":     target.ID,
		"rollback_of_seq": target.Seq,
		"image_ref":       dep.ImageRef,
		"warnings":        warnings,
	})
}
