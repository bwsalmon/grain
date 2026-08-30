package ui

import (
	"context"
	"net/http"
)

// SandboxSnapshot is one dispatch slot's live sandbox status, as GET
// /api/sandboxes reports it. Deliberately its own type rather than
// orchestrator.SlotHealth itself: this package does not import
// pkg/orchestrator (a presentation-layer package importing core dispatch
// logic runs the wrong way), so cmd/grain/daemon.go's own
// sandboxHealthAdapter is the one place both types are ever in scope,
// converting one into the other field for field.
type SandboxSnapshot struct {
	Slot          string `json:"slot"`
	Backend       string `json:"backend"`
	Name          string `json:"name"`
	Ready         bool   `json:"ready"`
	Error         string `json:"error,omitempty"`
	LoadAverage   string `json:"loadAverage,omitempty"`
	MemoryUsedMB  int    `json:"memoryUsedMB,omitempty"`
	MemoryTotalMB int    `json:"memoryTotalMB,omitempty"`
}

// SandboxHealth is implemented by whatever can report every dispatch
// slot's current sandbox status -- cmd/grain/daemon.go's own
// sandboxHealthAdapter over orchestrator.KonturSandboxes/HostSandboxes'
// own Health methods, in a real deployment. See Config.Sandboxes' own
// doc comment for the nil-means-unavailable contract this interface's
// zero value (nil) satisfies.
type SandboxHealth interface {
	Health(ctx context.Context) []SandboxSnapshot
}

// HostPressure is one point-in-time reading of the machine this
// deployment's own daemon process runs on, as GET /api/sandboxes' own
// host section reports it -- see Config.HostStats' own doc comment on why
// this is reported separately from a sandbox's own usage above.
type HostPressure struct {
	LoadAverage1  float64 `json:"loadAverage1"`
	LoadAverage5  float64 `json:"loadAverage5"`
	LoadAverage15 float64 `json:"loadAverage15"`
	MemoryUsedMB  int     `json:"memoryUsedMB"`
	MemoryTotalMB int     `json:"memoryTotalMB"`
}

// sandboxHealthResponse is GET /api/sandboxes' whole body. Enabled is
// false, with nothing else set, when this deployment's UI has neither
// Config.Sandboxes nor Config.HostStats configured -- the same
// nil-means-unavailable convention logSourcesResponse's own Enabled
// already establishes for the Logs pane the debug section
// (bwsalmon/agents#536) puts this one alongside.
type sandboxHealthResponse struct {
	Enabled   bool              `json:"enabled"`
	Sandboxes []SandboxSnapshot `json:"sandboxes,omitempty"`
	Host      *HostPressure     `json:"host,omitempty"`
	// HostError carries Config.HostStats' own error (e.g. this process is
	// not running on Linux, see pkg/sysstat's doc comment) without taking
	// the rest of the pane down with it -- a caller with a configured
	// Config.Sandboxes but a failing HostStats reading still gets its
	// sandbox list.
	HostError string `json:"hostError,omitempty"`
}

func (s *Server) handleGetSandboxHealth(w http.ResponseWriter, r *http.Request) {
	if s.tasks.Config.Sandboxes == nil && s.tasks.Config.HostStats == nil {
		writeJSON(w, http.StatusOK, sandboxHealthResponse{Enabled: false})
		return
	}
	resp := sandboxHealthResponse{Enabled: true}
	if s.tasks.Config.Sandboxes != nil {
		resp.Sandboxes = s.tasks.Config.Sandboxes.Health(r.Context())
	}
	if s.tasks.Config.HostStats != nil {
		if host, err := s.tasks.Config.HostStats(); err != nil {
			resp.HostError = err.Error()
		} else {
			resp.Host = &host
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
