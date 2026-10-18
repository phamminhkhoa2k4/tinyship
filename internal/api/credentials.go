package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/secrypto"
	"github.com/tinyship/tinyship/internal/store"
)

// credNameRe bounds credential names: they appear in app configs, so
// they follow the same lowercase shape as app names.
var credNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)

// credentialBody is the create/update shape. Token arrives clear over
// the loopback API, gets sealed immediately, and never round-trips.
type credentialBody struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// credentialDTO is the response shape: no token, sealed or not.
type credentialDTO struct {
	Name      string    `json:"name"`
	Registry  string    `json:"registry"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// handleCreateCredential stores or rotates a registry login.
func (s *server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var body credentialBody
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if !credNameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, "E_VALIDATION",
			fmt.Sprintf("name %q must match %s (3-40 chars, lowercase)", name, credNameRe.String()))
		return
	}
	if strings.TrimSpace(body.Registry) == "" {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", "registry: required")
		return
	}
	if strings.TrimSpace(body.Username) == "" {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", "username: required")
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "E_VALIDATION",
			"token: required (send it in the request body, never as a flag)")
		return
	}

	pub, err := s.publicKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_SECRETS_KEY", err.Error())
		return
	}
	sealed, err := secrypto.Seal(pub, []byte(body.Token))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_SECRETS_KEY", err.Error())
		return
	}
	cred, err := s.store.UpsertCredential(r.Context(), name,
		strings.TrimSpace(body.Registry), strings.TrimSpace(body.Username), sealed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	// Rotate and create share one action; the detail names what changed
	// and never the token.
	_ = s.store.AppendAudit(r.Context(), actorName(r), "credential.set", "credential", cred.ID,
		"ok", map[string]any{"name": name, "registry": cred.Registry})
	writeJSON(w, http.StatusOK, map[string]any{
		"credential": credentialDTO{
			Name: cred.Name, Registry: cred.Registry, Username: cred.Username,
			CreatedAt: cred.CreatedAt, UpdatedAt: cred.UpdatedAt,
		},
		"message": "token stored sealed; it is never returned by the API",
	})
}

// handleListCredentials lists logins without tokens.
func (s *server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.store.ListCredentials(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	dtos := make([]credentialDTO, 0, len(creds))
	for _, c := range creds {
		dtos = append(dtos, credentialDTO{
			Name: c.Name, Registry: c.Registry, Username: c.Username,
			CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": dtos})
}

// handleDeleteCredential removes a login that no app references.
func (s *server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.DeleteCredential(r.Context(), name); err != nil {
		switch {
		case errors.Is(err, store.ErrCredentialNotFound):
			writeError(w, http.StatusNotFound, "E_NOT_FOUND",
				fmt.Sprintf("no credential named %q", name))
		case errors.Is(err, store.ErrCredentialInUse):
			writeError(w, http.StatusConflict, "E_CONFLICT",
				fmt.Sprintf("%v; drop registry_credential from those apps first", err))
		default:
			writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		}
		return
	}
	_ = s.store.AppendAudit(r.Context(), actorName(r), "credential.delete", "credential", "",
		"ok", map[string]any{"name": name})
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}
