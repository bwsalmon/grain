// Package grain is how a controller manages a fleet of grains: the seam
// it drives them through (Grains, Grain) and the whole of its per-grain
// policy (Reconcile, Policy).
//
// What one grain *is* -- its Spec, its Status, the records it emits, the
// files and environment it is configured by -- is pkg/granule's, and a
// granule never imports anything from here. That is the boundary: this
// package is graind's view of many, and that one is the contract with
// each.
//
// A grain is a container: the agent CLI, a kontur VMM, and the guest VM
// that VMM boots, with a shim as PID 1 holding the three together. The
// guest is the sandbox -- the repo's own checkout, builds and tests --
// and the agent reaches it over the container-local vsock socket
// cloud-hypervisor exposes at /run/kontur/vsock.sock. The agent's own
// credential lives in the container, on the far side of that socket from
// anything the repo's code can run, which is the isolation the VM is
// there for.
//
// This inverts what pkg/orchestrator does today. There, the agent CLI is
// a subprocess on the controller and every tool call is a `docker exec
// <container> kontur exec` round trip into the VM; a run's liveness is a
// goroutine's stack in the daemon, which is why a setup failure can
// strand a live row (cycle.go's own comment on it), why InFlight and
// drainInFlight exist, and why orphan.go and recover.go sweep at startup.
// Moving the agent next to its VM and having the controller *poll* takes
// all of that out: liveness is the container's, identity is derivable, and
// reattach after a controller restart is the ordinary path rather than a
// startup pass.
//
// # The line this package draws
//
// Anything that touches only the sandbox is local to the grain and never
// reaches the controller: running a command, reading and writing files,
// and -- the one that used to be a whole subsystem -- throwing the guest
// away and building a fresh one. Recreating a sandbox is a local kontur
// call now, so pkg/orchestrator's SandboxRecreations registry, its
// lookup-by-task-ID, its four restore methods and the
// POST /api/tasks/{id}/sandbox/recreate hop behind them all go: the Spec
// that says how to rebuild sits in the container next to the thing being
// rebuilt, so a rebuild replays Spec.Placements rather than coordinating
// a re-mint with a controller that holds the only copy.
//
// Everything else is not a tool at all. Whatever a deployment wants an
// agent to be able to ask for -- open a pull request, wait on checks, ask
// a human a question -- it puts a CLI in the guest and a credential
// beside it as a Spec.Placement, and the agent runs it with run_command
// like anything else it runs. Grain holds no vocabulary for any of it.
//
// That is the git proxy's shape, reused rather than reinvented: git
// already reaches a controller-side service from inside the guest, with
// its token as a placement and its authorization resolved through the
// live run (model.Store.GitScope, and authorize.go's "a sandbox with no
// live run authorizes nothing"). A second service on the same pattern
// costs no new mechanism, and a leaked credential is dead the moment the
// run ends.
//
// It also means a grain needs no controller to run: nothing attaches,
// nothing is held open, nothing waits, and a dropped connection cannot
// cost an agent its tools an hour in. The container needs no daemon URL,
// no task ID and no bearer token of its own -- the three things
// agent.RunConfig currently carries so that a forked mcpserver can call
// back into the daemon.
//
// # Why polling
//
// Every method here is idempotent, none blocks on the work, and what a
// controller reads is a whole snapshot rather than a delta. It compares
// that to what it wants and issues at most one round of actions per tick
// (Reconcile). Level-triggered, the same discipline
// orchestrator.Reconciler already states: running one is always safe and
// skipping one costs latency rather than correctness.
//
// What is polled is the container runtime, not the grain. Nothing here is
// a call into a shim: a grain's whole output is the records it writes to
// stdout and the file it leaves at FileTerminationLog, and its whole
// input arrives before it starts, as environment and files. It has no
// inbound surface at all -- nothing to authenticate, nothing to version
// as an API, and nothing that can hang.
//
// The direction still matters as much as the shape. The controller
// reaches in; the grain never reaches out. A grain that has stopped
// producing records while its container is still listed is a wedged shim,
// which is a state the controller can act on -- as against a grain whose
// push failed, which is silence it cannot tell from health.
package grain

import (
	"context"

	"github.com/bwsalmon/grain/pkg/granule"
)

