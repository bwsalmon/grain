package ui

import (
	"errors"
	"net/http"

	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

// errSecretsUnavailable is what every secrets handler reports when
// Config.Secrets is nil -- this deployment's Server was built with no
// local secrets directory to write to, as `grain demo` and this
// package's own tests do (bwsalmon/agents#357). Mapped to 404: there is
// no secrets resource here to act on, the same as any other name nothing
// exists behind.
var errSecretsUnavailable = errors.New(
	"secrets are not available: this deployment was not configured with a local secrets directory to write to")

// secretsResponse is GET /api/secrets' whole body, and what every
// mutation below re-fetches and returns afterward, the same
// respond-with-the-current-shape convention respondWithTask already
// uses. Enabled is false, with an empty Secrets list, when this
// deployment's UI has no local secrets directory configured -- the
// frontend uses that to hide the pane entirely rather than show controls
// that can only ever 404.
type secretsResponse struct {
	Enabled bool                 `json:"enabled"`
	Secrets []secrets.SecretInfo `json:"secrets"`
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.Secrets == nil {
		writeJSON(w, http.StatusOK, secretsResponse{Enabled: false, Secrets: []secrets.SecretInfo{}})
		return
	}
	s.respondWithSecrets(w)
}

type setSecretRequest struct {
	Value string `json:"value"`
}

// handleSetSecret is the UI's only way to put a value into a secret --
// and, per this package's own doc comment on Config.Secrets, the only
// direction a value ever travels through this package at all.
func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	store := s.tasks.Config.Secrets
	if store == nil {
		writeError(w, http.StatusNotFound, errSecretsUnavailable)
		return
	}
	var req setSecretRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, errors.New("value is required"))
		return
	}
	if err := store.Set(r.PathValue("secret"), r.PathValue("key"), []byte(req.Value)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.respondWithSecrets(w)
}

func (s *Server) handleDeleteSecretKey(w http.ResponseWriter, r *http.Request) {
	store := s.tasks.Config.Secrets
	if store == nil {
		writeError(w, http.StatusNotFound, errSecretsUnavailable)
		return
	}
	if err := store.DeleteKey(r.PathValue("secret"), r.PathValue("key")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.respondWithSecrets(w)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	store := s.tasks.Config.Secrets
	if store == nil {
		writeError(w, http.StatusNotFound, errSecretsUnavailable)
		return
	}
	if err := store.DeleteSecret(r.PathValue("secret")); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.respondWithSecrets(w)
}

func (s *Server) respondWithSecrets(w http.ResponseWriter) {
	list, err := s.tasks.Config.Secrets.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, secretsResponse{Enabled: true, Secrets: list})
}
