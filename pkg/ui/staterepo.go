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
	// SecretsPublicKey is the public half of the key this host holds, and
	// SecretsKeyFile where its private half is read from -- the file an
	// operator has to back up.
	SecretsPublicKey string `json:"secretsPublicKey,omitempty"`
	SecretsKeyFile   string `json:"secretsKeyFile,omitempty"`
	// SecretsFileRecipient is the public key this host's secrets file is
	// actually sealed to, which is not always the key above: a file
	// restored from another installation is encrypted to that
	// installation's key. Both are shown because the difference is the
	// diagnosis, and a public key reveals nothing -- it can seal a
	// secret, not open one.
	SecretsFileRecipient string `json:"secretsFileRecipient,omitempty"`
	// SecretsError says why this host cannot read its own secrets file,
	// and is empty when it can. Separate from Error below because the two
	// have different fixes -- one is a key to import, the other a remote
	// or a credential -- and because a store that cannot be read is not a
	// sync that failed.
	SecretsError string `json:"secretsError,omitempty"`
	// RemoteAhead reports that the remote holds changes this deployment
	// has not loaded -- a merged pull request against grain's settings,
	// which is what the repository is for. It is its own field rather
	// than another Error because it is not a failure and the operator's
	// move is different: an ordinary tick pulls a merge down and makes
	// its settings live by itself, so this says the pull did not get that
	// far -- grain has stopped exporting rather than committing over the
	// merge, and what loads what is waiting is the next start.
	RemoteAhead bool `json:"remoteAhead,omitempty"`
	// Diverged reports that this deployment's working tree and its remote
	// have both moved past their common parent, and that grain could not
	// resolve it: nothing is being pulled in and nothing is being pushed
	// out until somebody does. Its own field, again, because it is the
	// one condition here that needs a human at a terminal -- grain clears
	// a divergence made only of its own exports by itself, without ever
	// reaching this pane, so a deployment that reports one is reporting a
	// commit that is somebody's to resolve. Error, alongside, says which.
	Diverged bool `json:"diverged,omitempty"`
	// Error is a last-sync failure worth showing (an expired credential,
	// an unreachable remote), rather than one this pane's own request
	// caused. Empty when the last sync was fine.
	Error string `json:"error,omitempty"`
}

// AdoptRequest is everything "point this installation at a repository"
// takes: where it is, and the two credentials that are nobody else's to
// supply -- one to push it, one to open the secrets file this host was
// restored with. A struct rather than four positional strings because
// three of them are optional and two of them are secret, and a call site
// that got the order wrong would put a private key where a push token
// goes.
type AdoptRequest struct {
	// Remote is the git URL, and the only required field.
	Remote string
	// Branch is the branch state lives on; the package default if empty.
	Branch string
	// Token is the credential to push with, for a repository this
	// deployment's own GitHub credential ladder does not cover. Empty
	// leaves that ladder to answer.
	Token string
	// SecretsKey is the private key this host's secrets file is sealed
	// to, in the form pkg/secrets renders. Empty keeps the key already
	// here, which is right for an installation that has always run on
	// this host and wrong for one whose <data-dir>/secrets was restored
	// from a deployment that ran somewhere else.
	SecretsKey string
}

// StateRepoManager is the daemon's side of the bootstrap. cmd/grain
// implements it over pkg/staterepo; this package holds only the shape,
// so pkg/ui carries no git and no database dump of its own.
type StateRepoManager interface {
	// Status reports where state lives right now.
	Status(ctx context.Context) (StateRepoStatus, error)
	// UseLocal drops the remote, keeping every commit already made.
	UseLocal(ctx context.Context) (StateRepoStatus, error)
	// Adopt points the installation at req.Remote. A repository that
	// already holds a dump replaces this installation's database with it;
	// an empty one is seeded from the database instead.
	Adopt(ctx context.Context, req AdoptRequest) (StateRepoStatus, error)
	// ImportSecretsKey installs a private key the operator holds, so a
	// secrets file sealed to another installation's key becomes readable
	// on this host. It refuses a key that cannot open the file rather
	// than installing it and failing later.
	ImportSecretsKey(ctx context.Context, key string) (StateRepoStatus, error)
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
	// SecretsKey is the private key of the installation being restored
	// onto this host, for the case where its secrets file was sealed to a
	// key this host does not hold. Write-only in the same sense Token is.
	SecretsKey string `json:"secretsKey"`
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
		status, err = s.tasks.Config.StateRepo.Adopt(r.Context(), AdoptRequest{
			Remote:     strings.TrimSpace(req.Remote),
			Branch:     strings.TrimSpace(req.Branch),
			Token:      strings.TrimSpace(req.Token),
			SecretsKey: strings.TrimSpace(req.SecretsKey),
		})
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

// secretsKeyRequest is POST /api/state-repo/secrets-key's body: the
// private key itself, which goes to the daemon and is never read back
// out through here -- no route in this package returns one, and Status
// carries only public halves.
type secretsKeyRequest struct {
	Key string `json:"key"`
}

// handleImportSecretsKey installs the operator's own secrets key.
//
// This is the third input the bootstrap's "point grain at an existing
// repository" needs, for a deployment that arrived here from another
// host: the clone brings the tables and a restored <data-dir>/secrets
// brings secrets.enc, and nothing here can open that until the key that
// sealed it arrives by hand. It is a route of its own as well as a field
// on adopt, because the two happen at different times -- a repository
// can be adopted before its operator has gone and fetched the key out of
// wherever they keep it.
func (s *Server) handleImportSecretsKey(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.StateRepo == nil {
		writeError(w, http.StatusNotImplemented, errors.New("this deployment's UI does not manage a state repository"))
		return
	}
	var req secretsKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, errors.New("a key is required"))
		return
	}
	status, err := s.tasks.Config.StateRepo.ImportSecretsKey(r.Context(), strings.TrimSpace(req.Key))
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