// Grains is one backend's fleet. KonturGrains is the real one; HostGrains
// runs the agent as a plain subprocess against a directory, so the test
// suite does not need a VM per case.
type Grains interface {
	// Create asks for a grain matching spec, under the name the
	// controller derived for it. It is idempotent by that id -- asking
	// twice returns the grain that exists rather than building a second
	// -- and returns as soon as the ask is durable, never
	// waiting for a boot. Waiting is the thing that put a VM boot on a
	// caller's stack and stranded rows when it failed; here a boot is a
	// Phase (PhaseProvisioning) the next poll observes.
	Create(ctx context.Context, id granule.ID, spec granule.Spec) (Grain, error)

	// List reports every grain this backend holds, whether or not the
	// controller knows about it. It is the poll primitive -- one call per
	// tick rather than one per grain -- and it is also, on its own, both
	// reattach-after-restart and the reaping of grains no live run
	// claims. There is no separate orphan pass because there is nothing
	// a separate pass would learn that this does not.
	//
	// It is served from two things a controller reads anyway: the
	// container runtime's own listing, for which grains exist and whether
	// each is running, and the log stream each one is already tailed for,
	// whose latest KindStatus record is that grain's state. Those two are
	// the whole of it -- there is no third read and no exec, so a tick
	// costs the same whether the fleet is healthy or not.
	//
	// The backend is what merges them, which is why a Status coming back
	// from here carries fields no grain emits: ID, from the container the
	// record was read out of, and Health.Container, from the listing. A
	// grain cannot report that it is unreachable.
	//
	// A grain whose container is running and whose stream has nothing
	// recent is not a gap in this: it is a wedged shim, which is a state
	// worth being able to see, and one an exec could not have told apart
	// -- whatever a status subcommand printed would come from the same
	// wedged shim.
	List(ctx context.Context) ([]granule.Status, error)

	// Get re-attaches to one grain by name.
	Get(ctx context.Context, id granule.ID) (Grain, error)
}

// Grain is one unit of agent work.
//
// There is deliberately no Rebuild here. Rebuilding the guest is internal
// to the grain -- the agent asks kontur, in its own container, over a
// local MCP call -- and the controller learns of it only as
// Status.Rebuilds going up. What the controller keeps is the policy that
// needs a view the grain does not have: a grain rebuilding over and over
// is one to kill, and Policy.MaxRebuilds is where that is decided.
type Grain interface {
	ID() granule.ID

	// Transcript reads this run's trajectory from a cursor -- the
	// sequence number of the last record the caller saw, Status.Seq's own
	// counter -- and returns the next one. Nothing here touches the
	// sandbox: the trajectory is the shim's own output, not the guest's.
	//
	// It is a cursor rather than a stream because the trajectory is
	// carried on the container's stdout and read back with the runtime's
	// own log stream (docs/grain.md, "Poll for state, logs for the trajectory").
	// That lets a backend serve a UI watching a grain live from a
	// `docker logs -f` held open for as long as somebody is watching, and
	// serve everyone else one call at a time, without the two being
	// different interfaces. The runtime buffers, so a caller that goes
	// away -- a controller restarting mid-run -- resumes from its cursor
	// rather than losing what it missed, and a caller that stops reading
	// never applies backpressure to the agent.
	//
	// The trajectory a run is judged by is still the controller's to
	// keep: container logs rotate, so this is how a transcript is
	// *carried*, never where it is stored.
	Transcript(ctx context.Context, from int64) (chunk []byte, next int64, err error)

	// Release destroys the grain: the container, the VMM inside it and
	// the guest under that, in one operation. Idempotent.
	//
	// It is also how a grain is cancelled, and there is no separate
	// mechanism for that. Stopping a container sends SIGTERM and waits
	// out a grace period before SIGKILL, so the shim -- which is PID 1 --
	// gets that window to stop the agent, write its Result, and power the
	// guest down. That is the whole of a graceful cancellation, and it is
	// the pattern kontur already holds to: its ShutdownTimeout bounds how
	// long the runtime waits for a guest to power off after SIGTERM, and
	// its pod manifest's terminationGracePeriodSeconds "must comfortably
	// exceed" it.
	//
	// Being abrupt costs nothing that today does not already cost:
	// watchForTaskClosed and Pause.register both just cancel the run's
	// context, killing the agent mid-turn. And a pushed branch survives a
	// SIGKILL, because salvagePushedBranch asks GitHub whether the branch
	// is there rather than asking the run.
	//
	// **A grain must never be restarted.** A restarted one boots a fresh
	// guest, re-runs its setup and starts the agent again on the same
	// prompt while the controller still believes it is the same run: seq
	// resets and the trajectory interleaves two runs. Kubernetes needs
	// restartPolicy: Never said explicitly -- kontur's own static pod
	// manifest says Always, which is right for a long-lived VM and wrong
	// for this. Docker's default is already correct.
	Release(ctx context.Context) error
}
