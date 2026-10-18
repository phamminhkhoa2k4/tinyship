package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
	"github.com/tinyship/tinyship/internal/secrypto"
	"github.com/tinyship/tinyship/internal/store"
)

// envBody is the write shape for both plain values and secrets.
type envBody struct {
	Value string `json:"value"`
	Kind  string `json:"kind"` // plain (default) | secret
}

// envDTO is one variable as the API reports it: a secret keeps its
// name and timestamp, never its value, in any response.
type envDTO struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	Value     *string   `json:"value,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// handleListEnv lists an app's variables. Secrets appear as names
// only: the store holds them sealed and this process could not read
// them even if it wanted to.
func (s *server) handleListEnv(w http.ResponseWriter, r *http.Request) {
	app, err := s.appByName(w, r)
	if err != nil {
		return
	}
	vars, err := s.store.ListAppEnv(r.Context(), app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	dtos := make([]envDTO, 0, len(vars))
	for _, v := range vars {
		d := envDTO{Key: v.Key, Kind: v.Kind, UpdatedAt: v.UpdatedAt}
		if v.Kind == store.EnvPlain {
			val := v.ValuePlain
			d.Value = &val
		}
		dtos = append(dtos, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": dtos})
}

// handleSetEnv upserts one variable. A secret is sealed here with the
// public key; the clear value exists only in this request.
func (s *server) handleSetEnv(w http.ResponseWriter, r *http.Request) {
	app, err := s.appByName(w, r)
	if err != nil {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	var body envBody
	if err := readJSONBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}
	kind := body.Kind
	if kind == "" {
		kind = store.EnvPlain
	}
	if kind != store.EnvPlain && kind != store.EnvSecret {
		writeError(w, http.StatusBadRequest, "E_VALIDATION",
			fmt.Sprintf("kind %q must be %q or %q", kind, store.EnvPlain, store.EnvSecret))
		return
	}
	// Validation checks the clear shape either way: a secret is a clear
	// string at this point and gets the same key and size rules.
	if err := appconfig.ValidateEnv(key, body.Value); err != nil {
		writeError(w, http.StatusBadRequest, "E_VALIDATION", err.Error())
		return
	}

	vars, err := s.store.ListAppEnv(r.Context(), app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	exists := false
	for _, v := range vars {
		if v.Key == key {
			exists = true
			break
		}
	}
	if !exists && len(vars) >= appconfig.MaxEnvVars {
		writeError(w, http.StatusConflict, "E_VALIDATION",
			fmt.Sprintf("app already has %d variables; the limit is %d", len(vars), appconfig.MaxEnvVars))
		return
	}

	var sealed []byte
	if kind == store.EnvSecret {
		pub, err := s.publicKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "E_SECRETS_KEY", err.Error())
			return
		}
		if sealed, err = secrypto.Seal(pub, []byte(body.Value)); err != nil {
			writeError(w, http.StatusInternalServerError, "E_SECRETS_KEY", err.Error())
			return
		}
	}
	// A secret must reach the store with no clear value at all: the
	// sealed payload is the only representation that leaves this handler.
	valuePlain := body.Value
	if kind == store.EnvSecret {
		valuePlain = ""
	}
	if err := s.store.SetAppEnv(r.Context(), app.ID, key, kind, valuePlain, sealed); err != nil {
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	// The audit detail carries the key and kind, never the value.
	_ = s.store.AppendAudit(r.Context(), "api", "env.set", "app", app.ID,
		"ok", map[string]any{"key": key, "kind": kind})

	detail := "takes effect on the next deploy"
	if kind == store.EnvSecret {
		detail = "secret stored sealed; takes effect on the next deploy"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key": key, "kind": kind, "message": detail,
	})
}

// handleUnsetEnv removes one variable.
func (s *server) handleUnsetEnv(w http.ResponseWriter, r *http.Request) {
	app, err := s.appByName(w, r)
	if err != nil {
		return
	}
	key := r.PathValue("key")
	if err := s.store.UnsetAppEnv(r.Context(), app.ID, key); err != nil {
		if errors.Is(err, store.ErrEnvNotFound) {
			writeError(w, http.StatusNotFound, "E_NOT_FOUND",
				fmt.Sprintf("app %q has no variable %q", app.Name, key))
			return
		}
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return
	}
	_ = s.store.AppendAudit(r.Context(), "api", "env.unset", "app", app.ID,
		"ok", map[string]any{"key": key})
	writeJSON(w, http.StatusOK, map[string]any{
		"key": key, "message": "takes effect on the next deploy",
	})
}

// publicKey loads the sealing public key once and caches it. It is
// only needed when a secret is written; plain env and public deploys
// work without any key material present.
func (s *server) publicKey() ([]byte, error) {
	if s.pubKey != nil {
		return s.pubKey, nil
	}
	key, err := secrypto.LoadPublicKey(s.pubKeyPath)
	if err != nil {
		return nil, fmt.Errorf("sealing key %s: %w (generate it with `tinyship keygen`)",
			s.pubKeyPath, err)
	}
	s.pubKey = key
	return key, nil
}

// appByName resolves the {name} path segment once per handler.
func (s *server) appByName(w http.ResponseWriter, r *http.Request) (*store.App, error) {
	name := r.PathValue("name")
	app, err := s.store.GetAppByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "E_NOT_FOUND", fmt.Sprintf("no such app %q", name))
			return nil, err
		}
		writeError(w, http.StatusInternalServerError, "E_INTERNAL", err.Error())
		return nil, err
	}
	return app, nil
}
