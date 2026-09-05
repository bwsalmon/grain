package ui

import (
	"context"
	"net/http"
)

// SandboxSnapshot is one live sandbox's status, as GET /api/sandboxes
// reports it -- one per run currently in flight, so an idle deployment
// reports none rather than one row per slot, most of them idle.
// Deliberately its own type
// rather than orchestrator.SandboxHealth itself: this package does not import
// pkg/orchestrator (a presentation-layer package importing core dispatch
// logic runs the wrong way), so cmd/grain/daemon.go's own
// sandboxHealthAdapter is the one place both types are ever in scope,
// converting one into the other field for field.
type SandboxSnapshot struct {
	Sandbox       string `json:"sandbox"`
	Backend       string `json:"backend"`
	Name          string `json:"name"`
	Ready         bool   `json:"ready"`
	Error         string `json:"error,omitempty"`
	LoadAverage   string `json:"loadAverage,omitempty"`
	MemoryUsedMB  int    `json:"memoryUsedMB,omitempty"`
	MemoryTotalMB int    `json:"memoryTotalMB,omitempty"`
	DiskUsedMB    int    `json:"diskUsedMB,omitempty"`
	DiskTotalMB   int    `json:"diskTotalMB,omitempty"`
	// NestedVirt is whether this sandbox can run virtual machines of its
	// own -- orchestrator.SandboxHealth.NestedVirt's own four states
	// ("ready", "denied", "no-device", "unsupported"), carried through
	// as the string it is rather than reduced to a boolean, since the
	// three ways of not being ready each want a different fix. Empty
	// when the backend gave no reading, which the pane shows as a dash.
	NestedVirt string `json:"nestedVirt,omitempty"`
}

// SandboxHealth is implemented by whatever can report every live
// sandbox's current status -- cmd/grain/daemon.go's own
// sandboxHealthAdapter over orchestrator.KonturSandboxes/HostSandboxes'
// own Health methods, in a real deployment. See Config.Sandboxes' own
// doc comment for the nil-means-unavailable contract this interface's
// zero value (nil) satisfies.
type SandboxHealth interface {
	Health(ctx context.Context) []SandboxSnapshot
}

// DiskUsage is one filesystem's own size and usage, as one entry of
// HostPressure.Disks below.
//
// A deployment has more than one disk worth watching (grain/task-148):
// terraform/gcp gives the store a small volume of its own and puts
// everything a *sandbox* writes -- docker's data root, so the sandbox
// image and every kontur VM's qcow2 overlay, plus HostSandboxes' per-run
// checkouts -- on a second, much larger one. That second volume is the
// one a runaway build actually fills while the store's sits near-empty,
// so a single "disk" figure taken from the store's own filesystem reads
// as healthy at exactly the moment the deployment is running out.
type DiskUsage struct {
	// Holds names what of the daemon's own state sits on this filesystem
	// -- "store" (-data-dir), "sandboxes" (-sandbox-dir), "docker"
	// (docker's data root). More than one when two of those turn out to
	// be the same disk, which is the ordinary case on a developer's
	// machine and on any deployment without terraform/gcp's own separate
	// sandbox volume: they are folded into one entry rather than
	// reported as two identical rows (cmd/grain/daemon.go's hostDisks).
	Holds []string `json:"holds"`
	// Path is the path the reading was taken through -- the first of
	// Holds' own paths, since a filesystem answers the same for all of
	// them. Shown so an operator can run `df` against the same thing.
	Path string `json:"path"`
	// UsedMB and TotalMB are the numbers `df` prints for that
	// filesystem. 0/0 means no reading, which is also what Error being
	// set implies: the pane shows a dash rather than a 0 that would read
	// as an empty disk.
	UsedMB  int `json:"usedMB"`
	TotalMB int `json:"totalMB"`
	// Error is why this one filesystem has no figure -- a path that has
	// stopped answering statfs, which for a disk that was reported a
	// moment ago means a volume that is no longer mounted. Carried per
	// disk rather than failing the whole host reading, the same way
	// HostError below keeps a failing host reading from taking the
	// sandbox list with it: the other disks' figures are unaffected, and
	// "the sandbox volume went away" is the answer this pane exists to
	// give rather than a reason to show nothing.
	Error string `json:"error,omitempty"`
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
	// Disks is one entry per filesystem the daemon's own state sits on,
	// in the order the pane shows them: the store's first, then the
	// sandbox root, then docker's data root when it is a filesystem
	// neither of those already covered. It replaces the single
	// disk figure this carried while -data-dir's own volume was assumed
	// to be the one that fills (grain/task-41, then grain/task-148 once
	// terraform/gcp gave sandboxes a volume of their own).
	//
	// Empty when no reading could be taken at all, which is how a
	// non-Linux host reads: the pane shows the load and memory beside it
	// rather than going down over a number it could not get.
	Disks []DiskUsage `json:"disks,omitempty"`
	// NestedVirt and NestedVirtDetail are whether this machine's own KVM
	// will let the VMs it boots run VMs in turn -- sysstat's own
	// "enabled"/"disabled"/"unavailable" and the sysfs reading behind it
	// ("kvm_intel nested=Y").
	//
	// The host half of the same question SandboxSnapshot.NestedVirt
	// answers per sandbox, and shown beside it because neither is much
	// use alone: a sandbox reporting no CPU virtualization flag is
	// either a host with nesting off or a guest configured without it,
	// and this is what tells those apart.
	NestedVirt       string `json:"nestedVirt,omitempty"`
	NestedVirtDetail string `json:"nestedVirtDetail,omitempty"`
}

// sandboxHealthResponse is GET /api/sandboxes' whole body. Enabled is
// false, with nothing else set, when this deployment's UI has neither
// Config.Sandboxes nor Config.HostStats configured -- the same
// nil-means-unavailable convention logSourcesResponse's own Enabled
// already establishes for the Logs pane the System overlay
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
