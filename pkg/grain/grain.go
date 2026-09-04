// Package grain is one unit of agent work and the seam a controller
// drives it through.
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
// Anything that touches the store, GitHub or a human is a Request the
// grain raises and the controller answers on its next poll. That is the
// whole of what leaves the container, and it means the container needs no
// daemon URL, no task ID and no bearer token of its own -- the three
// things agent.RunConfig currently carries so that a forked mcpserver can
// call back into the daemon.
//
// # Why polling
//
// Every method here is idempotent, none blocks on the work, and Observe
// returns the whole of what can be seen rather than a delta. The
// controller compares that answer to what it wants and issues at most one
// round of actions per tick (Reconcile). Level-triggered, the same
// discipline orchestrator.Reconciler already states: running one is
// always safe and skipping one costs latency rather than correctness.
//
// The direction matters as much as the shape. The controller reaches in;
// the grain never reaches out. A grain that cannot be polled is a grain
// that has failed, which is a state the controller can act on -- as
// against a grain whose push failed, which is silence it cannot tell from
// health.
package grain

import "context"

// ID names one grain. It is derived from the run it serves
// (dispatch.RunID) and never allocated by a backend, because a controller
// that restarts has to be able to name every grain it left running
// without holding a handle to it: List plus a derivable name is the whole
// of reattach.
type ID string

// Grains is one backend's fleet. KonturGrains is the real one; HostGrains
// runs the agent as a plain subprocess against a directory, so the test
// suite does not need a VM per case.
type Grains interface {
	// Create asks for a grain matching spec. It is idempotent by spec.ID
	// -- asking twice returns the grain that exists rather than building
	// a second -- and returns as soon as the ask is durable, never
	// waiting for a boot. Waiting is the thing that put a VM boot on a
	// caller's stack and stranded rows when it failed; here a boot is a
	// Phase (PhaseProvisioning) the next poll observes.
	Create(ctx context.Context, spec Spec) (Grain, error)

	// List reports every grain this backend holds, whether or not the
	// controller knows about it. It is the poll primitive -- one call per
	// tick rather than one per grain -- and it is also, on its own, both
	// reattach-after-restart and the reaping of grains no live run
	// claims. There is no separate orphan pass because there is nothing
	// a separate pass would learn that this does not.
	List(ctx context.Context) ([]Status, error)

	// Get re-attaches to one grain by name.
	Get(ctx context.Context, id ID) (Grain, error)
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
	ID() ID

	// Observe is the whole of what the controller can see: one call, one
	// round trip, everything. Fat by design -- the poll is the only read,
	// so a field left out here is a second exec per grain per tick.
	Observe(ctx context.Context) (Status, error)

	// Answer settles a Request the grain raised. Answering an unknown or
	// already-settled request is a no-op, so a controller that polls
	// again before the shim has consumed the first answer is harmless --
	// which is the property that lets Reconcile be re-run freely.
	Answer(ctx context.Context, req RequestID, ans Answer) error

	// Signal delivers something the grain did not ask for: the prompt at
	// the start of the second phase, a comment a human added mid-run, a
	// cancellation, a usage-limit pause. One mechanism replacing three --
	// orchestrator's addendaPoller, watchForTaskClosed and
	// Pause.register, each of which exists because a run in flight had no
	// address anything could deliver to.
	Signal(ctx context.Context, sig Signal) error

	// Transcript tails this run's output from a byte offset, for a UI
	// watching a grain live, and returns the next offset. It reads a
	// container-local file: no guest hop, so tailing costs one exec and
	// does not touch the sandbox at all.
	//
	// This is the one place the polled control plane is not enough: tick
	// granularity reads as lag to a human watching a run work. A backend
	// may therefore serve a watched grain from an exec held open for as
	// long as somebody is watching, rather than one exec per call --
	// still the same transport and still no credential in the container,
	// and nothing at all when nobody is watching. The offsets make the
	// two indistinguishable to a caller, which is why the seam is a
	// cursor rather than a stream.
	Transcript(ctx context.Context, from int64) (chunk []byte, next int64, err error)

	// Release destroys the grain: the container, the VMM inside it and
	// the guest under that, in one operation. Idempotent, and safe on a
	// grain that is still running -- destroying the container is how a
	// cancellation that did not take gets enforced, rather than a
	// detached context racing a deferred cleanup.
	Release(ctx context.Context) error
}
