package grain

import "time"

// Phase is where a grain is in its life. It is the one field Reconcile
// branches on first, and it is deliberately coarse: anything finer -- what
// the shim is doing this second, why a guest was rebuilt -- belongs in
// Activity or Rebuilds, where it informs a human without becoming a
// transition the controller has to have a rule for.
type Phase string

const (
	// PhaseProvisioning is everything before there is an agent: the
	// container starting, the VMM booting a guest, the guest answering,
	// placements landing, and Setup -- the clone and the repo's own setup
	// command. A grain goes straight from here to PhaseRunning: the
	// prompt is a file it already has (FilePrompt), so there is nothing
	// to wait for in between. It is
	// bounded by Policy.ProvisionBudget rather than by anything the grain
	// knows, because a grain wedged here is precisely one that cannot
	// report that it is wedged.
	PhaseProvisioning Phase = "provisioning"
	// PhaseRunning is the agent CLI executing in the container.
	PhaseRunning Phase = "running"
	// PhaseBlocked is the agent waiting on an upstream tool call -- a
	// question for a human, a secret, a pull request. Distinct from
	// PhaseRunning because a grain blocked for an hour is waiting on
	// someone, and one running for an hour is working.
	PhaseBlocked Phase = "blocked"

	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
	PhaseCancelled Phase = "cancelled"

	// PhaseLost is the container unreachable: the grain is gone, agent
	// and guest with it, and there is nothing left to ask. It does not
	// cover a wedged *guest* -- the shim rebuilds that itself, and the
	// controller sees only Rebuilds go up -- which is why this has no
	// repair path and goes straight to fail-and-release.
	PhaseLost Phase = "lost"
	// PhaseReleased is the container destroyed. Kept as a phase rather
	// than dropped from the fleet so that a tick can tell "cleaned up"
	// from "never seen", and so releasing stays idempotent.
	PhaseReleased Phase = "released"
)

// Terminal reports whether this phase is one a grain does not leave on
// its own.
func (p Phase) Terminal() bool {
	switch p {
	case PhaseSucceeded, PhaseFailed, PhaseCancelled:
		return true
	}
	return false
}

// Status is the whole of what one poll returns. Every field is here
// rather than behind its own call because the poll is the only read: a
// field split out is a second exec per grain per tick.
type Status struct {
	// Version is the wire format this document is written to.
	Version string `json:"version"`
	// ID is the grain this describes, and is deliberately not on the
	// wire: a backend fills it in from the container it read the document
	// out of, because the container is the identity. A controller execs
	// into one specific container to get a status, so the answer cannot
	// be ambiguous about whose it is, and a grain telling you its own
	// name would be repeating what you had to know to ask.
	//
	// Nothing else is echoed back either -- no task, no repo, no
	// framework. The controller keys by this and looks the rest up in its
	// own store, which is the one place any of it is true.
	ID ID `json:"-"`

	Phase Phase `json:"phase"`
	// Since is when Phase was entered. Every timeout the controller
	// enforces is a subtraction against this, so a grain needs no clock
	// agreement with the controller beyond it.
	Since time.Time `json:"since"`
	// Activity is the grain's own short account of itself -- "cloning
	// acme/widgets", "waiting for CI". It is what today's update_status
	// tool writes, except that it is now read off this poll instead of
	// posted to the daemon, so it costs the grain nothing and cannot
	// fail.
	Activity string `json:"activity,omitempty"`
	// Rebuilds counts the times this grain threw its guest away and built
	// a fresh one. The decision is the grain's; the count is here so the
	// controller can see a grain thrashing and end it.
	Rebuilds int `json:"rebuilds,omitempty"`

	// Setup is what Spec.Setup did, once it has run. The shim reports its
	// exit code and output without reading either: the controller wrote
	// that script, so it is the one that knows what its output means.
	//
	// It is the diagnosis for a grain that failed before its agent ever
	// ran, which is the case a bare outcome explains worst.
	Setup *SetupResult `json:"setup,omitempty"`
	// Upstream is this grain's connection to the controller's MCP
	// server, which is where every tool but BuiltinTools comes from.
	Upstream Upstream `json:"upstream"`

	// Result is set exactly when Phase is terminal.
	Result *Result `json:"result,omitempty"`
	Health Health  `json:"health"`
	// Seq is the sequence number of the last trajectory record this grain
	// emitted, so a poller knows whether there is anything new without
	// reading it.
	//
	// A sequence rather than a byte offset because the trajectory is
	// carried by the container runtime's own log stream (docs/grain.md,
	// "Poll for state, logs for the trajectory"), and `docker logs`
	// are addressed by time and line rather than by byte. A monotonic
	// per-record sequence is the one cursor both a log stream and a plain
	// file can honour.
	Seq int64 `json:"seq"`
}

// SetupResult is how Spec.Setup ended, verbatim and uninterpreted.
type SetupResult struct {
	// ExitCode is the script's own. Non-zero is what ends the grain
	// before its agent starts, and is the whole of the shim's opinion
	// about it -- which is also what makes a failed checkout cost no
	// model tokens.
	ExitCode int `json:"exitCode"`
	// Output is the script's combined stdout and stderr, truncated to a
	// bound the shim chooses. Combined rather than split because a setup
	// script's diagnosis is routinely interleaved across both, and the
	// controller is reading it to find out what went wrong.
	Output string `json:"output,omitempty"`
}

