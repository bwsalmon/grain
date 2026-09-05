package ui

import (
	"errors"
	"net/http"
	"strconv"
)

// errRebootUnavailable is what handleRebootHost reports when
// Config.Reboot is nil -- see Config.Reboot's own doc comment.
var errRebootUnavailable = errors.New(
	"reboot is not available: this deployment has no reboot command configured")

// handleRebootHost is the UI's "reboot host" button (bwsalmon/agents#395):
// a human operator's way to reboot the machine grain itself is running
// on without reaching for a separate SSH session -- the same escape
// hatch v1's mcp_server.py already gave a task via `reboot_controller`,
// but triggered directly by the operator here rather than granted to a
// task as a capability.
//
// A successful reboot cuts this same process along with everything else
// on the machine, so a caller seeing the connection drop instead of a
// 200 response is the expected outcome, not a sign the call failed.
func (s *Server) handleRebootHost(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.Reboot == nil {
		writeError(w, http.StatusNotFound, errRebootUnavailable)
		return
	}
	if err := s.tasks.Config.Reboot(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// maxTopLines caps what a caller can ask for in one request regardless
// of ?lines=, the same guard maxLogLines gives the Logs pane beside it.
// There is no defaultTopLines to go with it: an absent ?lines= is passed
// on as 0, which Config.HostTop reads as "your own default"
// (hosttop.DefaultLines in a real deployment) rather than a second
// number spelled here that could drift from it.
const maxTopLines = 500

// hostTopResponse is GET /api/host/top's whole body. Enabled is false,
// with no lines, when this deployment's UI has no Config.HostTop -- the
// same nil-means-unavailable convention logSourcesResponse and
// sandboxHealthResponse already give the other panels of the System
// overlay this one is a tab of.
type hostTopResponse struct {
	Enabled bool     `json:"enabled"`
	Lines   []string `json:"lines,omitempty"`
}

// handleGetHostTop is the System overlay's Top tab: what is actually
// running on the machine this daemon runs on, right now. GET
// /api/sandboxes' own host section already says how loaded that machine
// is; this says by what, which is the question an operator asks next and
// which no aggregate reading can answer (pkg/hosttop's doc comment).
func (s *Server) handleGetHostTop(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.HostTop == nil {
		writeJSON(w, http.StatusOK, hostTopResponse{Enabled: false})
		return
	}
	lines := 0
	if raw := r.URL.Query().Get("lines"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("lines must be a positive integer"))
			return
		}
		lines = parsed
	}
	if lines > maxTopLines {
		lines = maxTopLines
	}
	out, err := s.tasks.Config.HostTop(r.Context(), lines)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, hostTopResponse{Enabled: true, Lines: out})
}
