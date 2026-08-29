package ui

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/upgrade"
)

// errUpgradeUnavailable is what every upgrade handler reports when
// Config.Upgrader is nil -- this deployment was not started with
// -upgrade-src-dir, which is the normal case for `grain demo`'s
// throwaway UI and for any deployment that has not opted in yet.
// Mapped to 404, matching errSecretsUnavailable's own reasoning: there
// is no upgrade resource here to act on.
var errUpgradeUnavailable = errors.New(
	"upgrading is not available: this deployment has no -upgrade-src-dir configured")

// upgradeStatusResponse is both GET /api/upgrade's whole body and what
// POST /api/upgrade returns once it has kicked an upgrade off. Enabled
// is false, with every other field its zero value, when this
// deployment's UI has no upgrader configured -- the frontend uses that
// to hide the pane entirely, the same convention secretsResponse's own
// Enabled already establishes.
type upgradeStatusResponse struct {
	Enabled    bool       `json:"enabled"`
	Branch     string     `json:"branch"`
	Phase      string     `json:"phase"`
	Detail     string     `json:"detail"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

func upgradeStatusResponseFrom(status upgrade.Status) upgradeStatusResponse {
	return upgradeStatusResponse{
		Enabled:    true,
		Branch:     status.Branch,
		Phase:      string(status.Phase),
		Detail:     status.Detail,
		StartedAt:  status.StartedAt,
		FinishedAt: status.FinishedAt,
	}
}

func (s *Server) handleGetUpgradeStatus(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.Upgrader == nil {
		writeJSON(w, http.StatusOK, upgradeStatusResponse{Enabled: false})
		return
	}
	status, err := s.tasks.Config.Upgrader.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, upgradeStatusResponseFrom(status))
}

type startUpgradeRequest struct {
	Branch string `json:"branch"`
}

// handleStartUpgrade kicks off an upgrade and returns at once -- it does
// not wait for the checkout, build, install or restart it triggers to
// finish (the restart, on a real deployment, ends this very process
// before that could ever happen). GET /api/upgrade is how a caller
// watches it land.
func (s *Server) handleStartUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.Upgrader == nil {
		writeError(w, http.StatusNotFound, errUpgradeUnavailable)
		return
	}
	var req startUpgradeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Branch) == "" {
		writeError(w, http.StatusBadRequest, errors.New("branch is required"))
		return
	}
	if err := s.tasks.Config.Upgrader.Start(req.Branch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, err := s.tasks.Config.Upgrader.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, upgradeStatusResponseFrom(status))
}
