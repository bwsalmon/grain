package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// The bootstrap: where this installation's state actually lives.
//
// grain runs out of a git repository now (pkg/staterepo) -- its database
// exported as text, so settings can be changed through pull requests like
// anything else -- and there are exactly three answers to where that
// repository is. It is local-only, needing no account, no credential and
// no input from anybody; or it is a repository that already holds a
// grain installation's state, which this one adopts; or it is an empty
// repository grain seeds from what it already has. The last two are one
// operation, because "adopt" cannot tell them apart up front and does not
// need to: an empty repository is seeded, a populated one is imported.
//
// These handlers are that choice, and nothing more. They deliberately do
// not offer a way to *edit* the repository's contents: that is what the
// repository is for, and a UI that also wrote rows behind grain's back
// would be a second source of truth for the thing this whole design
// exists to make single.
//
// Config.StateRepo nil means this UI is not colocated with a daemon that
// owns one (`grain demo`, or any UI serving a store it does not manage),
// and every route below reports itself unavailable rather than erroring
// on each call -- the same nil-means-unavailable contract Secrets,
// Reboot and Upgrader already give their own panes.

// StateRepoStatus is what the bootstrap pane renders: where state lives,
// what is in the repository, and which key its secrets are encrypted to.
// No secret value and no private key is ever in here -- the public key is
// safe to show, and showing it is how an operator confirms the key they
// hold is the key this deployment encrypts to.
type StateRepoStatus struct {
	// Available is false when this UI has no state repository to manage.
	Available bool `json:"available"`
	// Mode is "local" or "remote", which is the whole of the choice.
	Mode string `json:"mode"`
	// Remote is the git URL, empty in local mode.
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Dir is the working tree on this host, for an operator who wants to
	// go and look at it.
	Dir string `json:"dir,omitempty"`
	// Head is the commit the working tree is on, empty before the first
	// commit exists.
	Head string `json:"head,omitempty"`
	// SchemaVersion is the schema the repository's dump was written by,
	// and BuildSchemaVersion the one this build knows. They differ only
	// when something is wrong, which is why both are reported rather than
	// a single "compatible" boolean that hides which way round it is.
	SchemaVersion      int `json:"schemaVersion"`
	BuildSchemaVersion int `json:"buildSchemaVersion"`
	// SecretsPublicKey is the public half of the key the secrets file is
	// encrypted to, and SecretsKeyFile where its private half is read
	// from on this host -- the file an operator has to back up.
	SecretsPublicKey string `json:"secretsPublicKey,omitempty"`
	SecretsKeyFile   string `json:"secretsKeyFile,omitempty"`
	// Error is a last-sync failure worth showing (an expired credential,
	// an unreachable remote), rather than one this pane's own request
	// caused. Empty when the last sync was fine.
	Error string `json:"error,omitempty"`
}

// StateRepoManager is the daemon's side of the bootstrap. cmd/grain
// implements it over pkg/staterepo; this package holds only the shape,
// so pkg/ui carries no git and no database dump of its own.
type StateRepoManager interface {
	// Status reports where state lives right now.
	Status(ctx context.Context) (StateRepoStatus, error)
	// UseLocal drops the remote, keeping every commit already made.
	UseLocal(ctx context.Context) (StateRepoStatus, error)
	// Adopt points the installation at remote. A repository that already
	// holds a dump replaces this installation's database with it; an empty
	// one is seeded from the database instead. token, when given, is the
	// credential to push with, for a repository this deployment's own
	// GitHub credential ladder does not cover.
	Adopt(ctx context.Context, remote, branch, token string) (StateRepoStatus, error)
	// Sync runs the daemon's own sync cycle now, rather than at the next
	// tick: pull whatever was merged and make its settings live, then
	// export, commit and push. Both directions, because an operator who
	// has just merged a change presses this to stop waiting for it.
	Sync(ctx context.Context) (StateRepoStatus, error)
}

func (s *Server) handleGetStateRepo(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.StateRepo == nil {
		writeJSON(w, http.StatusOK, StateRepoStatus{Available: false})
		return
	}
	status, err := s.tasks.Config.StateRepo.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status.Available = true
	writeJSON(w, http.StatusOK, status)
}

// stateRepoRequest is POST /api/state-repo's body: the mode chosen, and
// what that mode needs. A token, when one is given, is write-only in the
// same sense every secret this package handles is -- it goes to the
// daemon and is never read back out through here.
type stateRepoRequest struct {
	Mode   string `json:"mode"`
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Token  string `json:"token"`
}

// handleSetStateRepo is the bootstrap's one write.
//
// Adopting is destructive to this installation's database when the
// repository already holds one -- that is what adopting means, and
// bwsalmon/grain#174 says so outright -- so it is a deliberate POST from
// a pane that says as much, not something any other action does as a
// side effect.
func (s *Server) handleSetStateRepo(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.StateRepo == nil {
		writeError(w, http.StatusNotImplemented, errors.New("this deployment's UI does not manage a state repository"))
		return
	}
	var req stateRepoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	var (
		status StateRepoStatus
		err    error
	)
	switch strings.TrimSpace(req.Mode) {
	case "local":
		status, err = s.tasks.Config.StateRepo.UseLocal(r.Context())
	case "remote":
		if strings.TrimSpace(req.Remote) == "" {
			writeError(w, http.StatusBadRequest, errors.New("a remote is required to adopt a repository"))
			return
		}
		status, err = s.tasks.Config.StateRepo.Adopt(r.Context(),
			strings.TrimSpace(req.Remote), strings.TrimSpace(req.Branch), strings.TrimSpace(req.Token))
	default:
		writeError(w, http.StatusBadRequest, errors.New(`mode must be "local" or "remote"`))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status.Available = true
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleSyncStateRepo(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.StateRepo == nil {
		writeError(w, http.StatusNotImplemented, errors.New("this deployment's UI does not manage a state repository"))
		return
	}
	status, err := s.tasks.Config.StateRepo.Sync(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	status.Available = true
	writeJSON(w, http.StatusOK, status)
}