// Upstream is the state of a grain's connection to the controller's MCP
// server -- the one the controller attaches by exec'ing `grain proxy`,
// and over which every tool that is not a built-in is served.
//
// The shim terminates the agent's own MCP session rather than proxying it
// through, which is what makes a dropped upstream survivable. MCP is
// session-oriented: a client initializes once and a dead transport takes
// the whole session with it, and clients generally mark such a server
// failed rather than re-initializing. A grain that merely piped bytes
// would therefore lose those tools for the rest of the run -- an hour in,
// silently -- rather than for a tick. Terminating means the agent's
// connection is to the shim, the shim never closes it, and a drop is
// something only the shim sees.
//
// What the shim does while detached is hold: in-flight and new calls to
// upstream tools wait rather than failing, so the agent goes on working
// with the sandbox tools -- most of what it does -- and blocks only if it
// reaches for an upstream one. The controller reattaches on its next tick
// and the held calls go through. If it never returns, Spec.MaxRuntime
// ends the grain like any other stall.
type Upstream struct {
	// Attached is whether a controller is connected right now. A grain
	// running with this false is one to attach to, which is a reconcile
	// row.
	Attached bool `json:"attached"`
	// Pending is how many calls the shim is holding for a controller that
	// is not there. Reported because a grain with calls piling up and
	// nobody attached is a situation worth seeing, and the phase alone
	// does not say it.
	Pending int `json:"pending,omitempty"`
	// Since is when Attached last changed.
	Since time.Time `json:"since,omitempty"`
}

// Outcome vocabulary, matching what model.Store.FinishRun already
// records. It is open, the way metrics.Runs.Outcomes' own doc comment
// says: task_streak counts anything that is not OutcomeSucceeded the same
// way, so a new word here backs off and retries like the rest.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"
	OutcomeNoAction  = "no_action"
	// OutcomeCancelled is also what a grain killed for a closed task or a
	// paused deployment gets: the controller supplies it, because
	// destroying the container is how those stop and there may be no
	// Result to read.
	OutcomeSetupFailed = "setup-failed"
	// OutcomeLost is the container gone out from under a live run --
	// distinct from "failed" because nothing was attempted that could
	// have gone wrong on its own terms.
	OutcomeLost = "lost"
	// OutcomeThrashing is a grain that rebuilt its guest past
	// Policy.MaxRebuilds. The rebuilds themselves are legitimate repair;
	// what this names is repair that is not converging.
	OutcomeThrashing = "thrashing"
)

// Result is how a grain ended.
type Result struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	// Pushed is the branch this grain got onto the remote, if any. It is
	// here even on a failed grain, and that is the point: an agent that
	// commits, pushes and then runs out of turns did the work and only
	// the ending failed. Today salvaging that branch is a special case in
	// runOne's error path; here it is a field the ordinary finish path
	// reads.
	Pushed *PushedBranch `json:"pushed,omitempty"`
	Usage  Usage         `json:"usage"`
}

// PushedBranch is a branch and the commit at its head.
type PushedBranch struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

// Usage is what this grain spent.
type Usage struct {
	Turns        int      `json:"turns,omitempty"`
	InputTokens  int64    `json:"inputTokens,omitempty"`
	OutputTokens int64    `json:"outputTokens,omitempty"`
	Wall         Duration `json:"wall,omitempty"`
}

// Health is a grain's two halves, which fail independently and mean
// different things: a container that is fine around a guest that is not
// is a grain that can repair itself, and the reverse is a grain that
// cannot report anything at all.
type Health struct {
	Container ContainerHealth `json:"container"`
	Guest     GuestHealth     `json:"guest"`
}

// ContainerHealth is the grain itself, as the container runtime sees it.
type ContainerHealth struct {
	Running bool `json:"running"`
	// Err is why the container could not be reached just now, empty when
	// Running is true.
	Err string `json:"err,omitempty"`
}

// GuestHealth is the sandbox, read over vsock -- what
// orchestrator.SandboxHealth reports today, per grain rather than per
// deployment.
type GuestHealth struct {
	Ready         bool   `json:"ready"`
	Err           string `json:"err,omitempty"`
	LoadAverage   string `json:"loadAverage,omitempty"`
	MemoryUsedMB  int    `json:"memoryUsedMB,omitempty"`
	MemoryTotalMB int    `json:"memoryTotalMB,omitempty"`
	DiskUsedMB    int    `json:"diskUsedMB,omitempty"`
	DiskTotalMB   int    `json:"diskTotalMB,omitempty"`
	// ConntrackCount and ConntrackMax are the pod namespace's connection
	// tracking table, which the guest's traffic fills and the guest
	// cannot see.
	//
	// They are here because of what the network decision costs
	// (docs/grain.md, "The network: NAT mode"). Under NAT every flow
	// leaving the guest takes an entry, and a full table drops packets
	// rather than slowing them down -- which inside the guest reads as
	// connection timeouts and hanging fetches, indistinguishable from a
	// flaky test or a registry having a bad day. The table lives in the
	// pod's namespace, outside the VM, so nothing the agent can run will
	// show it: without this field the agent forms the wrong hypothesis
	// and spends turns on it, and the transcript gives a human the same
	// misleading evidence.
	//
	// Reported rather than merely recorded: the shim is expected to tell
	// the agent when the table is under pressure, which is the difference
	// between a failure it can reason about and one it cannot.
	//
	// 0/0 on a backend with no such table -- HostGrains, and any kontur
	// deployment running flat mode, whose spliced datapath puts no guest
	// traffic through netfilter at all. Flat is not a legacy here: it
	// stays the right mode wherever nothing at the container layer needs
	// network (docs/grain.md), so 0/0 is an ordinary reading and not a
	// backend that failed to report.
	ConntrackCount int `json:"conntrackCount,omitempty"`
	ConntrackMax   int `json:"conntrackMax,omitempty"`
}
