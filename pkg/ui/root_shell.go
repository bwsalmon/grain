package ui

import (
	"errors"
	"net/http"
	"strings"
)

// errRootShellUnavailable is what handleRootShell reports when
// Config.RootShell is nil -- see that field's own doc comment, and
// errRebootUnavailable beside it for the same shape of answer.
var errRootShellUnavailable = errors.New(
	"the root shell is not available: this deployment has no root shell configured")

// maxRootShellCommand caps one command's length. Nothing on the far end
// cares -- it is written to a file and handed to bash -- but this route
// accepts an arbitrary string from a browser and hands it to root, and
// an unbounded one is a body a client can make as large as it likes. A
// command an operator types into a debugging pane is a line or two; this
// is orders of magnitude past that and still not a limit anybody meets.
const maxRootShellCommand = 64 << 10

// RootShellResult is what one command left behind: what it printed, and
// what it exited with. It mirrors rootshell.Result field for field
// rather than being it, so this package keeps importing nothing that
// runs anything -- the same separation Config.HostTop's own func
// signature keeps between here and pkg/hosttop.
type RootShellResult struct {
	// Output is stdout and stderr interleaved, in the order the command
	// wrote them.
	Output string
	// ExitCode is the command's own status. Non-zero is an answer, not an
	// error: a `grep` that found nothing is a perfectly good thing for
	// the pane to show.
	ExitCode int
}

// rootShellRequest is POST /api/host/shell's body: the command line to
// run, exactly as an operator typed it, for the far end's own `bash -lc`.
// One string rather than an argv because that is what a shell prompt is
// -- pipelines, redirections and `&&` are most of why the pane is open.
type rootShellRequest struct {
	Command string `json:"command"`
}

// rootShellResponse is what came back. ExitCode rides alongside the
// output rather than being folded into an HTTP status: the request
// succeeded either way, and "the command exited 1" is an answer this
// pane shows rather than a failure of the call.
type rootShellResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

// handleRootShell is the Debug overlay's Root shell tab: one command,
// run as root on the machine this daemon runs on, and everything it
// printed.
//
// It is the last resort of the Debug overlay's panes, and shaped like
// one. Logs, Top and Sandbox health each answer a fixed question well;
// this answers whatever question is left when none of them covered it,
// on a deployment whose operator may have no other way in at all -- no
// SSH to a machine that stopped accepting it, no console on a VM in
// somebody else's cloud project. That reach is the point and the cost:
// what is on the other end of this route is root on the host, which is
// strictly more than every other route in this package put together.
// See Config.RootShell for the gate, and pkg/rootshell for the channel
// it goes through.
func (s *Server) handleRootShell(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.RootShell == nil {
		writeError(w, http.StatusNotFound, errRootShellUnavailable)
		return
	}
	var req rootShellRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeError(w, http.StatusBadRequest, errors.New("command is required"))
		return
	}
	if len(req.Command) > maxRootShellCommand {
		writeError(w, http.StatusBadRequest, errors.New("command is too long"))
		return
	}
	out, err := s.tasks.Config.RootShell(r.Context(), req.Command)
	if err != nil {
		// The exchange itself failed -- no responder on this host, an
		// unwritable control directory. That is a fact about the
		// deployment rather than about the command, and the pane shows
		// it as an error rather than as output.
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rootShellResponse{Output: out.Output, ExitCode: out.ExitCode})
}
