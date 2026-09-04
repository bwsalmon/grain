package grain

import (
	"encoding/json"
	"time"
)

// Phase is where a grain is in its life. It is the one field Reconcile
// branches on first, and it is deliberately coarse: anything finer -- what
// the shim is doing this second, why a guest was rebuilt -- belongs in
// Activity or Rebuilds, where it informs a human without becoming a
// transition the controller has to have a rule for.
type Phase string

const (
	// PhaseProvisioning is everything before there is an agent: the
	// container starting, the VMM booting a guest, the guest answering,
	// placements landing, the clone and the repo's setup command. It is
	// bounded by Policy.ProvisionBudget rather than by anything the grain
	// knows, because a grain wedged here is precisely one that cannot
	// report that it is wedged.
	PhaseProvisioning Phase = "provisioning"
	// PhaseProvisioned is a grain whose sandbox is real and whose
	// checkout exists, waiting for the prompt. It is the join between the
	// two halves of a start: the controller cannot assemble a prompt
	// until the checkout can be read (Status.Checkout), and it will not
	// spend model tokens on a grain whose checkout failed.
	PhaseProvisioned Phase = "provisioned"
	// PhaseRunning is the agent CLI executing in the container.
	PhaseRunning Phase = "running"
	// PhaseBlocked is the agent waiting on a Request nobody has answered
	// -- a question for a human, a secret, a pull request only the
	// controller can open. Distinct from PhaseRunning because a grain
	// that has been blocked for an hour is waiting on someone, and a
	// grain that has been running for an hour is working.
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
	// This is how the two-phase start gets its facts. The controller's
	// prompt needs things that exist only once there is a working tree --
	// previousAttemptsSection reads the commits earlier attempts pushed,
	// which can only be read from the checkout they are in -- so it ends
	// its own script with the commands that print them and parses what
	// comes back. A shim that understood repositories would be a shim
	// that had to agree with the controller about them.
	Setup *SetupResult `json:"setup,omitempty"`
	// Requests are the escape hatches this grain is waiting on. Only the
	// ones needing the store, GitHub or a human ever appear here.
	Requests []Request `json:"requests,omitempty"`
	// Result is set exactly when Phase is terminal.
	Result *Result `json:"result,omitempty"`
	Health Health  `json:"health"`
	// Seq is the sequence number of the last trajectory record this grain
	// emitted, so a poller knows whether there is anything new without
	// reading it.
	//
	// A sequence rather than a byte offset because the trajectory is
	// carried by the container runtime's own log stream (docs/grain.md,
	// "The trajectory goes to stdout"), and `docker logs`/`kubectl logs`
	// are addressed by time and line rather than by byte. A monotonic
	// per-record sequence is the one cursor both a log stream and a plain
	// file can honour.
	Seq int64 `json:"seq"`
	// Consumed are the Answer and Signal ids this grain has taken
	// delivery of, so a controller stops resending them.
	//
	// It is the acknowledgement half of a spool that is deliberately
	// at-least-once: `grain answer` and `grain signal` are separate
	// processes from the supervisor that acts on them, so they hand over
	// through a directory rather than a call, and a controller cannot
	// tell a write it made from one the supervisor has read. Echoing the
	// ids back is what closes that, and it is why both verbs take an id
	// even though a Signal is a reply to nothing.
	//
	// Bounded rather than complete: a grain need only remember far enough
	// back that a controller polling every tick cannot still be holding
	// an unacknowledged id, not for the life of the run.
	Consumed []string `json:"consumed,omitempty"`
}

// SetupResult is how Spec.Setup ended, verbatim and uninterpreted.
type SetupResult struct {
	// ExitCode is the script's own. Non-zero is what turns
	// PhaseProvisioning into a terminal failure rather than
	// PhaseProvisioned, and is the whole of the shim's opinion about it.
	ExitCode int `json:"exitCode"`
	// Output is the script's combined stdout and stderr, truncated to a
	// bound the shim chooses. Combined rather than split because a setup
	// script's diagnosis is routinely interleaved across both, and the
	// controller is reading it to find out what went wrong.
	Output string `json:"output,omitempty"`
}

// RequestID identifies one Request within one grain.
type RequestID string

// RequestKind is what a Request wants done. Every kind here needs
// something the container does not have and must not be given: the store,
// a GitHub credential, or a human. Anything that needs only the sandbox
// is served inside the grain and never becomes a Request -- rebuilding
// the guest most of all.
type RequestKind string

const (
	KindOpenPullRequest   RequestKind = "open_pull_request"
	KindPullRequestStatus RequestKind = "pull_request_status"
	KindWaitForChecks     RequestKind = "wait_for_checks"
	KindAskQuestion       RequestKind = "ask_question"
	KindRequestSecret     RequestKind = "request_secret"
	KindCommentOnIssue    RequestKind = "comment_on_issue"
	KindProposeTask       RequestKind = "propose_task"
	KindAddReviewComment  RequestKind = "add_review_comment"
)

// Request is one thing a grain has asked the controller to do for it.
type Request struct {
	ID      RequestID       `json:"id"`
	Kind    RequestKind     `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Raised  time.Time       `json:"raised"`
}

// Answer settles a Request. A refusal is an answer: Err set and OK false
// tells the agent it asked for something it will not get, which is a turn
// it can act on, rather than leaving it blocked until its deadline.
type Answer struct {
	// Contract is the wire version this document is written to.
	Version string          `json:"version"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Err     string          `json:"err,omitempty"`
}

// SignalKind is what a Signal carries.
type SignalKind string

const (
	// SignalPrompt starts the agent. It is the second half of the
	// two-phase start and the only Signal a grain requires.
	SignalPrompt SignalKind = "prompt"
	// SignalAddenda folds comments a human added since the grain started
	// into the conversation as fresh turns.
	SignalAddenda SignalKind = "addenda"
	// SignalCancel asks the grain to stop and report a terminal phase. If
	// it does not, Release destroys it, which is why cancellation needs
	// no cooperation to be effective.
	SignalCancel SignalKind = "cancel"
	// SignalPause stops the agent because the deployment met its own
	// usage limit -- what orchestrator.Pause broadcasts today by
	// cancelling every registered run's context.
	SignalPause SignalKind = "pause"
)

// Signal is something the controller delivers unasked.
type Signal struct {
	// Contract is the wire version this document is written to.
	Version string     `json:"version"`
	Kind    SignalKind `json:"kind"`
	Prompt  string     `json:"prompt,omitempty"`  // SignalPrompt
	Addenda []string   `json:"addenda,omitempty"` // SignalAddenda, oldest first
	Reason  string     `json:"reason,omitempty"`  // SignalCancel, SignalPause
}

// Outcome vocabulary, matching what model.Store.FinishRun already
// records. It is open, the way metrics.Runs.Outcomes' own doc comment
// says: task_streak counts anything that is not OutcomeSucceeded the same
// way, so a new word here backs off and retries like the rest.
const (
	OutcomeSucceeded   = "succeeded"
	OutcomeFailed      = "failed"
	OutcomeCancelled   = "cancelled"
	OutcomeNoAction    = "no_action"
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
	// Deferred are the escape hatches the agent raised that nobody had to
	// answer for it to finish -- a question it asked on its way out, a
	// task it proposed.
	Deferred []Request `json:"deferred,omitempty"`
	Usage    Usage     `json:"usage"`
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
