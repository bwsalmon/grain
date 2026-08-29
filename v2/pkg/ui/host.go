package ui

import (
	"errors"
	"net/http"
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
