package ui

// The deployment-wide "the agent has no budget left in this window" gate
// (orchestrator.Pause), as an operator sees it: GET /api/config carries
// it so a banner can stand on every page, GET /api/pause is the same
// answer on its own for anything polling it, and DELETE /api/pause is
// the operator's own override.
//
// It is deliberately *not* a section of GET /api/metrics. That report is
// computed over rows for a window that has ended -- nothing in it
// changes between two calls a second apart with no work in between --
// and a pause is a gauge of what this process is doing right now, with
// nothing in any table behind it. The one part of that report already in
// that position, the cycles section, is there because it is an input to
// the same metrics.Compute the rest comes from (Config.Cycles); a pause
// is not, and putting it there would only mean an operator learning to
// look for "why is nothing dispatching?" in a latency report.

import (
	"errors"
	"net/http"
	"time"
)

// AgentPause is implemented by the deployment-wide dispatch gate an
// agent's own usage limit closes -- *orchestrator.Pause itself, in a real
// deployment, satisfied structurally rather than named there: this
// package does not import pkg/orchestrator (see sandbox_health.go's own
// doc comment on why), and unlike SandboxHealth there is no shape to
// convert here, only two methods this API forwards.
type AgentPause interface {
	// Until is the instant dispatch resumes and the provider's own
	// sentence about why it stopped. A zero time means nothing is
	// paused.
	//
	// This is Pause.Until rather than Pause.Blocked on purpose: Blocked
	// clears a pause whose instant has passed, as the reconcile loop's
	// own read, and a UI poll must not be able to open the gate out from
	// under the loop that owns it. A pause whose instant is in the past
	// is one nothing has asked about since it expired, and reads here as
	// not paused.
	Until() (time.Time, string)
	// Lift clears a pause early, reporting whether there was one to
	// clear. See Pause.Lift for what an operator is actually buying with
	// it.
	Lift() bool
}

// AgentPauseStatus is one reading of that gate: what GET /api/pause
// reports, and what rides on GET /api/config as agentPause.
//
// SecondsRemaining is derived rather than left to the caller so that a
// browser with a wrong clock -- the usual case for "in about four hours"
// -- still counts down from the right number. Until is sent beside it
// because a wall-clock time is what an operator plans around, and a
// remaining duration alone cannot be turned back into one.
type AgentPauseStatus struct {
	Paused bool `json:"paused"`
	// Until and Reason are empty when Paused is false: there is no
	// window and no provider sentence to report.
	Until            time.Time `json:"until"`
	Reason           string    `json:"reason,omitempty"`
	SecondsRemaining float64   `json:"secondsRemaining"`
}

// agentPauseResponse is GET /api/pause's and DELETE /api/pause's whole
// body. Enabled is false, with no status at all, when this UI was handed
// no gate to ask (Config.AgentPause nil) -- the same
// nil-means-unavailable convention sandboxHealthResponse.Enabled and
// MetricsCycles.Enabled already give, and a different answer from a
// deployment that simply is not paused.
type agentPauseResponse struct {
	Enabled bool              `json:"enabled"`
	Pause   *AgentPauseStatus `json:"pause,omitempty"`
	// Lifted is DELETE /api/pause's own report: whether that call is what
	// ended a pause, as opposed to finding none to end. Absent on a GET.
	Lifted *bool `json:"lifted,omitempty"`
}

// errAgentPauseUnavailable is what the pause routes report when
// Config.AgentPause is nil -- see that field's own doc comment.
var errAgentPauseUnavailable = errors.New(
	"the agent usage-limit pause is not available: this deployment's UI is not wired to a reconcile loop that has one")

// AgentPause reports whether dispatch is currently gated by an agent's
// usage limit, and until when. ok is false when this UI has no gate to
// ask at all, which is not the same as an unpaused one.
func (c *Client) AgentPause() (status AgentPauseStatus, ok bool) {
	if c.Config.AgentPause == nil {
		return AgentPauseStatus{}, false
	}
	return agentPauseStatus(c.Config.AgentPause, c.now()), true
}

// LiftAgentPause clears the current pause by hand, reporting the state
// it left behind and whether there was a pause to clear. An operator who
// has topped a plan up, or moved the deployment onto another agent
// framework, is holding information this process cannot have -- see
// Pause.Lift.
func (c *Client) LiftAgentPause() (status AgentPauseStatus, lifted bool, err error) {
	if c.Config.AgentPause == nil {
		return AgentPauseStatus{}, false, errAgentPauseUnavailable
	}
	lifted = c.Config.AgentPause.Lift()
	return agentPauseStatus(c.Config.AgentPause, c.now()), lifted, nil
}

// agentPauseStatus turns a gate's own two values into the wire shape. A
// pause whose instant has already passed reads as not paused: it is one
// the reconcile loop has not looked at since it expired (Pause.Blocked
// is what clears it, and only that loop calls it), and reporting it
// would tell an operator dispatch is stopped when the very next tick
// resumes it.
func agentPauseStatus(pause AgentPause, now time.Time) AgentPauseStatus {
	until, reason := pause.Until()
	if until.IsZero() || !now.Before(until) {
		return AgentPauseStatus{}
	}
	return AgentPauseStatus{
		Paused:           true,
		Until:            until.UTC(),
		Reason:           reason,
		SecondsRemaining: until.Sub(now).Seconds(),
	}
}

func (s *Server) handleGetAgentPause(w http.ResponseWriter, r *http.Request) {
	status, ok := s.tasks.AgentPause()
	if !ok {
		writeJSON(w, http.StatusOK, agentPauseResponse{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, agentPauseResponse{Enabled: true, Pause: &status})
}

// handleLiftAgentPause answers DELETE /api/pause.
//
// An unwired deployment answers 404 -- "this deployment does not offer
// that" -- rather than the 200-with-enabled-false a GET gives: reporting
// success on an action that did nothing is worse than saying there is
// nothing here to act on, which is the same line handleRecreateSandbox
// draws for its own missing feature.
//
// Lifting a pause that is not there is not an error, though. The button
// that sends this is on a banner an operator and an expiring window are
// both racing to clear, and "there was nothing to lift" is a perfectly
// good outcome for whoever wanted dispatch running again.
func (s *Server) handleLiftAgentPause(w http.ResponseWriter, r *http.Request) {
	status, lifted, err := s.tasks.LiftAgentPause()
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, agentPauseResponse{Enabled: true, Pause: &status, Lifted: &lifted})
}
